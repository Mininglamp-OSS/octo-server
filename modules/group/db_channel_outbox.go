package group

import (
	"time"

	"github.com/gocraft/dbr/v2"
)

// ChannelOutboxModel 群 IM 频道创建事务发件箱行（表 group_channel_outbox）。
//
// 一行 == 「某 group_no 的 IM 频道尚未确认创建」。建群事务内写入，随 group/group_member
// 一起原子提交；IM 频道确认成功后删除。见迁移 20260714000001_group_channel_outbox.sql 与
// reconcile.go。收口 #394 的孤儿群窗口。
type ChannelOutboxModel struct {
	Id          int64
	GroupNo     string
	ChannelType int
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// insertChannelOutboxTx 在建群事务内写入一条待确认频道的 outbox 行。
//
// 只显式写 group_no / channel_type，其余列(attempts/last_error/时间戳)交给表默认值，
// 避免把 Go 零值时间写进 TIMESTAMP 列。group_no 唯一键；建群 group_no 是新 UUID 不会撞键，
// 但仍用 ON DUPLICATE KEY 兜底重试幂等（重复建同 group_no 时不报错、不重置退避计数）。
func (d *DB) insertChannelOutboxTx(groupNo string, channelType uint8, tx *dbr.Tx) error {
	_, err := tx.InsertBySql(
		"INSERT INTO group_channel_outbox (group_no, channel_type) VALUES (?, ?) "+
			"ON DUPLICATE KEY UPDATE channel_type=VALUES(channel_type)",
		groupNo, int(channelType),
	).Exec()
	return err
}

// deleteChannelOutbox 删除某 group_no 的 outbox 行（IM 频道已确认 / 群行已不存在的清理）。
// 幂等：行不存在时删 0 行、不报错。
func (d *DB) deleteChannelOutbox(groupNo string) error {
	_, err := d.session.DeleteFrom("group_channel_outbox").Where("group_no=?", groupNo).Exec()
	return err
}

// listDueChannelOutbox 返回 updated_at 早于 before 的 outbox 行（超过宽限期、可 reconcile 的），
// 按 updated_at 升序（最久未处理优先），最多 limit 行。宽限期让 in-flight 建群（尚在 tx.Commit 与
// IM 创建之间的正常窗口）不被误当孤儿处理。
func (d *DB) listDueChannelOutbox(before time.Time, limit int) ([]*ChannelOutboxModel, error) {
	var models []*ChannelOutboxModel
	_, err := d.session.Select("*").
		From("group_channel_outbox").
		Where("updated_at < ?", before).
		OrderDir("updated_at", true).
		Limit(uint64(limit)).
		Load(&models)
	return models, err
}

// markChannelOutboxAttempt 记一次失败尝试：attempts+1、写 last_error、并借 ON UPDATE
// CURRENT_TIMESTAMP 推进 updated_at（既是观测，也让下一轮 due 判定按最近尝试时间退避，
// 避免同一坏行每个 tick 都被打）。lastErr 截断到列宽 512。
func (d *DB) markChannelOutboxAttempt(groupNo, lastErr string) error {
	if len(lastErr) > 512 {
		lastErr = lastErr[:512]
	}
	_, err := d.session.Update("group_channel_outbox").
		Set("attempts", dbr.Expr("attempts+1")).
		Set("last_error", lastErr).
		Where("group_no=?", groupNo).
		Exec()
	return err
}
