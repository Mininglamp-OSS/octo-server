package user

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
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

// TestAvatarCacheKeyResistsETagCollision pins the PR#481 fix: the shared render
// cache key must NOT be the 32-bit CRC32 ETag. We search for two distinct display
// texts whose avatarETag collides, then assert (a) their avatarCacheKey does NOT
// collide and (b) a real shared avatarrender.Cache does not cross-serve A's bytes
// to B. Before the fix the ETag was the cache key, so a collision meant user B was
// served user A's cached avatar.
//
// Note: CRC32 is linear, so structured/sequential inputs barely collide — we use a
// fixed-seed PRNG over fixed-length random text, where collisions follow the 32-bit
// birthday bound (~tens of thousands of tries; 1<<20 budget is ample). Fixed seed
// keeps it deterministic.
func TestAvatarCacheKeyResistsETagCollision(t *testing.T) {
	const uid = "u_collide"
	r := rand.New(rand.NewSource(42))
	seen := make(map[string]string) // etag -> text
	var a, b string
	for i := 0; i < 1<<20 && a == ""; i++ {
		buf := make([]byte, 8)
		for j := range buf {
			buf[j] = byte('a' + r.Intn(26))
		}
		text := string(buf)
		et := avatarETag("name-v3", uid, text)
		if prev, ok := seen[et]; ok && prev != text {
			a, b = prev, text
		} else {
			seen[et] = text
		}
	}
	if a == "" {
		t.Skip("no CRC32 ETag collision found within budget (unexpected for 32-bit)")
	}
	// Precondition: the weak ETags really do collide — that's the hazard.
	if avatarETag("name-v3", uid, a) != avatarETag("name-v3", uid, b) {
		t.Fatalf("expected colliding ETags for %q/%q", a, b)
	}
	// The fix: cache keys for distinct inputs must stay distinct despite the ETag collision.
	keyA := avatarCacheKey("name-v3", uid, a)
	keyB := avatarCacheKey("name-v3", uid, b)
	if keyA == keyB {
		t.Fatalf("cache key collides for distinct inputs %q/%q sharing a CRC32 ETag", a, b)
	}
	// End-to-end: a real shared cache must not serve B user A's cached bytes.
	cache, err := avatarrender.NewCache(avatarrender.Config{})
	if err != nil {
		t.Fatal(err)
	}
	bytesA, _ := cache.GetOrRender(keyA, func() ([]byte, error) { return []byte("avatar-A:" + a), nil })
	bytesB, _ := cache.GetOrRender(keyB, func() ([]byte, error) { return []byte("avatar-B:" + b), nil })
	if string(bytesA) == string(bytesB) {
		t.Fatalf("shared cache cross-served: B got A's bytes for ETag-colliding inputs %q/%q", a, b)
	}
}

// TestAvatarCacheKeyDistinguishesFactors:不同模式/uid/文字都应得到不同 key。
func TestAvatarCacheKeyDistinguishesFactors(t *testing.T) {
	base := avatarCacheKey("name-v3", "uid1", "三丰")
	if base == avatarCacheKey("name-v3", "uid1", "丰丰") {
		t.Fatal("text change must change cache key")
	}
	if base == avatarCacheKey("name-v3", "uid2", "三丰") {
		t.Fatal("uid change must change cache key")
	}
	if avatarCacheKey("name-v3", "uid1") == avatarCacheKey("ascii-v1", "uid1") {
		t.Fatal("render mode must change cache key")
	}
}

// TestAvatarCacheKeyInjectiveAcrossPartBoundaries pins the PR#481 hardening:
// the key encoding must stay injective even when a part contains the separator
// byte. A naive strings.Join(parts, "\x00") would map these two distinct
// factor lists to the same key; length-framing keeps them distinct.
func TestAvatarCacheKeyInjectiveAcrossPartBoundaries(t *testing.T) {
	if avatarCacheKey("name-v3", "u", "a\x00b") == avatarCacheKey("name-v3", "u\x00a", "b") {
		t.Fatal("cache key must stay injective across part boundaries (NUL in a part)")
	}
	// Empty-vs-absent part must also not alias.
	if avatarCacheKey("x", "") == avatarCacheKey("x") {
		t.Fatal("trailing empty part must change the key")
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
