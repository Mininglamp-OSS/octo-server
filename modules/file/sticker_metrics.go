package file

import (
	"strings"
	"time"

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
		// sticker-upload-compression 任务新增：服务端压缩管线的分支观测。
		// compress_success  = 压缩成功且落库为压缩后字节
		// compress_failed   = 解码/编码失败，fail-open 走原路径（原字节仍上传）
		// compress_skipped  = 未压缩（禁用/非可压格式/并发满/超时），fail-open 走原路径
		// compress_over_limit = 压缩后仍超 target_kb，caller 拒绝上传
		// compress_oversized_rejected = 维度门为 jpg/png 放宽到接收硬上限后，压缩实际
		//   未把图缩到 upload_max_dimension 以内（compressor==nil / skipped / failed /
		//   compress_max_dimension 配得过大），fail-closed 拒绝，避免存/发超限大图。
		"compress_success",
		"compress_failed",
		"compress_skipped",
		"compress_over_limit",
		"compress_oversized_rejected",
	}
}

func stickerUploadHandleResultLabels() []string {
	return []string{"issued", "disabled"}
}

// stickerCompressDurationOutcomeLabels 是 compress_duration_seconds 的 outcome
// 维度枚举。与 observeStickerUpload 里的 compress_* 一一对应，独立列出以便预热
// 每种 outcome 的 histogram 序列（区分"零次"与"未注册"）。
func stickerCompressDurationOutcomeLabels() []string {
	return []string{
		stickerCompressOutcomeCompressed,
		stickerCompressOutcomeFailed,
		stickerCompressOutcomeSkipped,
		stickerCompressOutcomeOverLimit,
	}
}

// stickerCompressDurationFormatLabels 是 compress_duration_seconds 的 format
// 维度枚举。保持 low-cardinality：只放可能到达压缩管线的格式 + "other" 兜底
// （防 caller 传奇怪 ext 导致 label 爆炸）。
func stickerCompressDurationFormatLabels() []string {
	return []string{"jpg", "jpeg", "png", "gif", "webp", "other"}
}

func init() {
	for _, result := range stickerUploadResultLabels() {
		metricStickerUploadTotal.WithLabelValues(result).Add(0)
	}
	for _, result := range stickerUploadHandleResultLabels() {
		metricStickerUploadHandleTotal.WithLabelValues(result).Add(0)
	}
	// 预热压缩耗时 histogram 的每对 (outcome, format) 序列为 0 sample —— 与
	// counter 预热同款约定，让 dashboard 区分"零次"与"序列不存在"。
	for _, outcome := range stickerCompressDurationOutcomeLabels() {
		for _, format := range stickerCompressDurationFormatLabels() {
			metricStickerCompressDurationSeconds.WithLabelValues(outcome, format)
		}
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

	// metricStickerCompressDurationSeconds 记录服务端贴纸压缩的实际耗时，按
	// outcome + 归一化后的 format 打点。buckets 覆盖 5ms..5s：<10ms 常见于
	// skipped(format/disabled) 分支；50-500ms 是典型 jpg/png 512-1024 边压缩
	// 观测区间；>=1s 说明命中大图或系统抖动，>=5s 通常表示已被 timeout 截断
	// （不会到达 Observe，但保留 bucket 便于 P99 上界可见）。
	metricStickerCompressDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: fileStickerMetricNamespace,
		Name:      "compress_duration_seconds",
		Help:      "Sticker server-side compression wall-clock duration by outcome and format.",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"outcome", "format"})
)

func observeStickerUpload(result string) {
	metricStickerUploadTotal.WithLabelValues(result).Inc()
}

func observeStickerUploadHandle(result string) {
	metricStickerUploadHandleTotal.WithLabelValues(result).Inc()
}

// normalizeStickerCompressFormat 把 ext 归一化到 low-cardinality 集合，防止
// caller 传奇怪 ext 时 label 爆炸。ext 允许带或不带前导点，且完全大小写不敏感
// —— 先 ToLower 再 TrimPrefix，兑现函数注释里的合约（review F2）。
func normalizeStickerCompressFormat(ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(ext), ".")
	switch ext {
	case "jpg":
		return "jpg"
	case "jpeg":
		return "jpeg"
	case "png":
		return "png"
	case "gif":
		return "gif"
	case "webp":
		return "webp"
	}
	return "other"
}

// observeStickerCompressDuration 记录一次压缩耗时。outcome 应取
// stickerCompressOutcome* 常量；format 从 caller ext 传入，内部归一化。
func observeStickerCompressDuration(outcome, ext string, d time.Duration) {
	metricStickerCompressDurationSeconds.
		WithLabelValues(outcome, normalizeStickerCompressFormat(ext)).
		Observe(d.Seconds())
}
