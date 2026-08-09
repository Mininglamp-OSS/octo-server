package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type SessionMetrics struct {
	Operations       *prometheus.CounterVec
	OperationLatency *prometheus.HistogramVec
	ValidationReject *prometheus.CounterVec
	PersistentSeen   prometheus.Counter
	EffectiveTTL     prometheus.Gauge
	operationOK      map[string]prometheus.Counter
	operationError   map[string]prometheus.Counter
	operationLatency map[string]prometheus.Observer
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
	}
	reg.MustRegister(m.Operations, m.OperationLatency, m.ValidationReject, m.PersistentSeen, m.EffectiveTTL)
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
