package file

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const fileStickerMetricNamespace = "sticker"

func stickerUploadResultLabels() []string {
	return []string{
		"success",
		"path_rejected",
		"read_failed",
		"size_rejected",
		"format_rejected",
		"magic_rejected",
		"dimension_rejected",
		"upload_failed",
	}
}

func stickerUploadHandleResultLabels() []string {
	return []string{"issued", "disabled"}
}

func init() {
	for _, result := range stickerUploadResultLabels() {
		metricStickerUploadTotal.WithLabelValues(result).Add(0)
	}
	for _, result := range stickerUploadHandleResultLabels() {
		metricStickerUploadHandleTotal.WithLabelValues(result).Add(0)
	}
}

var (
	metricStickerUploadTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: fileStickerMetricNamespace,
		Name:      "upload_total",
		Help:      "Sticker multipart upload outcomes by low-cardinality result.",
	}, []string{"result"})

	metricStickerUploadHandleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: fileStickerMetricNamespace,
		Name:      "upload_handle_total",
		Help:      "Sticker upload handle issuance outcomes.",
	}, []string{"result"})
)

func observeStickerUpload(result string) {
	metricStickerUploadTotal.WithLabelValues(result).Inc()
}

func observeStickerUploadHandle(result string) {
	metricStickerUploadHandleTotal.WithLabelValues(result).Inc()
}
