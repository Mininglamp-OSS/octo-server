package avatarrender

import (
	"bytes"
	"image/png"
	"testing"
)

func TestRenderIconProducesValidPNG(t *testing.T) {
	data, err := RenderIcon(GroupStyleForSeed("g1"))
	if err != nil {
		t.Fatalf("RenderIcon: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if b := img.Bounds(); b.Dx() != DefaultSize || b.Dy() != DefaultSize {
		t.Fatalf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), DefaultSize, DefaultSize)
	}
}

func TestRenderIconUsesGroupStyleAndTwoToneGlyph(t *testing.T) {
	style := groupStyleByIndexForTest(t, 0)
	data, err := RenderIcon(style)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	assertCloseColor(t, img.At(100, 20), style.Fill, "icon circle fill")
	assertCloseColor(t, img.At(100, 2), style.Main, "icon circle stroke")
	// 圆外透明（alpha=0）：图标头像同样不铺白底，输出带 alpha 通道的 RGBA PNG。
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("icon corner (0,0) not transparent: alpha=%04x", a)
	}
	if got := countClosePixels(img, style.IconBack); got < 30 {
		t.Fatalf("icon back color pixels = %d, want >= 30", got)
	}
	if got := countClosePixels(img, style.Main); got < 30 {
		t.Fatalf("icon main color pixels = %d, want >= 30", got)
	}
}

func TestRenderIconDeterministic(t *testing.T) {
	a, err := RenderIcon(GroupStyleForSeed("g2"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderIcon(GroupStyleForSeed("g2"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("RenderIcon not deterministic for identical input")
	}
}

func TestETag(t *testing.T) {
	// 弱验证符格式：W/"xxxxxxxx"。
	e := ETag("group-name-v2", "g1", "seed", "研发")
	if len(e) < 4 || e[:3] != `W/"` || e[len(e)-1] != '"' {
		t.Fatalf("ETag format = %q, want W/\"...\"", e)
	}
	// 稳定：同输入同 ETag。
	if e2 := ETag("group-name-v2", "g1", "seed", "研发"); e != e2 {
		t.Fatalf("ETag not stable: %q vs %q", e, e2)
	}
	// 任一因子变化 → ETag 变化（文字、颜色、seed、版本）。
	for _, diff := range [][]string{
		{"group-name-v2", "g1", "seed", "产品"}, // 文字
		{"group-name-v2", "g1", "idx3", "研发"}, // 颜色
		{"group-name-v2", "gX", "seed", "研发"}, // seed
		{"group-icon-v2", "g1", "seed", "研发"}, // 渲染模式版本
	} {
		if got := ETag(diff...); got == e {
			t.Fatalf("ETag collision for %v", diff)
		}
	}
}

func TestIfNoneMatch(t *testing.T) {
	etag := ETag("group-name-v2", "g1", "seed", "研发")
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"empty header", "", false},
		{"exact match", etag, true},
		{"wildcard", "*", true},
		{"strong-form of same opaque tag", `"` + etagOpaqueTag(etag) + `"`, true},
		{"comma list contains", `W/"deadbeef", ` + etag, true},
		{"mismatch", `W/"00000000"`, false},
		{"whitespace tolerant", "  " + etag + "  ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IfNoneMatch(tt.header, etag); got != tt.want {
				t.Fatalf("IfNoneMatch(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}
