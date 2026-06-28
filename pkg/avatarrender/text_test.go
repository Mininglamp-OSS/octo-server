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
		{"mixed drops latin keeps cjk", "李雷Han", "李雷"},
		{"mixed a李 han only", "a李", "李"},
		{"latin single word one initial", "Alice", "A"},
		{"latin two-letter single token", "AB", "A"},
		{"latin two words initials", "John Smith", "JS"},
		{"latin camelCase initials", "johnSmith", "JS"},
		{"long latin single word one initial", "Alexander", "A"},
		// D — CJK syllabaries now take two glyphs (was: collapsed to one initial).
		{"hangul last two", "김철수", "철수"},
		{"hiragana last two", "さとう", "とう"},
		{"katakana last two", "サトウ", "トウ"},
		{"han kana mixed last two", "田中さくら", "くら"},
		{"hangul drops latin keeps cjk", "Lee김철", "김철"},
		// B — whitespace splits initials tokens.
		{"space splits lowercase multiword", "dev team", "DT"},
		// C — a digit between camelCase words no longer suppresses the split.
		{"digit between camel words", "Web3Team", "WT"},
		{"digit not a separator lowercase", "dev2team", "D"},
		{"digit not a separator acronym", "API2Gateway", "A"},
		{"zero width inside word ignored", "dev" + zwsp + "ops", "D"},
		// Known limitation: a single cased non-Latin word collapses to one initial.
		{"cyrillic single word one initial", "Анна", "А"},
		{"pure digits last two", "123456", "56"},
		{"trim surrounding space", "  李雷  ", "李雷"},
		{"trim then take last two", "  张三丰  ", "三丰"},
		{"strip inner space cjk", "李 雷", "李雷"},
		{"strip zero width", "李" + zwsp + "雷" + zwsp, "李雷"},
		{"strip bom and keep last two", "张" + bom + "三" + bom + "丰", "三丰"},
		{"pure emoji empty then caller falls back", "😀😀", ""},
		{"symbol only empty", "!!!", ""},
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

func TestGroupNameText(t *testing.T) {
	zwsp := string(rune(0x200B)) // 零宽空格
	bom := string(rune(0xFEFF))  // BOM / 零宽不换行空格
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"cjk takes first two", "后端架构讨论", "后端"},
		{"two cjk", "开发", "开发"},
		{"single cjk", "发", "发"},
		{"mixed drops latin keeps cjk", "Bug反馈群", "反馈"},
		{"mixed drops digits keeps cjk", "2024春招群", "春招"},
		{"latin two words initials", "Backend Team", "BT"},
		{"latin single word one initial", "Sales", "S"},
		{"latin camelCase initials", "myCoolGroup", "MC"},
		// D — CJK syllabaries take two glyphs (leading, group direction).
		{"hangul first two", "김철수", "김철"},
		{"hiragana first two", "さとう", "さと"},
		{"katakana first two", "サトウ", "サト"},
		{"han with trailing digits one cjk", "张123", "张"},
		// B — whitespace splits initials tokens.
		{"space splits lowercase multiword", "dev team", "DT"},
		{"space splits allcaps multiword", "HR BP", "HB"},
		// C — a digit between camelCase words no longer suppresses the split.
		{"digit between camel words", "Web3Team", "WT"},
		{"digit not a separator lowercase", "dev2team", "D"},
		{"digit not a separator acronym", "API2Gateway", "A"},
		{"zero width inside word ignored", "dev" + zwsp + "ops", "D"},
		// Known limitation: a single cased non-Latin word collapses to one initial.
		{"cyrillic single word one initial", "Анна", "А"},
		{"pure digits first two", "2024", "20"},
		{"trim surrounding space", "  产品群  ", "产品"},
		{"strip inner space keeps cjk", "前 端 U I", "前端"},
		{"strip zero width", "云" + zwsp + "服" + zwsp + "务", "云服"},
		{"strip bom keep first two", "后" + bom + "端" + bom + "架构", "后端"},
		{"pure emoji empty then icon", "🎉🎉", ""},
		{"symbol plus digit no cjk empty", "@2024", ""},
		{"symbol only empty", "!!!", ""},
		{"empty", "", ""},
		{"all space", "   ", ""},
		{"all invisible", zwsp + bom + "\t", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GroupNameText(tt.in); got != tt.want {
				t.Fatalf("GroupNameText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGroupText(t *testing.T) {
	zwsp := string(rune(0x200B)) // 零宽空格
	bom := string(rune(0xFEFF))  // BOM / 零宽不换行空格
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"four cjk kept", "架构讨论", "架构讨论"},
		{"five cjk takes first four", "后端架构讨论", "后端架构"},
		{"three cjk", "三个字", "三个字"},
		{"single cjk", "发", "发"},
		{"two cjk", "开发", "开发"},
		{"four latin", "abcd", "abcd"},
		{"long latin takes first four", "alexander", "alex"},
		{"trim surrounding space", "  产品群  ", "产品群"},
		{"trim then take first four", "  后端架构讨论  ", "后端架构"},
		{"strip inner space", "前 端 U I", "前端UI"},
		{"strip zero width", "云" + zwsp + "服" + zwsp + "务", "云服务"},
		{"strip bom keep first four", "后" + bom + "端" + bom + "架构讨论", "后端架构"},
		{"mixed cjk latin first four", "A产品B群组", "A产品B"},
		{"emoji kept for caller to filter", "😀😀😀😀😀", "😀😀😀😀"},
		{"empty", "", ""},
		{"all space", "   ", ""},
		{"all invisible", zwsp + bom + "\t", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GroupText(tt.in); got != tt.want {
				t.Fatalf("GroupText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestVisibleRuneCount(t *testing.T) {
	zwsp := string(rune(0x200B))
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"发", 1},
		{"开发", 2},
		{"架构讨论", 4},
		{"后端架构讨论", 6},   // 超 4，调用方据此拒绝
		{"  abcd  ", 4}, // 首尾空白不计
		{"a" + zwsp + "b", 2},
		{"   ", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := VisibleRuneCount(tt.in); got != tt.want {
				t.Fatalf("VisibleRuneCount(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestColorByIndex(t *testing.T) {
	n := PaletteSize()
	if n != len(palette) {
		t.Fatalf("PaletteSize() = %d, want %d", n, len(palette))
	}
	// 合法下标返回对应色板色。
	for i := 0; i < n; i++ {
		got, ok := ColorByIndex(i)
		if !ok {
			t.Fatalf("ColorByIndex(%d) ok=false, want true", i)
		}
		if got != palette[i] {
			t.Fatalf("ColorByIndex(%d) = %v, want %v", i, got, palette[i])
		}
	}
	// 越界返回 ok=false。
	for _, bad := range []int{-1, n, n + 1} {
		if _, ok := ColorByIndex(bad); ok {
			t.Fatalf("ColorByIndex(%d) ok=true, want false (out of range)", bad)
		}
	}
}

func TestGroupStylePalette(t *testing.T) {
	if len(groupFillPalette) != len(palette) {
		t.Fatalf("groupFillPalette len = %d, want %d", len(groupFillPalette), len(palette))
	}
	if len(groupIconBackPalette) != len(palette) {
		t.Fatalf("groupIconBackPalette len = %d, want %d", len(groupIconBackPalette), len(palette))
	}
	for i := range palette {
		style, ok := GroupStyleByIndex(i)
		if !ok {
			t.Fatalf("GroupStyleByIndex(%d) ok=false, want true", i)
		}
		if style.Main != palette[i] {
			t.Fatalf("GroupStyleByIndex(%d).Main = %v, want %v", i, style.Main, palette[i])
		}
		if style.Fill != groupFillPalette[i] {
			t.Fatalf("GroupStyleByIndex(%d).Fill = %v, want %v", i, style.Fill, groupFillPalette[i])
		}
		if style.IconBack != groupIconBackPalette[i] {
			t.Fatalf("GroupStyleByIndex(%d).IconBack = %v, want %v", i, style.IconBack, groupIconBackPalette[i])
		}
	}
	for _, bad := range []int{-1, len(palette), len(palette) + 1} {
		if _, ok := GroupStyleByIndex(bad); ok {
			t.Fatalf("GroupStyleByIndex(%d) ok=true, want false", bad)
		}
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
