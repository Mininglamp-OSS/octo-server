package space

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gocraft/dbr/v2"
)

// imPendingRow 一条待重试的退订。
type imPendingRow struct {
	ID          uint64 `db:"id"`
	ChannelID   string `db:"channel_id"`
	ChannelType uint8  `db:"channel_type"`
	UID         string `db:"uid"`
	Attempts    uint32 `db:"attempts"`
}

// imPendingInserter 覆盖 *dbr.Tx 与 *dbr.Session 的公共面，
// 让「事务内入队」与「无事务入队」共用同一段 SQL。
type imPendingInserter interface {
	InsertBySql(query string, value ...interface{}) *dbr.InsertStmt
}

// enqueueIMUnsubscribe 写出退订待办。
//
// INSERT IGNORE + 唯一键 (channel_id, channel_type, uid)：重试路径会反复入队，
// 不折叠的话一次 broker 抖动就能刷出成千条等价行。已存在的待办保持原有的
// attempts / next_attempt_at，不被重新入队重置——否则一条永远失败的待办
// 会被反复"续命"，永远到不了 abandoned。
func enqueueIMUnsubscribe(ins imPendingInserter, channelID string, channelType uint8, uids []string) error {
	if channelID == "" || len(uids) == 0 {
		return nil
	}
	// next_attempt_at 由 Go 侧算，与认领侧同一个时钟（同 space_member_removal_cleanup
	// 的理由：CURRENT_TIMESTAMP 走 MySQL 会话时区，两个时钟不同源会整段偏移）。
	now := time.Now().UTC()
	// 拼成**一条**多行 INSERT，而不是每个 uid 一条。批量踢人上限是 200，一次解散
	// 更是逐群成百上千次调用；在事务里打 200 个来回会把移除事务的持锁时间拖成
	// 网络延迟的倍数，正好压在 group_member 的行锁上。
	placeholders := make([]string, 0, len(uids))
	args := make([]interface{}, 0, len(uids)*5)
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		placeholders = append(placeholders, "(?, ?, ?, ?, ?)")
		args = append(args, channelID, channelType, uid, removalCleanupPending, now)
	}
	if len(placeholders) == 0 {
		return nil
	}
	_, err := ins.InsertBySql(
		"INSERT IGNORE INTO im_pending_subscriber_removal "+
			"(channel_id, channel_type, uid, status, next_attempt_at) VALUES "+
			strings.Join(placeholders, ", "),
		args...,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: enqueue im unsubscribe: %w", err)
	}
	return nil
}

// deleteIMPendingByTarget 按 (channel, uid) 删掉待办。立即尝试成功后调用。
func deleteIMPendingByTarget(sess *dbr.Session, channelID string, channelType uint8, uids []string) error {
	if channelID == "" || len(uids) == 0 {
		return nil
	}
	_, err := sess.DeleteBySql(
		"DELETE FROM im_pending_subscriber_removal WHERE channel_id=? AND channel_type=? AND uid IN ?",
		channelID, channelType, uids,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: delete im pending by target: %w", err)
	}
	return nil
}

// claimIMPendingRemoval 认领一条待办，与 claimMemberRemovalCleanup 同款：
// FOR UPDATE SKIP LOCKED + 每次认领一个新 owner + 认领即自增 attempts +
// 认领处卡住重试预算（理由见 claimMemberRemovalCleanup 的注释）。
func (d *DB) claimIMPendingRemoval(owner string, now time.Time) (*imPendingRow, error) {
	if owner == "" {
		return nil, errors.New("space: im pending claim owner required")
	}
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("space: begin im pending claim: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var row imPendingRow
	err = tx.SelectBySql(
		"SELECT id, channel_id, channel_type, uid, attempts "+
			"FROM im_pending_subscriber_removal "+
			"WHERE status=? AND attempts<? AND next_attempt_at<=? "+
			"AND (lease_until IS NULL OR lease_until<=?) "+
			"ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED",
		removalCleanupPending, removalCleanupMaxAttempts, now, now,
	).LoadOne(&row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("space: select im pending: %w", err)
	}

	result, err := tx.UpdateBySql(
		"UPDATE im_pending_subscriber_removal SET lease_owner=?, lease_until=?, attempts=attempts+1 "+
			"WHERE id=? AND status=?",
		owner, now.Add(removalCleanupLease), row.ID, removalCleanupPending,
	).Exec()
	if err != nil {
		return nil, fmt.Errorf("space: claim im pending: %w", err)
	}
	if affected, aErr := result.RowsAffected(); aErr != nil || affected != 1 {
		return nil, errors.New("space: invalid im pending claim result")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("space: commit im pending claim: %w", err)
	}
	row.Attempts++
	return &row, nil
}

// deleteIMPendingByID 成功后删行。带 lease_owner 守卫：租约易主时不删，
// 免得把接手方正在处理的行删掉。
func (d *DB) deleteIMPendingByID(id uint64, owner string) error {
	_, err := d.session.DeleteBySql(
		"DELETE FROM im_pending_subscriber_removal WHERE id=? AND lease_owner=?", id, owner,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: delete im pending: %w", err)
	}
	return nil
}

// releaseIMPending 失败后释放租约并按指数退避安排下次尝试（复用同一条退避曲线）。
func (d *DB) releaseIMPending(id uint64, owner string, attempts uint32, lastError string) error {
	_, err := d.session.UpdateBySql(
		"UPDATE im_pending_subscriber_removal "+
			"SET next_attempt_at=?, lease_owner='', lease_until=NULL, last_error=? "+
			"WHERE id=? AND status=? AND lease_owner=?",
		time.Now().UTC().Add(memberRemovalRetryDelay(attempts)), truncateCleanupError(lastError),
		id, removalCleanupPending, owner,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: release im pending: %w", err)
	}
	return nil
}

// abandonIMPending 重试耗尽：置终态并保留行，供运维查询与告警。
func (d *DB) abandonIMPending(id uint64, owner string, lastError string) error {
	_, err := d.session.UpdateBySql(
		"UPDATE im_pending_subscriber_removal "+
			"SET status=?, lease_owner='', lease_until=NULL, last_error=? "+
			"WHERE id=? AND status=? AND lease_owner=?",
		removalCleanupAbandoned, truncateCleanupError(lastError),
		id, removalCleanupPending, owner,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: abandon im pending: %w", err)
	}
	return nil
}

// abandonExhaustedIMPending 进程外扫描，与 abandonExhaustedMemberRemovalCleanups 同理：
// 进程被硬杀时没人写终态，认领处的 attempts 上限又让这行再也取不走。
func (d *DB) abandonExhaustedIMPending(now time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	result, err := d.session.UpdateBySql(
		"UPDATE im_pending_subscriber_removal "+
			"SET status=?, lease_owner='', lease_until=NULL, last_error=? "+
			"WHERE status=? AND attempts>=? AND (lease_until IS NULL OR lease_until<=?) LIMIT ?",
		removalCleanupAbandoned, "sweep: retries exhausted",
		removalCleanupPending, removalCleanupMaxAttempts, now, limit,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("space: sweep exhausted im pending: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("space: read im pending sweep result: %w", err)
	}
	return affected, nil
}
