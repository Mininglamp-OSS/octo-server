package avatarrender

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// 深红背景：与白色文字在所有通道上都拉开距离，便于墨迹检测。
var testBg = color.RGBA{R: 200, G: 30, B: 40, A: 255}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	return img
}

// inkBox 返回白色文字墨迹的包围盒及其相对边长的宽高比例。仅在圆内核心区采样，
// 排除圆外（现为透明）与圆边抗锯齿环——文字受墨迹盒/字号上限约束，恒落在核心区内。
// 背景圆须为非白色（testBg）。
func inkBox(t *testing.T, data []byte) (minX, minY, maxX, maxY int, wRatio, hRatio float64) {
	t.Helper()
	img := decodePNG(t, data)
	b := img.Bounds()
	size := float64(b.Dx())
	cx, cy := size/2, size/2
	core := size/2 - 3
	minX, minY = b.Max.X, b.Max.Y
	maxX, maxY = -1, -1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if math.Sqrt(dx*dx+dy*dy) > core {
				continue
			}
			r, g, bb, a := img.At(x, y).RGBA()
			// 接近纯白且不透明的像素视为文字墨迹。
			if a >= 0xf000 && r >= 0xe000 && g >= 0xe000 && bb >= 0xe000 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < 0 {
		t.Fatal("no white ink pixels found in circle core")
	}
	return minX, minY, maxX, maxY, float64(maxX-minX+1) / size, float64(maxY-minY+1) / size
}

// TestDynamicFontSizeLatin 拉丁字母墨迹远窄于 CJK，固定字号下视觉明显偏小
// （线上反馈）。动态字号应把窄墨迹文字放大：两个拉丁字母的墨迹宽度至少要到
// 边长的 38%（旧固定 10/32 字号下 "aw" 仅 ~30%）。
func TestDynamicFontSizeLatin(t *testing.T) {
	for _, text := range []string{"aw", "TY", "er"} {
		data, err := Render(Options{Text: text, Bg: testBg, Size: 200})
		if err != nil {
			t.Fatalf("Render(%q): %v", text, err)
		}
		_, _, _, _, w, h := inkBox(t, data)
		if w < 0.38 {
			t.Errorf("text %q ink width = %.3f of size, want >= 0.38 (font too small)", text, w)
		}
		t.Logf("text %q ink box = %.3f x %.3f", text, w, h)
	}
}

// TestDynamicFontSizeCJK CJK 双字本就接近设计稿比例（32px 容器/10px 字号 →
// 墨迹宽 ~60%），动态字号下应保持在设计稿附近，不能跟着拉丁字母一起膨胀。
func TestDynamicFontSizeCJK(t *testing.T) {
	data, err := Render(Options{Text: "一序", Bg: testBg, Size: 200})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	_, _, _, _, w, h := inkBox(t, data)
	if w < 0.52 || w > 0.72 {
		t.Errorf("CJK two-char ink width = %.3f of size, want in [0.52, 0.72] (design ~0.6)", w)
	}
	t.Logf("CJK ink box = %.3f x %.3f", w, h)
}

// TestDynamicFontSizeSingleChar 单字（昵称只有一个可见字符）允许比双字大，
// 但墨迹必须留在圆内。
func TestDynamicFontSizeSingleChar(t *testing.T) {
	for _, text := range []string{"王", "A"} {
		data, err := Render(Options{Text: text, Bg: testBg, Size: 200})
		if err != nil {
			t.Fatalf("Render(%q): %v", text, err)
		}
		assertInkInsideCircle(t, data, text)
	}
}

// TestInkStaysInsideCircle 所有形态（CJK 双字/拉丁/混合）墨迹包围盒都不出圆。
func TestInkStaysInsideCircle(t *testing.T) {
	for _, text := range []string{"一序", "aw", "TY", "梅梅", "W序"} {
		data, err := Render(Options{Text: text, Bg: testBg, Size: 200})
		if err != nil {
			t.Fatalf("Render(%q): %v", text, err)
		}
		assertInkInsideCircle(t, data, text)
	}
}

// TestRenderTransparentCorners 输出圆外必须**透明**（带 alpha 通道的 RGBA PNG）：四角
// alpha=0，且整图非 Opaque——客户端在任意背景上合成时圆外不出白方块。
func TestRenderTransparentCorners(t *testing.T) {
	for _, text := range []string{"一序", "aw", "王", "W序"} {
		data, err := Render(Options{Text: text, Bg: testBg, Size: 200})
		if err != nil {
			t.Fatalf("Render(%q): %v", text, err)
		}
		img := decodePNG(t, data)
		b := img.Bounds()
		corners := [][2]int{
			{b.Min.X, b.Min.Y},
			{b.Max.X - 1, b.Min.Y},
			{b.Min.X, b.Max.Y - 1},
			{b.Max.X - 1, b.Max.Y - 1},
		}
		for _, c := range corners {
			_, _, _, a := img.At(c[0], c[1]).RGBA()
			if a != 0 {
				t.Errorf("text %q: corner (%d,%d) not transparent: alpha=%04x", text, c[0], c[1], a)
			}
		}
		if oi, ok := img.(interface{ Opaque() bool }); ok && oi.Opaque() {
			t.Errorf("text %q: rendered image must not be opaque (corners should be transparent)", text)
		}
	}
}

func assertInkInsideCircle(t *testing.T, data []byte, text string) {
	t.Helper()
	minX, minY, maxX, maxY, _, _ := inkBox(t, data)
	img := decodePNG(t, data)
	size := float64(img.Bounds().Dx())
	cx, cy := size/2, size/2
	radius := size / 2
	for _, c := range [][2]int{{minX, minY}, {maxX, minY}, {minX, maxY}, {maxX, maxY}} {
		dx, dy := float64(c[0])+0.5-cx, float64(c[1])+0.5-cy
		if d := math.Sqrt(dx*dx + dy*dy); d > radius {
			t.Fatalf("text %q: ink box corner (%d,%d) outside circle (dist %.1f > r %.1f)", text, c[0], c[1], d, radius)
		}
	}
}
