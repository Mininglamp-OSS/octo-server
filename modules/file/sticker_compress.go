package file

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"path/filepath"
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
// "校验通过用一套值、压缩用另一套值"这类跨阶段不一致（review F7）。压缩相关
// 字段（compressTargetKB/MaxConcurrency/TimeoutMs）也放这里，Compress 通过
// stickerCompressParams 拿到同一份值，确保 dimension 校验用的 maxDim 与
// 压缩阶段 Fit 用的 maxDim 严格同源。
type stickerLimitsSnapshot struct {
	maxSize                int64
	maxDim                 int
	allowedFormats         map[string]bool
	compressEnabled        bool
	compressTargetKB       int
	compressMaxConcurrency int
	compressTimeoutMs      int
}

// stickerCompressParams 是从 stickerLimitsSnapshot 派生出、只喂给 Compress 的
// 子集，让 compressor 的 API 表面明确表达"这些值由 caller 决定，请求内不再
// 变化"。Compress 内部**不**再读 c.settings 的对应字段（review F7 修复要点）。
type stickerCompressParams struct {
	MaxDim         int
	TargetKB       int
	MaxConcurrency int
	TimeoutMs      int
}

// compressParams 从 snapshot 派生 Compress 需要的 4 个值。集中在这里派生
// 是为了让 caller 端一处清晰地展现"这四个值都来自同一份 snapshot"。
func (s stickerLimitsSnapshot) compressParams() stickerCompressParams {
	return stickerCompressParams{
		MaxDim:         s.maxDim,
		TargetKB:       s.compressTargetKB,
		MaxConcurrency: s.compressMaxConcurrency,
		TimeoutMs:      s.compressTimeoutMs,
	}
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
			maxSize:                StickerMaxFileSize,
			maxDim:                 StickerMaxDimension,
			allowedFormats:         allow,
			compressEnabled:        false,
			compressTargetKB:       0,
			compressMaxConcurrency: 0,
			compressTimeoutMs:      0,
		}
	}
	kb := f.settings.StickerUploadMaxSizeKB()
	formats := f.settings.StickerUploadAllowedFormats()
	m := make(map[string]bool, len(formats))
	for _, e := range formats {
		m[e] = true
	}
	return stickerLimitsSnapshot{
		maxSize:                int64(kb) * 1024,
		maxDim:                 f.settings.StickerUploadMaxDimension(),
		allowedFormats:         m,
		compressEnabled:        f.settings.StickerCompressEnabled(),
		compressTargetKB:       f.settings.StickerCompressTargetKB(),
		compressMaxConcurrency: f.settings.StickerCompressMaxConcurrency(),
		compressTimeoutMs:      f.settings.StickerCompressTimeoutMs(),
	}
}

// stickerUploadExtForRequest 是 stickerUploadExt 的"配置感知"版：把客户端
// filename 匹配到当前配置允许的扩展名集合（settings.StickerUploadAllowedFormats
// 的读侧交集结果），而不是硬编码历史 5 种。这样运营通过 upload_allowed_formats
// 收窄格式后，GET-side 生成的 preflight URL 与 POST-side 校验用的允许集保持
// 一致 —— 客户端不会拿到之后会被 upload 阶段拒的扩展名（review F1）。
//
// filename miss / 无扩展名时的 fallback 顺序：
//  1. 优先 ".gif"（历史默认，不传 filename 的老客户端保持原有行为）
//  2. 若 .gif 已被运营剔除，按 raster allowlist 固定顺序取第一个仍允许的
//     (.png → .jpg → .jpeg → .webp) —— 顺序确定性保证 fallback 稳定
//  3. 极端情况允许集为空（stickerLimits 读侧交集会回退默认所以不会到这里，
//     但作为兜底）：仍返回 ".gif"
//
// 未挂 settings 的路径（老 unit test 构造 &File{}）走 stickerLimits() 的
// nil-safe 分支，会拿到与老 stickerUploadExt 等价的 stickerUploadExts 集合，
// 因此本方法在那条路径上与旧行为逐字节等价。
func (f *File) stickerUploadExtForRequest(filename string) string {
	ext := strings.ToLower(filepath.Ext(sanitizeFilename(filename)))
	limits := f.stickerLimits()
	if limits.allowedFormats[ext] {
		return ext
	}
	if limits.allowedFormats[".gif"] {
		return ".gif"
	}
	for _, cand := range []string{".png", ".jpg", ".jpeg", ".webp"} {
		if limits.allowedFormats[cand] {
			return cand
		}
	}
	return ".gif"
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
// params 是 caller 从请求进入时锁定的 stickerLimitsSnapshot 派生的一份不可变
// 副本 —— maxDim/targetKB/maxConcurrency/timeoutMs 都取自同一份快照，兑现
// "一次请求一份快照"的不变式（review F7）。Compress 内部**不再**读 c.settings
// 的这四个字段；仅从 c.settings 读 StickerCompressEnabled 做双重短路（保留
// disabled 单元测试维度）。
//
// 耗时观测：仅在 acquire slot 之后（真正投入 CPU 的路径）打点，包括
// compressed / over_limit / failed / skipped:timeout。disabled/format/
// animated/concurrency_saturated 分支耗时约等于 0，用 counter 观测即可，不打
// histogram 以降低噪声。
func (c *stickerCompressor) Compress(ext string, src []byte, params stickerCompressParams) stickerCompressResult {
	if !c.settings.StickerCompressEnabled() {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "disabled"}
	}
	if !canCompressStickerExt(ext) {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "format"}
	}
	// APNG(Animated PNG) 复用 PNG magic 号，会通过 ext + magic + dimension 三
	// 道门。若不在此显式识别，image.Decode 只解首帧 IDAT，imaging.Encode 重
	// 编码回 PNG 时动画帧被静默丢失（review P2）。方案 C 明确"gif/animated
	// 只校验不压"，APNG 同样应走 skipped:animated，字节流原样落库。检测放在
	// tryAcquireCompressSlot 之前，避免为一次不需要压缩的请求占用并发 slot。
	if isAnimatedPNGSource(ext, src) {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "animated"}
	}
	if !c.tryAcquireCompressSlot(params.MaxConcurrency) {
		return stickerCompressResult{Outcome: stickerCompressOutcomeSkipped, Reason: "concurrency_saturated"}
	}
	// 关键：**不要**在此 defer release。timeout 分支返回时 doCompress goroutine
	// 仍在跑，若此时 release 会让下一个请求 acquire 到 slot，真实并发绕过
	// max_concurrency 上界。改由 worker goroutine 结束时（自然完成或 recover
	// 后）释放，让 slot 与真正的 CPU 占用生命周期一致（review P1）。

	timeout := time.Duration(params.TimeoutMs) * time.Millisecond
	maxDim := params.MaxDim
	targetKB := params.TargetKB

	type outcome struct {
		r   stickerCompressResult
		err error
	}
	// 缓冲 1 让即使超时后 goroutine 完成也能非阻塞地写回，避免泄漏 sender。
	ch := make(chan outcome, 1)
	go func() {
		// slot 一直持有到真正的 CPU 工作结束（含 recover 兜底），这是 P1 修复
		// 的核心：Compress 提前 return（timeout）时 slot 仍占用。
		defer c.releaseCompressSlot()
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

// isAnimatedPNGSource 检测 src 是否 APNG (Animated PNG)。ext 必须是 ".png"
// —— 其他扩展名一律返回 false（JPEG 无动画；GIF/WebP 走 canCompressStickerExt
// 已挡在压缩管线外）。APNG 通过额外的 acTL chunk 声明动画，标准 image/png
// 只解首帧 IDAT，若不显式识别会静默丢动画（review P2）。
func isAnimatedPNGSource(ext string, src []byte) bool {
	if ext != ".png" {
		return false
	}
	return hasAPNGActlChunk(src)
}

// hasAPNGActlChunk 扫描 PNG 字节流查找 acTL chunk（Animation Control Chunk）。
// APNG 规范：acTL 必须出现在**第一个 IDAT 之前**才有效；一致的渲染器会忽略
// IDAT 之后的 acTL。所以看到 IDAT 先于 acTL 即可判定为静态 PNG，短路结束扫描
// 也避免扫完整个大文件。CRC 不校验 —— 只关心结构标记，容忍非法字节。
func hasAPNGActlChunk(data []byte) bool {
	const pngSignature = "\x89PNG\r\n\x1a\n"
	if len(data) < 8 || string(data[:8]) != pngSignature {
		return false
	}
	// PNG chunk 格式: 4B length | 4B type | N-B data | 4B CRC
	pos := 8
	for pos+8 <= len(data) {
		length := binary.BigEndian.Uint32(data[pos : pos+4])
		chunkType := string(data[pos+4 : pos+8])
		switch chunkType {
		case "acTL":
			return true
		case "IDAT":
			return false
		}
		// 前进到下一个 chunk；用 int64 防 uint32 length + 12 溢出 int。
		next := int64(pos) + 8 + int64(length) + 4
		if next <= int64(pos) || next > int64(len(data)) {
			return false
		}
		pos = int(next)
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
