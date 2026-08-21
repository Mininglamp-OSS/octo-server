package space

import (
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// 清理工单状态
const (
	removalCleanupPending   uint8 = 0
	removalCleanupDone      uint8 = 1
	removalCleanupAbandoned uint8 = 2
)

const (
	// removalCleanupLease 一次认领的租约时长。worker 崩溃后租约到期即可被其它副本接管。
	removalCleanupLease = 60 * time.Second
	// removalCleanupMaxAttempts 超过后置为 abandoned，不再无限重试。
	// 达到上限会打 error 日志，交由人工/reconcile 处理，而不是静默丢弃。
	removalCleanupMaxAttempts uint32 = 10
	// removalCleanupBatchSize 单次调度最多处理的工单数，避免一次占满 DB 连接。
	removalCleanupBatchSize = 20
)

// memberRemovalCleanupJob 一条待执行的会话面清理工单。
type memberRemovalCleanupJob struct {
	ID          uint64 `db:"id"`
	SpaceID     string `db:"space_id"`
	UID         string `db:"uid"`
	OperatorUID string `db:"operator_uid"`
	Reason      string `db:"reason"`
	Attempts    uint32 `db:"attempts"`
}

// enqueueMemberRemovalCleanupTx 在成员移除的同一事务内写出清理工单（transactional
// outbox）。调用方必须已经确认这次移除真的改动了成员行——对不存在 / 已移除的成员
// 入队会产出一条永远无事可做的工单。
func enqueueMemberRemovalCleanupTx(tx *dbr.Tx, spaceID, uid, operatorUID, reason string) error {
	if spaceID == "" || uid == "" {
		return errors.New("space: removal cleanup requires space_id and uid")
	}
	if !IsMemberRemoveReason(reason) {
		return fmt.Errorf("space: unknown member removal reason %q", reason)
	}
	_, err := tx.InsertBySql(
		"INSERT INTO space_member_removal_cleanup (space_id, uid, operator_uid, reason, status, next_attempt_at) "+
			"VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP(3))",
		spaceID, uid, operatorUID, reason, removalCleanupPending,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: enqueue removal cleanup: %w", err)
	}
	return nil
}

// claimMemberRemovalCleanup 认领一条到期且未被租约占用的工单。
//
// SKIP LOCKED 让多副本并行推进而不互相阻塞；租约（lease_owner/lease_until）保证
// 同一工单在租约内只被一个 worker 执行。没有可认领的工单时返回 (nil, nil)。
func (d *DB) claimMemberRemovalCleanup(owner string, now time.Time) (*memberRemovalCleanupJob, error) {
	if owner == "" {
		return nil, errors.New("space: removal cleanup claim owner required")
	}
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("space: begin removal cleanup claim: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var job memberRemovalCleanupJob
	err = tx.SelectBySql(
		"SELECT id, space_id, uid, operator_uid, reason, attempts "+
			"FROM space_member_removal_cleanup "+
			"WHERE status=? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?) "+
			"ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED",
		removalCleanupPending, now, now,
	).LoadOne(&job)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("space: select removal cleanup job: %w", err)
	}

	result, err := tx.UpdateBySql(
		"UPDATE space_member_removal_cleanup SET lease_owner=?, lease_until=? WHERE id=? AND status=?",
		owner, now.Add(removalCleanupLease), job.ID, removalCleanupPending,
	).Exec()
	if err != nil {
		return nil, fmt.Errorf("space: claim removal cleanup job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, errors.New("space: invalid removal cleanup claim result")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("space: commit removal cleanup claim: %w", err)
	}
	return &job, nil
}

// finishMemberRemovalCleanup 把工单置为终态（done / abandoned），要求仍持有租约。
// 租约易主时返回 false，调用方据此放弃写入——另一个 worker 已经接手。
func (d *DB) finishMemberRemovalCleanup(id uint64, owner string, status uint8, lastError string) (bool, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE space_member_removal_cleanup "+
			"SET status=?, finished_at=CURRENT_TIMESTAMP(3), lease_owner='', lease_until=NULL, last_error=? "+
			"WHERE id=? AND status=? AND lease_owner=?",
		status, lastError, id, removalCleanupPending, owner,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("space: finish removal cleanup job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("space: read removal cleanup finish result: %w", err)
	}
	return affected == 1, nil
}

// releaseMemberRemovalCleanup 执行失败后释放租约并按指数退避安排下次尝试。
func (d *DB) releaseMemberRemovalCleanup(id uint64, owner string, attempts uint32, lastError string) error {
	next := time.Now().Add(memberRemovalRetryDelay(attempts + 1))
	result, err := d.session.UpdateBySql(
		"UPDATE space_member_removal_cleanup "+
			"SET attempts=attempts+1, next_attempt_at=?, lease_owner='', lease_until=NULL, last_error=? "+
			"WHERE id=? AND status=? AND lease_owner=?",
		next, truncateCleanupError(lastError), id, removalCleanupPending, owner,
	).Exec()
	if err != nil {
		return fmt.Errorf("space: release removal cleanup job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return errors.New("space: removal cleanup lease ownership lost on release")
	}
	return nil
}

// memberRemovalRetryDelay 指数退避，封顶 5 分钟。与 user 侧 session revocation 同构。
func memberRemovalRetryDelay(attempt uint32) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

// truncateCleanupError 把失败摘要截到 last_error 列宽以内。
// 步骤名 + 错误串可能带上游细节，这里只做长度收敛，不做内容加工。
func truncateCleanupError(s string) string {
	const max = 255
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// queryActiveMemberUIDsTx 事务内读取 Space 的活跃成员 UID，用于解散时逐个入队。
func queryActiveMemberUIDsTx(tx *dbr.Tx, spaceID string) ([]string, error) {
	var uids []string
	_, err := tx.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id=? AND status=1", spaceID,
	).Load(&uids)
	return uids, err
}
