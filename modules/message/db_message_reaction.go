package message

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

type messageReactionDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newMessageReactionDB(ctx *config.Context) *messageReactionDB {
	return &messageReactionDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// 查询某个频道的回应数据
func (d *messageReactionDB) queryReactionWithChannelAndSeq(channelID string, channelType uint8, seq int64, limit uint64) ([]*reactionModel, error) {
	var list []*reactionModel
	var err error
	if seq <= 0 { // TODO: 如果seq为0 不能去同步整个频道的 应该同步最新指定数量的回应数据（建议limit 100）
		_, err = d.session.Select("*").From("reaction_users").Where("channel_id=? and channel_type=?", channelID, channelType).OrderDesc("seq").Limit(limit).Load(&list)
	} else {
		_, err = d.session.Select("*").From("reaction_users").Where("channel_id=? and channel_type=? and seq>?", channelID, channelType, seq).OrderAsc("seq").Limit(limit).Load(&list)
	}
	return list, err
}

func (d *messageReactionDB) queryWithMessageIDsInChannel(channelID string, channelType uint8, messageIDs []string) ([]*reactionModel, error) {
	if len(messageIDs) <= 0 {
		return nil, nil
	}
	var models []*reactionModel
	_, err := d.session.Select("*").From("reaction_users").
		Where("channel_id=? and channel_type=? and message_id in ?", channelID, channelType, messageIDs).
		Load(&models)
	return models, err
}

// toggleReaction 对单个 (uid, message_id, channel_id, channel_type, emoji) 做原子 toggle：
// 首次命中 → 插入 is_deleted=0（点亮）；再次命中唯一键 → is_deleted 翻转（0→1 取消 /
// 1→0 复活）。依赖迁移 20260712000001 建的唯一索引触发 ON DUPLICATE KEY UPDATE，
// 单条语句原子完成，天然防并发重复行。每次都写入新的 seq，供频道级增量 sync 感知变更。
//
// 多 reaction 语义：不同 emoji 命中不同唯一键 → 各自独立行，互不影响（追加），
// 不再有"改 emoji 覆盖"分支。
//
// upsert 后回读该行最终 is_deleted 返回，供 Web 乐观更新对账（盲翻转 + 并发下需知道结果）。
func (d *messageReactionDB) toggleReaction(model *reactionModel) (int, error) {
	_, err := d.session.InsertBySql(
		"INSERT INTO reaction_users (message_id, seq, channel_id, channel_type, uid, name, emoji, is_deleted) "+
			"VALUES (?,?,?,?,?,?,?,0) "+
			"ON DUPLICATE KEY UPDATE is_deleted = 1 - is_deleted, seq = VALUES(seq), name = VALUES(name), updated_at = CURRENT_TIMESTAMP",
		model.MessageID, model.Seq, model.ChannelID, model.ChannelType, model.UID, model.Name, model.Emoji,
	).Exec()
	if err != nil {
		return 0, err
	}
	var isDeleted int
	err = d.session.Select("is_deleted").From("reaction_users").
		Where("channel_id=? and channel_type=? and message_id=? and uid=? and emoji=?",
			model.ChannelID, model.ChannelType, model.MessageID, model.UID, model.Emoji).
		LoadOne(&isDeleted)
	return isDeleted, err
}

// setReaction 对单个 (uid, message_id, channel_id, channel_type, emoji) 做「幂等」写：
// 与 toggleReaction 不同，它把 is_deleted 显式落成入参目标值而非盲翻转 —— 供 Bot API
// 的显式 add(isDeleted=0) / remove(isDeleted=1) 语义使用，天然重试安全（agent 侧超时
// 重试不会像 toggle 那样把已点亮的 reaction 意外取消）。命中迁移 20260712000001 建的
// 唯一索引触发 ON DUPLICATE KEY UPDATE，把该行覆盖为目标 is_deleted 并刷新 seq/name/
// updated_at，单条语句原子完成。写后最终 is_deleted 恒等于入参（显式落值，无需回读）。
func (d *messageReactionDB) setReaction(model *reactionModel, isDeleted int) (int, error) {
	_, err := d.session.InsertBySql(
		"INSERT INTO reaction_users (message_id, seq, channel_id, channel_type, uid, name, emoji, is_deleted) "+
			"VALUES (?,?,?,?,?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE is_deleted = VALUES(is_deleted), seq = VALUES(seq), name = VALUES(name), updated_at = CURRENT_TIMESTAMP",
		model.MessageID, model.Seq, model.ChannelID, model.ChannelType, model.UID, model.Name, model.Emoji, isDeleted,
	).Exec()
	if err != nil {
		return 0, err
	}
	return isDeleted, nil
}

type reactionModel struct {
	MessageID   string // 消息唯一ID
	Seq         int64  // 回复序列号
	ChannelID   string // 频道唯一ID
	ChannelType uint8  // 频道类型
	UID         string // 用户ID
	Name        string // 用户名称
	Emoji       string // 回应表情
	IsDeleted   int    // 是否已删除
	db.BaseModel
}
