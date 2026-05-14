//go:build integration

package conversation_ext

import (
	"sync"
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

// TestDB_UpdateSort_RejectsMissingNonFirstItem reproduces PR review (Round 3)
// Blocking #5: with the previous fix, only items[0] was locked & version-checked.
// If items[1..] referenced rows that do not exist, the per-row UPDATE silently
// affected 0 rows and the call returned nil — concurrent reorders with disjoint
// first anchors could fully interleave.
//
// Fix contract: UpdateSort must verify every requested item exists. Missing any
// item → ErrSortTargetNotFound (rollback, no partial write).
func TestDB_UpdateSort_RejectsMissingNonFirstItem(t *testing.T) {
	db := newDBForTest(t)

	// items[0] exists; items[1] does not.
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-anchor", ConvExtFields{
		FollowedDM: int8Ptr(1),
	}))

	err := db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "dm-anchor"},
		{TargetType: 1, TargetID: "missing-tail"},
	}, 0)

	require.Error(t, err,
		"UpdateSort with any missing item must fail, not silently UPDATE zero rows")
	assert.ErrorIs(t, err, ErrSortTargetNotFound)

	// Anchor row must still be present (DELETE 在 tx 内不会发生；FOR UPDATE 锁
	// 只是检测目标存在性）。Phase 3 后 per-row version 已废弃，不再断言。
	m, err := db.Get("u1", "s1", 1, "dm-anchor")
	require.NoError(t, err)
	require.NotNil(t, m, "anchor row must still exist after rolled-back UpdateSort")
}

// TestDB_UpdateSort_ConcurrentDifferentAnchors_OverlappingItems_Serializes
// proves the fix closes the "different first anchor" race.
//
// Two concurrent UpdateSort calls share item B but each uses a different first
// item (A vs C). The old impl only locked items[0] (A or C respectively) →
// nothing shared → both calls "succeeded" with version 1 on different
// non-overlapping rows, and B's final state depended on UPDATE interleaving.
//
// Fix: locking ALL items in deterministic order means both calls contend on B.
// Exactly one observes expectedVersion==0 and wins; the other sees version==1
// and gets ErrVersionConflict.
func TestDB_UpdateSort_ConcurrentDifferentAnchors_OverlappingItems_Serializes(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	for _, id := range []string{"A", "B", "C"} {
		require.NoError(t, db.Upsert(uid, space, 1, id, ConvExtFields{FollowedDM: int8Ptr(1)}))
	}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	successCh := make(chan struct{}, workers)

	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			// Half the goroutines anchor on "A", the rest on "C", but all touch "B".
			var items []SortItem
			if i%2 == 0 {
				items = []SortItem{{TargetType: 1, TargetID: "A"}, {TargetType: 1, TargetID: "B"}}
			} else {
				items = []SortItem{{TargetType: 1, TargetID: "C"}, {TargetType: 1, TargetID: "B"}}
			}
			if err := db.UpdateSort(uid, space, items, 0); err == nil {
				successCh <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(successCh)

	successes := 0
	for range successCh {
		successes++
	}
	assert.Equal(t, 1, successes,
		"only one concurrent UpdateSort starting at version=0 may succeed, "+
			"regardless of which item each call places first")

	// B 行必须仍然存在（PR #21 Round-6 之后 per-row version 字段已删除，
	// 验证锚点改为 user_follow_version 表，由 TestService_AllFollowWritePaths_BumpFollowVersion
	// 端到端覆盖）。
	mB, err := db.Get(uid, space, 1, "B")
	require.NoError(t, err)
	require.NotNil(t, mB, "shared row B must survive")
}

// TestDB_UpdateSort_AllAffectedItemsLocked verifies UpdateSort 在事务里
// 锁住了所有 item（而不只是 items[0]）；PR #21 Round-6 之后 per-row version 已删除，
// 这里只断言所有 item 仍存在（rolled-forward 一致性），版本由 user_follow_version 反映。
func TestDB_UpdateSort_AllAffectedItemsLocked(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, 1, "x", ConvExtFields{FollowedDM: int8Ptr(1)}))
	require.NoError(t, db.Upsert(uid, space, 1, "y", ConvExtFields{FollowedDM: int8Ptr(1)}))
	require.NoError(t, db.Upsert(uid, space, 1, "z", ConvExtFields{FollowedDM: int8Ptr(1)}))

	require.NoError(t, db.UpdateSort(uid, space, []SortItem{
		{TargetType: 1, TargetID: "x"},
		{TargetType: 1, TargetID: "y"},
		{TargetType: 1, TargetID: "z"},
	}, 0))

	for _, id := range []string{"x", "y", "z"} {
		m, err := db.Get(uid, space, 1, id)
		require.NoError(t, err)
		require.NotNil(t, m, "row %q must still exist after UpdateSort", id)
	}
}

// PR review (Round 3) Blocking #1/#2 — every follow-state write path must bump
// user_follow_version in the same transaction.  This regression test exercises
// each public Service write method and verifies the bump.
func TestService_AllFollowWritePaths_BumpFollowVersion(t *testing.T) {
	ctx := newCtxForTest(t)
	_, _ = ctx.DB().DeleteFrom(table).Exec()
	_, _ = ctx.DB().DeleteFrom(followVersionTable).Exec()

	svc := NewService(ctx)
	fvDB := NewFollowVersionDB(ctx)

	must := func(t *testing.T, op string, fn func() error) int64 {
		t.Helper()
		require.NoError(t, fn(), "%s must succeed", op)
		v, err := fvDB.Get("u1", "s1")
		require.NoError(t, err)
		return v
	}

	v := must(t, "FollowDM", func() error { return svc.FollowDM("u1", "s1", "peer1", nil) })
	require.Equal(t, int64(1), v)

	v = must(t, "FollowChannel", func() error { return svc.FollowChannel("u1", "s1", "grp1") })
	require.Equal(t, int64(2), v)

	v = must(t, "UnfollowChannel", func() error { return svc.UnfollowChannel("u1", "s1", "grp1") })
	require.Equal(t, int64(3), v)

	v = must(t, "FollowChannel", func() error { return svc.FollowChannel("u1", "s1", "grp1") })
	require.Equal(t, int64(4), v)

	v = must(t, "FollowThread", func() error { return svc.FollowThread("u1", "s1", "grp1____t1") })
	require.Equal(t, int64(5), v)

	v = must(t, "UnfollowThread", func() error { return svc.UnfollowThread("u1", "s1", "grp1____t1") })
	require.Equal(t, int64(6), v)

	v = must(t, "UnfollowDM", func() error { return svc.UnfollowDM("u1", "s1", "peer1") })
	require.Equal(t, int64(7), v)
}

// TestDB_UpdateSort_CASUsesFollowVersion verifies the round-3 protocol change:
// expectedVersion now refers to user_follow_version, not per-row version.
// Successful UpdateSort bumps follow_version by 1 and the next call must use
// the new value or fail with ErrVersionConflict.
func TestDB_UpdateSort_CASUsesFollowVersion(t *testing.T) {
	ctx := newCtxForTest(t)
	_, _ = ctx.DB().DeleteFrom(table).Exec()
	_, _ = ctx.DB().DeleteFrom(followVersionTable).Exec()

	db := NewDB(ctx)
	fvDB := NewFollowVersionDB(ctx)

	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-1", ConvExtFields{FollowedDM: int8Ptr(1)}))
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-2", ConvExtFields{FollowedDM: int8Ptr(1)}))

	// 1st call: expectedVersion=0 (fresh user) → succeeds.
	require.NoError(t, db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "dm-1"},
		{TargetType: 1, TargetID: "dm-2"},
	}, 0))
	v, err := fvDB.Get("u1", "s1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), v)

	// 2nd call with stale expectedVersion=0 → ErrVersionConflict.
	err = db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "dm-1"},
	}, 0)
	assert.ErrorIs(t, err, ErrVersionConflict)

	// 3rd call with current expectedVersion=1 → succeeds.
	require.NoError(t, db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "dm-1"},
	}, 1))
	v, err = fvDB.Get("u1", "s1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), v)
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
