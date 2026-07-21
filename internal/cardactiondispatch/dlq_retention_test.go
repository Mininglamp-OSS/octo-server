package cardactiondispatch

import (
	"testing"
	"time"
)

func TestDLQRetentionFromEnv(t *testing.T) {
	// The default matches the value shipped before retention was configurable, so an
	// upgrade that does not set the override keeps the existing 30-day recovery window.
	if DefaultDLQRetention != 30*24*time.Hour {
		t.Fatalf("DefaultDLQRetention = %v, want 30 days", DefaultDLQRetention)
	}
	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"unset falls back to default", "", DefaultDLQRetention},
		{"whitespace falls back to default", "   ", DefaultDLQRetention},
		{"seven-day override", "7", 7 * 24 * time.Hour},
		{"one day", "1", 24 * time.Hour},
		{"valid whole days", "14", 14 * 24 * time.Hour},
		{"at max is accepted", "365", 365 * 24 * time.Hour},
		{"zero falls back", "0", DefaultDLQRetention},
		{"negative falls back", "-3", DefaultDLQRetention},
		{"non-integer falls back", "7d", DefaultDLQRetention},
		{"over-max falls back", "9999", DefaultDLQRetention},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == DLQRetentionEnv {
					return tt.val
				}
				return ""
			}
			if got := DLQRetentionFromEnv(getenv); got != tt.want {
				t.Fatalf("DLQRetentionFromEnv(%q) = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}
