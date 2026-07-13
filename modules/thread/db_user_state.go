package thread

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

// UserStateModel 映射 thread_user_state 表：子区「按用户」的可见性状态。
// 承载四级仲裁里的 P2（本人手工归档意图）。read_intent_at 为未来 mute/read 并入预留。
type UserStateModel struct {
	UID             string     `db:"uid"`
	GroupNo         string     `db:"group_no"`
	ShortID         string     `db:"short_id"`
	ArchiveIntent   int        `db:"archive_intent"`
	ArchiveIntentAt *time.Time `db:"archive_intent_at"`
	ReadIntentAt    *time.Time `db:"read_intent_at"`
	Version         int64      `db:"version"`
	db.BaseModel
}

// QueryUserStates 批量查询「一个 uid × 多 thread」的 per-user 状态。
// 键为 "{groupNo}____{shortID}"（与 thread 条目 TargetID / QueryActiveByGroupShortIDs
// 同口径）。无对应行的键不出现在 map 中，调用方按零值（未归档，回落全局）判定。
// refs 为空时返回空 map、不发查询。
func (d *DB) QueryUserStates(uid string, refs []ShortRef) (map[string]*UserStateModel, error) {
	result := make(map[string]*UserStateModel, len(refs))
	if uid == "" || len(refs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(refs))
	args := make([]interface{}, 0, 1+len(refs)*2)
	args = append(args, uid)
	for i, r := range refs {
		placeholders[i] = "(?, ?)"
		args = append(args, r.GroupNo, r.ShortID)
	}
	var rows []*UserStateModel
	_, err := d.session.SelectBySql(
		"SELECT uid, group_no, short_id, archive_intent, archive_intent_at, read_intent_at, version"+
			" FROM thread_user_state"+
			" WHERE uid = ? AND (group_no, short_id) IN ("+strings.Join(placeholders, ", ")+")",
		args...,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("query thread_user_state by uid+refs: %w", err)
	}
	for _, r := range rows {
		result[r.GroupNo+"____"+r.ShortID] = r
	}
	return result, nil
}

// UpsertArchiveIntentTx 在 tx 内按 (uid, group_no, short_id) 幂等写入归档意图。
// intent: 0=未归档 1=已归档(本人)。version 由调用方传（复用 thread 版本号发号器）。
// 照 UpsertSetting 惯例用 INSERT ... ON DUPLICATE KEY UPDATE，避免并发 read-then-write 竞态。
func (d *DB) UpsertArchiveIntentTx(tx *dbr.Tx, uid, groupNo, shortID string, intent int, version int64) error {
	_, err := tx.InsertBySql(
		"INSERT INTO thread_user_state (uid, group_no, short_id, archive_intent, archive_intent_at, version)"+
			" VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, ?)"+
			" ON DUPLICATE KEY UPDATE archive_intent=VALUES(archive_intent),"+
			" archive_intent_at=CURRENT_TIMESTAMP, version=VALUES(version)",
		uid, groupNo, shortID, intent, version,
	).Exec()
	if err != nil {
		return fmt.Errorf("upsert thread_user_state archive_intent: %w", err)
	}
	return nil
}

// DeleteUserStatesForThread 按 (group_no, short_id) 删除某子区所有用户的状态行，
// 走 idx_thread。用于 DeleteThread / 退群清理，避免孤儿行膨胀（plan T-GC）。
func (d *DB) DeleteUserStatesForThread(groupNo, shortID string) error {
	_, err := d.session.DeleteFrom("thread_user_state").
		Where("group_no=? AND short_id=?", groupNo, shortID).Exec()
	if err != nil {
		return fmt.Errorf("delete thread_user_state for thread: %w", err)
	}
	return nil
}

// QueryMuteForUID 批量查询「一个 uid × 多 thread」的 mute 设置。
// 键为 "{groupNo}____{shortID}"，值为 mute（0/1）。
//
// 注意：现有 QuerySettingsWithUIDs 是「一个 thread × 多 uid」形状（db.go），与本批
// 四级仲裁需要的「一个 uid × 多 thread」正交，故新增本 helper（plan F6/T2）。
// refs 为空返回空 map、不发查询。
func (d *DB) QueryMuteForUID(uid string, refs []ShortRef) (map[string]int, error) {
	result := make(map[string]int, len(refs))
	if uid == "" || len(refs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(refs))
	args := make([]interface{}, 0, 1+len(refs)*2)
	args = append(args, uid)
	for i, r := range refs {
		placeholders[i] = "(?, ?)"
		args = append(args, r.GroupNo, r.ShortID)
	}
	var rows []*SettingModel
	_, err := d.session.SelectBySql(
		"SELECT group_no, short_id, uid, mute, version FROM thread_setting"+
			" WHERE uid = ? AND (group_no, short_id) IN ("+strings.Join(placeholders, ", ")+")",
		args...,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("query thread_setting mute by uid+refs: %w", err)
	}
	for _, r := range rows {
		result[r.GroupNo+"____"+r.ShortID] = r.Mute
	}
	return result, nil
}

// HasUnhandledMention 判断 uid 对某子区是否存在「未处理 per-uid @」（plan T8 P1，detail 端）。
//
// 与 message 侧 queryUnhandledMentionChannels 同口径：reminders LEFT JOIN reminder_done，
// channel_type=5，uid=本人（@所有人 uid='' 不触发），is_deleted=0，reminder_done 无对应行。
// SQL uid 打头命中现有索引 channel_uid_uidx（plan F8）。
//
// detail 端与 list 端同源仲裁（plan T8/R5），但 thread 包不能 import message（会成环），
// 故在 thread DB 层对同一张 reminders 表做等价 EXISTS 查询。
func (d *DB) HasUnhandledMention(uid, groupNo, shortID string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	channelID := groupNo + "____" + shortID
	var one int
	err := d.session.SelectBySql(
		"SELECT 1 FROM reminders r"+
			" LEFT JOIN reminder_done dn ON dn.reminder_id = r.id AND dn.uid = ?"+
			" WHERE r.uid = ?"+ // uid 打头命中 channel_uid_uidx；per-uid 排除 @所有人(uid='')
			" AND r.channel_id = ?"+
			" AND r.channel_type = ?"+
			" AND r.is_deleted = 0"+
			" AND dn.id IS NULL"+
			" LIMIT 1",
		uid, uid, channelID, int(common.ChannelTypeCommunityTopic),
	).LoadOne(&one)
	if err == dbr.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query unhandled mention (detail): %w", err)
	}
	return one == 1, nil
}
