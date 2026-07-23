package msgextraseq

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// This file registers the shared message_extra version allocator's Prometheus
// metrics (brief D8).
//
// Design choices:
//   - Registered on the global default Registry (promauto.New*); an
//     infrastructure PR exposes /metrics.
//   - Low cardinality: never add a channel_id / uid label (it would blow up
//     Prometheus). reserve_total carries only a result dimension splitting
//     success / retry / failure.
//   - lock_wait and state_lock_wait are separate: the former is the per-channel
//     sequence-row X lock, the latter the global state-row S lock (brief C2: the
//     latter is mutually compatible, observed separately for its contention).
//   - mode/epoch/floor are gauges, refreshed on every state read once active, so
//     they reflect the currently effective allocator generation.

const metricNamespace = "message_extra_version"

var (
	// Reservation result counter. result=success|retry|failure. retry is
	// reported by the caller on a whole-transaction 1213/1205 retry; the
	// allocator itself only distinguishes success/failure.
	metricReserveTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "reserve_total",
		Help:      "message_extra version reservations by result",
	}, []string{"result"})

	// Wall time of a full ReserveTx call (state read + sequence lock + allocation).
	metricReserveSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "reserve_seconds",
		Help:      "wall time of a single ReserveTx call",
		Buckets:   prometheus.DefBuckets,
	})

	// Wait to acquire the per-channel sequence row FOR UPDATE (the SELECT ... FOR
	// UPDATE duration only).
	metricLockWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "lock_wait_seconds",
		Help:      "wait to acquire the per-channel sequence row lock",
		Buckets:   prometheus.DefBuckets,
	})

	// Wait to acquire the global state row FOR SHARE (separate from the sequence
	// lock, brief C2).
	metricStateLockWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "state_lock_wait_seconds",
		Help:      "wait to acquire the shared allocator-state row lock",
		Buckets:   prometheus.DefBuckets,
	})

	// Batch size (count) requested per reservation.
	metricBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "batch_size",
		Help:      "count requested per ReserveTx call",
		Buckets:   []float64{1, 2, 5, 10, 50, 100, 500, 1000},
	})

	// Currently effective allocator mode, with a mode label (D8 contract:
	// mode=legacy|transactional). Each state read sets the active mode to 1 and
	// the other to 0, so dashboards can enumerate both series stably.
	metricAllocatorMode = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "allocator_mode",
		Help:      "active allocator mode (1 on the active label)",
	}, []string{"mode"})

	// Current allocator generation (state.epoch). Observability only, never a
	// decision input (brief C1).
	metricAllocatorEpoch = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "allocator_epoch",
		Help:      "active allocator epoch (observability only)",
	})

	// Currently effective cutover_floor.
	metricCutoverFloor = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "cutover_floor",
		Help:      "active transactional cutover floor",
	})

	// Defensive invariant-violation counter: a candidate version not strictly
	// greater than the locked row, or the row vanishing after allocation, etc.
	metricInvariantViolationTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "invariant_violation_total",
		Help:      "defensive invariant violations observed by the allocator",
	})
)

// setAllocatorModeGauge sets the active mode label to 1 and the other to 0, so
// both series are always present for dashboards (D8 mode=legacy|transactional).
func setAllocatorModeGauge(mode int) {
	legacy, transactional := 0.0, 0.0
	if mode == ModeTransactional {
		transactional = 1
	} else {
		legacy = 1
	}
	metricAllocatorMode.WithLabelValues("legacy").Set(legacy)
	metricAllocatorMode.WithLabelValues("transactional").Set(transactional)
}
