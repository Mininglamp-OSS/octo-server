package reqid

import (
	"context"
	"strings"
	"testing"
)

func TestNewIsUniqueHex16(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id := New()
		if len(id) != 16 {
			t.Fatalf("New() len = %d, want 16 (got %q)", len(id), id)
		}
		for _, c := range id {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("New() = %q contains non-hex char %q", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("New() produced duplicate id %q within 100 calls", id)
		}
		seen[id] = true
	}
}

func TestWithTraceIDRoundTrip(t *testing.T) {
	ctx := WithTraceID(context.Background(), "abc123")
	if got := FromContext(ctx); got != "abc123" {
		t.Fatalf("FromContext = %q, want %q", got, "abc123")
	}
}

func TestFromContextAbsentReturnsEmpty(t *testing.T) {
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("FromContext(empty) = %q, want \"\"", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trim", "  abc123  ", "abc123"},
		{"strip_crlf_injection", "abc\r\nFAKE LOG LINE", "abcFAKE LOG LINE"},
		{"strip_tab_and_controls", "a\tb\x00c", "abc"},
		{"empty_after_clean", "\r\n\t ", ""},
		{"truncate_over_128", strings.Repeat("x", 200), strings.Repeat("x", 128)},
		{"keep_normal", "req-7f3a9c", "req-7f3a9c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sanitize(tc.in); got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
