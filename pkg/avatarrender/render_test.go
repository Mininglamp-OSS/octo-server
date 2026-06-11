package avatarrender

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestRenderProducesValidPNG(t *testing.T) {
	data, err := Render(Options{Text: "刘一", Bg: ColorForSeed("u1"), Size: 200})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 200 || b.Dy() != 200 {
		t.Fatalf("size = %dx%d, want 200x200", b.Dx(), b.Dy())
	}
}

func TestRenderEmptyTextErrors(t *testing.T) {
	if _, err := Render(Options{Text: "", Bg: ColorForSeed("u1")}); err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestRenderDeterministic(t *testing.T) {
	a, err := Render(Options{Text: "三丰", Bg: ColorForSeed("u2"), Size: 200})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(Options{Text: "三丰", Bg: ColorForSeed("u2"), Size: 200})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Render not deterministic for identical input")
	}
}

// TestGenerateSamples 仅在设置 AVATAR_SAMPLE_DIR 时运行，把一组示例头像写出来供
// 肉眼比对设计稿。例：
//
//	AVATAR_SAMPLE_DIR=.context/avatar-samples go test ./pkg/avatarrender/ -run TestGenerateSamples -v
func TestGenerateSamples(t *testing.T) {
	dir := os.Getenv("AVATAR_SAMPLE_DIR")
	if dir == "" {
		t.Skip("set AVATAR_SAMPLE_DIR to generate sample PNGs")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// (uid, 昵称) — uid 决定颜色，昵称后两字决定文字。
	samples := []struct{ uid, name string }{
		{"u01", "刘一"},
		{"u02", "张三丰"},
		{"u03", "王"},
		{"u04", "欧阳娜娜"},
		{"u05", "AB"},
		{"u06", "Alexander"},
		{"u07", "李雷和韩梅梅"},
		{"u08", "陈"},
		{"u09", "诸葛孔明"},
		{"u10", "Tom"},
	}
	for _, s := range samples {
		text := IndividualText(s.name)
		data, err := Render(Options{Text: text, Bg: ColorForSeed(s.uid), Size: 200})
		if err != nil {
			t.Fatalf("render %s: %v", s.name, err)
		}
		fp := filepath.Join(dir, s.uid+"_"+text+".png")
		if err := os.WriteFile(fp, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (text=%q color=%v)", fp, text, ColorForSeed(s.uid))
	}
}
