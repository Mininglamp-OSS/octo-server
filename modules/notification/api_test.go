package notification

import (
	"encoding/json"
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

	response := service.response(&pauseRecord{Mode: strptr(pauseModeTimed), PausedUntil: &until, Revision: 42}, now)
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

func TestPauseIntentSupportsManualAndDurations(t *testing.T) {
	now := time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)
	manual, until, ok := (updatePauseRequest{Mode: strptr(pauseModeManual)}).intent(now)
	if !ok || manual != pauseModeManual || until != nil {
		t.Fatalf("manual intent = %q, %v, %v", manual, until, ok)
	}
	mode, until, ok := (updatePauseRequest{Duration: strptr("30m")}).intent(now)
	if !ok || mode != pauseModeTimed || until == nil || !until.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("duration intent = %q, %v, %v", mode, until, ok)
	}
}

func TestUpdatePauseRequestRequiresRFC3339TimestampAndRejectsUnknownFields(t *testing.T) {
	var request updatePauseRequest
	if err := json.Unmarshal([]byte(`{"paused_until":"2026-08-16T09:00:00.000Z"}`), &request); err != nil {
		t.Fatalf("valid UTC timestamp rejected: %v", err)
	}
	if request.PausedUntil == nil || !request.hasPausedUntil {
		t.Fatalf("paused_until was not decoded: %+v", request)
	}
	if err := json.Unmarshal([]byte(`{"paused_until":"2026-08-16T09:00:00"}`), &request); err == nil {
		t.Fatal("timezone-less timestamp should be rejected")
	}
	if err := json.Unmarshal([]byte(`{"scope":"sound","duration":"30m"}`), &request); err != nil {
		t.Fatalf("unknown extension field should remain forward-compatible: %v", err)
	}
	if _, _, ok := request.intent(time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)); !ok {
		t.Fatal("valid request with an unknown extension field should be accepted")
	}
	if err := json.Unmarshal([]byte(`{"mode":null}`), &request); err == nil {
		t.Fatal("null mode should be rejected")
	}
	if err := json.Unmarshal([]byte(`{"duration":null}`), &request); err == nil {
		t.Fatal("null duration should be rejected")
	}
}

func TestPauseIntentRejectsAmbiguousAndInvalidRequests(t *testing.T) {
	now := time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)
	cases := []updatePauseRequest{
		{Mode: strptr(pauseModeManual), Duration: strptr("30m")},
		{Duration: strptr("2h")},
		{Mode: strptr("timed")},
		{PausedUntil: &now, hasPausedUntil: true},
	}
	for _, request := range cases {
		if _, _, ok := request.intent(now); ok {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
}

func TestManualResponseStaysActiveWithoutAnExpiry(t *testing.T) {
	now := time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)
	response := (&Service{}).response(&pauseRecord{Mode: strptr(pauseModeManual), Revision: 9}, now.Add(7*24*time.Hour))
	if !response.Paused || response.Mode == nil || *response.Mode != pauseModeManual || response.PausedUntil != nil {
		t.Fatalf("manual response = %+v", response)
	}
}

func TestResponseDoesNotTreatUnknownModeAsTimed(t *testing.T) {
	now := time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	response := (&Service{}).response(&pauseRecord{Mode: strptr("future-mode"), PausedUntil: &future}, now)
	if response.Paused || response.Mode != nil || response.PausedUntil != nil {
		t.Fatalf("unknown mode should not activate a pause: %+v", response)
	}
}

func strptr(value string) *string { return &value }

func TestNormalizePauseRecordPreservesDatabaseUTCWallClock(t *testing.T) {
	stored := time.Date(2026, 8, 15, 8, 20, 30, 123000000, time.FixedZone("CST", 8*60*60))
	record := normalizePauseRecord(&pauseRecord{PausedUntil: &stored})

	if record.PausedUntil == nil {
		t.Fatal("paused_until should remain present")
	}
	want := time.Date(2026, 8, 15, 8, 20, 30, 123000000, time.UTC)
	if !record.PausedUntil.Equal(want) || record.PausedUntil.Location() != time.UTC {
		t.Fatalf("paused_until should preserve UTC wall-clock value, got %s", record.PausedUntil)
	}
}

func TestValidPauseUntilCapsThePauseWindow(t *testing.T) {
	now := time.Date(2026, 8, 12, 11, 30, 0, 0, time.UTC)
	if !validPauseUntil(now, now.Add(time.Minute)) {
		t.Fatal("future pause should be accepted")
	}
	if validPauseUntil(now, now) || validPauseUntil(now, now.Add(-time.Minute)) {
		t.Fatal("current and past pause times should be rejected")
	}
	if validPauseUntil(now, now.Add(maxPauseDuration+time.Nanosecond)) {
		t.Fatal("pause window beyond the cap should be rejected")
	}
}
