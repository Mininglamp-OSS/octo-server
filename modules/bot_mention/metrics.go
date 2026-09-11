package bot_mention

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const metricKindUnknown = "unknown"

type botMentionMetricRecorder interface {
	ObserveIngress(result, docKind string, duration time.Duration)
	ObserveEnqueue(result string, duration time.Duration)
}

type botMentionMetrics struct {
	// Keep the result-only counter for existing dashboards; the by-kind series
	// adds diagnostics without changing that public metric label contract.
	ingressByKindTotal *prometheus.CounterVec
	ingressTotal       *prometheus.CounterVec
	enqueueTotal       *prometheus.CounterVec
	ingressDuration    *prometheus.HistogramVec
	enqueueDuration    *prometheus.HistogramVec
}

var defaultBotMentionMetrics = newBotMentionMetrics(prometheus.DefaultRegisterer)

func newBotMentionMetrics(registerer prometheus.Registerer) *botMentionMetrics {
	if registerer == nil {
		return nil
	}
	metrics := &botMentionMetrics{
		ingressByKindTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_doc_bot_mention_ingress_by_kind_total",
			Help: "Document comment bot mention ingress outcomes by normalized document kind.",
		}, []string{"result", "doc_kind"}),
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
		metrics.ingressByKindTotal,
		metrics.ingressTotal,
		metrics.enqueueTotal,
		metrics.ingressDuration,
		metrics.enqueueDuration,
	)
	return metrics
}

func (m *botMentionMetrics) ObserveIngress(result, docKind string, duration time.Duration) {
	if m == nil {
		return
	}
	result = normalizeIngressMetricResult(result)
	m.ingressTotal.WithLabelValues(result).Inc()
	m.ingressByKindTotal.WithLabelValues(result, normalizeIngressMetricKind(docKind)).Inc()
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
	case "accepted", "replay", "disabled", "invalid", "not_found", "unauthorized", "conflict", "error":
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

func normalizeIngressMetricKind(kind string) string {
	switch kind {
	case "":
		return "legacy"
	case docKindHTML, docKindPPT:
		return kind
	default:
		return metricKindUnknown
	}
}
