package thread

import (
	"sort"
	"strconv"
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

// TestQueryNonDeletedShortIDsByGroupNos — RC blocker on PR #553.
// The messages_search global endpoint’s allowlist now goes through this
// query (not QueryActiveShortIDsByGroupNos) so archived threads keep
// showing up in global search, aligning with single-channel search /
// message read (both of which only reject deleted). Verifies:
//
//  1. Multiple groups return distinct shortID lists keyed by group_no.
//  2. **status IN (active, archived)** rows are surfaced; **deleted rows
//     are excluded** (the invariant that was violated before this fix).
//  3. Un-requested groups don’t appear.
//  4. Empty input short-circuits to an empty map with no SQL.
//  5. Result rows are ORDER BY (group_no, short_id), so the DB-side
//     LIMIT (NonDeletedByGroupNosDBHardLimit) truncates deterministically.
//
// Integration test: requires a running MySQL (see main_test.go), same as
// every other DB test in this package.
func TestQueryNonDeletedShortIDsByGroupNos(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// Seed: two groups with mixed statuses + a third group we won't query
	// for + one group whose only threads are deleted (must NOT appear).
	seed := []struct {
		shortID string
		groupNo string
		status  int
	}{
		// grpA: one active, one archived, one deleted — archived MUST
		// surface (the RC fix), deleted MUST NOT.
		{"thr_a1", "grpA", ThreadStatusActive},
		{"thr_a2", "grpA", ThreadStatusArchived},
		{"thr_a3", "grpA", ThreadStatusDeleted},
		// grpB: only an archived thread — pre-fix this group would be
		// silently absent; post-fix it must be present.
		{"thr_b1", "grpB", ThreadStatusArchived},
		// grpC: NOT queried.
		{"thr_c1", "grpC", ThreadStatusActive},
		// grpDeletedOnly: only deleted rows — must NOT appear in the map.
		{"thr_d1", "grpDeletedOnly", ThreadStatusDeleted},
		{"thr_d2", "grpDeletedOnly", ThreadStatusDeleted},
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

	// (1) Empty input → empty map, no SQL error.
	empty, err := db.QueryNonDeletedShortIDsByGroupNos(nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "empty input must return empty map")

	// (2) grpA + grpB + grpDeletedOnly: archived rows surface; deleted rows
	//     stay excluded; grpDeletedOnly is absent because all its rows are
	//     deleted.
	got, err := db.QueryNonDeletedShortIDsByGroupNos(
		[]string{"grpA", "grpB", "grpDeletedOnly"})
	require.NoError(t, err)

	sort.Strings(got["grpA"])
	assert.Equal(t, []string{"thr_a1", "thr_a2"}, got["grpA"],
		"grpA must yield active + archived (deleted excluded)")
	assert.Equal(t, []string{"thr_b1"}, got["grpB"],
		"grpB must yield its archived-only thread — pre-fix this leaked to empty")
	_, hasDeletedOnly := got["grpDeletedOnly"]
	assert.False(t, hasDeletedOnly,
		"a group whose only rows are deleted must not appear (deleted rows must never leak)")

	// Also assert the archived shortID for grpA is actually present, and
	// the deleted shortID is actually absent — catches any regression that
	// silently swaps status semantics.
	assert.Contains(t, got["grpA"], "thr_a2", "archived shortID must be present")
	assert.NotContains(t, got["grpA"], "thr_a3", "deleted shortID must not leak")

	// (3) grpC was not requested — must be absent.
	_, hasC := got["grpC"]
	assert.False(t, hasC, "un-requested group must not leak into results")

	// (4) A brand-new group with no rows in the table returns nothing.
	unknown, err := db.QueryNonDeletedShortIDsByGroupNos([]string{"grpUnknown"})
	require.NoError(t, err)
	_, present := unknown["grpUnknown"]
	assert.False(t, present, "group with no rows must not appear")
}

// TestQueryNonDeletedShortIDsByGroupNos_HardLimit — the DB-side LIMIT
// (thread.NonDeletedByGroupNosDBHardLimit) exists to keep a pathological
// membership footprint from dragging tens of thousands of rows into
// memory (RC N2 on PR #553). Seed just enough rows to cross the limit and
// assert the returned row count never exceeds it. Deterministic ordering
// (ORDER BY group_no, short_id) means the truncation cut point is stable
// across runs.
func TestQueryNonDeletedShortIDsByGroupNos_HardLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hard-limit seed test in -short mode")
	}
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// Seed just above the DB hard limit so LIMIT kicks in.
	total := NonDeletedByGroupNosDBHardLimit + 50
	groupNo := "grpHuge"
	for i := 0; i < total; i++ {
		m := &Model{
			ShortID:    "thr_" + itoa(i),
			GroupNo:    groupNo,
			Name:       "t",
			CreatorUID: testutil.UID,
			Status:     ThreadStatusActive,
			Version:    1,
		}
		require.NoError(t, db.Insert(m))
	}

	got, err := db.QueryNonDeletedShortIDsByGroupNos([]string{groupNo})
	require.NoError(t, err)
	rows := got[groupNo]
	assert.LessOrEqual(t, len(rows), NonDeletedByGroupNosDBHardLimit,
		"LIMIT %d must cap the returned row count", NonDeletedByGroupNosDBHardLimit)
	assert.Equal(t, NonDeletedByGroupNosDBHardLimit, len(rows),
		"seed guarantees exactly LIMIT rows come back")
}

// itoa — minimal, matches the pattern used elsewhere in package tests.
func itoa(i int) string {
	return strconv.Itoa(i)
}
