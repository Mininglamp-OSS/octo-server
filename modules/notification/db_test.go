package notification

import (
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
)

func TestPauseTimeSurvivesNonUTCLocRoundTrip(t *testing.T) {
	store, ctx := newPauseTestStore(t)
	uid := fmt.Sprintf("notification-tz-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = ctx.DB().DB.Exec("DELETE FROM user_notification_pause WHERE uid=?", uid) })
	now := time.Now().UTC().Truncate(time.Millisecond)
	want := now.Add(2 * time.Hour)
	if _, changed, err := store.upsert(uid, pauseModeTimed, &want, now); err != nil || !changed {
		t.Fatal(err)
	}
	record, err := store.get(uid)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.PausedUntil == nil || !record.PausedUntil.Equal(want) {
		t.Fatalf("paused_until round trip = %v, want %v", record, want)
	}
	if response := (&Service{}).response(record, now); !response.Paused || response.PausedUntil == nil || !response.PausedUntil.Equal(want) {
		t.Fatalf("response = %+v, want active pause until %s", response, want)
	}
}

func newPauseTestStore(t *testing.T) (*dbStore, *config.Context) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai"
	cfg.DB.Migration = false
	ctx := testutil.NewTestContext(cfg)
	if err := ctx.DB().DB.Ping(); err != nil {
		t.Skipf("MySQL unavailable: %v", err)
	}
	_, err := ctx.DB().DB.Exec(`CREATE TABLE IF NOT EXISTS user_notification_pause (
		uid VARCHAR(40) NOT NULL,
		mode VARCHAR(16) NULL,
		paused_until DATETIME(3) NULL,
		revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
		updated_at DATETIME(3) NOT NULL,
		PRIMARY KEY (uid)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctx.DB().DB.Close() })
	if _, err := ctx.DB().DB.Exec("ALTER TABLE user_notification_pause ADD COLUMN IF NOT EXISTS mode VARCHAR(16) NULL AFTER uid"); err != nil {
		t.Fatal(err)
	}
	return newDBStore(ctx), ctx
}

func TestGetActiveByUIDsCoversManualTimedLegacyAndExpiredRows(t *testing.T) {
	store, ctx := newPauseTestStore(t)
	now := time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)
	// Use SQL wall-clock strings here. This test deliberately uses a
	// non-UTC DSN, and passing time.Time values through database/sql would
	// convert the fixture before MySQL stores it, making the "past" row future
	// relative to the fixed query instant.
	future := "2026-08-14 06:00:00"
	past := "2026-08-14 04:00:00"
	uidPrefix := fmt.Sprintf("notification-active-%d-", time.Now().UnixNano())
	rows := []struct {
		uid, mode string
		until     interface{}
	}{
		{uidPrefix + "manual", pauseModeManual, nil},
		{uidPrefix + "timed", pauseModeTimed, future},
		{uidPrefix + "legacy", "", future},
		{uidPrefix + "expired", pauseModeTimed, past},
		{uidPrefix + "cleared", "", nil},
	}
	for _, row := range rows {
		_, err := ctx.DB().DB.Exec("DELETE FROM user_notification_pause WHERE uid=?", row.uid)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ctx.DB().DB.Exec("INSERT INTO user_notification_pause (uid, mode, paused_until, revision, updated_at) VALUES (?, NULLIF(?, ''), ?, 1, ?)", row.uid, row.mode, row.until, now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = ctx.DB().DB.Exec("DELETE FROM user_notification_pause WHERE uid=?", row.uid) })
	}

	uids := make([]string, 0, len(rows))
	for _, row := range rows {
		uids = append(uids, row.uid)
	}
	active, err := store.getActiveByUIDs(uids, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{uidPrefix + "manual", uidPrefix + "timed", uidPrefix + "legacy"} {
		if _, ok := active[uid]; !ok {
			t.Fatalf("%s should be active, got %v", uid, active)
		}
	}
	for _, uid := range []string{uidPrefix + "expired", uidPrefix + "cleared"} {
		if _, ok := active[uid]; ok {
			t.Fatalf("%s should be inactive, got %v", uid, active)
		}
	}
}

func TestClearIsIdempotentAndDoesNotCreateRows(t *testing.T) {
	store, ctx := newPauseTestStore(t)
	now := time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC)
	uid := fmt.Sprintf("notification-clear-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = ctx.DB().DB.Exec("DELETE FROM user_notification_pause WHERE uid=?", uid) })
	if _, changed, err := store.upsert(uid, pauseModeManual, nil, now); err != nil || !changed {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("initial upsert should change the row")
	}
	repeated, repeatedChanged, err := store.upsert(uid, pauseModeManual, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if repeatedChanged || repeated == nil || repeated.Revision != 1 {
		t.Fatalf("repeated identical upsert should be a no-op: record=%+v changed=%v", repeated, repeatedChanged)
	}
	first, firstChanged, err := store.clear(uid, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, secondChanged, err := store.clear(uid, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || !firstChanged || secondChanged || first.Revision != 2 || second.Revision != first.Revision {
		t.Fatalf("clear should update once and then be a no-op: first=%+v changed=%v second=%+v changed=%v", first, firstChanged, second, secondChanged)
	}
	missingUID := fmt.Sprintf("notification-clear-missing-%d", time.Now().UnixNano())
	missing, missingChanged, err := store.clear(missingUID, now)
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil || missingChanged {
		t.Fatalf("clear of a missing row should not create state: %+v changed=%v", missing, missingChanged)
	}
}
