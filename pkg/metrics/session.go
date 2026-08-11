package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type SessionMetrics struct {
	Operations         *prometheus.CounterVec
	OperationLatency   *prometheus.HistogramVec
	ValidationReject   *prometheus.CounterVec
	PersistentSeen     prometheus.Counter
	EffectiveTTL       prometheus.Gauge
	RolloutMode        *prometheus.GaugeVec
	RevocationBacklog  prometheus.Gauge
	RevocationRetries  *prometheus.CounterVec
	LiveWriters        prometheus.Gauge
	WriterFences       prometheus.Counter
	FloorAdvances      *prometheus.CounterVec
	ReconcileBlocked   *prometheus.CounterVec
	ReconcileScans     prometheus.Counter
	RolloutBootOutcome *prometheus.GaugeVec
	UndecodableRecords prometheus.Gauge
	operationOK        map[string]prometheus.Counter
	operationError     map[string]prometheus.Counter
	operationLatency   map[string]prometheus.Observer
}

var defaultSessionMetrics atomic.Pointer[SessionMetrics]

func NewSessionMetrics(reg prometheus.Registerer) *SessionMetrics {
	m := &SessionMetrics{
		Operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "operations_total",
			Help:      "Token session store operations by low-cardinality operation and outcome.",
		}, []string{"operation", "outcome"}),
		OperationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "operation_duration_seconds",
			Help:      "Token session store operation latency by operation.",
			Buckets:   []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"operation"}),
		ValidationReject: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "validation_rejected_total",
			Help:      "Token validation rejections by low-cardinality reason.",
		}, []string{"reason"}),
		PersistentSeen: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "persistent_detected_total",
			Help:      "Legacy persistent token records observed by validation or bounded on update.",
		}),
		EffectiveTTL: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "effective_ttl_seconds",
			Help:      "Configured access-token TTL effective in this process.",
		}),
		RolloutMode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "rollout_mode",
			Help:      "Current session rollout mode; exactly one known mode is 1 per process.",
		}, []string{"mode"}),
		RevocationBacklog: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "revocation_backlog",
			Help:      "Pending durable session revocation intents visible to this process.",
		}),
		RevocationRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "revocation_retries_total",
			Help:      "Durable session revocation retry failures by bounded reason.",
		}, []string{"reason"}),
		LiveWriters: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "live_writers",
			Help:      "Writers holding a live write lease, as seen by this process.",
		}),
		WriterFences: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "writer_fence_total",
			Help:      "Times this process lost its write lease and refused to create new tokens.",
		}),
		FloorAdvances: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "floor_advance_total",
			Help:      "Rollout floor advances by resulting floor and who performed them.",
		}, []string{"to", "actor"}),
		ReconcileBlocked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "reconcile_blocked_total",
			Help:      "Reconciler cycles that could not advance, by bounded reason.",
		}, []string{"reason"}),
		ReconcileScans: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "reconcile_scan_total",
			Help:      "Keyspace scans performed by the rollout reconciler.",
		}),
		UndecodableRecords: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "undecodable_records",
			Help:      "Token records that cannot be decoded as any version; they never block the floor and nothing clears them.",
		}),
		RolloutBootOutcome: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "session",
			Name:      "rollout_boot_outcome",
			Help:      "How this process resolved its rollout mode at boot; exactly one outcome is 1.",
		}, []string{"outcome"}),
	}
	reg.MustRegister(m.Operations, m.OperationLatency, m.ValidationReject, m.PersistentSeen, m.EffectiveTTL,
		m.RolloutMode, m.RevocationBacklog, m.RevocationRetries,
		m.LiveWriters, m.WriterFences, m.FloorAdvances, m.ReconcileBlocked, m.ReconcileScans,
		m.RolloutBootOutcome, m.UndecodableRecords)
	for _, outcome := range sessionRolloutBootOutcomes {
		m.RolloutBootOutcome.WithLabelValues(outcome).Set(0)
	}
	for _, mode := range sessionRolloutModes {
		m.RolloutMode.WithLabelValues(mode).Set(0)
	}
	m.operationOK = make(map[string]prometheus.Counter, 5)
	m.operationError = make(map[string]prometheus.Counter, 5)
	m.operationLatency = make(map[string]prometheus.Observer, 5)
	for _, operation := range []string{"issue", "reuse", "update_payload", "read", "observe"} {
		m.operationOK[operation] = m.Operations.WithLabelValues(operation, "ok")
		m.operationError[operation] = m.Operations.WithLabelValues(operation, "error")
		m.operationLatency[operation] = m.OperationLatency.WithLabelValues(operation)
	}
	defaultSessionMetrics.Store(m)
	return m
}

func ObserveSessionOperation(operation string, started time.Time, err error) {
	if m := defaultSessionMetrics.Load(); m != nil {
		counter := m.operationOK[operation]
		if err != nil {
			counter = m.operationError[operation]
		}
		if counter == nil {
			outcome := "ok"
			if err != nil {
				outcome = "error"
			}
			counter = m.Operations.WithLabelValues(operation, outcome)
		}
		counter.Inc()
		observer := m.operationLatency[operation]
		if observer == nil {
			observer = m.OperationLatency.WithLabelValues(operation)
		}
		observer.Observe(time.Since(started).Seconds())
	}
}

func ObserveSessionValidationReject(reason string) {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.ValidationReject.WithLabelValues(reason).Inc()
	}
}

func ObservePersistentSession() {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.PersistentSeen.Inc()
	}
}

func SetSessionEffectiveTTL(ttl time.Duration) {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.EffectiveTTL.Set(ttl.Seconds())
	}
}

var sessionRolloutModes = []string{"expand", "v3-write", "revoke", "bounded", "enforce"}

// sessionRolloutBootOutcomes mirrors auth.RolloutBootOutcome. Kept as literals
// so pkg/metrics does not import pkg/auth.
var sessionRolloutBootOutcomes = []string{"fresh", "adopted", "rollback-recovered", "normal"}

// SetSessionLiveWriters records how many writers hold a live lease.
func SetSessionLiveWriters(count int) {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.LiveWriters.Set(float64(count))
	}
}

// ObserveSessionWriterFence counts a lost write lease. A sustained non-zero
// rate means replicas are refusing new logins, which is the intended
// degradation but still wants an alert.
func ObserveSessionWriterFence() {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.WriterFences.Inc()
	}
}

// ObserveSessionFloorAdvance records a floor transition. actor is "reconciler"
// or "operator" so an automatic advance is distinguishable during an incident.
func ObserveSessionFloorAdvance(to, actor string) {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.FloorAdvances.WithLabelValues(to, actor).Inc()
	}
}

// ObserveSessionReconcileBlocked counts a cycle that could not advance.
func ObserveSessionReconcileBlocked(reason string) {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.ReconcileBlocked.WithLabelValues(reason).Inc()
	}
}

// ObserveSessionReconcileScan counts a keyspace scan by the reconciler, so the
// backoff can be verified from metrics rather than trusted.
func ObserveSessionReconcileScan() {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.ReconcileScans.Inc()
	}
}

// SetSessionUndecodableRecords tracks records that cannot be decoded as any
// token version. They no longer block the floor — they were never usable
// credentials — but nothing clears them either, so a keyspace can sit at
// enforce with N of them and this is the only thing that says so.
func SetSessionUndecodableRecords(count int64) {
	if m := defaultSessionMetrics.Load(); m != nil {
		m.UndecodableRecords.Set(float64(count))
	}
}

// SetSessionRolloutBootOutcome records how boot resolved the mode.
func SetSessionRolloutBootOutcome(current string) {
	m := defaultSessionMetrics.Load()
	if m == nil {
		return
	}
	for _, outcome := range sessionRolloutBootOutcomes {
		value := 0.0
		if outcome == current {
			value = 1
		}
		m.RolloutBootOutcome.WithLabelValues(outcome).Set(value)
	}
}

func SetSessionRolloutMode(current string) {
	if m := defaultSessionMetrics.Load(); m != nil {
		for _, mode := range sessionRolloutModes {
			value := float64(0)
			if mode == current {
				value = 1
			}
			m.RolloutMode.WithLabelValues(mode).Set(value)
		}
	}
}

func SetSessionRevocationBacklog(count int64) {
	if m := defaultSessionMetrics.Load(); m != nil {
		if count < 0 {
			count = 0
		}
		m.RevocationBacklog.Set(float64(count))
	}
}

func ObserveSessionRevocationRetry(reason string) {
	switch reason {
	case "claim", "redis_apply", "mark_applied", "release":
	default:
		reason = "other"
	}
	if m := defaultSessionMetrics.Load(); m != nil {
		m.RevocationRetries.WithLabelValues(reason).Inc()
	}
}
