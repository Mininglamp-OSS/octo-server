package notification

import (
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
)

func TestPauseTimeSurvivesNonUTCLocRoundTrip(t *testing.T) {
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

	uid := fmt.Sprintf("notification-tz-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = ctx.DB().DB.Exec("DELETE FROM user_notification_pause WHERE uid=?", uid) })
	store := newDBStore(ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	want := now.Add(2 * time.Hour)
	if _, err := store.upsert(uid, pauseModeTimed, &want, now); err != nil {
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
