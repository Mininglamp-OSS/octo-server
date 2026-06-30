package sticker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const stickerMetricNamespace = "sticker"

func stickerRegisterResultLabels() []string {
	return []string{
		"success",
		"validation_failed",
		"path_invalid",
		"missing_handle",
		"invalid_handle",
		"no_capability",
		"quota_exceeded",
		"shortcode_conflict",
		"query_failed",
		"store_failed",
	}
}

func init() {
	for _, result := range stickerRegisterResultLabels() {
		metricStickerRegisterTotal.WithLabelValues(result).Add(0)
	}
}

var metricStickerRegisterTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: stickerMetricNamespace,
	Name:      "register_total",
	Help:      "Custom sticker registration outcomes by low-cardinality result.",
}, []string{"result"})

func observeStickerRegister(result string) {
	metricStickerRegisterTotal.WithLabelValues(result).Inc()
}
