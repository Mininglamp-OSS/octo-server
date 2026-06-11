package user

import (
	"strings"
	"testing"
)

func TestAvatarETag(t *testing.T) {
	// 确定性：同输入同 ETag。
	if avatarETag("name-v1", "uid1", "三丰") != avatarETag("name-v1", "uid1", "三丰") {
		t.Fatal("avatarETag not deterministic")
	}
	// 文字变化（改名）→ ETag 变化，这是改名后缓存失效的基础。
	if avatarETag("name-v1", "uid1", "三丰") == avatarETag("name-v1", "uid1", "丰丰") {
		t.Fatal("avatarETag must change when display text changes")
	}
	// uid 变化（颜色变化）→ ETag 变化。
	if avatarETag("name-v1", "uid1", "三丰") == avatarETag("name-v1", "uid2", "三丰") {
		t.Fatal("avatarETag must change when uid changes")
	}
	// 不同模式（昵称 vs ASCII 兜底）→ ETag 不撞。
	if avatarETag("name-v1", "uid1") == avatarETag("ascii-v1", "uid1") {
		t.Fatal("avatarETag must distinguish render modes")
	}
	// 弱 ETag：带 W/ 前缀且不透明标签加引号。
	got := avatarETag("name-v1", "uid1", "三丰")
	if !strings.HasPrefix(got, `W/"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("avatarETag should be a quoted weak ETag, got %s", got)
	}
}

func TestIfNoneMatchSatisfied(t *testing.T) {
	etag := `W/"abc12345"`
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"exact weak", `W/"abc12345"`, true},
		{"strong form of same opaque tag", `"abc12345"`, true}, // 弱比较忽略 W/
		{"wildcard", "*", true},
		{"multi list contains", `W/"xxxxxxxx", W/"abc12345"`, true},
		{"multi strong contains", `"xxxxxxxx", "abc12345"`, true},
		{"surrounding spaces", `  W/"abc12345"  `, true},
		{"no match", `W/"zzzzzzzz"`, false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"different tag", `"deadbeef"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ifNoneMatchSatisfied(tt.header, etag); got != tt.want {
				t.Fatalf("ifNoneMatchSatisfied(%q, %q) = %v, want %v", tt.header, etag, got, tt.want)
			}
		})
	}
}
