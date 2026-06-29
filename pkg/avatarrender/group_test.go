package avatarrender

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"
)

func TestGroupAvatarLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"four cjk 2x2", "架构讨论", []string{"架构", "讨论"}},
		{"four cjk 2x2 b", "后端架构", []string{"后端", "架构"}},
		{"three cjk 1+2", "三个字", []string{"三", "个字"}},
		{"three cjk 1+2 b", "云服务", []string{"云", "服务"}},
		{"two cjk single", "开发", []string{"开发"}},
		{"one cjk single", "发", []string{"发"}},
		{"four latin single", "abcd", []string{"abcd"}},
		{"two latin single", "ab", []string{"ab"}},
		{"mixed wide wraps", "A产品B", []string{"A产", "品B"}},
		{"two mixed single", "a产", []string{"a产"}},
		{"empty", "", []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GroupAvatarLines(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GroupAvatarLines(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderGroupProducesValidPNG(t *testing.T) {
	for _, text := range []string{"后端架构", "三个字", "开发", "abcd", "发"} {
		data, err := RenderGroup(text, GroupStyleForSeed("g_"+text), 200)
		if err != nil {
			t.Fatalf("RenderGroup(%q): %v", text, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode png for %q: %v", text, err)
		}
		if b := img.Bounds(); b.Dx() != 200 || b.Dy() != 200 {
			t.Fatalf("size for %q = %dx%d, want 200x200", text, b.Dx(), b.Dy())
		}
	}
}

func TestRenderGroupUsesGroupStyleFillAndStroke(t *testing.T) {
	style := groupStyleByIndexForTest(t, 0)
	data, err := RenderGroup("研发", style, 200)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	assertCloseColor(t, img.At(100, 20), style.Fill, "circle fill")
	assertCloseColor(t, img.At(100, 2), style.Main, "circle stroke")
	// 圆外透明（alpha=0）：不再铺白底，输出带 alpha 通道的 RGBA PNG。
	assertCloseColor(t, img.At(0, 0), color.RGBA{R: 0, G: 0, B: 0, A: 0}, "outside circle transparent")
}

func TestRenderGroupEmptyTextErrors(t *testing.T) {
	if _, err := RenderGroup("", GroupStyleForSeed("g1"), DefaultSize); err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestRenderGroupDeterministic(t *testing.T) {
	a, err := RenderGroup("架构讨论", GroupStyleForSeed("g9"), 200)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderGroup("架构讨论", GroupStyleForSeed("g9"), 200)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("RenderGroup not deterministic for identical input")
	}
}

func groupStyleByIndexForTest(t *testing.T, idx int) GroupStyle {
	t.Helper()
	style, ok := GroupStyleByIndex(idx)
	if !ok {
		t.Fatalf("GroupStyleByIndex(%d) ok=false", idx)
	}
	return style
}

func assertCloseColor(t *testing.T, got color.Color, want color.RGBA, label string) {
	t.Helper()
	r16, g16, b16, a16 := got.RGBA()
	gotRGBA := color.RGBA{R: uint8(r16 >> 8), G: uint8(g16 >> 8), B: uint8(b16 >> 8), A: uint8(a16 >> 8)}
	if colorDistance(gotRGBA, want) > 10 {
		t.Fatalf("%s color = %#v, want close to %#v", label, gotRGBA, want)
	}
}

func colorDistance(a, b color.RGBA) int {
	return absInt(int(a.R)-int(b.R)) +
		absInt(int(a.G)-int(b.G)) +
		absInt(int(a.B)-int(b.B)) +
		absInt(int(a.A)-int(b.A))
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func countClosePixels(img image.Image, c color.RGBA) int {
	b := img.Bounds()
	count := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r16, g16, b16, a16 := img.At(x, y).RGBA()
			got := color.RGBA{R: uint8(r16 >> 8), G: uint8(g16 >> 8), B: uint8(b16 >> 8), A: uint8(a16 >> 8)}
			if colorDistance(got, c) <= 20 {
				count++
			}
		}
	}
	return count
}
