package user

import (
	"errors"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// PinnedChannelModel 用户置顶频道模型
type PinnedChannelModel struct {
	ID          int64     `db:"id"`
	UID         string    `db:"uid"`
	SpaceID     string    `db:"space_id"`
	ChannelID   string    `db:"channel_id"`
	ChannelType uint8     `db:"channel_type"`
	SortOrder   int       `db:"sort_order"`
	CreatedAt   time.Time `db:"created_at"`
}

// PinnedDB 置顶频道数据库操作
type PinnedDB struct {
	session *dbr.Session
	ctx     *config.Context
	log.Log
}

// NewPinnedDB 创建 PinnedDB
func NewPinnedDB(ctx *config.Context) *PinnedDB {
	return &PinnedDB{
		session: ctx.DB(),
		ctx:     ctx,
		Log:     log.NewTLog("PinnedDB"),
	}
}

// 错误定义
var (
	ErrPinnedLimitExceeded = errors.New("置顶数量已达上限")
	ErrPinnedAlreadyExists = errors.New("该频道已置顶")
)

// Add 添加置顶频道。
//
// 并发与一致性：
//   - 唯一索引 uk_user_space_channel 保证 (uid, space_id, channel_id, channel_type) 不会重复，
//     因此使用 INSERT IGNORE 即可检测重复，无需 SELECT ... FOR UPDATE。
//   - 数量上限在事务内 "INSERT 后再 COUNT" 检查：若超出上限则回滚。
//     InnoDB 默认的 REPEATABLE READ 下，COUNT 能看见本事务刚插入的行；
//     极端并发下两个会话同时插入的总数可能短暂达到 max+1，届时后 commit 的一方
//     会看到 COUNT > max 并回滚，最终稳定状态仍满足上限。
func (d *PinnedDB) Add(uid, spaceID, channelID string, channelType uint8, maxLimit int) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	var maxSort int
	if _, err := tx.SelectBySql(
		"SELECT IFNULL(MAX(sort_order), 0) FROM user_pinned_channel WHERE uid=? AND space_id=?",
		uid, spaceID,
	).Load(&maxSort); err != nil {
		return err
	}

	result, err := tx.InsertBySql(
		"INSERT IGNORE INTO user_pinned_channel (uid, space_id, channel_id, channel_type, sort_order, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		uid, spaceID, channelID, channelType, maxSort+1, time.Now(),
	).Exec()
	if err != nil {
		return err
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return ErrPinnedAlreadyExists
	}

	var count int
	if _, err := tx.SelectBySql(
		"SELECT COUNT(*) FROM user_pinned_channel WHERE uid=? AND space_id=?",
		uid, spaceID,
	).Load(&count); err != nil {
		return err
	}
	if count > maxLimit {
		// 超过上限：事务回滚撤销本次插入。
		return ErrPinnedLimitExceeded
	}

	return tx.Commit()
}

// Remove 移除置顶频道
func (d *PinnedDB) Remove(uid, spaceID, channelID string, channelType uint8) error {
	_, err := d.session.DeleteFrom("user_pinned_channel").
		Where("uid=? AND space_id=? AND channel_id=? AND channel_type=?", uid, spaceID, channelID, channelType).
		Exec()
	return err
}

// List 获取用户置顶频道列表
func (d *PinnedDB) List(uid, spaceID string) ([]*PinnedChannelModel, error) {
	var list []*PinnedChannelModel
	_, err := d.session.Select("channel_id", "channel_type", "sort_order").
		From("user_pinned_channel").
		Where("uid=? AND space_id=?", uid, spaceID).
		OrderBy("sort_order ASC").
		Load(&list)
	return list, err
}

// PinnedSortItem 排序项
type PinnedSortItem struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	SortOrder   int    `json:"sort_order"`
}

// pinnedKey 唯一标识一个置顶频道（uid + space 外部已绑定）
type pinnedKey struct {
	ChannelID   string
	ChannelType uint8
}

// validatePinnedSortItems 校验排序请求：
//   - items 不能为空
//   - 不能包含重复的 (channel_id, channel_type)
//   - 每个 item 必须已被当前用户在当前 Space 下置顶
//
// 该函数为纯函数，便于单元测试。客户端提交的 SortOrder 字段被忽略，
// 调用方应按数组顺序赋值 sort_order，避免客户端伪造冲突或越界的值。
func validatePinnedSortItems(items []PinnedSortItem, existing map[pinnedKey]struct{}) error {
	if len(items) == 0 {
		return errors.New("items 不能为空")
	}
	seen := make(map[pinnedKey]struct{}, len(items))
	for _, it := range items {
		k := pinnedKey{ChannelID: it.ChannelID, ChannelType: it.ChannelType}
		if _, dup := seen[k]; dup {
			return errors.New("items 中存在重复的频道")
		}
		seen[k] = struct{}{}
		if _, ok := existing[k]; !ok {
			return errors.New("提交的频道未置顶或不属于当前用户")
		}
	}
	return nil
}

// UpdateSort 更新置顶排序。
//
// 行为说明：
//   - 忽略客户端提交的 SortOrder 字段，按 items 数组顺序从 1 开始重新编号，
//     避免客户端伪造 sort_order 造成冲突或超出范围。
//   - 校验所有提交的频道都已被当前用户在当前 Space 下置顶。
func (d *PinnedDB) UpdateSort(uid, spaceID string, items []PinnedSortItem) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	var rows []PinnedChannelModel
	if _, err = tx.Select("channel_id", "channel_type").
		From("user_pinned_channel").
		Where("uid=? AND space_id=?", uid, spaceID).
		Load(&rows); err != nil {
		return err
	}
	existing := make(map[pinnedKey]struct{}, len(rows))
	for _, r := range rows {
		existing[pinnedKey{ChannelID: r.ChannelID, ChannelType: r.ChannelType}] = struct{}{}
	}

	if err := validatePinnedSortItems(items, existing); err != nil {
		return err
	}

	for i, item := range items {
		if _, err = tx.Update("user_pinned_channel").
			Set("sort_order", i+1).
			Where("uid=? AND space_id=? AND channel_id=? AND channel_type=?", uid, spaceID, item.ChannelID, item.ChannelType).
			Exec(); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RemoveByChannel 根据频道删除所有用户的置顶（用于频道删除/群解散时清理）
func (d *PinnedDB) RemoveByChannel(channelID string, channelType uint8) error {
	_, err := d.session.DeleteFrom("user_pinned_channel").
		Where("channel_id=? AND channel_type=?", channelID, channelType).
		Exec()
	return err
}

// RemoveByUIDSpaceChannel 删除用户在指定 Space 指定频道的置顶（用于退群时清理）
func (d *PinnedDB) RemoveByUIDSpaceChannel(uid, spaceID, channelID string, channelType uint8) error {
	_, err := d.session.DeleteFrom("user_pinned_channel").
		Where("uid=? AND space_id=? AND channel_id=? AND channel_type=?", uid, spaceID, channelID, channelType).
		Exec()
	return err
}

// RemoveByUIDAndChannel 删除用户在所有 Space 下指定频道的置顶（用于删好友时清理）
// 注意：此方法会跨 Space 删除，仅用于全局性操作（如删好友）
func (d *PinnedDB) RemoveByUIDAndChannel(uid, channelID string, channelType uint8) error {
	_, err := d.session.DeleteFrom("user_pinned_channel").
		Where("uid=? AND channel_id=? AND channel_type=?", uid, channelID, channelType).
		Exec()
	return err
}

// 全局 PinnedDB 实例，供其他模块调用清理方法
var globalPinnedDB *PinnedDB
var globalPinnedDBOnce sync.Once

// InitGlobalPinnedDB 初始化全局 PinnedDB（在 user 模块初始化时调用）
func InitGlobalPinnedDB(ctx *config.Context) {
	globalPinnedDBOnce.Do(func() {
		globalPinnedDB = NewPinnedDB(ctx)
	})
}

// RemovePinnedForUserInSpace 清理用户在指定 Space 指定频道的置顶（供其他模块调用）
func RemovePinnedForUserInSpace(uid, spaceID, channelID string, channelType uint8) {
	if globalPinnedDB == nil {
		return
	}
	if err := globalPinnedDB.RemoveByUIDSpaceChannel(uid, spaceID, channelID, channelType); err != nil {
		globalPinnedDB.Warn("清理用户置顶失败",
			zap.String("uid", uid),
			zap.String("spaceID", spaceID),
			zap.String("channelID", channelID),
			zap.Uint8("channelType", channelType),
			zap.Error(err))
	}
}

// RemovePinnedForUser 清理用户在所有 Space 下指定频道的置顶（供其他模块调用）
// 注意：此方法会跨 Space 删除，仅用于全局性操作（如删好友）
func RemovePinnedForUser(uid, channelID string, channelType uint8) {
	if globalPinnedDB == nil {
		return
	}
	if err := globalPinnedDB.RemoveByUIDAndChannel(uid, channelID, channelType); err != nil {
		globalPinnedDB.Warn("清理用户置顶失败",
			zap.String("uid", uid),
			zap.String("channelID", channelID),
			zap.Uint8("channelType", channelType),
			zap.Error(err))
	}
}

// RemovePinnedForChannel 清理频道的所有置顶（供其他模块调用）
func RemovePinnedForChannel(channelID string, channelType uint8) {
	if globalPinnedDB == nil {
		return
	}
	if err := globalPinnedDB.RemoveByChannel(channelID, channelType); err != nil {
		globalPinnedDB.Warn("清理频道置顶失败",
			zap.String("channelID", channelID),
			zap.Uint8("channelType", channelType),
			zap.Error(err))
	}
}
