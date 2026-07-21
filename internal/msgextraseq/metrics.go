package msgextraseq

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// 本文件登记共享 message_extra 版本分配器的 Prometheus 指标(brief D8)。
//
// 设计取舍:
//   - 注册到全局默认 Registry(promauto.New*),由基础设施 PR 暴露 /metrics。
//   - 低基维:绝不加 channel_id / uid label(会爆 Prometheus)。reserve_total 仅
//     带 result 维度切分成功/重试/失败。
//   - lock_wait 与 state_lock_wait 分开:前者是每频道 sequence 行 X 锁,后者是全局
//     state 行 S 锁(brief C2:后者互相兼容,单独观测其争用)。
//   - mode/epoch/floor 为 gauge,启用后由每次 state 读刷新,反映当前生效的分配代。

const metricNamespace = "message_extra_version"

var (
	// reserve 结果计数。result=success|retry|failure。retry 由调用方在整事务
	// 1213/1205 重试时上报;分配器本身只区分 success/failure。
	metricReserveTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "reserve_total",
		Help:      "message_extra version reservations by result",
	}, []string{"result"})

	// 一次 ReserveTx 全程耗时(含 state 读 + 序列锁 + 分配)。
	metricReserveSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "reserve_seconds",
		Help:      "wall time of a single ReserveTx call",
		Buckets:   prometheus.DefBuckets,
	})

	// 获取每频道 sequence 行 FOR UPDATE 的等待耗时(近似:SELECT ... FOR UPDATE 时长)。
	metricLockWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "lock_wait_seconds",
		Help:      "wait to acquire the per-channel sequence row lock",
		Buckets:   prometheus.DefBuckets,
	})

	// 获取全局 state 行 FOR SHARE 的等待耗时(与 sequence 锁分开,brief C2)。
	metricStateLockWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "state_lock_wait_seconds",
		Help:      "wait to acquire the shared allocator-state row lock",
		Buckets:   prometheus.DefBuckets,
	})

	// 每次预留的批量大小(count)。
	metricBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "batch_size",
		Help:      "count requested per ReserveTx call",
		Buckets:   []float64{1, 2, 5, 10, 50, 100, 500, 1000},
	})

	// 当前生效的 allocator 模式(0=legacy,1=transactional)。每次 state 读刷新。
	metricAllocatorMode = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "allocator_mode",
		Help:      "active allocator mode: 0=legacy, 1=transactional",
	})

	// 当前 allocator 代(state.epoch)。仅观测,不参与判定(brief C1)。
	metricAllocatorEpoch = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "allocator_epoch",
		Help:      "active allocator epoch (observability only)",
	})

	// 当前生效的 cutover_floor。
	metricCutoverFloor = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "cutover_floor",
		Help:      "active transactional cutover floor",
	})

	// 防御性不变量违反计数:观测到候选版本未严格大于锁定行、或分配后行消失等。
	metricInvariantViolationTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "invariant_violation_total",
		Help:      "defensive invariant violations observed by the allocator",
	})
)
