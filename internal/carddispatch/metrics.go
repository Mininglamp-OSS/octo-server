package carddispatch

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	attempts     *prometheus.CounterVec
	results      *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	inFlight     *prometheus.GaugeVec
	configErrors *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_dispatch_attempt_total",
			Help: "Total trusted internal card dispatch attempts.",
		}, []string{"producer", "target"}),
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_dispatch_result_total",
			Help: "Terminal trusted internal card dispatch results.",
		}, []string{"producer", "target", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dmwork_card_dispatch_duration_seconds",
			Help:    "Trusted internal card dispatch duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"producer", "target", "result"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dmwork_card_dispatch_in_flight",
			Help: "Current trusted internal card dispatch calls holding a producer slot.",
		}, []string{"producer"}),
		configErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_dispatch_config_error_total",
			Help: "Invalid trusted internal card producer configurations.",
		}, []string{"producer", "reason"}),
	}
	reg.MustRegister(m.attempts, m.results, m.duration, m.inFlight, m.configErrors)
	return m
}

func (m *Metrics) begin(producer, target string) time.Time {
	if m != nil {
		m.attempts.WithLabelValues(producer, target).Inc()
	}
	return time.Now()
}

func (m *Metrics) finish(producer, target string, start time.Time, result Category) {
	if m == nil {
		return
	}
	label := string(result)
	m.results.WithLabelValues(producer, target, label).Inc()
	m.duration.WithLabelValues(producer, target, label).Observe(time.Since(start).Seconds())
}

func (m *Metrics) addInFlight(producer string, delta float64) {
	if m != nil {
		m.inFlight.WithLabelValues(producer).Add(delta)
	}
}

func (m *Metrics) configError(producer, reason string) {
	if m != nil {
		m.configErrors.WithLabelValues(producer, reason).Inc()
	}
}
