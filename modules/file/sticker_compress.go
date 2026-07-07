package file

import (
	"bytes"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
)

// 服务端贴纸压缩（sticker-upload-compression 任务，方案 C）。
//
// 首期只压静态 jpg/png：decode → 必要时缩放到 maxDim → 用原格式重编码（去 EXIF/
// XMP 元数据；imaging 默认不保留）。webp/gif/动图 恒 skip（首期依赖无 webp encoder，
// 引入 cgo 明确 out of scope）。压缩后仍超 target 直接拒绝，避免存"压完还是大图"。
//
// 稳定性隔离：每次尝试要占用一个"压缩并发 slot"（进程级 mutex+counter，容量由
// StickerCompressMaxConcurrency 动态读取，改配置即时生效）；饱和时立刻 fail-open
// 走原路径。压缩本体跑在 goroutine 里由 time.Timer 抢占，超时即时返回 skipped，
// goroutine 在 imaging 完成时自然退出（无长时间泄漏，最坏是几十 ms 的 dangling）。
//
// 结果 Outcome：
//   - compressed → 用返回 Bytes 替换 upload 源字节
//   - skipped    → 走原路径（disabled/format/concurrency_saturated/timeout）
//   - failed     → 走原路径 fail-open（decode/encode 错误；不阻塞主链路）
//   - over_limit → caller 直接拒绝上传（压完仍超 target）

// stickerLimitsSnapshot 是 uploadFile 一次请求内的贴纸限制快照。全部字段在
// 请求进入时锁定；SystemSettings 60s 后 reload 不影响正在进行的请求，避免
// "校验通过用一套值、签发 handle 用另一套值"这类跨阶段不一致。
type stickerLimitsSnapshot struct {
	maxSize         int64
	maxDim          int
	allowedFormats  map[string]bool
	compressEnabled bool
}

// stickerLimits 从 File 挂的 SystemSettings 派生本次请求的限制值；未挂 settings
// （历史 unit test 直接 &File{} 构造）回落到硬编码默认值，让老 unit test 行为
// 逐字节等价。allowedFormats 是拷贝的 map，caller 不应修改。
func (f *File) stickerLimits() stickerLimitsSnapshot {
	if f.settings == nil {
		// 回落：老 unit test path。复刻改动前的硬编码默认值。
		allow := make(map[string]bool, len(stickerUploadExts))
		for k, v := range stickerUploadExts {
			allow[k] = v
		}
		return stickerLimitsSnapshot{
			maxSize:         StickerMaxFileSize,
			maxDim:          StickerMaxDimension,
			allowedFormats:  allow,
			compressEnabled: false,
		}
	}
	kb := f.settings.StickerUploadMaxSizeKB()
	formats := f.settings.StickerUploadAllowedFormats()
	m := make(map[string]bool, len(formats))
	for _, e := range formats {
		m[e] = true
	}
	return stickerLimitsSnapshot{
		maxSize:         int64(kb) * 1024,
		maxDim:          f.settings.StickerUploadMaxDimension(),
		allowedFormats:  m,
		compressEnabled: f.settings.StickerCompressEnabled(),
	}
}

// stickerSystemSettings 是 File 只用到的 SystemSettings 子集接口。定义在
// modules/file 侧（接口在使用端）让测试可以注入内存 fake，无需 MySQL/Redis
// 起 test server；生产用 *common.SystemSettings 天然实现。
type stickerSystemSettings interface {
	StickerUploadMaxSizeKB() int
	StickerUploadMaxDimension() int
	StickerUploadAllowedFormats() []string
	StickerCompressEnabled() bool
	StickerCompressTargetKB() int
	StickerCompressMaxConcurrency() int
	StickerCompressTimeoutMs() int
}

// stickerCompressSettings 抽出 SystemSettings 需要的接口，方便 test 注入 fake。
type stickerCompressSettings interface {
	StickerCompressEnabled() bool
	StickerCompressTargetKB() int
	StickerCompressMaxConcurrency() int
	StickerCompressTimeoutMs() int
	StickerUploadMaxDimension() int
}

const (
	stickerCompressOutcomeCompressed = "compressed"
	stickerCompressOutcomeSkipped    = "skipped"
	stickerCompressOutcomeFailed     = "failed"
	stickerCompressOutcomeOverLimit  = "over_limit"
)

// stickerCompressResult 是 Compress 的结果。
type stickerCompressResult struct {
	Outcome string // stickerCompressOutcome* 之一
	Reason  string // 细分原因（"disabled"/"format"/"concurrency_saturated"/"timeout"/decode 或 encode 错误信息）
	Bytes   []byte // 仅 Outcome=="compressed" 时有效
	Size    int64  // 结果字节数（compressed）或压后大小（over_limit）
}

// stickerCompressor 承担压缩流程 + 稳定性闸。零值不可用；用 newStickerCompressor。
type stickerCompressor struct {
	settings stickerCompressSettings
	mu       sync.Mutex
	inflight int
	// doCompress 是纯 CPU 压缩本体；tests 注入替代函数以稳定触发超时分支。生产
	// 路径固定为 doCompressStaticSticker。
	doCompress func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error)
}

// newStickerCompressor 用给定 settings 构造压缩器，绑定生产实现。
func newStickerCompressor(s stickerCompressSettings) *stickerCompressor {
	return &stickerCompressor{
		settings:   s,
		doCompress: doCompressStaticSticker,
	}
}

// Compress 尝试压缩 src；ext 应是 caller 归一化后的小写扩展名（含前导 "."）。
// 语义见文件顶部 Outcome 注释。永不 panic：解码 / 编码错误映射成 failed。
//
// 耗时观测：仅在 acquire slot 之后（真正投入 CPU 的路径）打点，包括
// compressed / over_limit / failed / skipped:timeout。disabled/format/
// concurrency_saturated 分支耗时约等于 0，用 counter 观测即可，不打 histogram
// 以降低噪声。
func (c *stickerCompressor) Compress(ext string, src []byte) stickerCompressResult {
	if !c.settings.StickerCompressEnabled() {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "disabled"}
	}
	if !canCompressStickerExt(ext) {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "format"}
	}
	maxConc := c.settings.StickerCompressMaxConcurrency()
	if !c.tryAcquireCompressSlot(maxConc) {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "concurrency_saturated"}
	}
	defer c.releaseCompressSlot()

	timeoutMs := c.settings.StickerCompressTimeoutMs()
	timeout := time.Duration(timeoutMs) * time.Millisecond
	maxDim := c.settings.StickerUploadMaxDimension()
	targetKB := c.settings.StickerCompressTargetKB()

	type outcome struct {
		r   stickerCompressResult
		err error
	}
	// 缓冲 1 让即使超时后 goroutine 完成也能非阻塞地写回，避免泄漏 sender。
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				// image.Decode 依赖第三方解码器，理论上返回 error，但 defence in
				// depth：panic 也走 failed 分支而不是崩溃主流程。
				ch <- outcome{err: fmt.Errorf("panic: %v", rec)}
			}
		}()
		r, err := c.doCompress(ext, src, maxDim, targetKB)
		ch <- outcome{r: r, err: err}
	}()

	start := time.Now()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		observeStickerCompressDuration(stickerCompressOutcomeSkipped, ext, time.Since(start))
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "timeout"}
	case o := <-ch:
		elapsed := time.Since(start)
		if o.err != nil {
			observeStickerCompressDuration(stickerCompressOutcomeFailed, ext, elapsed)
			return stickerCompressResult{Outcome: stickerCompressOutcomeFailed, Reason: o.err.Error()}
		}
		observeStickerCompressDuration(o.r.Outcome, ext, elapsed)
		return o.r
	}
}

func (c *stickerCompressor) tryAcquireCompressSlot(max int) bool {
	if max <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight >= max {
		return false
	}
	c.inflight++
	return true
}

func (c *stickerCompressor) releaseCompressSlot() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight > 0 {
		c.inflight--
	}
}

// canCompressStickerExt 匹配"本期压缩范围内"的扩展名。caller 必须传归一化过的
// 小写形式；大小写不敏感由 caller 负责（file/api.go 里 ext 已被 ToLower）。
func canCompressStickerExt(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".png":
		return true
	}
	return false
}

// doCompressStaticSticker 是生产实现：decode → optional resize → re-encode。
// 纯 CPU/内存，无 IO，无 context —— caller 通过 goroutine + timer 抢占。
func doCompressStaticSticker(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return stickerCompressResult{}, fmt.Errorf("decode: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxDim > 0 && (w > maxDim || h > maxDim) {
		// Fit 保持长宽比缩到 maxDim×maxDim 的外接框内。Lanczos 提供最佳视觉质量，
		// 对贴纸小图（<=1024 边）耗时可控（毫秒级）。
		img = imaging.Fit(img, maxDim, maxDim, imaging.Lanczos)
	}

	var buf bytes.Buffer
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		// 85 是贴纸场景兼顾清晰度与体积的经验值：<=80 出现明显 chroma artifact，
		// >=90 体积回升明显。JPEG 编码天然不携 EXIF/XMP，元数据自动脱除。
		if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(85)); err != nil {
			return stickerCompressResult{}, fmt.Errorf("encode: %w", err)
		}
	case ".png":
		// PNG 无损；imaging.Encode 默认不写 tEXt/iTXt/eXIf chunk，元数据脱除。
		if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
			return stickerCompressResult{}, fmt.Errorf("encode: %w", err)
		}
	default:
		// Guard: canCompressStickerExt 应过滤掉这个分支；到达即编程错误。
		return stickerCompressResult{}, fmt.Errorf("unsupported ext: %s", ext)
	}

	out := buf.Bytes()
	size := int64(len(out))
	if targetKB > 0 && size > int64(targetKB)*1024 {
		return stickerCompressResult{
			Outcome: stickerCompressOutcomeOverLimit,
			Size:    size,
		}, nil
	}
	return stickerCompressResult{
		Outcome: stickerCompressOutcomeCompressed,
		Bytes:   out,
		Size:    size,
	}, nil
}
