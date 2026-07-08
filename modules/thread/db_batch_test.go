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
//  5. Result rows are ORDER BY (group_no, short_id), so the DB-side
//     LIMIT (NonDeletedByGroupNosDBHardLimit) truncates deterministically.
//  6. `truncated` return is false when the returned rows are under the LIMIT.
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

	// (1) Empty input → empty map, no SQL error, truncated=false.
	empty, truncated, err := db.QueryNonDeletedShortIDsByGroupNos(nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "empty input must return empty map")
	assert.False(t, truncated, "empty input must not set truncated")

	// (2) grpA + grpB + grpDeletedOnly: archived rows surface; deleted rows
	//     stay excluded; grpDeletedOnly is absent because all its rows are
	//     deleted.
	got, truncated, err := db.QueryNonDeletedShortIDsByGroupNos(
		[]string{"grpA", "grpB", "grpDeletedOnly"})
	require.NoError(t, err)
	assert.False(t, truncated,
		"small-fixture query must not report truncated (returned rows well under LIMIT)")

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
	unknown, truncated, err := db.QueryNonDeletedShortIDsByGroupNos([]string{"grpUnknown"})
	require.NoError(t, err)
	assert.False(t, truncated)
	_, present := unknown["grpUnknown"]
	assert.False(t, present, "group with no rows must not appear")
}

// TestQueryNonDeletedShortIDsByGroupNos_HardLimit — the DB-side LIMIT
// (thread.NonDeletedByGroupNosDBHardLimit) exists to keep a pathological
// membership footprint from dragging tens of thousands of rows into
// memory. Single-group seed above the LIMIT: assert (a) returned row
// count == LIMIT, (b) `truncated` sentinel fires so the caller can WARN.
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

	got, truncated, err := db.QueryNonDeletedShortIDsByGroupNos([]string{groupNo})
	require.NoError(t, err)
	rows := got[groupNo]
	assert.Equal(t, NonDeletedByGroupNosDBHardLimit, len(rows),
		"seed guarantees exactly LIMIT rows come back")
	assert.True(t, truncated,
		"len(rows)==LIMIT must set truncated=true so callers can WARN (RC 2 on PR #553)")
}

// TestQueryNonDeletedShortIDsByGroupNos_HardLimit_MultiGroupTailDrop — RC 2
// on PR #553. The failure mode the outside reviewers built against:
//
//   - When the sum of non-deleted rows across N groups exceeds the DB LIMIT,
//     the `ORDER BY group_no, short_id LIMIT` cut lands mid-stream. Tail
//     groups (highest `group_no`) may end up partial or entirely missing.
//   - The caller's per-group (>200) / aggregate-total (>2000) downgrade
//     branches only trigger on too-MANY rows, so tail zero/partial rows
//     were silently dropped in the previous revision with NO WARN.
//
// This test seeds many groups whose combined rows cross the LIMIT and
// asserts:
//   - truncated=true is returned so the caller has a signal to WARN on;
//   - total returned rows == LIMIT (deterministic cut);
//   - the head groups (lowest `group_no`) are complete;
//   - the tail groups (highest `group_no`) after the cut point receive zero
//     rows — exactly the "silently missing" symptom the WARN now covers.
func TestQueryNonDeletedShortIDsByGroupNos_HardLimit_MultiGroupTailDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hard-limit multi-group seed test in -short mode")
	}
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// Design: N groups × K threads each, N*K just above LIMIT. Group
	// naming is zero-padded so ORDER BY group_no is deterministic and
	// intuitive ("grp_000" < "grp_001" < ... < "grp_009").
	const (
		K = 300 // threads per group — well over caller's per-group cap 200
	)
	N := (NonDeletedByGroupNosDBHardLimit / K) + 2 // guarantees N*K > LIMIT
	groupNos := make([]string, 0, N)
	for gi := 0; gi < N; gi++ {
		gn := "grp_" + zeroPad(gi, 3)
		groupNos = append(groupNos, gn)
		for si := 0; si < K; si++ {
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

	got, truncated, err := db.QueryNonDeletedShortIDsByGroupNos(groupNos)
	require.NoError(t, err)
	assert.True(t, truncated,
		"multi-group query crossing the LIMIT must report truncated=true")

	// Total rows returned must equal the DB LIMIT.
	totalReturned := 0
	for _, ids := range got {
		totalReturned += len(ids)
	}
	assert.Equal(t, NonDeletedByGroupNosDBHardLimit, totalReturned,
		"LIMIT %d must cap the aggregate row count", NonDeletedByGroupNosDBHardLimit)

	// The cut point: how many complete head groups fit under the LIMIT +
	// (optional) one partial group. All tail groups beyond that must
	// return zero rows.
	fullHeadCount := NonDeletedByGroupNosDBHardLimit / K
	partialAt := fullHeadCount // may or may not exist (0..K-1 leftover rows)
	leftover := NonDeletedByGroupNosDBHardLimit - fullHeadCount*K

	// Head: exactly K rows each.
	for i := 0; i < fullHeadCount; i++ {
		gn := groupNos[i]
		assert.Equal(t, K, len(got[gn]),
			"head group %q must be complete", gn)
	}
	// Partial: leftover rows. Its shortIDs must all start with that group's prefix.
	if leftover > 0 && partialAt < N {
		gn := groupNos[partialAt]
		assert.Equal(t, leftover, len(got[gn]),
			"boundary group %q must have exactly leftover rows", gn)
		for _, sid := range got[gn] {
			assert.True(t, strings.HasPrefix(sid, gn+"_thr_"),
				"boundary group rows must belong to that group; got %q under %q", sid, gn)
		}
	}
	// Tail: must be silently absent (zero rows returned for these groups).
	// This is the exact symptom the truncated=true signal exists to cover.
	tailStart := partialAt
	if leftover > 0 {
		tailStart = partialAt + 1
	}
	for i := tailStart; i < N; i++ {
		gn := groupNos[i]
		assert.Empty(t, got[gn],
			"tail group %q past the LIMIT must be silently absent (that's what truncated=true warns about)", gn)
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
