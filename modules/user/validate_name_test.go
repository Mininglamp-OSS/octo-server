package user

import "testing"

func TestValidateName(t *testing.T) {
	zwsp := string(rune(0x200B)) // 零宽空格
	bom := string(rune(0xFEFF))  // BOM
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"normal cjk", "刘一", false},
		{"normal latin", "Alice", false},
		{"with inner space ok", "李 雷", false},
		{"empty", "", true},
		{"spaces only", "   ", true},
		{"fullwidth space only", "　　", true},
		{"tab newline only", "\t\n", true},
		{"zero width only", zwsp + zwsp, true},
		{"bom only", bom, true},
		{"mixed invisible only", zwsp + bom + " \t", true},
		{"at char rejected", "a@b", true},
		{"visible with surrounding space ok", "  王  ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateName(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestIsBlankName(t *testing.T) {
	zwsp := string(rune(0x200B))
	bom := string(rune(0xFEFF))
	blanks := []string{"", "   ", "　", "\t\n\r", zwsp, bom, zwsp + bom + " "}
	for _, s := range blanks {
		if !isBlankName(s) {
			t.Errorf("isBlankName(%q) = false, want true", s)
		}
	}
	nonBlanks := []string{"a", "王", " 王 ", "a" + zwsp, "1"}
	for _, s := range nonBlanks {
		if isBlankName(s) {
			t.Errorf("isBlankName(%q) = true, want false", s)
		}
	}
}
