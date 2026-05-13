package conversation_ext

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDB_UpdateSort_RejectsMissingFirstItem reproduces PR review Blocking #1.
//
// Bug: UpdateSort uses items[0] as the CAS anchor with SELECT ... FOR UPDATE.
// When items[0] does not exist in the DB, the SELECT returns dbr.ErrNotFound
// which is silently swallowed; currentVersion stays at the int zero value (0).
// If expectedVersion is also 0 (the initial state for any user), the CAS check
// passes vacuously, no row is locked, and subsequent UPDATEs affect 0 rows —
// the call returns nil pretending success.
//
// Fix: when items[0] is missing, return an explicit error so the caller can
// distinguish a true zero-version state from a non-existent target.
func TestDB_UpdateSort_RejectsMissingFirstItem(t *testing.T) {
	db := newDBForTest(t)

	// No rows for (u1, s1) at all — most extreme case.
	err := db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "non-existent"},
	}, 0)

	require.Error(t, err,
		"UpdateSort with non-existent items[0] and expectedVersion=0 must fail, "+
			"otherwise the CAS check passes vacuously and concurrent calls can interleave")
}

// TestDB_UpdateSort_RejectsMissingFirstItem_WithOtherRowsPresent confirms the
// bug is not masked when the user has unrelated existing rows.
func TestDB_UpdateSort_RejectsMissingFirstItem_WithOtherRowsPresent(t *testing.T) {
	db := newDBForTest(t)

	// Seed one real row to prove the user does have data — bug must still trigger.
	require.NoError(t, db.Upsert("u1", "s1", 1, "real-target", ConvExtFields{
		FollowedDM: int8Ptr(1),
	}))

	err := db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "non-existent-anchor"},
		{TargetType: 1, TargetID: "real-target"},
	}, 0)

	require.Error(t, err,
		"UpdateSort with missing items[0] must fail even when other listed items exist; "+
			"otherwise the CAS lock is not acquired and concurrent updates can race")
}

// TestRemoveConvExtForUserInSpace_GroupCascade_LeavesNoOrphans is a regression
// guard for PR review Blocking #2.
//
// The prior implementation issued the channel DELETE and the child-thread
// cascade DELETE as two independent statements outside a transaction. If the
// first succeeded and the second failed (connection drop, deadlock), the
// child-thread rows were orphaned. The fix wraps both into a single tx so the
// cleanup either fully commits or fully rolls back.
//
// This test exercises the happy path end-to-end and asserts both rows are gone
// — paired with the source-level fix it acts as a smoke test that the
// transactional rewrite still cascades correctly.
func TestRemoveConvExtForUserInSpace_GroupCascade_LeavesNoOrphans(t *testing.T) {
	ctx := newCtxForTest(t)
	_, _ = ctx.DB().DeleteFrom(table).Exec()
	InitGlobalConvExtDB(ctx)
	db := NewDB(ctx)

	// Seed: user follows a group + 3 of its sub-threads.
	require.NoError(t, db.Upsert("u1", "s1", targetTypeGroup, "g1", ConvExtFields{
		GroupUnfollowed: int8Ptr(0),
	}))
	for _, sid := range []string{"g1____t1", "g1____t2", "g1____t3"} {
		require.NoError(t, db.Upsert("u1", "s1", targetTypeThread, sid, ConvExtFields{}))
	}

	RemoveConvExtForUserInSpace("u1", "s1", "g1", targetTypeGroup)

	// Group row gone.
	got, err := db.Get("u1", "s1", targetTypeGroup, "g1")
	require.NoError(t, err)
	assert.Nil(t, got, "group ext row must be gone after cleanup")

	// All 3 thread rows gone.
	for _, sid := range []string{"g1____t1", "g1____t2", "g1____t3"} {
		got, err := db.Get("u1", "s1", targetTypeThread, sid)
		require.NoError(t, err)
		assert.Nil(t, got, "thread ext row %q must be gone after cascade", sid)
	}
}
