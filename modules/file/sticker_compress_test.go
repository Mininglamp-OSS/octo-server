package file

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 服务端贴纸压缩单测（sticker-upload-compression 任务）。
//
// 覆盖：disabled / 非可压格式 / 成功压 + 缩放 / 压完仍超限拒绝 / decode 失败
// fail-open / 并发满 fail-open / 超时 fail-open。timeout 分支通过注入
// doCompress 让代码路径可控。

type fakeStickerCompressSettings struct {
	enabled        bool
	targetKB       int
	maxConcurrency int
	timeoutMs      int
	maxDim         int
}

func (f *fakeStickerCompressSettings) StickerCompressEnabled() bool       { return f.enabled }
func (f *fakeStickerCompressSettings) StickerCompressTargetKB() int       { return f.targetKB }
func (f *fakeStickerCompressSettings) StickerCompressMaxConcurrency() int { return f.maxConcurrency }
func (f *fakeStickerCompressSettings) StickerCompressTimeoutMs() int      { return f.timeoutMs }
func (f *fakeStickerCompressSettings) StickerUploadMaxDimension() int     { return f.maxDim }

// params 从 fake 组装出 Compress 需要的 stickerCompressParams。测试从 fake
// 派生参数模拟"caller 已在请求进入时锁定了一份 snapshot"的语义（review F7）。
func (f *fakeStickerCompressSettings) params() stickerCompressParams {
	return stickerCompressParams{
		MaxDim:         f.maxDim,
		TargetKB:       f.targetKB,
		MaxConcurrency: f.maxConcurrency,
		TimeoutMs:      f.timeoutMs,
	}
}

// makeTestJPEG 生成 (w,h) JPEG，颜色随机以避免过度可压缩。
func makeTestJPEG(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{byte((x * 7) % 255), byte((y * 11) % 255), byte((x + y*3) % 255), 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}))
	return buf.Bytes()
}

func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// 用可预测的伪随机填充（不是简单公式），让 PNG 无法过度压缩，避免 512×512
	// 也能被压到几 KB 破坏 over_limit 测试。种子固定确保可重现。
	seed := uint32(0xdeadbeef)
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			seed = seed*1103515245 + 12345
			r := byte(seed >> 16)
			seed = seed*1103515245 + 12345
			g := byte(seed >> 16)
			seed = seed*1103515245 + 12345
			bch := byte(seed >> 16)
			img.Set(x, y, color.RGBA{r, g, bch, 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func newFakeCompressor(fs *fakeStickerCompressSettings) *stickerCompressor {
	return &stickerCompressor{
		settings:   fs,
		doCompress: doCompressStaticSticker,
	}
}

func TestStickerCompressor_DisabledSkips(t *testing.T) {
	fs := &fakeStickerCompressSettings{
		enabled: false, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	r := c.Compress(".png", makeTestPNG(t, 8, 8), fs.params())
	assert.Equal(t, stickerCompressOutcomeSkipped, r.Outcome)
	assert.Equal(t, "disabled", r.Reason)
}

func TestStickerCompressor_UnsupportedFormatSkips(t *testing.T) {
	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	for _, ext := range []string{".gif", ".webp", ".bmp", ".mp4", ".JPG", "", "jpg"} {
		r := c.Compress(ext, []byte{0}, fs.params())
		assert.Equalf(t, stickerCompressOutcomeSkipped, r.Outcome, "ext=%q", ext)
		assert.Equalf(t, "format", r.Reason, "ext=%q", ext)
	}
	// 大小写归一化后 jpg/jpeg/png 是允许的（caller 已归一化到小写；此处严格匹配）。
	// 归一化在调用点保证，这里的期望是"接口层只接 .jpg/.jpeg/.png 小写"。
}

func TestStickerCompressor_CompressesLargeJPEG(t *testing.T) {
	src := makeTestJPEG(t, 1024, 1024, 95)
	require.Greaterf(t, len(src), 50*1024, "source JPEG %dB must be big enough to test shrinking", len(src))

	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 256, maxConcurrency: 4, timeoutMs: 5000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	r := c.Compress(".jpg", src, fs.params())
	require.Equalf(t, stickerCompressOutcomeCompressed, r.Outcome, "reason=%q size=%d", r.Reason, r.Size)
	assert.LessOrEqual(t, r.Size, int64(256*1024))
	// 结果字节仍是可解码 JPEG，且尺寸缩到 <= maxDim。
	decoded, format, err := image.Decode(bytes.NewReader(r.Bytes))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	bounds := decoded.Bounds()
	assert.LessOrEqual(t, bounds.Dx(), 512)
	assert.LessOrEqual(t, bounds.Dy(), 512)
}

func TestStickerCompressor_CompressesPNG(t *testing.T) {
	src := makeTestPNG(t, 300, 300)

	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 5000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	r := c.Compress(".png", src, fs.params())
	require.Equalf(t, stickerCompressOutcomeCompressed, r.Outcome, "reason=%q", r.Reason)
	_, format, err := image.Decode(bytes.NewReader(r.Bytes))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
}

func TestStickerCompressor_RejectsWhenOverLimitAfterCompress(t *testing.T) {
	// 512x512 随机像素 PNG 通常在 ~380KB 量级；target=1KB 必然超限。
	src := makeTestPNG(t, 512, 512)
	require.Greaterf(t, len(src), 10*1024, "seed PNG %dB must exceed 10KB so target=1KB is unreachable", len(src))

	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1, maxConcurrency: 4, timeoutMs: 10000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	r := c.Compress(".png", src, fs.params())
	assert.Equal(t, stickerCompressOutcomeOverLimit, r.Outcome)
	assert.Greater(t, r.Size, int64(1024))
	assert.Nil(t, r.Bytes, "over_limit result must not surface compressed bytes")
}

func TestStickerCompressor_FailsOpenOnDecodeError(t *testing.T) {
	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	r := c.Compress(".jpg", []byte("not a real jpeg"), fs.params())
	assert.Equal(t, stickerCompressOutcomeFailed, r.Outcome)
	assert.Contains(t, r.Reason, "decode")
}

func TestStickerCompressor_ConcurrencyFailOpen(t *testing.T) {
	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 1, timeoutMs: 10000, maxDim: 512,
	}
	c := newFakeCompressor(fs)
	// 手动 hold 唯一 slot，随后 Compress 应立即 skipped。
	require.True(t, c.tryAcquireCompressSlot(1))
	r := c.Compress(".jpg", makeTestJPEG(t, 16, 16, 80), fs.params())
	assert.Equal(t, stickerCompressOutcomeSkipped, r.Outcome)
	assert.Equal(t, "concurrency_saturated", r.Reason)
	c.releaseCompressSlot()

	// 释放后能正常压缩（覆盖 release 语义）。
	r2 := c.Compress(".jpg", makeTestJPEG(t, 16, 16, 80), fs.params())
	assert.NotEqualf(t, stickerCompressOutcomeSkipped, r2.Outcome, "after release must compress, got reason=%q", r2.Reason)
}

func TestStickerCompressor_TimeoutFailOpen(t *testing.T) {
	// 注入一个刻意 sleep 长于 timeout 的 doCompress，验证 select 超时分支。
	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 20, maxDim: 512,
	}
	c := &stickerCompressor{
		settings: fs,
		doCompress: func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
			time.Sleep(200 * time.Millisecond)
			return stickerCompressResult{Outcome: stickerCompressOutcomeCompressed, Bytes: src, Size: int64(len(src))}, nil
		},
	}
	start := time.Now()
	r := c.Compress(".jpg", []byte("payload"), fs.params())
	elapsed := time.Since(start)
	assert.Equal(t, stickerCompressOutcomeSkipped, r.Outcome)
	assert.Equal(t, "timeout", r.Reason)
	// 应远小于 sleep 时长（timer 触发即返回，不等 doCompress 完成）。
	assert.Less(t, elapsed, 150*time.Millisecond, "timeout branch must not wait for the slow doCompress")
}

// canCompressStickerExt 只接受 .jpg / .jpeg / .png 小写；其余全部拒。
func TestCanCompressStickerExt(t *testing.T) {
	cases := map[string]bool{
		".jpg":  true,
		".jpeg": true,
		".png":  true,
		".JPG":  false,
		".gif":  false,
		".webp": false,
		"":      false,
		"jpg":   false,
		".mp4":  false,
	}
	for in, want := range cases {
		assert.Equalf(t, want, canCompressStickerExt(in), "ext=%q", in)
	}
}

// TestDoCompressStaticSticker_PreservesAspectRatio 直接测底层 doer：非正方形源
// 图缩放后必须保持宽高比（imaging.Fit 的语义保证 —— 若换成 Resize/Fill 会分别
// 拉伸/裁剪，这条测试就是那道防拉伸的门）。
func TestDoCompressStaticSticker_PreservesAspectRatio(t *testing.T) {
	// 1024×600 → maxDim=512：等比后长边=512、短边=300（16:10 比例保留）
	origBytes := makeTestJPEG(t, 1024, 600, 90)
	r, err := doCompressStaticSticker(".jpg", origBytes, 512, 10240 /* targetKB 高，不触发 over_limit */)
	require.NoError(t, err)
	require.Equal(t, stickerCompressOutcomeCompressed, r.Outcome)

	dec, _, err := image.Decode(bytes.NewReader(r.Bytes))
	require.NoError(t, err)
	bounds := dec.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// 长边严格等于 maxDim（imaging.Fit 会把长边贴到外框上界）
	assert.Equal(t, 512, max2(w, h), "long edge must snap to maxDim")
	// 短边严格小于 maxDim（源非正方形 → 短边不达外框上界）
	assert.Less(t, min2(w, h), 512, "short edge must be less than maxDim for non-square source")
	// 宽高比在 1/1024 内保持（浮点 + 像素取整必有 <1px 的漂移，1024→512 缩放
	// 因子极小，比原比例的偏差不应超过 1%。这里用 0.01 tolerance）。
	origRatio := 1024.0 / 600.0
	newRatio := float64(w) / float64(h)
	assert.InDelta(t, origRatio, newRatio, 0.01,
		"aspect ratio must be preserved: orig 1024×600 (r=%.4f), got %d×%d (r=%.4f)",
		origRatio, w, h, newRatio)
}

// TestDoCompressStaticSticker_NoUpscaleWhenSourceUnderMaxDim 保证一个隐式契约：
// 源图短/长边都 <= maxDim 时不做缩放（imaging.Fit 只在 w>maxDim || h>maxDim 才
// 触发），避免上采样徒增字节又损失锐度。
func TestDoCompressStaticSticker_NoUpscaleWhenSourceUnderMaxDim(t *testing.T) {
	origBytes := makeTestJPEG(t, 200, 150, 90)
	r, err := doCompressStaticSticker(".jpg", origBytes, 512, 10240)
	require.NoError(t, err)
	require.Equal(t, stickerCompressOutcomeCompressed, r.Outcome)
	dec, _, err := image.Decode(bytes.NewReader(r.Bytes))
	require.NoError(t, err)
	bounds := dec.Bounds()
	assert.Equal(t, 200, bounds.Dx())
	assert.Equal(t, 150, bounds.Dy())
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// P1 regression: timeout must NOT release the concurrency slot early.
// Slot lifetime is bound to the worker goroutine, not to Compress's return.
// ---------------------------------------------------------------------------

// TestStickerCompressor_TimeoutDoesNotBleedConcurrency: maxConcurrency=1 +
// worker sleeps 500ms + timeout=20ms 场景。首次 Compress 从 timeout 分支返回
// 时，dangling worker 仍在跑，slot 应保持占用；紧接着来的第二次 Compress 必须
// 拿到 skipped:concurrency_saturated 而不是 timeout（若 slot 提前释放，就会
// 走 timeout 分支，等于并发闸被绕过 —— review P1 就是这个 bug）。等 worker
// 结束后 slot 释放，第三次 Compress 才能真正进压缩管线。
func TestStickerCompressor_TimeoutDoesNotBleedConcurrency(t *testing.T) {
	const workerSleep = 500 * time.Millisecond
	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 1, timeoutMs: 20, maxDim: 512,
	}
	c := &stickerCompressor{
		settings: fs,
		doCompress: func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
			time.Sleep(workerSleep)
			return stickerCompressResult{Outcome: stickerCompressOutcomeCompressed, Bytes: src, Size: int64(len(src))}, nil
		},
	}

	// 第一次：worker 慢，Compress 从 timeout 分支返回
	r1 := c.Compress(".jpg", []byte("x"), fs.params())
	require.Equal(t, stickerCompressOutcomeSkipped, r1.Outcome)
	require.Equal(t, "timeout", r1.Reason)

	// 立即再打：worker 还没跑完，slot 必须仍占用 → 拿到 concurrency_saturated
	r2 := c.Compress(".jpg", []byte("y"), fs.params())
	assert.Equal(t, stickerCompressOutcomeSkipped, r2.Outcome)
	assert.Equalf(t, "concurrency_saturated", r2.Reason,
		"slot must remain held by dangling worker after timeout return; got reason=%q", r2.Reason)

	// 等 worker 真正结束 + 少量余量，slot 应被 worker 的 defer release 释放。
	// 断言点是"reason 变回 timeout"（新一轮成功 acquire，仅是新 worker 又慢导致
	// 超时）而不是 concurrency_saturated —— 只要不再是 saturated 就证明 slot 已
	// 归还。这条 test 的 doCompress 是永远慢的固定 fake，用 outcome!=skipped 判
	// 定会假失败。
	time.Sleep(workerSleep + 100*time.Millisecond)
	r3 := c.Compress(".jpg", []byte("z"), fs.params())
	assert.Equal(t, stickerCompressOutcomeSkipped, r3.Outcome)
	assert.NotEqualf(t, "concurrency_saturated", r3.Reason,
		"after worker finished, slot must be released; got reason=%q", r3.Reason)
}

// ---------------------------------------------------------------------------
// P2 regression: APNG must skip compression (would otherwise lose animation).
// ---------------------------------------------------------------------------

// buildPNGWithChunks 造一个最小 PNG 字节流：signature + 顺序写入 caller 指定的
// chunk。CRC 全写 0（hasAPNGActlChunk 不校验 CRC，只看结构 marker）。用于测
// APNG 检测函数在各种 chunk 顺序下的行为。
func buildPNGWithChunks(t *testing.T, chunks []struct {
	Type string
	Data []byte
},
) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	for _, ch := range chunks {
		var lenBytes [4]byte
		binary.BigEndian.PutUint32(lenBytes[:], uint32(len(ch.Data)))
		b.Write(lenBytes[:])
		b.WriteString(ch.Type)
		b.Write(ch.Data)
		b.Write([]byte{0, 0, 0, 0}) // fake CRC
	}
	return b.Bytes()
}

// APNG 典型 chunk 顺序：IHDR → acTL → (fcTL/IDAT/fdAT)*  → IEND。acTL 位于
// IDAT 之前是有效动画的必要条件。
func TestHasAPNGActlChunk_DetectsAPNG(t *testing.T) {
	src := buildPNGWithChunks(t, []struct {
		Type string
		Data []byte
	}{
		{"IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0}},
		{"acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0}}, // num_frames=2, num_plays=0
		{"IDAT", []byte{0x78, 0x9c, 0x62, 0, 0, 0, 0, 1}},
		{"IEND", nil},
	})
	assert.True(t, hasAPNGActlChunk(src))
	assert.True(t, isAnimatedPNGSource(".png", src))
	// 非 PNG ext 一律 false（防误挡 JPEG 等）。
	assert.False(t, isAnimatedPNGSource(".jpg", src))
}

// 静态 PNG（无 acTL）不应命中。真实 image/png 生成的 PNG 只含 IHDR/IDAT/IEND。
func TestHasAPNGActlChunk_RejectsStaticPNG(t *testing.T) {
	assert.False(t, hasAPNGActlChunk(makeTestPNG(t, 16, 16)))
	// 手工造的"只有 IHDR/IDAT/IEND"字节流也应否定，与 image/png 输出等价。
	minimalStatic := buildPNGWithChunks(t, []struct {
		Type string
		Data []byte
	}{
		{"IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0}},
		{"IDAT", []byte{0x78, 0x9c, 0x62, 0, 0, 0, 0, 1}},
		{"IEND", nil},
	})
	assert.False(t, hasAPNGActlChunk(minimalStatic))
}

// 规范：IDAT 之后的 acTL 会被一致的渲染器忽略。我们跟随规范判定为静态 PNG，
// 保守选择"能压缩"（否则任意有 acTL trailing bytes 的普通 PNG 都被误 skip）。
func TestHasAPNGActlChunk_IgnoresActlAfterIDAT(t *testing.T) {
	src := buildPNGWithChunks(t, []struct {
		Type string
		Data []byte
	}{
		{"IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0}},
		{"IDAT", []byte{0x78, 0x9c, 0x62, 0, 0, 0, 0, 1}},
		{"acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0}}, // trailing acTL —— 无效
		{"IEND", nil},
	})
	assert.False(t, hasAPNGActlChunk(src))
}

// 恶意/畸形输入：签名不匹配、length 溢出等，函数必须 return false 而不 panic
// 或越界。fuzz 面覆盖 review P2 的"信任边界"要求。
func TestHasAPNGActlChunk_MalformedInputsReturnFalse(t *testing.T) {
	cases := map[string][]byte{
		"empty":            nil,
		"short":            []byte{0x89, 0x50, 0x4e},
		"wrong_signature":  []byte("NOTAPNG!\x00\x00\x00\x00type"),
		"truncated_length": append([]byte("\x89PNG\r\n\x1a\n"), 0xff, 0xff),
		"length_overflow": append(
			[]byte("\x89PNG\r\n\x1a\n"),
			0xff, 0xff, 0xff, 0xff, // length = MaxUint32
			'X', 'X', 'X', 'X', // chunk type
		),
	}
	for name, src := range cases {
		assert.Falsef(t, hasAPNGActlChunk(src), "case=%s", name)
	}
}

// TestStickerCompressor_APNGSkips: 压缩入口拿到 APNG → 走 skipped:animated,
// 不进 doCompress，不占用并发 slot（在 acquire 前拦截）。observability 上
// 走 counter compress_skipped；不打 duration histogram（disabled/format/
// animated/concurrency_saturated 一起归属"轻量路径"）。
func TestStickerCompressor_APNGSkips(t *testing.T) {
	// 用一个"命中就 fatal"的 doCompress 证明 APNG 路径不到达 worker
	fs := &fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	}
	c := &stickerCompressor{
		settings: fs,
		doCompress: func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
			t.Fatalf("doCompress must not be called for APNG; ext=%s len=%d", ext, len(src))
			return stickerCompressResult{}, nil
		},
	}
	apng := buildPNGWithChunks(t, []struct {
		Type string
		Data []byte
	}{
		{"IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0}},
		{"acTL", []byte{0, 0, 0, 3, 0, 0, 0, 0}},
		{"IDAT", []byte{0x78, 0x9c, 0x62, 0, 0, 0, 0, 1}},
		{"IEND", nil},
	})
	r := c.Compress(".png", apng, fs.params())
	assert.Equal(t, stickerCompressOutcomeSkipped, r.Outcome)
	assert.Equal(t, "animated", r.Reason,
		"APNG must be identified before acquiring a compress slot")
}
