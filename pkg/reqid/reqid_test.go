package reqid

import (
	"context"
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
