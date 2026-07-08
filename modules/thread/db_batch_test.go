package thread

import (
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryActiveShortIDsByGroupNos — batch IN query used by
// messages_search.buildAllowlist to build the v1 thread allowlist. Verifies:
//
//  1. Multiple groups return distinct shortID lists keyed by group_no.
//  2. Only status=active rows are surfaced (archived / deleted filtered).
//  3. Groups the caller didn't list simply don't appear in the returned map.
//  4. Empty input short-circuits to an empty map with no SQL.
//  5. A group with zero active threads returns no key (not an empty slice).
//
// Integration test: requires a running MySQL (see main_test.go), same as
// every other DB test in this package.
func TestQueryActiveShortIDsByGroupNos(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// Seed data: two groups with mixed statuses + a third group we won't
	// query for (must not appear in results).
	seed := []struct {
		shortID string
		groupNo string
		status  int
	}{
		// grpA: two active, one archived, one deleted
		{"thr_a1", "grpA", ThreadStatusActive},
		{"thr_a2", "grpA", ThreadStatusActive},
		{"thr_a3", "grpA", ThreadStatusArchived},
		{"thr_a4", "grpA", ThreadStatusDeleted},
		// grpB: one active only
		{"thr_b1", "grpB", ThreadStatusActive},
		// grpC: NOT queried
		{"thr_c1", "grpC", ThreadStatusActive},
		// grpD: no active threads at all (only archived)
		{"thr_d1", "grpD", ThreadStatusArchived},
	}
	for _, s := range seed {
		m := &Model{
			ShortID:    s.shortID,
			GroupNo:    s.groupNo,
			Name:       "t-" + s.shortID,
			CreatorUID: testutil.UID,
			Status:     s.status,
			Version:    1,
		}
		require.NoError(t, db.Insert(m))
	}

	// (1) empty input → empty map, no SQL error.
	empty, err := db.QueryActiveShortIDsByGroupNos(nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "empty input must return empty map")

	// (2) grpA + grpB + grpD: only active rows surface, grpD absent entirely.
	got, err := db.QueryActiveShortIDsByGroupNos([]string{"grpA", "grpB", "grpD"})
	require.NoError(t, err)

	sort.Strings(got["grpA"])
	assert.Equal(t, []string{"thr_a1", "thr_a2"}, got["grpA"],
		"grpA must yield exactly its two active threads (archived/deleted filtered)")
	assert.Equal(t, []string{"thr_b1"}, got["grpB"],
		"grpB must yield exactly its single active thread")
	_, hasD := got["grpD"]
	assert.False(t, hasD, "grpD has zero active threads and must not appear in the map")

	// (3) grpC was not requested — must be absent.
	_, hasC := got["grpC"]
	assert.False(t, hasC, "un-requested group must not leak into results")

	// (4) A brand-new group with no rows in the table returns nothing.
	unknown, err := db.QueryActiveShortIDsByGroupNos([]string{"grpUnknown"})
	require.NoError(t, err)
	_, present := unknown["grpUnknown"]
	assert.False(t, present, "group with no rows must not appear")
}
