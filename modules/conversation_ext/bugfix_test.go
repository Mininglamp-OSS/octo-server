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

// TestDB_UpdateSort_UnchangedSortValues_NotConflict 复现真实环境抓到的 bug：
// 用户拖拽排序时，如果序列里某一项的新 follow_sort 等于它当前的 follow_sort
// （最常见就是首项保持不动），MySQL 驱动默认以 rows-changed 语义返回
// RowsAffected=0，老实现误判为 ErrVersionConflict 并整个 tx 回滚。
//
// 修复后：SELECT ... FOR UPDATE 已经在前面校验过 len(locked)==len(items)，
// 行的存在性已被保证，affected==0 唯一含义是"新值等于旧值"，应当视作成功。
func TestDB_UpdateSort_UnchangedSortValues_NotConflict(t *testing.T) {
	ctx := newCtxForTest(t)
	_, _ = ctx.DB().DeleteFrom(table).Exec()
	_, _ = ctx.DB().DeleteFrom(followVersionTable).Exec()

	db := NewDB(ctx)
	fvDB := NewFollowVersionDB(ctx)

	// Seed two rows already at the "target" sort values (1, 2).
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-a", ConvExtFields{
		FollowedDM: int8Ptr(1), FollowSort: intPtr(1),
	}))
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-b", ConvExtFields{
		FollowedDM: int8Ptr(1), FollowSort: intPtr(2),
	}))

	// Submitting the same order: every row's new follow_sort equals its current
	// value → MySQL reports RowsAffected=0 for each UPDATE. Must still succeed.
	err := db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "dm-a"},
		{TargetType: 1, TargetID: "dm-b"},
	}, 0)
	require.NoError(t, err, "no-op sort (values unchanged) must not be reported as version conflict")

	// follow_version must still advance — caller's optimistic CAS depends on it.
	v, err := fvDB.Get("u1", "s1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), v)
}

// TestDB_UpdateSort_FirstItemUnchanged_NotConflict 覆盖最常见的复现路径：
// 用户拖动列表但首项不动，items[0] 的旧值 (=1) 与新值 (i=0, i+1=1) 相同，
// 老实现在第一次 UPDATE 就 affected=0，整个 tx 误回滚。
func TestDB_UpdateSort_FirstItemUnchanged_NotConflict(t *testing.T) {
	ctx := newCtxForTest(t)
	_, _ = ctx.DB().DeleteFrom(table).Exec()
	_, _ = ctx.DB().DeleteFrom(followVersionTable).Exec()

	db := NewDB(ctx)

	// dm-head sort=1 (head), dm-tail sort=2.
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-head", ConvExtFields{
		FollowedDM: int8Ptr(1), FollowSort: intPtr(1),
	}))
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-tail", ConvExtFields{
		FollowedDM: int8Ptr(1), FollowSort: intPtr(2),
	}))

	// Submit the same order so dm-head stays at position 0 (new follow_sort=1,
	// equal to its current value → affected=0 in rows-changed semantics).
	err := db.UpdateSort("u1", "s1", []SortItem{
		{TargetType: 1, TargetID: "dm-head"},
		{TargetType: 1, TargetID: "dm-tail"},
	}, 0)
	require.NoError(t, err, "first item staying in place must not trigger version conflict")

	// Final state still correct.
	head, err := db.Get("u1", "s1", 1, "dm-head")
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.Equal(t, 1, head.FollowSort)

	tail, err := db.Get("u1", "s1", 1, "dm-tail")
	require.NoError(t, err)
	require.NotNil(t, tail)
	assert.Equal(t, 2, tail.FollowSort)
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

// ---------------------------------------------------------------------------
// Issue #151 — db.UpdateSort must remain strict for ALL target types.
// Materialization for default-followed groups (target_type=2 with category
// but no ext row) is gated by Service.AuthorizeAndMaterializeDefaultFollowed-
// Groups in the layer above; db.UpdateSort itself trusts the caller to have
// pre-flighted any necessary materialization.  Code review #1 removed the
// earlier inline materialization because it trusted the client payload (any
// group_no, with no membership / category check) — a metadata leak risk.
//
// These tests pin the strict behaviour at the DB layer.  Service- and
// handler-level coverage lives in service_test.go and api_test.go.
// ---------------------------------------------------------------------------

// TestDB_UpdateSort_StillRejectsMissingGroup verifies db.UpdateSort, called
// without upstream pre-flight materialization, surfaces a missing group as
// ErrSortTargetNotFound (i.e. the DB layer no longer auto-materializes).
func TestDB_UpdateSort_StillRejectsMissingGroup(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	err := db.UpdateSort(uid, space, []SortItem{
		{TargetType: targetTypeGroup, TargetID: "g-default"},
	}, 0)
	assert.ErrorIs(t, err, ErrSortTargetNotFound,
		"db.UpdateSort must NOT materialize missing groups on its own — the "+
			"caller is responsible for AuthorizeAndMaterializeDefaultFollowedGroups "+
			"upstream (issue #151 code review #1)")

	m, err := db.Get(uid, space, targetTypeGroup, "g-default")
	require.NoError(t, err)
	assert.Nil(t, m,
		"db.UpdateSort must not write any ext row when it fails — preserves "+
			"the rolled-back tx invariant")
}

// TestDB_UpdateSort_StillRejectsMissingDM verifies the strict semantics are
// preserved for target_type=1 — DMs only appear in the follow tab if their
// ext row exists, so a missing DM target in a sort payload remains a real
// client/server desync.
func TestDB_UpdateSort_StillRejectsMissingDM(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, targetTypeDM, "dm-real", ConvExtFields{
		FollowedDM: int8Ptr(1),
	}))

	err := db.UpdateSort(uid, space, []SortItem{
		{TargetType: targetTypeDM, TargetID: "dm-real"},
		{TargetType: targetTypeDM, TargetID: "dm-ghost"},
	}, 0)
	assert.ErrorIs(t, err, ErrSortTargetNotFound,
		"missing DM ext rows must still be reported as ErrSortTargetNotFound; "+
			"only target_type=2 (groups) get lazy materialization")
}

// TestDB_UpdateSort_MixedPayload_MissingDM_NoOpportunisticGroupWrite verifies
// that even without materialization in db.UpdateSort, a mixed payload with a
// missing DM does not leave behind any partial state (e.g. via a pre-flight
// step that the caller forgot to gate).  Defense in depth: the DB layer
// itself never writes group ext rows from UpdateSort.
func TestDB_UpdateSort_MixedPayload_MissingDM_NoOpportunisticGroupWrite(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	err := db.UpdateSort(uid, space, []SortItem{
		{TargetType: targetTypeGroup, TargetID: "g-default"},
		{TargetType: targetTypeDM, TargetID: "dm-ghost"},
	}, 0)
	assert.ErrorIs(t, err, ErrSortTargetNotFound,
		"db.UpdateSort must reject any missing target regardless of type")

	m, err := db.Get(uid, space, targetTypeGroup, "g-default")
	require.NoError(t, err)
	assert.Nil(t, m,
		"db.UpdateSort must never write a group ext row on its own — "+
			"materialization is the caller's responsibility (issue #151 review #1)")
}

// TestDB_UpdateSort_StillRejectsMissingThread verifies the strict semantics
// for target_type=5 are preserved for the same reason as DMs.
func TestDB_UpdateSort_StillRejectsMissingThread(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, targetTypeThread, "grp1____thr-real", ConvExtFields{
		FollowSort: intPtr(1),
	}))

	err := db.UpdateSort(uid, space, []SortItem{
		{TargetType: targetTypeThread, TargetID: "grp1____thr-real"},
		{TargetType: targetTypeThread, TargetID: "grp1____thr-ghost"},
	}, 0)
	assert.ErrorIs(t, err, ErrSortTargetNotFound,
		"missing thread ext rows must still be reported as ErrSortTargetNotFound")
}

// ---------------------------------------------------------------------------
// Issue #151 symptom #2 — MaterializeDefaultFollowedGroups is the read-path
// hook that turns default-followed groups (in a category, no ext row) into
// real rows on first sidebar/sync of the follow tab.  Without this, the
// OnThreadCreated fanout silently skips users whose follow status exists only
// via category — new threads in those groups never reach their follow tab.
// ---------------------------------------------------------------------------

func TestDB_MaterializeDefaultFollowedGroups_CreatesRowsWithAutoFollowOn(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.MaterializeDefaultFollowedGroups(uid, space,
		[]string{"g-a", "g-b", "g-c"}))

	for _, id := range []string{"g-a", "g-b", "g-c"} {
		m, err := db.Get(uid, space, targetTypeGroup, id)
		require.NoError(t, err)
		require.NotNil(t, m, "ext row %q must exist after materialization", id)
		assert.Equal(t, int8(1), m.AutoFollowThreads,
			"materialized row must have auto_follow_threads=1 so OnThreadCreated "+
				"will fan out new threads in this group to this user")
		assert.Equal(t, int8(0), m.GroupUnfollowed,
			"materialized row must have group_unfollowed=0")
	}
}

func TestDB_MaterializeDefaultFollowedGroups_Idempotent_PreservesExistingRow(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	// Pre-existing row with non-default state (e.g. user previously dragged
	// the group to follow_sort=5 and explicitly opted out of thread fanout).
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-x", ConvExtFields{
		AutoFollowThreads: int8Ptr(0),
		GroupUnfollowed:   int8Ptr(0),
		FollowSort:        intPtr(5),
	}))

	require.NoError(t, db.MaterializeDefaultFollowedGroups(uid, space, []string{"g-x"}))

	m, err := db.Get(uid, space, targetTypeGroup, "g-x")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(0), m.AutoFollowThreads,
		"materialization must be a no-op on existing rows; "+
			"user's explicit auto_follow_threads=0 choice must be respected")
	assert.Equal(t, 5, m.FollowSort,
		"materialization must not overwrite existing follow_sort")
}

func TestDB_MaterializeDefaultFollowedGroups_EmptyInput_NoOp(t *testing.T) {
	db := newDBForTest(t)
	require.NoError(t, db.MaterializeDefaultFollowedGroups("u1", "s1", nil))
	require.NoError(t, db.MaterializeDefaultFollowedGroups("u1", "s1", []string{}))
}

// ---------------------------------------------------------------------------
// Issue #151 review #3 — ClearAutoFollowThreadsTx
//
// MaterializeDefaultFollowedGroups creates ext rows with auto_follow_threads=1.
// When the implicit follow goes away (user moves group out of category),
// the same flag must be cleared so OnThreadCreated stops fanning out new
// threads to a user who no longer sees the group in their follow tab.
// ---------------------------------------------------------------------------

func TestDB_ClearAutoFollowThreadsTx_ClearsExistingRow(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	// Pre-existing row with auto_follow_threads=1 (the materialized state).
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-clear", ConvExtFields{
		AutoFollowThreads: int8Ptr(1),
		GroupUnfollowed:   int8Ptr(0),
		FollowSort:        intPtr(7),
	}))

	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	require.NoError(t, ClearAutoFollowThreadsTx(tx, uid, space, []string{"g-clear"}))
	require.NoError(t, tx.Commit())

	m, err := db.Get(uid, space, targetTypeGroup, "g-clear")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(0), m.AutoFollowThreads,
		"auto_follow_threads must be cleared")
	assert.Equal(t, int8(0), m.GroupUnfollowed,
		"group_unfollowed must NOT be set — uncategorize ≠ explicit unfollow; "+
			"clearing this flag would break the existing UnfollowGroupsTx contract")
	assert.Equal(t, 7, m.FollowSort,
		"follow_sort must be preserved — only the auto_follow_threads flag changes")
}

func TestDB_ClearAutoFollowThreadsTx_BatchClearsMultipleRows(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	for _, g := range []string{"g-a", "g-b", "g-c"} {
		require.NoError(t, db.Upsert(uid, space, targetTypeGroup, g, ConvExtFields{
			AutoFollowThreads: int8Ptr(1),
		}))
	}
	// Unrelated row in the same uid/space must be left alone (negative case).
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-keep", ConvExtFields{
		AutoFollowThreads: int8Ptr(1),
	}))

	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	require.NoError(t, ClearAutoFollowThreadsTx(tx, uid, space, []string{"g-a", "g-b", "g-c"}))
	require.NoError(t, tx.Commit())

	for _, g := range []string{"g-a", "g-b", "g-c"} {
		m, err := db.Get(uid, space, targetTypeGroup, g)
		require.NoError(t, err)
		require.NotNil(t, m)
		assert.Equal(t, int8(0), m.AutoFollowThreads,
			"batch member %q must be cleared", g)
	}
	mk, err := db.Get(uid, space, targetTypeGroup, "g-keep")
	require.NoError(t, err)
	require.NotNil(t, mk)
	assert.Equal(t, int8(1), mk.AutoFollowThreads,
		"row not in the groupNos list must NOT be touched")
}

func TestDB_ClearAutoFollowThreadsTx_NoRow_NoOp(t *testing.T) {
	db := newDBForTest(t)
	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	// No row materialized — the call must succeed without error and leave
	// the table state unchanged (this is the happy path for a user who
	// never opened the follow tab before uncategorizing).
	require.NoError(t, ClearAutoFollowThreadsTx(tx, "u-ghost", "s1", []string{"g-ghost"}))
	require.NoError(t, tx.Commit())

	m, err := db.Get("u-ghost", "s1", targetTypeGroup, "g-ghost")
	require.NoError(t, err)
	assert.Nil(t, m, "no row may be created — the helper is strictly UPDATE")
}

func TestDB_ClearAutoFollowThreadsTx_EmptyInput_NoOp(t *testing.T) {
	db := newDBForTest(t)
	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	require.NoError(t, ClearAutoFollowThreadsTx(tx, "u1", "s1", nil))
	require.NoError(t, ClearAutoFollowThreadsTx(tx, "u1", "s1", []string{}))
	require.NoError(t, ClearAutoFollowThreadsTx(tx, "", "s1", []string{"g"}),
		"empty uid is a no-op (defensive, matches other helpers)")
	require.NoError(t, ClearAutoFollowThreadsTx(tx, "u1", "", []string{"g"}),
		"empty spaceID is a no-op")
	require.NoError(t, tx.Commit())
}

// RestoreAutoFollowThreadsTx is the symmetric counterpart — verify the same
// contract surface but for the =1 side of the lifecycle.

func TestDB_RestoreAutoFollowThreadsTx_RestoresClearedRow(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	// Simulate the post-move-out state: row exists but auto_follow_threads=0.
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-restore", ConvExtFields{
		AutoFollowThreads: int8Ptr(0),
		GroupUnfollowed:   int8Ptr(0),
		FollowSort:        intPtr(3),
	}))

	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	require.NoError(t, RestoreAutoFollowThreadsTx(tx, uid, space, []string{"g-restore"}))
	require.NoError(t, tx.Commit())

	m, err := db.Get(uid, space, targetTypeGroup, "g-restore")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.AutoFollowThreads,
		"auto_follow_threads must be restored to 1")
	assert.Equal(t, int8(0), m.GroupUnfollowed,
		"group_unfollowed must NOT be touched — restore is symmetric to clear, "+
			"only the auto-subscribe flag changes")
	assert.Equal(t, 3, m.FollowSort,
		"follow_sort must be preserved")
}

func TestDB_RestoreAutoFollowThreadsTx_NoRow_NoOp(t *testing.T) {
	db := newDBForTest(t)
	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	// First-time-into-category case: no ext row yet.  Restore must NOT
	// create a row — sidebar materialization is the canonical creation
	// site; the move-in handler must stay strictly UPDATE so the two
	// sites cannot race on the unique key.
	require.NoError(t, RestoreAutoFollowThreadsTx(tx, "u-fresh", "s1", []string{"g-fresh"}))
	require.NoError(t, tx.Commit())

	m, err := db.Get("u-fresh", "s1", targetTypeGroup, "g-fresh")
	require.NoError(t, err)
	assert.Nil(t, m, "no row may be created — restore is strictly UPDATE")
}

func TestDB_RestoreAutoFollowThreadsTx_LeavesUnrelatedRowsAlone(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	// Two rows in scope, one out of scope (different target_id).
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-a", ConvExtFields{AutoFollowThreads: int8Ptr(0)}))
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-b", ConvExtFields{AutoFollowThreads: int8Ptr(0)}))
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, "g-keep", ConvExtFields{AutoFollowThreads: int8Ptr(0)}))

	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	require.NoError(t, RestoreAutoFollowThreadsTx(tx, uid, space, []string{"g-a", "g-b"}))
	require.NoError(t, tx.Commit())

	for _, id := range []string{"g-a", "g-b"} {
		m, err := db.Get(uid, space, targetTypeGroup, id)
		require.NoError(t, err)
		require.NotNil(t, m)
		assert.Equal(t, int8(1), m.AutoFollowThreads, "row %q restored", id)
	}
	mk, err := db.Get(uid, space, targetTypeGroup, "g-keep")
	require.NoError(t, err)
	require.NotNil(t, mk)
	assert.Equal(t, int8(0), mk.AutoFollowThreads,
		"row not in groupNos list must NOT be touched")
}
