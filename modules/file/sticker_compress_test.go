package file

import (
	"bytes"
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
	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: false, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	})
	r := c.Compress(".png", makeTestPNG(t, 8, 8))
	assert.Equal(t, stickerCompressOutcomeSkipped, r.Outcome)
	assert.Equal(t, "disabled", r.Reason)
}

func TestStickerCompressor_UnsupportedFormatSkips(t *testing.T) {
	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	})
	for _, ext := range []string{".gif", ".webp", ".bmp", ".mp4", ".JPG", "", "jpg"} {
		r := c.Compress(ext, []byte{0})
		assert.Equalf(t, stickerCompressOutcomeSkipped, r.Outcome, "ext=%q", ext)
		assert.Equalf(t, "format", r.Reason, "ext=%q", ext)
	}
	// 大小写归一化后 jpg/jpeg/png 是允许的（caller 已归一化到小写；此处严格匹配）。
	// 归一化在调用点保证，这里的期望是"接口层只接 .jpg/.jpeg/.png 小写"。
}

func TestStickerCompressor_CompressesLargeJPEG(t *testing.T) {
	src := makeTestJPEG(t, 1024, 1024, 95)
	require.Greaterf(t, len(src), 50*1024, "source JPEG %dB must be big enough to test shrinking", len(src))

	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: true, targetKB: 256, maxConcurrency: 4, timeoutMs: 5000, maxDim: 512,
	})
	r := c.Compress(".jpg", src)
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

	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 5000, maxDim: 512,
	})
	r := c.Compress(".png", src)
	require.Equalf(t, stickerCompressOutcomeCompressed, r.Outcome, "reason=%q", r.Reason)
	_, format, err := image.Decode(bytes.NewReader(r.Bytes))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
}

func TestStickerCompressor_RejectsWhenOverLimitAfterCompress(t *testing.T) {
	// 512x512 随机像素 PNG 通常在 ~380KB 量级；target=1KB 必然超限。
	src := makeTestPNG(t, 512, 512)
	require.Greaterf(t, len(src), 10*1024, "seed PNG %dB must exceed 10KB so target=1KB is unreachable", len(src))

	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: true, targetKB: 1, maxConcurrency: 4, timeoutMs: 10000, maxDim: 512,
	})
	r := c.Compress(".png", src)
	assert.Equal(t, stickerCompressOutcomeOverLimit, r.Outcome)
	assert.Greater(t, r.Size, int64(1024))
	assert.Nil(t, r.Bytes, "over_limit result must not surface compressed bytes")
}

func TestStickerCompressor_FailsOpenOnDecodeError(t *testing.T) {
	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 2000, maxDim: 512,
	})
	r := c.Compress(".jpg", []byte("not a real jpeg"))
	assert.Equal(t, stickerCompressOutcomeFailed, r.Outcome)
	assert.Contains(t, r.Reason, "decode")
}

func TestStickerCompressor_ConcurrencyFailOpen(t *testing.T) {
	c := newFakeCompressor(&fakeStickerCompressSettings{
		enabled: true, targetKB: 1024, maxConcurrency: 1, timeoutMs: 10000, maxDim: 512,
	})
	// 手动 hold 唯一 slot，随后 Compress 应立即 skipped。
	require.True(t, c.tryAcquireCompressSlot(1))
	r := c.Compress(".jpg", makeTestJPEG(t, 16, 16, 80))
	assert.Equal(t, stickerCompressOutcomeSkipped, r.Outcome)
	assert.Equal(t, "concurrency_saturated", r.Reason)
	c.releaseCompressSlot()

	// 释放后能正常压缩（覆盖 release 语义）。
	r2 := c.Compress(".jpg", makeTestJPEG(t, 16, 16, 80))
	assert.NotEqualf(t, stickerCompressOutcomeSkipped, r2.Outcome, "after release must compress, got reason=%q", r2.Reason)
}

func TestStickerCompressor_TimeoutFailOpen(t *testing.T) {
	// 注入一个刻意 sleep 长于 timeout 的 doCompress，验证 select 超时分支。
	c := &stickerCompressor{
		settings: &fakeStickerCompressSettings{
			enabled: true, targetKB: 1024, maxConcurrency: 4, timeoutMs: 20, maxDim: 512,
		},
		doCompress: func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
			time.Sleep(200 * time.Millisecond)
			return stickerCompressResult{Outcome: stickerCompressOutcomeCompressed, Bytes: src, Size: int64(len(src))}, nil
		},
	}
	start := time.Now()
	r := c.Compress(".jpg", []byte("payload"))
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
