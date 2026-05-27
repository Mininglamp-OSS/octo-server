package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestEncodeRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   TokenInfo
	}{
		{"full", TokenInfo{UID: "u1", Name: "alice", Role: "admin", Language: "en-US"}},
		{"no_role", TokenInfo{UID: "u1", Name: "alice", Language: "zh-CN"}},
		{"no_language", TokenInfo{UID: "u1", Name: "alice", Role: "superAdmin"}},
		{"only_uid_name", TokenInfo{UID: "u1", Name: "alice"}},
		{"name_with_at_sign", TokenInfo{UID: "u1", Name: "a@b@c", Role: "admin", Language: "en-US"}},
		{"unicode_name", TokenInfo{UID: "u1", Name: "张三", Role: "admin", Language: "zh-CN"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !strings.HasPrefix(encoded, v2Prefix) {
				t.Fatalf("encoded value missing v2 prefix: %q", encoded)
			}
			got, err := Decode(encoded)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != tc.in {
				t.Fatalf("round trip mismatch: got %+v want %+v", got, tc.in)
			}
		})
	}
}

func TestEncodeRejectsEmptyUID(t *testing.T) {
	t.Parallel()
	_, err := Encode(TokenInfo{Name: "alice"})
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func TestDecodeLegacy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want TokenInfo
	}{
		{"uid_name", "u1@alice", TokenInfo{UID: "u1", Name: "alice"}},
		{"uid_name_role", "u1@alice@admin", TokenInfo{UID: "u1", Name: "alice", Role: "admin"}},
		{"empty_name_allowed", "u1@", TokenInfo{UID: "u1", Name: ""}},
		{"superadmin_role", "u1@alice@superAdmin", TokenInfo{UID: "u1", Name: "alice", Role: "superAdmin"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decode(tc.raw)
			if err != nil {
				t.Fatalf("Decode(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Decode(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
			if got.Language != "" {
				t.Fatalf("legacy payload must not synthesize a language, got %q", got.Language)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "", ErrEmptyToken},
		{"legacy_only_uid", "u1", ErrInvalidToken},
		{"legacy_empty_uid", "@alice", ErrInvalidToken},
		{"legacy_too_many_parts", "u1@alice@admin@extra", ErrInvalidToken},
		{"v2_bad_json", v2Prefix + "{not json}", ErrInvalidToken},
		{"v2_missing_uid", v2Prefix + `{"name":"alice"}`, ErrInvalidToken},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tc.raw)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Decode(%q): want %v, got %v", tc.raw, tc.want, err)
			}
		})
	}
}

// TestDecodeV2PrefixIsRequired guards against accidental relaxation of the
// versioning prefix — a future "v3:" envelope must not be silently parsed as
// v2 (and vice versa). 老消费者拿到未来版本应当显式报错。
func TestDecodeV2PrefixIsRequired(t *testing.T) {
	t.Parallel()
	// 缺少 v2: 前缀 → 走 legacy 分支。带合法 JSON 但没有 @ 分隔会被识别为非法。
	jsonNoPrefix := `{"uid":"u1","name":"alice"}`
	_, err := Decode(jsonNoPrefix)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("plain JSON without v2 prefix must fail, got %v", err)
	}
}
