package thread

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	// ordering 读取当前生效的「归档窗口 vs 最近列表隐藏窗口」偏序，仅在 Start() 调一次。
	// 生产路径注入 system_settings 读取器；为 nil 时跳过（单测默认）。
	ordering func() commonapi.ThreadArchiveOrdering
	// lastPolicy 只用于「策略变更时打一条日志」的去抖，避免每小时重复刷同样的值。
	// 仅在 ticker goroutine 内读写，无并发。
	lastPolicy   string
	policyLogged bool
	// clampWarnedDays 是 warnIfClamped 的去抖状态（上次已告警的越界天数，0=当前未越界）。
	// policy 闭包可能被 ticker goroutine 之外的 RunOnce 调用，故用 atomic。
	clampWarnedDays atomic.Int64

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
	w := &ArchiveWorker{
		cfg: cfg,
		db:  NewDB(ctx),
		gen: ctx,
		now: time.Now,
		Log: log.NewTLog("ThreadArchiveWorker"),
	}
	w.policy = func() (bool, time.Duration) {
		ss := commonapi.EnsureSystemSettings(ctx)
		enabled := ss.ThreadAutoArchiveEnabled()
		days := ss.ThreadAutoArchiveDays()
		w.warnIfClamped(enabled, days)
		return enabled, archiveThresholdFromDays(days)
	}
	w.ordering = func() commonapi.ThreadArchiveOrdering {
		return commonapi.EnsureSystemSettings(ctx).ThreadArchiveOrdering()
	}
	return w
}

// orderingViolatesIfEnabled 回答「这组取值**一旦归档开启**是否违反偏序」。
//
// 直接调 ViolatesThreadArchiveOrdering 不够：它在 !ArchiveEnabled 时短路返回 false，
// 这正是本函数要绕开的那个洞（见 logOrderingAtStart）。做法是把 ArchiveEnabled 置真后
// 再交给守卫本人判定 —— 不等式因此仍然只有一份实现，不在这里复制一遍。
func orderingViolatesIfEnabled(o commonapi.ThreadArchiveOrdering) bool {
	o.ArchiveEnabled = true
	return commonapi.ViolatesThreadArchiveOrdering(o)
}

// logOrderingAtStart 在 worker 启动时把生效的两级衰减取值打一条日志，违反偏序时升为 WARN。
//
// 为什么需要它：管理写路径的守卫只覆盖 DB→DB，且 ViolatesThreadArchiveOrdering 在
// !ArchiveEnabled 时短路，于是「纯 env 配出来的违规状态」不去写一次管理接口就永远不暴露
// （PR #679 review 第 7 轮，known boundaries 的「守卫只覆盖 DB→DB」那条）。
// 进程启动是唯一一个必然发生、且能同时看到两个窗口的时刻。
//
// 违规意味着子区会在最近列表的隐藏窗口走完之前就被归档 —— 运维调宽隐藏窗口会看不到任何
// 效果，因为归档先一步把子区拿走了。归档关着时同样告警（带 archive_enabled=false），
// 因为那正是「打开开关的瞬间就会生效」的潜伏状态，也正是守卫看不见的那一类。
func (w *ArchiveWorker) logOrderingAtStart() {
	if w.ordering == nil {
		return
	}
	o := w.ordering()
	fields := []zap.Field{
		zap.Bool("archive_enabled", o.ArchiveEnabled),
		zap.Int("archive_days", o.ArchiveDays),
		zap.Int("recent_filter_thread_days", o.RecentDays),
	}
	if orderingViolatesIfEnabled(o) {
		w.Warn("thread archive window is shorter than the recent-tab hiding window; "+
			"threads are archived before the hiding window elapses, so widening the "+
			"hiding window has no visible effect",
			fields...)
		return
	}
	w.Info("thread archive / recent-hiding windows at start", fields...)
}

// warnIfClamped 在生效天数被 maxArchiveDays 钳住时打一条 WARN。
//
// 需要它是因为管理台与 worker 会在这一点上分歧：`ThreadAutoArchiveDays()` 有意原样
// 保留任意大的 env 值（legacy 兼容），`GET /v1/manager/common/system_setting` 的
// `effective_value` 与跨键顺序守卫用的都是这个原始数字，而 worker 实际按钳制后的
// 阈值归档。`DM_THREAD_AUTO_ARCHIVE_DAYS=999999` 时管理台显示 999999 天、worker 按
// 36500 天执行 —— 不打日志就只能靠读代码才知道（PR #679 review, yujiawei）。
//
// 按取值去抖：同一个越界值只在首次出现和每次变更时各打一条，不会逐 tick 刷屏。
//
// 归档关闭时仍然告警（只是带上 `archive_enabled=false`）：这条日志报的是**配置读数
// 分歧**，不是「刚刚归档了什么」。关掉归档不会让管理台上那个数字变得诚实，而它会在
// 有人把开关打开的那一刻立即变成实际阈值 —— 那时才发现分歧就太晚了
// （PR #679 review, yujiawei）。
func (w *ArchiveWorker) warnIfClamped(enabled bool, days int) {
	if days <= maxArchiveDays {
		w.clampWarnedDays.Store(0)
		return
	}
	if w.clampWarnedDays.Swap(int64(days)) == int64(days) {
		return
	}
	w.Warn("thread auto-archive days exceeds the supported maximum; the worker clamps it "+
		"while the admin API still reports the raw value (moot while archiving is off, "+
		"but it takes effect the moment it is switched on)",
		zap.Int("configured_days", days),
		zap.Int("effective_days", maxArchiveDays),
		zap.Bool("archive_enabled", enabled))
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
//
// 注意这个钳制**只在 worker 内**：`GET /v1/manager/common/system_setting` 的
// `effective_value` 和跨键顺序守卫读的都是未钳制的原始天数，所以管理台可能显示一个
// 大于 worker 实际执行的值。发生时 warnIfClamped 会打 WARN，logPolicyOnChange 打出的
// 也是钳制后的 Duration。
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
	// 放在 Interval 门**之前**：这条日志的全部意义是「进程启动是唯一必然发生、
	// 且能同时看到两个窗口的时刻」，摆在提前返回之后就等于把同一个盲区下移一层。
	//
	// 注意这是防御性的，不是修一个当前可达的洞：LoadArchiveConfig 走 parseDuration，
	// 它把 d <= 0 映射回默认值，所以 cfg.Interval <= 0 从 env 根本产生不出来，只有
	// 手工构造的测试配置能命中。先前的注释断言 DM_THREAD_AUTO_ARCHIVE_INTERVAL=0
	// 会 park worker，那是错的（PR #695 review 第 4 轮，yujiawei 自陈其前提有误）。
	// 保留这个顺序是为了移除对该门的依赖，万一它日后变得可达。
	w.logOrderingAtStart()
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
