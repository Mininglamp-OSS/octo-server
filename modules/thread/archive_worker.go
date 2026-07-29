package thread

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/common"

	"go.uber.org/zap"
)

// archiveDB 抽出 ArchiveWorker 需要的最小 DB 接口，便于单测 mock。
type archiveDB interface {
	ArchiveStaleBatch(threshold time.Time, batchSize int, version int64, channelType uint8) (int64, error)
}

// versionGen 抽出版本号生成，便于单测注入确定性版本号。
type versionGen interface {
	GenSeq(key string) (int64, error)
}

// ArchiveWorker 周期性扫描 thread 表，把过期 active 子区切到 archived。
type ArchiveWorker struct {
	cfg ArchiveConfig
	db  archiveDB
	gen versionGen
	now func() time.Time
	// policy 解析「是否开启 + 陈旧阈值」这两个**策略**项，每轮 tick 重新调用，
	// 因此管理台改值在一个 tick 内生效、无需重启（task
	// inactive-hiding-user-control / P1）。生产路径注入 system_settings 读取器；
	// 为 nil 时回落到 cfg.Enabled / cfg.Threshold，单测据此保持原有构造方式。
	policy func() (bool, time.Duration)
	// lastPolicy 只用于「策略变更时打一条日志」的去抖，避免每小时重复刷同样的值。
	// 仅在 ticker goroutine 内读写，无并发。
	lastPolicy   string
	policyLogged bool

	cancel context.CancelFunc
	wg     sync.WaitGroup
	log.Log
}

// NewArchiveWorker 构造 worker。生产路径用 thread.NewDB 和 config.Context 注入。
//
// cfg 只提供**运维调优**项（Interval / BatchSize / BatchSleep，仍来自 env）；
// **策略**项（开关 + 天数）走 system_settings → env → 代码默认的三级回落，由
// policy 在每个 tick 解析。cfg.Enabled / cfg.Threshold 在生产路径不被使用。
func NewArchiveWorker(ctx *config.Context, cfg ArchiveConfig) *ArchiveWorker {
	return &ArchiveWorker{
		cfg: cfg,
		db:  NewDB(ctx),
		gen: ctx,
		now: time.Now,
		policy: func() (bool, time.Duration) {
			ss := commonapi.EnsureSystemSettings(ctx)
			return ss.ThreadAutoArchiveEnabled(), archiveThresholdFromDays(ss.ThreadAutoArchiveDays())
		},
		Log: log.NewTLog("ThreadArchiveWorker"),
	}
}

// maxArchiveDays 是 days → time.Duration 转换的安全上限。
//
// time.Duration 是 int64 纳秒，约 106,751 天即溢出；再大就会回绕成一个**小的正数**
// 阈值（例如 ~213,504 天 ≈ 25 分钟），把几乎所有子区都归档掉。env 层现在按 legacy
// 语义原样接受任意非负整数（见 common.threadAutoArchiveDaysFromEnv），所以这个转换
// 必须自己设防（PR #679 review, yujiawei）。
//
// 上限取 36500 天（100 年）：远超任何真实策略，且离溢出点有两个数量级的余量。超过
// 即视为「实质不归档」，钳到上限而不是回绕。
const maxArchiveDays = 36500

// archiveThresholdFromDays 把天数转成 time.Duration，并挡住会让 int64 回绕的取值。
// days <= 0 原样返回 0，保留「禁用时间阈值」语义。
func archiveThresholdFromDays(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	if days > maxArchiveDays {
		days = maxArchiveDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// resolvePolicy 取当前生效的策略。注入了 policy 就用它（生产：每 tick 读
// system_settings 快照）；否则回落 cfg —— 单测走这条路径。
func (w *ArchiveWorker) resolvePolicy() (bool, time.Duration) {
	if w.policy != nil {
		return w.policy()
	}
	return w.cfg.Enabled, w.cfg.Threshold
}

// Start 启动后台 ticker。Interval 非法时不启动新 goroutine，但仍会先停掉可能存在的
// 旧 goroutine——避免热更新（Start→Start）留下孤儿 ticker。
// 重复调用幂等：先 stop 旧 goroutine 再启动新的。
//
// 启动门只卡 Interval。enabled 与 threshold 都是每 tick 重读的策略项（P1）：
//   - threshold=0 是明文支持的模式（DM_THREAD_AUTO_ARCHIVE_DAYS=0 / DB 写 0 →
//     禁用时间归档但开关仍可为 on），RunOnce 内部短路让每轮空转；
//   - enabled=false 同样只让每轮空转，而不是拒起 goroutine —— 否则管理台把开关
//     打开必须重启进程才生效，正是本次迁移要消除的。
func (w *ArchiveWorker) Start(ctx context.Context) {
	if w.cancel != nil {
		w.cancel()
		w.wg.Wait()
		w.cancel = nil
	}
	// 启动门只卡 Interval —— enabled 与 threshold 都已是每 tick 重读的策略项，
	// 卡在启动期会让「管理台开启归档」必须重启才生效，正是 P1 要消除的。
	// ticker 空转的代价是每 Interval 一次 system_settings 快照读（无 DB 往返）。
	if w.cfg.Interval <= 0 {
		w.Info("thread auto-archive worker not started: invalid interval",
			zap.Duration("interval", w.cfg.Interval))
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.Interval)
		defer t.Stop()
		w.Info("thread auto-archive worker started",
			zap.Duration("interval", w.cfg.Interval),
			zap.Int("batch_size", w.cfg.BatchSize),
			zap.Duration("batch_sleep", w.cfg.BatchSleep))
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				w.logPolicyOnChange()
				archived, err := w.RunOnce(rctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					w.Error("thread auto-archive run failed", zap.Error(err))
					continue
				}
				if archived > 0 {
					w.Info("thread auto-archive run", zap.Int64("archived", archived))
				}
			}
		}
	}()
}

// logPolicyOnChange 在策略（开关 / 阈值）相对上一轮发生变化时打一条 Info。
//
// 配置迁入 system_settings 后，「现在到底开没开、窗口几天」既能从
// GET /v1/manager/common/system_setting 的 effective_value 查到，也能在日志里
// 看到变更时刻 —— 这是本次迁移的主要动机之一（brief Background §2）。首轮无条件
// 打一条，作为运行态基线。
func (w *ArchiveWorker) logPolicyOnChange() {
	enabled, threshold := w.resolvePolicy()
	cur := fmt.Sprintf("%t/%s", enabled, threshold)
	if w.policyLogged && cur == w.lastPolicy {
		return
	}
	w.lastPolicy = cur
	w.policyLogged = true
	w.Info("thread auto-archive policy in effect",
		zap.Bool("enabled", enabled),
		zap.Duration("threshold", threshold))
}

// Stop 通知 worker 退出并等待当前 RunOnce 跑完。
func (w *ArchiveWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// RunOnce 执行一轮归档循环：批量 UPDATE 直到一批返回 < batchSize 或 ctx 取消。
// 返回本轮累计归档行数。
//
// 安全保护：threshold<=0 / batchSize<=0 视为禁用，直接返回 (0, nil)；
// ctx 取消时返回 ctx.Err() 让上层日志可区分"正常停机"vs"异常"。
func (w *ArchiveWorker) RunOnce(ctx context.Context) (int64, error) {
	enabled, threshold := w.resolvePolicy()
	if !enabled || threshold <= 0 || w.cfg.BatchSize <= 0 {
		return 0, nil
	}
	cutoff := w.now().Add(-threshold)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		version, err := w.gen.GenSeq(ThreadSeqKey)
		if err != nil {
			return total, err
		}
		rows, err := w.db.ArchiveStaleBatch(cutoff, w.cfg.BatchSize, version, common.ChannelTypeCommunityTopic.Uint8())
		if err != nil {
			return total, err
		}
		total += rows
		if rows < int64(w.cfg.BatchSize) {
			return total, nil
		}
		if w.cfg.BatchSleep > 0 {
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(w.cfg.BatchSleep):
			}
		}
	}
}
