package robot

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRobotDBClosingTestsRestoreSystemSettings pins the pairing that keeps a
// deliberately closed pool from leaking out of the test that closed it.
//
// common.EnsureSystemSettings hands every caller the instance built for the
// FIRST one, that instance's *sql.DB pool included. So a test here that closes
// a pool it let New(...) latch — TestOwnedBots_CheckMembershipDBError does,
// to reach a database-error branch — poisons the singleton for every test
// scheduled after it, and the next EnsureSystemSettings(...).Reload()
// (setupBotSettings runs one per case) fails with "sql: database is closed".
// It only shows up under -shuffle=on, in a test that did nothing wrong.
//
// A source guard, because the damage is cross-test: the test that causes it
// cannot observe it, and the test that suffers it cannot attribute it.
func TestRobotDBClosingTestsRestoreSystemSettings(t *testing.T) {
	// This file names both tokens in prose, so checking it would be circular.
	const self = "system_settings_singleton_test.go"

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == self || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		src := stripGoLineComments(string(data))
		if !strings.Contains(src, ".DB().Close()") {
			continue
		}
		// Reported by hand rather than with assert.Contains, whose failure
		// message would print the whole file it searched.
		if !strings.Contains(src, "SnapshotSystemSettingsForTest") {
			t.Errorf("modules/robot/%s closes a *sql.DB pool, so it must bracket the "+
				"close with commonmodule.SnapshotSystemSettingsForTest / "+
				"RestoreSystemSettingsForTest (via t.Cleanup); otherwise common's "+
				"process-wide SystemSettings singleton can keep the closed pool and "+
				"later tests fail with \"sql: database is closed\"", name)
		}
	}
}

// stripGoLineComments removes // comments so a source guard does not trip on
// prose that merely names the token it is looking for.
func stripGoLineComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
