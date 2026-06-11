package avatarrender

import "testing"

func TestIndividualText(t *testing.T) {
	zwsp := string(rune(0x200B)) // 零宽空格
	bom := string(rune(0xFEFF))  // BOM / 零宽不换行空格
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"two cjk", "刘一", "刘一"},
		{"three cjk takes last two", "张三丰", "三丰"},
		{"single cjk", "王", "王"},
		{"two latin", "AB", "AB"},
		{"long latin takes last two", "Alexander", "er"},
		{"trim surrounding space", "  李雷  ", "李雷"},
		{"trim then take last two", "  张三丰  ", "三丰"},
		{"strip inner space", "李 雷", "李雷"},
		{"strip zero width", "李" + zwsp + "雷" + zwsp, "李雷"},
		{"strip bom and keep last two", "张" + bom + "三" + bom + "丰", "三丰"},
		{"mixed", "a李", "a李"},
		{"emoji kept for caller to filter", "😀😀", "😀😀"},
		{"empty", "", ""},
		{"all space", "   ", ""},
		{"all invisible", zwsp + bom + "\t", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IndividualText(tt.in); got != tt.want {
				t.Fatalf("IndividualText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderable(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"cjk", "刘一", true},
		{"japanese kana", "ひら", true},
		{"korean hangul", "한글", true},
		{"latin", "AB", true},
		{"rare cjk in basic block", "龘鱻", true},
		{"empty", "", false},
		{"pure emoji", "😀😀", false},
		{"mixed with emoji", "a😀", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Renderable(tt.in); got != tt.want {
				t.Fatalf("Renderable(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestColorForSeedStable(t *testing.T) {
	// 同 seed 必须稳定返回同色（改名不变色的基础）。
	a := ColorForSeed("uid_12345")
	b := ColorForSeed("uid_12345")
	if a != b {
		t.Fatalf("ColorForSeed not stable: %v vs %v", a, b)
	}
	// 返回值必须落在色板内。
	found := false
	for _, c := range palette {
		if c == a {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ColorForSeed returned %v not in palette", a)
	}
}
