package file

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// 服务端贴纸压缩 benchmark（sticker-upload-compression 任务 perf 验证 Task B）。
//
// 用途：给灰度前的 concurrency/timeout 默认值提供实证基线。跑法：
//   go test -bench='BenchmarkDoCompressStaticSticker' -benchmem -benchtime=3s ./modules/file/
//
// 覆盖 (JPEG q95, PNG) × (512², 1024²) 四组尺寸，都用伪随机像素做基线（真实
// 用户贴纸差异会更大，但基线用同一种输入以便可重现回归）。基准结果不作为
// pass/fail 门禁，用于生成 perf-todo.md 里的容量规划。

func benchJPEG(b *testing.B, w, h, quality int) []byte {
	b.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
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
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

func benchPNG(b *testing.B, w, h int) []byte {
	b.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
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
	if err := png.Encode(&buf, img); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

func benchDoCompress(b *testing.B, ext string, src []byte, maxDim, targetKB int) {
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := doCompressStaticSticker(ext, src, maxDim, targetKB)
		if err != nil {
			b.Fatal(err)
		}
		if r.Outcome != stickerCompressOutcomeCompressed && r.Outcome != stickerCompressOutcomeOverLimit {
			b.Fatalf("unexpected outcome=%s reason=%s", r.Outcome, r.Reason)
		}
	}
}

// 512×512 —— 命中默认 maxDim=512 时最常见的到达上界尺寸。
func BenchmarkDoCompressStaticSticker_JPEG_512(b *testing.B) {
	benchDoCompress(b, ".jpg", benchJPEG(b, 512, 512, 95), 512, 5*1024)
}

func BenchmarkDoCompressStaticSticker_PNG_512(b *testing.B) {
	benchDoCompress(b, ".png", benchPNG(b, 512, 512), 512, 5*1024)
}

// 1024×1024 —— 命中新配置的 maxDim 硬上限 1024 时的最坏情况；也是维度校验
// 与压缩阶段都需要 downscale 的场景。
func BenchmarkDoCompressStaticSticker_JPEG_1024_ShrinkTo512(b *testing.B) {
	benchDoCompress(b, ".jpg", benchJPEG(b, 1024, 1024, 95), 512, 5*1024)
}

func BenchmarkDoCompressStaticSticker_PNG_1024_ShrinkTo512(b *testing.B) {
	benchDoCompress(b, ".png", benchPNG(b, 1024, 1024), 512, 5*1024)
}

// 1024×1024 保持不缩放（maxDim=1024）—— 用于测量"只 re-encode 不 downscale"
// 情况下的成本，与 ShrinkTo512 对比可估 Lanczos 缩放贡献的 CPU 时间。
func BenchmarkDoCompressStaticSticker_JPEG_1024_ReencodeOnly(b *testing.B) {
	benchDoCompress(b, ".jpg", benchJPEG(b, 1024, 1024, 95), 1024, 5*1024)
}

func BenchmarkDoCompressStaticSticker_PNG_1024_ReencodeOnly(b *testing.B) {
	benchDoCompress(b, ".png", benchPNG(b, 1024, 1024), 1024, 5*1024)
}
