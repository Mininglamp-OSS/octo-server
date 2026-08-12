package notification

import (
	"testing"
	"time"
)

func TestResponseNormalizesMissingAndExpiredPause(t *testing.T) {
	now := time.Date(2026, 8, 12, 11, 30, 0, 125000000, time.UTC)
	service := &Service{}

	missing := service.response(nil, now)
	if missing.Paused || missing.PausedUntil != nil || missing.Revision != 0 {
		t.Fatalf("missing record should be inactive: %+v", missing)
	}

	expiredAt := now.Add(-time.Millisecond)
	expired := service.response(&pauseRecord{PausedUntil: &expiredAt, Revision: 7}, now)
	if expired.Paused || expired.PausedUntil != nil || expired.Revision != 7 {
		t.Fatalf("expired record should be normalized to inactive: %+v", expired)
	}
}

func TestResponseUsesUTCAbsolutePauseTime(t *testing.T) {
	now := time.Date(2026, 8, 12, 11, 30, 0, 125000000, time.FixedZone("CST", 8*60*60))
	until := now.Add(30 * time.Minute)
	service := &Service{}

	response := service.response(&pauseRecord{PausedUntil: &until, Revision: 42}, now)
	if !response.Paused || response.PausedUntil == nil {
		t.Fatalf("future record should be active: %+v", response)
	}
	if response.PausedUntil.Location() != time.UTC {
		t.Fatalf("paused_until should be normalized to UTC, got %s", response.PausedUntil.Location())
	}
	if response.PausedUntil.Equal(until) == false || response.Revision != 42 {
		t.Fatalf("unexpected authoritative response: %+v", response)
	}
	if response.ServerTime.Location() != time.UTC {
		t.Fatalf("server_time should be UTC, got %s", response.ServerTime.Location())
	}
}
