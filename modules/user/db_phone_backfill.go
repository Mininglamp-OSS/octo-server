package user

import (
	"context"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// 手机号影子列回填。
//
// 为什么必须有这个任务：迁移只新增空列，存量行的 phone_encrypted / phone_hash /
// phone_last4 全是空值。读路径（QueryByPhone / oidc QueryUIDsByPhone / 空间成员后四位
// 检索）都带明文兜底，所以功能上看不出异常 —— 这恰恰会掩盖"存量用户永远没有密文和
// 盲索引"这个事实：加密形同虚设、新建的 idx_phone_hash 也只是一个巨大的空值桶。
//
// 设计取舍：
//   - 按主键游标（id > cursor）分批，不用 OFFSET —— OFFSET 在大表上越翻越慢；
//   - 每批之间 sleep 限速，避免长事务和写放大打爆主库；
//   - 单次调用有上界（批数 / 时间），由调用方拿着返回的游标反复调用推进，
//     不做长跑请求；
//   - 只回填 phone<>'' 且 phone_hash 不是当前版本的行，既覆盖空列也能
//     修复早期分支/旧密钥版本，并保持幂等；
//   - 逐行 UPDATE 而不是批量 CASE WHEN：每行密文的 nonce 不同，且单行更新
//     便于失败隔离（一行坏数据不拖垮整批）。

const (
	defaultBackfillBatchSize = 200
	maxBackfillBatchSize     = 1000
	defaultBackfillInterval  = 200 * time.Millisecond
	// maxBackfillBatchesPerCall 单次调用的批数上限，配合 interval 把一次请求的
	// 时长和 DB 压力都框住。
	maxBackfillBatchesPerCall = 50
)

// phoneShadowBackfillCAS 回填 UPDATE 的 CAS 条件（占位符顺序：
// id, 读取时的 phone_hash, zone, phone, is_destroy）。
//
// 提成常量是为了让测试能断言同一份谓词，避免"测试里手抄一份 WHERE"随生产代码漂移。
//
// 为什么不能只判 id + phone_hash：注销会把 phone 改写成墓碑值**并且**把
// phone_hash 清空，于是"读取手机号 → 账号被并发注销 → 把注销前手机号的密文/盲索引
// 写回去"这条竞态会让已注销账号重新占用原手机号，正是 destroyAccountFromState
// 要消除的那个 P0。带上读取时的 zone/phone 后，行在读取后发生任何改写都会让 CAS
// 失配、本行被跳过（下轮扫描按最新值重算）。
// 把读取时的 hash 也带入 CAS，才能安全地把 v1 替换为 v2，同时防止
// 两个回填 worker 相互覆盖。
const phoneShadowBackfillCAS = "id=? AND phone_hash=? AND zone=? AND phone=? AND is_destroy<>?"

// PhoneShadowBackfillResult 一次回填调用的进度快照。
type PhoneShadowBackfillResult struct {
	Scanned int64 `json:"scanned"` // 本次扫描的行数
	Updated int64 `json:"updated"` // 成功回填的行数
	Skipped int64 `json:"skipped"` // CAS 失配（读取后被并发改写）而跳过的行数，非错误
	Failed  int64 `json:"failed"`  // 加密/更新出错的行数
	// NextCursor 下次调用应传入的游标。扫描到表尾时归 0，让下一次调用从头重扫，
	// 从而把本轮 Failed/Skipped 的行捞回来重试（当前版本谓词保证幂等）。
	NextCursor int64 `json:"next_cursor"`
	// Done 仅当 Remaining==0 时为 true。刻意不用"本批不足 batchSize"来判定完成：
	// 那样会在存在 Failed/Skipped 行时报告 done=true 而 remaining>0，
	// 让运维以为回填已收敛。
	Done      bool  `json:"done"`
	Remaining int64 `json:"remaining"` // 仍待回填的行数
}

// phoneShadowBackfillRow 回填只需要这几列，避免 SELECT * 把整行拉回来。
type phoneShadowBackfillRow struct {
	ID        int64  `db:"id"`
	Zone      string `db:"zone"`
	Phone     string `db:"phone"`
	PhoneHash string `db:"phone_hash"`
}

// CountPhoneShadowPending 返回仍待回填的行数，供运维判断进度与收敛。
// 与扫描条件保持一致（含 is_destroy<>2），否则墓碑行会让 remaining 永远不归零。
func (d *DB) CountPhoneShadowPending() (int64, error) {
	var n int64
	_, err := d.session.Select("count(*)").From("user").
		Where("phone<>'' AND phone_hash NOT LIKE ? AND is_destroy<>?", phoneHashCurrentPattern, IsDestroyDone).Load(&n)
	if err != nil {
		return 0, fmt.Errorf("count phone shadow pending: %w", err)
	}
	return n, nil
}

// BackfillPhoneShadow 从 cursor 之后开始，分批回填手机号影子列。
//
// 主密钥必须就绪：没有密钥就没有回填的意义，直接返回错误而不是空跑一遍留下满地空列。
// 注意这里与 syncPhoneShadow 的取舍相反 —— 写路径缺密钥时降级放行（不能让主注册
// 路径挂掉），回填缺密钥时 fail-closed（它的全部意义就是产出影子列，没有可降级的语义）。
//
// 驱动方式：反复调用并把上次返回的 NextCursor 传回来，直到 Done==true。扫描到表尾时
// NextCursor 归 0，所以本轮 Failed/Skipped 的行会在下一轮被重新捞起（当前
// hash 版本谓词保证幂等）。ctx 取消会在批次边界干净退出，已提交的行不回滚。
//
// 运维注意：若连续多轮出现 Updated==0 而 Remaining>0，说明有行持续失败（脏数据等），
// 此时应看日志里的 id 人工排查，而不是继续无限重试。
func (d *DB) BackfillPhoneShadow(ctx context.Context, cursor int64, batchSize int, interval time.Duration) (PhoneShadowBackfillResult, error) {
	var result PhoneShadowBackfillResult
	scanCursor := cursor
	exhausted := false
	if d.phoneEnc == nil {
		return result, fmt.Errorf("%w: %s must be configured before backfill",
			ErrPhoneEncryptionUnavailable, phoneEncryptionSecretEnv)
	}
	if batchSize <= 0 {
		batchSize = defaultBackfillBatchSize
	}
	if batchSize > maxBackfillBatchSize {
		batchSize = maxBackfillBatchSize
	}
	if interval <= 0 {
		// 注意是 <=0 而不是 <0：HTTP 请求省略 interval_ms 时得到零值，若零值被当成
		// "不限速"，默认请求就会连续跑满 maxBackfillBatchesPerCall × batchSize 次更新，
		// 完全没有节流。默认必须是有节流的那一侧。
		interval = defaultBackfillInterval
	}

	for batch := 0; batch < maxBackfillBatchesPerCall; batch++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var rows []*phoneShadowBackfillRow
		// is_destroy<>2 排除已注销账号：它们的 phone 已被匿名化成
		// `<原号>@<stamp>@delete`，对这种墓碑值算盲索引既无意义又会污染列。
		// 冷静期(is_destroy=1)仍持有真实手机号，需要回填。
		_, err := d.session.Select("id", "zone", "phone", "phone_hash").From("user").
			Where("id>? AND phone<>'' AND phone_hash NOT LIKE ? AND is_destroy<>?", scanCursor, phoneHashCurrentPattern, IsDestroyDone).
			OrderAsc("id").Limit(uint64(batchSize)).Load(&rows)
		if err != nil {
			return result, fmt.Errorf("scan phone shadow batch: %w", err)
		}
		if len(rows) == 0 {
			exhausted = true
			break
		}
		for _, row := range rows {
			result.Scanned++
			scanCursor = row.ID
			encrypted, hash, last4, encErr := d.phoneEnc.encryptPhone(row.Zone, row.Phone)
			if encErr != nil {
				// 不记 phone 明文，只记 id 供人工排查
				log.Warn("回填手机号影子列失败,跳过该行",
					zap.Int64("id", row.ID), zap.Error(encErr))
				result.Failed++
				continue
			}
			// CAS 条件见 phoneShadowBackfillCAS 的注释（为什么必须带 zone/phone）。
			res, updErr := d.session.Update("user").SetMap(map[string]interface{}{
				"phone_encrypted": encrypted,
				"phone_hash":      hash,
				"phone_last4":     last4,
			}).Where(phoneShadowBackfillCAS, row.ID, row.PhoneHash, row.Zone, row.Phone, IsDestroyDone).Exec()
			if updErr != nil {
				log.Warn("回填手机号影子列更新失败,跳过该行",
					zap.Int64("id", row.ID), zap.Error(updErr))
				result.Failed++
				continue
			}
			affected, affErr := res.RowsAffected()
			if affErr != nil {
				log.Warn("回填手机号影子列无法确认影响行数,按跳过处理",
					zap.Int64("id", row.ID), zap.Error(affErr))
				result.Failed++
				continue
			}
			if affected == 0 {
				// 行在读取后被并发改写（注销/改号/其他回填进程抢先）。不是错误，
				// 也不能算已回填 —— 记为 skipped，由后续扫描按最新值处理。
				result.Skipped++
				continue
			}
			result.Updated++
		}
		log.Info("手机号影子列回填批次完成",
			zap.Int64("scanned", result.Scanned),
			zap.Int64("updated", result.Updated),
			zap.Int64("skipped", result.Skipped),
			zap.Int64("failed", result.Failed),
			zap.Int64("cursor", scanCursor))

		if len(rows) < batchSize {
			exhausted = true
			break
		}
		if interval > 0 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(interval):
			}
		}
	}

	// 游标语义：扫到表尾就归 0，让下一轮从头重扫以重试 Failed/Skipped 的行；
	// 否则那些行会永久落在 NextCursor 之后，再也不会被处理。
	if exhausted {
		result.NextCursor = 0
	} else {
		result.NextCursor = scanCursor
	}

	remaining, err := d.CountPhoneShadowPending()
	if err != nil {
		// 统计失败不影响回填本身的结果，但此时无法判定是否收敛 → 保持 Done=false
		log.Warn("统计待回填行数失败", zap.Error(err))
	} else {
		result.Remaining = remaining
		// 只有真正没有待回填行才算完成。用"本批不足 batchSize"判完成会在存在
		// Failed/Skipped 时误报 done=true + remaining>0。
		result.Done = remaining == 0
	}
	log.Info("手机号影子列回填调用结束",
		zap.Int64("scanned", result.Scanned),
		zap.Int64("updated", result.Updated),
		zap.Int64("skipped", result.Skipped),
		zap.Int64("failed", result.Failed),
		zap.Int64("next_cursor", result.NextCursor),
		zap.Bool("done", result.Done),
		zap.Int64("remaining", result.Remaining))
	return result, nil
}
