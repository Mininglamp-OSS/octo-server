package cardactiondispatch

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	duration                *prometheus.HistogramVec
	results                 *prometheus.CounterVec
	errors                  *prometheus.CounterVec
	retries                 *prometheus.CounterVec
	leased                  *prometheus.GaugeVec
	readyDepth              prometheus.Gauge
	dlqDepth                prometheus.Gauge
	applicantNotifyFailures *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	metrics := &Metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dmwork_card_action_dispatch_duration_seconds",
			Help:    "End-to-end first-party card action dispatch duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"owner", "result"}),
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_action_dispatch_result_total",
			Help: "First-party card action dispatch results.",
		}, []string{"owner", "result"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_action_dispatch_error_total",
			Help: "First-party card action callback and finalization errors.",
		}, []string{"owner", "category"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_action_dispatch_retry_total",
			Help: "Scheduled first-party card action retries.",
		}, []string{"owner"}),
		leased: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dmwork_card_action_dispatch_leased",
			Help: "Currently leased card action events in this process.",
		}, []string{"owner"}),
		readyDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dmwork_card_action_dispatch_ready_depth",
			Help: "Current shared ready card action queue depth.",
		}),
		dlqDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dmwork_card_action_dispatch_dlq_depth",
			Help: "Current shared card action dead-letter queue depth.",
		}),
		applicantNotifyFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_action_dispatch_applicant_notify_failure_total",
			Help: "Applicant outcome notification failures after a card action decision.",
		}, []string{"owner"}),
	}
	reg.MustRegister(metrics.duration, metrics.results, metrics.errors, metrics.retries,
		metrics.leased, metrics.readyDepth, metrics.dlqDepth, metrics.applicantNotifyFailures)
	return metrics
}

func (m *Metrics) beginLease(owner string) {
	if m != nil {
		m.leased.WithLabelValues(metricOwner(owner)).Inc()
	}
}

func (m *Metrics) endLease(owner string) {
	if m != nil {
		m.leased.WithLabelValues(metricOwner(owner)).Dec()
	}
}

func (m *Metrics) finish(owner, result string, started time.Time) {
	if m == nil {
		return
	}
	owner = metricOwner(owner)
	result = metricResult(result)
	m.results.WithLabelValues(owner, result).Inc()
	m.duration.WithLabelValues(owner, result).Observe(time.Since(started).Seconds())
}

func (m *Metrics) observeError(owner, category string) {
	if m != nil {
		m.errors.WithLabelValues(metricOwner(owner), metricErrorCategory(category)).Inc()
	}
}

func (m *Metrics) observeRetry(owner string) {
	if m != nil {
		m.retries.WithLabelValues(metricOwner(owner)).Inc()
	}
}

func (m *Metrics) observeApplicantNotifyFailure(owner string) {
	if m != nil {
		m.applicantNotifyFailures.WithLabelValues(metricOwner(owner)).Inc()
	}
}

func (m *Metrics) setDepths(ready, dlq int64) {
	if m != nil {
		m.readyDepth.Set(float64(ready))
		m.dlqDepth.Set(float64(dlq))
	}
}

func metricOwner(owner string) string {
	if ownerPattern.MatchString(owner) {
		return owner
	}
	return "invalid"
}

func metricResult(result string) string {
	switch result {
	case "ok", "deferred", "retry", "dlq", "error":
		return result
	default:
		return "error"
	}
}

func metricErrorCategory(category string) string {
	switch category {
	case "transport_failed", "rejected", "redirect_rejected", "invalid_response",
		"consumer_5xx", "finalize_failed", "applicant_notify_failed", "route_missing",
		"attempts_exhausted", "queue_error", "ack_lost":
		return category
	default:
		return "unknown"
	}
}
