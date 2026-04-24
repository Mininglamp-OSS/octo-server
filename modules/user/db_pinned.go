package user

import (
	"errors"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

// AreSpaceMembers 校验 uid1、uid2 是否都是指定 Space 的在籍成员。
// 用于私聊置顶时，允许同 Space 成员（即便未互加好友）之间互相置顶。
func (d *PinnedDB) AreSpaceMembers(spaceID, uid1, uid2 string) (bool, error) {
	return spacepkg.CheckBothMembers(d.session, spaceID, uid1, uid2)
}

// QueryPeerRobotInfo 返回目标用户是否为机器人，以及（若为机器人）其创建者 UID。
// 用于私聊置顶时区分普通用户与 bot：非本人创建的 bot 必须是好友关系才允许对话 / 置顶。
//
// 边界：LEFT JOIN 条件限定 r.status=1，因此 user.robot=1 但 robot.status!=1
// （已停用的 bot）的情况下，返回 isRobot=true, creatorUID=""。此时调用方的
// creator 匹配必然失败，会 fallback 到 "请先添加该机器人为好友"——对已停用 bot
// 采用更严格的访问控制，符合与 modules/robot/event.go existRobot 一致的语义。
func (d *PinnedDB) QueryPeerRobotInfo(peerUID string) (isRobot bool, creatorUID string, err error) {
	if peerUID == "" {
		return false, "", nil
	}
	type row struct {
		Robot      int    `db:"robot"`
		CreatorUID string `db:"creator_uid"`
	}
	var r row
	err = d.session.SelectBySql(
		"SELECT u.robot AS robot, IFNULL(r.creator_uid,'') AS creator_uid "+
			"FROM `user` u LEFT JOIN robot r ON r.robot_id = u.uid AND r.status = 1 "+
			"WHERE u.uid = ?",
		peerUID,
	).LoadOne(&r)
	if err != nil && err != dbr.ErrNotFound {
		return false, "", err
	}
	return r.Robot == 1, r.CreatorUID, nil
}

// Add 添加置顶频道。
//
// 并发一致性：
//   - 先 SELECT COUNT(*) ... FOR UPDATE 取当前读并在匹配范围上加 next-key lock，
//     串行化同一 (uid, space_id) 下的并发插入。REPEATABLE READ 下普通的
//     一致性读 COUNT 使用事务启动时的快照，看不到其他事务已提交的插入，
//     因此必须用 FOR UPDATE 保证上限检查的正确性。
//   - 唯一索引 uk_user_space_channel 配合 INSERT IGNORE 检测重复。
func (d *PinnedDB) Add(uid, spaceID, channelID string, channelType uint8, maxLimit int) error {
	tx, err := d.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	var count int
	if _, err := tx.SelectBySql(
		"SELECT COUNT(*) FROM user_pinned_channel WHERE uid=? AND space_id=? FOR UPDATE",
		uid, spaceID,
	).Load(&count); err != nil {
		return err
	}
	if count >= maxLimit {
		return ErrPinnedLimitExceeded
	}

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

// PinnedSortItem 排序项。SortOrder 由服务端按数组顺序重新分配，
// 客户端不需要也不应该提交；因此结构体中不包含 SortOrder 字段。
type PinnedSortItem struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// pinnedKey 唯一标识一个置顶频道（uid + space 外部已绑定）
type pinnedKey struct {
	ChannelID   string
	ChannelType uint8
}

// PinnedSortError 表示 UpdateSort 请求参数校验失败，属于客户端错误。
// handler 可以通过 errors.As 判断并直接把 message 透传给客户端，
// 以区分 DB/系统错误（走泛化的 "更新排序失败" 提示）。
type PinnedSortError struct{ msg string }

func (e *PinnedSortError) Error() string { return e.msg }

func newPinnedSortError(msg string) error { return &PinnedSortError{msg: msg} }

// validatePinnedSortItems 校验排序请求。返回的错误（若非 nil）始终是 *PinnedSortError。
//
// 规则：
//   - items 不能为空；
//   - items 中不能有重复的 (channel_id, channel_type)；
//   - items 必须覆盖当前用户在当前 Space 下的 *全部* 置顶频道，
//     否则未提交的频道会保留旧 sort_order，与新编号产生冲突；
//   - 每个 item 必须已被当前用户置顶。
func validatePinnedSortItems(items []PinnedSortItem, existing map[pinnedKey]struct{}) error {
	if len(items) == 0 {
		return newPinnedSortError("items 不能为空")
	}
	seen := make(map[pinnedKey]struct{}, len(items))
	for _, it := range items {
		k := pinnedKey{ChannelID: it.ChannelID, ChannelType: it.ChannelType}
		if _, dup := seen[k]; dup {
			return newPinnedSortError("items 中存在重复的频道")
		}
		seen[k] = struct{}{}
		if _, ok := existing[k]; !ok {
			return newPinnedSortError("提交的频道未置顶或不属于当前用户")
		}
	}
	if len(items) != len(existing) {
		return newPinnedSortError("必须提交所有置顶频道")
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
