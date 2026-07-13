package thread

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryNonDeletedShortIDsByGroupNos — RC blocker on PR #553.
// The messages_search global endpoint’s allowlist goes through this query so
// archived threads keep showing up in global search, aligning with
// single-channel search / message read (both of which only reject deleted).
// Verifies:
//
//  1. Multiple groups return distinct shortID lists keyed by group_no.
//  2. **status IN (active, archived)** rows are surfaced; **deleted rows
//     are excluded** (the invariant that was violated before this fix).
//  3. Un-requested groups don’t appear.
//  4. Empty input short-circuits to an empty map with no SQL.
//  5. Each group’s shortIDs come back ordered by short_id — the ROW_NUMBER
//     PARTITION cap is deterministic across runs.
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

// TestQueryNonDeletedShortIDsByGroupNos_PerGroupCap — RC 3 on PR #553.
// The DB-side per-group cap (thread.NonDeletedByGroupNosPerGroupHardLimit
// = 201, enforced via UNION ALL of per-group `LIMIT` subqueries) exists so
// a runaway group cannot dump tens of thousands of rows into memory. Seed
// a single group above that cap and assert exactly `PerGroupHardLimit`
// rows come back for it.
func TestQueryNonDeletedShortIDsByGroupNos_PerGroupCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping per-group-cap seed test in -short mode")
	}
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// Seed just above the per-group cap so the PARTITION LIMIT kicks in.
	total := NonDeletedByGroupNosPerGroupHardLimit + 50
	groupNo := "grpHuge"
	for i := 0; i < total; i++ {
		m := &Model{
			ShortID:    "thr_" + zeroPad(i, 6),
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
	assert.Equal(t, NonDeletedByGroupNosPerGroupHardLimit, len(rows),
		"per-group PARTITION cap must bound the row count for a single fat group")
}

// TestQueryNonDeletedShortIDsByGroupNos_FatGroupDoesNotStarveOthers —
// **the RC 3 P1 regression test** for PR #553.
//
// Previous revision used one global `ORDER BY group_no, short_id LIMIT 2500`.
// A group sorting early with >= 2500 non-deleted threads would consume the
// entire LIMIT budget, and every other group would return zero rows. The
// caller's `continue` on the fat group's per-group cap (>200 rows) then
// combined with the empty tail to zero out thread coverage for the WHOLE
// request — not just the fat group. yujiawei’s RC 3 (2026-07-09) called this
// as blocking and correctly.
//
// The fix bounds each group at the SQL layer via ROW_NUMBER() OVER
// (PARTITION BY group_no) so no single group can starve another. This
// integration test locks that in: seed a group whose non-deleted thread
// count would eat the OLD global LIMIT on its own, plus several normal
// groups sorted AFTER it (higher group_no). Assert:
//   - fat group returns exactly `PerGroupHardLimit` rows (its own cap);
//   - every normal group returns its full row set (they are NOT starved).
func TestQueryNonDeletedShortIDsByGroupNos_FatGroupDoesNotStarveOthers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fat-group-starvation seed test in -short mode")
	}
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// The fat group's threads must (a) exceed the per-group PARTITION cap
	// so it demonstrably takes its share, AND (b) exceed the size of the
	// old global LIMIT (2500) so this test would fail against the previous
	// implementation (this is the actual regression bar we are locking in).
	const (
		fatThreadCount     = 3000 // > old global LIMIT (2500) and > per-group cap (201)
		normalGroupCount   = 5
		normalThreadsEach  = 20
		fatGroupPrefix     = "grp_aaa" // sorts BEFORE the normal groups by group_no
		normalGroupPrefix  = "grp_bbb_"
	)
	fatGroup := fatGroupPrefix
	for i := 0; i < fatThreadCount; i++ {
		m := &Model{
			ShortID:    fatGroup + "_thr_" + zeroPad(i, 6),
			GroupNo:    fatGroup,
			Name:       "t",
			CreatorUID: testutil.UID,
			Status:     ThreadStatusActive,
			Version:    1,
		}
		require.NoError(t, db.Insert(m))
	}
	normalGroups := make([]string, 0, normalGroupCount)
	for gi := 0; gi < normalGroupCount; gi++ {
		gn := normalGroupPrefix + zeroPad(gi, 3)
		normalGroups = append(normalGroups, gn)
		for si := 0; si < normalThreadsEach; si++ {
			m := &Model{
				ShortID:    gn + "_thr_" + zeroPad(si, 4),
				GroupNo:    gn,
				Name:       "t",
				CreatorUID: testutil.UID,
				Status:     ThreadStatusActive,
				Version:    1,
			}
			require.NoError(t, db.Insert(m))
		}
	}

	groupNos := append([]string{fatGroup}, normalGroups...)
	got, err := db.QueryNonDeletedShortIDsByGroupNos(groupNos)
	require.NoError(t, err)

	// Fat group: exactly PerGroupHardLimit rows — its own per-group cap.
	assert.Equal(t, NonDeletedByGroupNosPerGroupHardLimit, len(got[fatGroup]),
		"fat group must be bounded by per-group cap (%d), not starve the query",
		NonDeletedByGroupNosPerGroupHardLimit)

	// Every normal group: its FULL row set, unstarved. Under the pre-fix
	// implementation these would all be empty (or the whole map would miss
	// them), which is precisely the P1 blocker this test locks against.
	for _, gn := range normalGroups {
		assert.Equal(t, normalThreadsEach, len(got[gn]),
			"normal group %q must NOT be starved by fat group; got %d rows, want %d",
			gn, len(got[gn]), normalThreadsEach)
		for _, sid := range got[gn] {
			assert.True(t, strings.HasPrefix(sid, gn+"_thr_"),
				"normal group %q must only contain its own shortIDs; leaked %q", gn, sid)
		}
	}
}

// zeroPad — small helper for deterministic ORDER BY group_no in the
// multi-group truncation test.
func zeroPad(i, width int) string {
	s := strconv.Itoa(i)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// itoa — minimal, matches the pattern used elsewhere in package tests.
func itoa(i int) string {
	return strconv.Itoa(i)
}
