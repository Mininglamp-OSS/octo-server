package space

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	//
	// 必须显著长于一次作业的实际耗时：群级联要逐个群做 IM 退订 + 发 Tip + 子区/置顶
	// 清理，几十个群就能跑上几分钟。租约一旦在作业跑到一半时过期，另一个 worker 会
	// 合法地重新认领并**并发执行同一条工单**——per-claim owner 只能让晚到的那次写入
	// 落空，挡不住重复执行本身（重复的系统消息、重复的 IM 调用）。
	// 步骤契约要求幂等，租约足够长则是第一道防线。
	removalCleanupLease = 10 * time.Minute
	// removalCleanupMaxAttempts 超过后置为 abandoned，不再无限重试。
	// 配合下面的退避，总窗口约 70 分钟：这是隔离性清理，短窗口下一次稍长的 IM /
	// DB 故障就会把所有待处理工单打成 abandoned，而 abandoned 没有任何东西会重新
	// 驱动，被移除的人就一直留在群里、IM 群订阅也还在。到达上限只打 error 日志，
	// 需要人工介入——目前没有自动 reconcile。
	removalCleanupMaxAttempts uint32 = 20
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
	// next_attempt_at 由 Go 侧算，不要用 CURRENT_TIMESTAMP(3)：认领时是拿 Go 的
	// time 去比这一列，而 CURRENT_TIMESTAMP 走的是 MySQL 会话时区。两个时钟一旦
	// 不同源（部署镜像 TZ=Asia/Shanghai，而 DSN 未指定 loc 时驱动按 UTC 发送），
	// 新工单会整整一个时区偏移都认领不到。写读两侧都走 Go 的 UTC 即自洽。
	_, err := tx.InsertBySql(
		"INSERT INTO space_member_removal_cleanup (space_id, uid, operator_uid, reason, status, next_attempt_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		spaceID, uid, operatorUID, reason, removalCleanupPending, time.Now().UTC(),
	).Exec()
	if err != nil {
		return fmt.Errorf("space: enqueue removal cleanup: %w", err)
	}
	return nil
}

// enqueueMemberRemovalCleanupBatchTx 一次性为多个成员写出清理工单。
//
// 解散场景会在同一个事务里为全体成员入队，而那个事务正握着 space_member 的
// FOR UPDATE 范围锁；逐条 INSERT 意味着上万次往返都在锁内完成，期间任何并发的
// 加入路径（atomicAddMemberIfNotFull / approveJoinApply 都要在同一范围上取
// FOR UPDATE）全部阻塞，甚至撞上 innodb_lock_wait_timeout。多值 INSERT 分批发出，
// 把锁内往返从 N 次压到 N/batch 次。
func enqueueMemberRemovalCleanupBatchTx(tx *dbr.Tx, spaceID string, uids []string, operatorUID, reason string) error {
	if len(uids) == 0 {
		return nil
	}
	if spaceID == "" {
		return errors.New("space: removal cleanup requires space_id")
	}
	if !IsMemberRemoveReason(reason) {
		return fmt.Errorf("space: unknown member removal reason %q", reason)
	}
	const perStatement = 200
	now := time.Now().UTC()
	for start := 0; start < len(uids); start += perStatement {
		end := start + perStatement
		if end > len(uids) {
			end = len(uids)
		}
		chunk := uids[start:end]
		placeholders := make([]string, 0, len(chunk))
		args := make([]interface{}, 0, len(chunk)*6)
		for _, uid := range chunk {
			if uid == "" {
				continue
			}
			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?)")
			args = append(args, spaceID, uid, operatorUID, reason, removalCleanupPending, now)
		}
		if len(placeholders) == 0 {
			continue
		}
		sql := "INSERT INTO space_member_removal_cleanup " +
			"(space_id, uid, operator_uid, reason, status, next_attempt_at) VALUES " +
			strings.Join(placeholders, ",")
		if _, err := tx.InsertBySql(sql, args...).Exec(); err != nil {
			return fmt.Errorf("space: enqueue removal cleanup batch: %w", err)
		}
	}
	return nil
}

// claimMemberRemovalCleanup 认领一条到期且未被租约占用的工单。
//
// SKIP LOCKED 让多副本并行推进而不互相阻塞；租约（lease_owner/lease_until）保证
// 同一工单在租约内只被一个执行者持有。没有可认领的工单时返回 (nil, nil)。
//
// owner 必须**每次认领都不同**（见 newRemovalClaimOwner）：进程级的固定 owner 会让
// 同进程内两个 goroutine 的 `AND lease_owner=?` 守卫同时成立，租约就形同虚设。
// now 请传 UTC，与 next_attempt_at 的写入侧保持同一个时钟。
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

	// attempts 在**认领时**自增，而不是等到失败释放时。
	//
	// 释放路径只覆盖「作业跑完并返回了错误」。进程在作业中途被杀（OOM、Pod 驱逐）
	// 时谁也没机会写 attempts：租约到期后同一行被重新认领，计数原地不动，
	// 一条必然打死进程的作业就能无限循环、永远到不了 abandoned。
	// 认领即计数让这种情况自我收敛，也让 attempts 真实反映「试过几次」。
	result, err := tx.UpdateBySql(
		"UPDATE space_member_removal_cleanup SET lease_owner=?, lease_until=?, attempts=attempts+1 WHERE id=? AND status=?",
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
	// SELECT 读到的是自增前的值，这里对齐成库里的值，让调用方的
	// 「已经试到第几次」判断不必再去猜偏移量。
	job.Attempts++
	return &job, nil
}

// finishMemberRemovalCleanup 把工单置为终态（done / abandoned），要求仍持有租约。
// 租约易主时返回 false，调用方据此放弃写入——另一个 worker 已经接手。
// attempts 不在这里动：认领时已经计过（见 claimMemberRemovalCleanup），
// 所以一条 abandoned 行的 attempts 天然等于 removalCleanupMaxAttempts，
// 运维可以直接按这个阈值查出被放弃的工单。
func (d *DB) finishMemberRemovalCleanup(id uint64, owner string, status uint8, lastError string) (bool, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE space_member_removal_cleanup "+
			"SET status=?, finished_at=?, lease_owner='', lease_until=NULL, last_error=? "+
			"WHERE id=? AND status=? AND lease_owner=?",
		status, time.Now().UTC(), truncateCleanupError(lastError), id, removalCleanupPending, owner,
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
//
// 不再自增 attempts —— 认领时已经计过了（见 claimMemberRemovalCleanup）。
// 两处都加会让计数翻倍，退避和放弃阈值一起提前一半。
func (d *DB) releaseMemberRemovalCleanup(id uint64, owner string, attempts uint32, lastError string) error {
	next := time.Now().UTC().Add(memberRemovalRetryDelay(attempts))
	result, err := d.session.UpdateBySql(
		"UPDATE space_member_removal_cleanup "+
			"SET next_attempt_at=?, lease_owner='', lease_until=NULL, last_error=? "+
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

// removalCleanupRetention 终态工单的保留期。
//
// 每次踢人 / 退出 / 强制移除 / 解散都会留下一行，只翻状态从不删除；解散几个大空间
// 就是几万行。留一段时间供排障，之后清掉，避免这张表和 pending 索引无限膨胀，
// 把每 10s 一次的认领扫描越拖越慢。
const removalCleanupRetention = 14 * 24 * time.Hour

// purgeFinishedMemberRemovalCleanups 删除超过保留期的**已完成**工单，返回删除行数。
// 单次有上限，避免一条 DELETE 锁住大量行。
//
// 只删 done，abandoned 一律保留：那是「隔离性清理最终放弃了」的唯一持久记录，
// 除了一条 error 日志之外没有别的东西记得它，删掉就再也查不出来了。
func (d *DB) purgeFinishedMemberRemovalCleanups(before time.Time, limit int) (int64, error) {
	result, err := d.session.UpdateBySql(
		"DELETE FROM space_member_removal_cleanup WHERE status=? AND finished_at IS NOT NULL AND finished_at < ? LIMIT ?",
		removalCleanupDone, before, limit,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("space: purge finished removal cleanups: %w", err)
	}
	return result.RowsAffected()
}

// memberRemovalRetryDelay 指数退避，封顶 5 分钟。
//
// 注意先算再夹，不要先把 attempt 夹到 8：2^8 = 256s < 300s，那样 5 分钟的上限
// 永远取不到，实际封顶变成 4m16s，整个重试预算也跟着缩水。
func memberRemovalRetryDelay(attempt uint32) time.Duration {
	const maxDelay = 5 * time.Minute
	if attempt > 12 { // 2^12 秒已远超上限，再大只会让移位溢出
		return maxDelay
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// truncateCleanupError 把失败摘要收敛成可安全写入 last_error 的合法 UTF-8。
//
// 两件事都必须做，缺一不可 —— 任何非法字节写进 utf8mb4 列，strict 模式都会
// 拒掉整条 UPDATE，于是 attempts 不增、next_attempt_at 不推进：本来用来保护重试的
// 函数反而把退避弄断，工单在租约到期后被反复认领且永远到不了 abandoned。
//
//  1. **先清洗**：错误串里可能夹着任意位置的非法字节（上游代理的错误页、IM 返回体
//     的裸字节）。只修末尾被截断的那个 rune 挡不住中间的非法字节；只有整串清洗才行。
//  2. **再按 rune 边界截断**：清洗后再切，切点自然落在合法边界上。
//
// 列宽 255 是字符数，这里按字节截断，因此永远不会超列。
func truncateCleanupError(s string) string {
	const max = 255
	cleaned := strings.ToValidUTF8(s, "")
	if len(cleaned) <= max {
		return cleaned
	}
	truncated := cleaned[:max]
	// 退到最近的 rune 边界。必须是循环而不是「削一个字节」：cleaned 已是合法
	// UTF-8，但在 max 处切开可能砍掉一个 4 字节 rune 的后 1~3 字节，而
	// DecodeLastRuneInString 对悬空序列每次只报 (RuneError, 1)，削一次仍会留下
	// 半个 rune（实测 "a"*252 + 😀 截断后尾部残留 f0 9f）。非法字节写进
	// last_error 会被 MySQL 以 Incorrect string value 拒掉整条 release，
	// 工单就卡在 running 上白等一轮租约。cleaned 合法 ⇒ 最多转 3 次。
	for len(truncated) > 0 {
		r, size := utf8.DecodeLastRuneInString(truncated)
		if r != utf8.RuneError || size > 1 {
			break // 收在完整 rune 上（U+FFFD 本身合法，size=3，不该被削）
		}
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// lockActiveMemberUIDsTx 事务内**加锁**读取 Space 的活跃成员 UID，用于解散时逐个入队。
//
// FOR UPDATE 不是可选的：普通 SELECT 是快照读，与随后那条把全员置 0 的 UPDATE
// （当前读）看到的行集可能不一致，夹在中间提交的新成员会被置 0 却拿不到清理工单。
func lockActiveMemberUIDsTx(tx *dbr.Tx, spaceID string) ([]string, error) {
	var uids []string
	_, err := tx.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id=? AND status=1 FOR UPDATE", spaceID,
	).Load(&uids)
	return uids, err
}
