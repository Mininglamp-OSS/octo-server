package voice

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestASRLogCleaner_RemovesExpiredDirs(t *testing.T) {
	dir := t.TempDir()

	// Create date directories: some expired, some not
	// cutoff = now.UTC().AddDate(0,0,-7), so dates before cutoff are removed.
	// A date directory parsed as midnight of that day. If today is 2026-04-15,
	// cutoff is 2026-04-08T{current_time}. "2026-04-08" parses as midnight,
	// which is before the cutoff time → removed. Only dates strictly after cutoff remain.
	today := time.Now().UTC()
	dirs := []struct {
		name    string
		expired bool
	}{
		{today.Format("2006-01-02"), false},
		{today.AddDate(0, 0, -1).Format("2006-01-02"), false},
		{today.AddDate(0, 0, -6).Format("2006-01-02"), false},
		{today.AddDate(0, 0, -7).Format("2006-01-02"), true},   // midnight of 7 days ago < cutoff time
		{today.AddDate(0, 0, -8).Format("2006-01-02"), true},   // expired
		{today.AddDate(0, 0, -30).Format("2006-01-02"), true},  // expired
	}

	for _, d := range dirs {
		subDir := filepath.Join(dir, d.name, "gemini")
		require.NoError(t, os.MkdirAll(subDir, 0755))
		// Create a dummy file inside
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "test.json"), []byte("{}"), 0644))
	}

	cleaner := NewASRLogCleaner(dir, 7)
	cleaner.clean()

	for _, d := range dirs {
		_, err := os.Stat(filepath.Join(dir, d.name))
		if d.expired {
			assert.True(t, os.IsNotExist(err), "dir %s should be removed", d.name)
		} else {
			assert.NoError(t, err, "dir %s should still exist", d.name)
		}
	}
}

func TestASRLogCleaner_SkipsNonDateDirs(t *testing.T) {
	dir := t.TempDir()

	// Create non-date directories
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "not-a-date"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "2026-13-99"), 0755)) // invalid date
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "readme"), 0755))

	cleaner := NewASRLogCleaner(dir, 7)
	cleaner.clean()

	// Non-date dirs should not be touched
	_, err := os.Stat(filepath.Join(dir, "not-a-date"))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "2026-13-99"))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "readme"))
	assert.NoError(t, err)
}

func TestASRLogCleaner_SkipsFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file (not directory) with a date name
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2020-01-01"), []byte("x"), 0644))

	cleaner := NewASRLogCleaner(dir, 7)
	cleaner.clean()

	// File should not be removed (it's not a directory)
	_, err := os.Stat(filepath.Join(dir, "2020-01-01"))
	assert.NoError(t, err)
}

func TestASRLogCleaner_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	cleaner := NewASRLogCleaner(dir, 7)
	// Should not error on empty directory
	cleaner.clean()
}

func TestASRLogCleaner_NonExistentDir(t *testing.T) {
	cleaner := NewASRLogCleaner("/nonexistent/path/asr-logs", 7)
	// Should not error on non-existent directory
	cleaner.clean()
}

func TestASRLogCleaner_StartAsyncAndClose(t *testing.T) {
	dir := t.TempDir()

	cleaner := NewASRLogCleaner(dir, 7)
	cleaner.StartAsync()

	// Wait a bit to ensure goroutine is running
	time.Sleep(50 * time.Millisecond)

	// Close should complete without hanging
	done := make(chan struct{})
	go func() {
		cleaner.Close()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Close() timed out")
	}
}

func TestASRLogCleaner_CustomRetention(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().UTC()

	// With 3-day retention
	threeDaysAgo := today.AddDate(0, 0, -3).Format("2006-01-02")
	fourDaysAgo := today.AddDate(0, 0, -4).Format("2006-01-02")
	twoDaysAgo := today.AddDate(0, 0, -2).Format("2006-01-02")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, threeDaysAgo), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, fourDaysAgo), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, twoDaysAgo), 0755))

	cleaner := NewASRLogCleaner(dir, 3)
	cleaner.clean()

	// 4 days ago should be removed
	_, err := os.Stat(filepath.Join(dir, fourDaysAgo))
	assert.True(t, os.IsNotExist(err))

	// 3 days ago should be removed (cutoff = now - 3 days, t.Before(cutoff))
	_, err = os.Stat(filepath.Join(dir, threeDaysAgo))
	assert.True(t, os.IsNotExist(err))

	// 2 days ago should remain
	_, err = os.Stat(filepath.Join(dir, twoDaysAgo))
	assert.NoError(t, err)
}
