package bot_mention

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type botMentionMetricRecorder interface {
	ObserveIngress(result string, duration time.Duration)
	ObserveEnqueue(result string, duration time.Duration)
}

type botMentionMetrics struct {
	ingressTotal    *prometheus.CounterVec
	enqueueTotal    *prometheus.CounterVec
	ingressDuration *prometheus.HistogramVec
	enqueueDuration *prometheus.HistogramVec
}

var defaultBotMentionMetrics = newBotMentionMetrics(prometheus.DefaultRegisterer)

func newBotMentionMetrics(registerer prometheus.Registerer) *botMentionMetrics {
	if registerer == nil {
		return nil
	}
	metrics := &botMentionMetrics{
		ingressTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_doc_bot_mention_ingress_total",
			Help: "Document comment bot mention ingress outcomes.",
		}, []string{"result"}),
		enqueueTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_doc_bot_mention_enqueue_total",
			Help: "Document comment bot mention event enqueue outcomes.",
		}, []string{"result"}),
		ingressDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dmwork_doc_bot_mention_ingress_duration_seconds",
			Help:    "Document comment bot mention ingress latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		enqueueDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dmwork_doc_bot_mention_enqueue_duration_seconds",
			Help:    "Document comment bot mention event enqueue latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
	}
	registerer.MustRegister(
		metrics.ingressTotal,
		metrics.enqueueTotal,
		metrics.ingressDuration,
		metrics.enqueueDuration,
	)
	return metrics
}

func (m *botMentionMetrics) ObserveIngress(result string, duration time.Duration) {
	if m == nil {
		return
	}
	result = normalizeIngressMetricResult(result)
	m.ingressTotal.WithLabelValues(result).Inc()
	m.ingressDuration.WithLabelValues(result).Observe(duration.Seconds())
}

func (m *botMentionMetrics) ObserveEnqueue(result string, duration time.Duration) {
	if m == nil {
		return
	}
	result = normalizeEnqueueMetricResult(result)
	m.enqueueTotal.WithLabelValues(result).Inc()
	m.enqueueDuration.WithLabelValues(result).Observe(duration.Seconds())
}

func normalizeIngressMetricResult(result string) string {
	switch result {
	case "accepted", "replay", "disabled", "invalid", "unauthorized", "conflict", "error":
		return result
	default:
		return "error"
	}
}

func normalizeEnqueueMetricResult(result string) string {
	switch result {
	case "accepted", "error":
		return result
	default:
		return "error"
	}
}
