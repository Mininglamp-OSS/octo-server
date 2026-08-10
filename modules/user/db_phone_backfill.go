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
//   - 只回填 phone<>'' AND phone_hash='' 的行，天然幂等，可以随时中断续跑；
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

// PhoneShadowBackfillResult 一次回填调用的进度快照。
type PhoneShadowBackfillResult struct {
	Scanned    int64 `json:"scanned"`     // 本次扫描的行数
	Updated    int64 `json:"updated"`     // 成功回填的行数
	Failed     int64 `json:"failed"`      // 加密/更新失败的行数（已跳过，可重跑）
	NextCursor int64 `json:"next_cursor"` // 下次调用应传入的游标
	Done       bool  `json:"done"`        // 是否已扫到表尾
	Remaining  int64 `json:"remaining"`   // 仍待回填的行数（phone<>'' AND phone_hash=''）
}

// phoneShadowBackfillRow 回填只需要这几列，避免 SELECT * 把整行拉回来。
type phoneShadowBackfillRow struct {
	ID    int64  `db:"id"`
	Zone  string `db:"zone"`
	Phone string `db:"phone"`
}

// CountPhoneShadowPending 返回仍待回填的行数，供运维判断进度与收敛。
func (d *DB) CountPhoneShadowPending() (int64, error) {
	var n int64
	_, err := d.session.Select("count(*)").From("user").
		Where("phone<>'' AND phone_hash=''").Load(&n)
	if err != nil {
		return 0, fmt.Errorf("count phone shadow pending: %w", err)
	}
	return n, nil
}

// BackfillPhoneShadow 从 cursor 之后开始，分批回填手机号影子列。
//
// 主密钥必须就绪（与 syncPhoneShadow 的 fail-closed 一致）：没有密钥就没有回填的意义，
// 直接返回错误而不是空跑一遍留下满地空列。
//
// 调用方按返回的 NextCursor 反复调用直到 Done；ctx 取消会在批次边界干净退出，
// 已完成的批次不回滚（幂等，续跑即可）。
func (d *DB) BackfillPhoneShadow(ctx context.Context, cursor int64, batchSize int, interval time.Duration) (PhoneShadowBackfillResult, error) {
	result := PhoneShadowBackfillResult{NextCursor: cursor}
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
	if interval < 0 {
		interval = defaultBackfillInterval
	}

	for batch := 0; batch < maxBackfillBatchesPerCall; batch++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var rows []*phoneShadowBackfillRow
		_, err := d.session.Select("id", "zone", "phone").From("user").
			Where("id>? AND phone<>'' AND phone_hash=''", result.NextCursor).
			OrderAsc("id").Limit(uint64(batchSize)).Load(&rows)
		if err != nil {
			return result, fmt.Errorf("scan phone shadow batch: %w", err)
		}
		if len(rows) == 0 {
			result.Done = true
			break
		}
		for _, row := range rows {
			result.Scanned++
			result.NextCursor = row.ID
			encrypted, hash, last4, encErr := d.phoneEnc.encryptPhone(row.Zone, row.Phone)
			if encErr != nil {
				// 不记 phone 明文，只记 id 供人工排查
				log.Warn("回填手机号影子列失败,跳过该行",
					zap.Int64("id", row.ID), zap.Error(encErr))
				result.Failed++
				continue
			}
			// 条件里再带一次 phone_hash=''，避免与并发写/重复跑互相覆盖
			if _, updErr := d.session.Update("user").SetMap(map[string]interface{}{
				"phone_encrypted": encrypted,
				"phone_hash":      hash,
				"phone_last4":     last4,
			}).Where("id=? AND phone_hash=''", row.ID).Exec(); updErr != nil {
				log.Warn("回填手机号影子列更新失败,跳过该行",
					zap.Int64("id", row.ID), zap.Error(updErr))
				result.Failed++
				continue
			}
			result.Updated++
		}
		log.Info("手机号影子列回填批次完成",
			zap.Int64("scanned", result.Scanned),
			zap.Int64("updated", result.Updated),
			zap.Int64("failed", result.Failed),
			zap.Int64("cursor", result.NextCursor))

		if len(rows) < batchSize {
			result.Done = true
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

	remaining, err := d.CountPhoneShadowPending()
	if err != nil {
		// 统计失败不影响回填本身的结果
		log.Warn("统计待回填行数失败", zap.Error(err))
	} else {
		result.Remaining = remaining
	}
	log.Info("手机号影子列回填调用结束",
		zap.Int64("scanned", result.Scanned),
		zap.Int64("updated", result.Updated),
		zap.Int64("failed", result.Failed),
		zap.Int64("next_cursor", result.NextCursor),
		zap.Bool("done", result.Done),
		zap.Int64("remaining", result.Remaining))
	return result, nil
}
