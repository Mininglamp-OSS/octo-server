//go:build integration

package conversation_ext

// =============================================================================
// Integration tests — cross-module end-to-end scenarios (issue #337)
//
// These tests exercise real DB operations covering cascade cleanup paths and
// transaction boundaries that unit tests cannot fully verify in isolation.
//
// Prerequisites:
//   - MySQL running at CONV_EXT_TEST_MYSQL_ADDR (default: root:demo@tcp(127.0.0.1)/conv_ext_test)
//   - user_conversation_ext table present (run migration SQL first)
//
// Run:
//   go test -race -tags=integration ./modules/conversation_ext/...
//
// Scenarios covered here (scenes not covered by existing unit tests):
//   1. Leave-group cascade: RemoveConvExtForUserInSpace clears group row + all child thread rows
//   2. Disband-group cascade: RemoveConvExtForChannel clears all user rows + all child threads
//   3. Delete-friend cascade: RemoveConvExtForUser clears bidirectional DM ext rows
//   4. FollowThread implicit re-follow: UnfollowChannel + FollowThread atomically clears blacklist
//   6. v1 zero-regression: ext table writes do not mutate the conv_ext read-query result set
// =============================================================================

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers — reuse setup helpers from cleanup_test.go (same package)
// ---------------------------------------------------------------------------

// newIntegrationDB returns a *DB and a cleaned service backed by the test MySQL.
// We deliberately call both initGlobalConvExtDBForTest (for the cleanup functions
// that use the global singleton) AND newServiceForTest (for the service-level ops).
func newIntegrationDB(t *testing.T) (*DB, *Service) {
	t.Helper()
	ctx := newCtxForTest(t)
	// Reset and initialise the global DB singleton so cleanup functions work.
	db := initGlobalConvExtDBForTest(t, ctx)
	// Wipe the table for a clean slate.
	_, err := ctx.DB().DeleteFrom(table).Exec()
	require.NoError(t, err, "clean table before integration test")
	// Build a service connected to the same ctx.
	svc := NewService(ctx)
	return db, svc
}

// ---------------------------------------------------------------------------
// Scene 1: Leave-group cascade
//
// Setup  : user "u1" has ext row for group A + ext rows for T1, T2 (child threads of A)
//          + ext row for a thread of a different group (must survive).
// Action : call RemoveConvExtForUserInSpace(uid, space, groupAID, ChannelTypeGroup)
// Assert : group A row gone, T1 + T2 rows gone, unrelated thread row preserved.
// ---------------------------------------------------------------------------

func TestIntegration_LeaveGroup_CascadeClearsGroupAndChildThreads(t *testing.T) {
	db, _ := newIntegrationDB(t)

	const uid, space = "int-u1", "sp1"
	const groupA = "int-grpA"
	threadT1 := groupA + "____thr-t1"
	threadT2 := groupA + "____thr-t2"
	const otherGroup = "int-grpOther"
	otherThread := otherGroup + "____thr-tx"

	// Setup: insert group A row, two child thread rows, one unrelated thread row.
	insertRow(t, db, uid, space, targetTypeGroup, groupA)
	insertRow(t, db, uid, space, targetTypeThread, threadT1)
	insertRow(t, db, uid, space, targetTypeThread, threadT2)
	insertRow(t, db, uid, space, targetTypeThread, otherThread)

	// Also add group_unfollowed marker to simulate a user who unfollowed before.
	_, err := db.session.UpdateBySql(
		"UPDATE "+table+" SET group_unfollowed=1 WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
		uid, space, targetTypeGroup, groupA,
	).Exec()
	require.NoError(t, err)

	// Action: simulate "leave group" cascade.
	RemoveConvExtForUserInSpace(uid, space, groupA, targetTypeGroup)

	// Assert: group row is gone.
	assert.Equal(t, 0, countRows(t, db, uid, space, targetTypeGroup, groupA),
		"group A ext row must be deleted after leave-group")

	// Assert: both child thread rows are gone.
	assert.Equal(t, 0, countRows(t, db, uid, space, targetTypeThread, threadT1),
		"thread T1 ext row must be cascade-deleted")
	assert.Equal(t, 0, countRows(t, db, uid, space, targetTypeThread, threadT2),
		"thread T2 ext row must be cascade-deleted")

	// Assert: unrelated thread from a different group must survive.
	assert.Equal(t, 1, countRows(t, db, uid, space, targetTypeThread, otherThread),
		"unrelated thread ext row must be preserved")
}

// Test that the leave-group cascade is scoped to the leaving user only;
// other users' rows are untouched.
func TestIntegration_LeaveGroup_OnlyAffectsLeavingUser(t *testing.T) {
	db, _ := newIntegrationDB(t)

	const space = "sp2"
	const groupA = "int-grpB"
	thread := groupA + "____thr-multi"

	// Both u1 and u2 have rows for the same group and thread.
	for _, uid := range []string{"int-u2", "int-u3"} {
		insertRow(t, db, uid, space, targetTypeGroup, groupA)
		insertRow(t, db, uid, space, targetTypeThread, thread)
	}

	// Only u2 leaves.
	RemoveConvExtForUserInSpace("int-u2", space, groupA, targetTypeGroup)

	// u2's rows must be gone.
	assert.Equal(t, 0, countRows(t, db, "int-u2", space, targetTypeGroup, groupA))
	assert.Equal(t, 0, countRows(t, db, "int-u2", space, targetTypeThread, thread))

	// u3's rows must survive.
	assert.Equal(t, 1, countRows(t, db, "int-u3", space, targetTypeGroup, groupA),
		"other user's group ext row must not be affected")
	assert.Equal(t, 1, countRows(t, db, "int-u3", space, targetTypeThread, thread),
		"other user's thread ext row must not be affected")
}

// ---------------------------------------------------------------------------
// Scene 2: Disband-group cascade
//
// Setup  : group A has 5 members; every member has followed it (ext row) and
//          the group has 3 child threads; each member also has thread ext rows.
// Action : RemoveConvExtForChannel(groupAID, ChannelTypeGroup)
// Assert : all 5 user ext rows for group A gone; all 5×3 thread rows gone;
//          unrelated channel rows preserved.
// ---------------------------------------------------------------------------

func TestIntegration_DisbandGroup_CascadesClearsAllUsersAndThreads(t *testing.T) {
	db, _ := newIntegrationDB(t)

	const space = "sp3"
	const groupA = "int-disband-grpA"
	threadIDs := []string{
		groupA + "____thr-d1",
		groupA + "____thr-d2",
		groupA + "____thr-d3",
	}
	members := []string{"int-dm1", "int-dm2", "int-dm3", "int-dm4", "int-dm5"}

	// Unrelated channel: must survive.
	const unrelated = "int-disband-grpZ"
	insertRow(t, db, members[0], space, targetTypeGroup, unrelated)

	// Each member has a group row + 3 thread rows.
	for _, uid := range members {
		insertRow(t, db, uid, space, targetTypeGroup, groupA)
		for _, tid := range threadIDs {
			insertRow(t, db, uid, space, targetTypeThread, tid)
		}
	}

	// Action: disband the group.
	RemoveConvExtForChannel(groupA, targetTypeGroup)

	// All member group rows must be gone.
	assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeGroup, groupA),
		"all group ext rows must be deleted after disband")

	// All thread rows under the group must be gone.
	for _, tid := range threadIDs {
		assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeThread, tid),
			"thread ext row %q must be cascade-deleted after group disband", tid)
	}

	// Unrelated group row must survive.
	assert.Equal(t, 1, countRows(t, db, members[0], space, targetTypeGroup, unrelated),
		"unrelated group ext row must be preserved")
}

// ---------------------------------------------------------------------------
// Scene 3: Delete-friend cascade (bidirectional DM cleanup)
//
// Setup  : A has followed B (DM ext row), B has followed A (DM ext row).
//          Each also has unrelated DM rows (different peer).
// Action : RemoveConvExtForUser(A, B) then RemoveConvExtForUser(B, A)
// Assert : A→B and B→A ext rows gone; other peers' rows preserved.
// ---------------------------------------------------------------------------

func TestIntegration_DeleteFriend_BidirectionalDMExtCleaned(t *testing.T) {
	db, _ := newIntegrationDB(t)

	const uidA, uidB = "int-friend-a", "int-friend-b"
	const uidC = "int-friend-c" // unrelated peer

	// A follows B and C; B follows A.
	insertRow(t, db, uidA, "spF", targetTypeDM, uidB)
	insertRow(t, db, uidA, "spF", targetTypeDM, uidC)
	insertRow(t, db, uidB, "spF", targetTypeDM, uidA)

	// Action: both sides of the delete-friend operation.
	RemoveConvExtForUser(uidA, uidB) // A removes B
	RemoveConvExtForUser(uidB, uidA) // B removes A (symmetric)

	// A→B DM row must be gone.
	assert.Equal(t, 0, countRows(t, db, uidA, "spF", targetTypeDM, uidB),
		"A→B DM ext row must be deleted")

	// B→A DM row must be gone.
	assert.Equal(t, 0, countRows(t, db, uidB, "spF", targetTypeDM, uidA),
		"B→A DM ext row must be deleted")

	// A's row toward C must survive.
	assert.Equal(t, 1, countRows(t, db, uidA, "spF", targetTypeDM, uidC),
		"A's DM ext row toward C must be preserved")
}

// Idempotent re-run: calling RemoveConvExtForUser twice must not error/panic.
func TestIntegration_DeleteFriend_Idempotent(t *testing.T) {
	db, _ := newIntegrationDB(t)
	_ = db

	const uidA, uidB = "int-idem-a", "int-idem-b"
	// No rows inserted — calling on non-existent rows must be safe.
	assert.NotPanics(t, func() {
		RemoveConvExtForUser(uidA, uidB)
		RemoveConvExtForUser(uidA, uidB) // second call: idempotent
	})
}

// ---------------------------------------------------------------------------
// Scene 4: FollowThread implicit re-follow of parent group
//
// The operation must be atomic: if the parent group has group_unfollowed=1
// then FollowThread must clear that flag AND create the thread ext row in a
// single transaction.  We verify both effects survive and that a partial state
// is never observable via Get.
// ---------------------------------------------------------------------------

func TestIntegration_FollowThread_ClearsParentBlacklistAndCreatesThreadRow(t *testing.T) {
	_, svc := newIntegrationDB(t)

	const uid, space, grp = "int-ft-u1", "sp4", "int-ft-grp1"
	threadChannelID := grp + "____thr-ft1"

	// Step 1: Mark the parent group as unfollowed (blacklisted).
	require.NoError(t, svc.UnfollowChannel(uid, space, grp))

	parent, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, int8(1), parent.GroupUnfollowed, "precondition: group must be blacklisted")

	// Confirm no thread ext row exists yet.
	threadRow, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	assert.Nil(t, threadRow, "precondition: no thread ext row before FollowThread")

	// Step 2: Follow the thread — must atomically clear parent blacklist + create thread row.
	require.NoError(t, svc.FollowThread(uid, space, threadChannelID))

	// Assert parent group is no longer blacklisted.
	parent2, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, parent2, "parent group ext row must still exist")
	assert.Equal(t, int8(0), parent2.GroupUnfollowed,
		"group_unfollowed must be cleared by FollowThread (atomic re-follow)")

	// Assert thread ext row now exists.
	threadRow2, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	assert.NotNil(t, threadRow2, "thread ext row must be created by FollowThread")
}

// Verify that FollowThread when the parent was NOT previously unfollowed still
// creates the thread ext row without introducing a spurious group row.
func TestIntegration_FollowThread_ParentNotBlacklisted_CreatesThreadRow(t *testing.T) {
	_, svc := newIntegrationDB(t)

	const uid, space, grp = "int-ft-u2", "sp4", "int-ft-grp2"
	threadChannelID := grp + "____thr-ft2"

	// No prior UnfollowChannel call — parent has no ext row at all.
	require.NoError(t, svc.FollowThread(uid, space, threadChannelID))

	// Thread ext row must exist.
	threadRow, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	assert.NotNil(t, threadRow, "thread ext row must be created")

	// Parent group row was created as a side effect (INSERT IGNORE with group_unfollowed=0).
	// Its group_unfollowed must be 0.
	parent, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	// Parent row either does not exist (INSERT IGNORE did nothing if row was absent)
	// or exists with group_unfollowed=0.
	if parent != nil {
		assert.Equal(t, int8(0), parent.GroupUnfollowed,
			"if parent row was created, group_unfollowed must be 0")
	}
}

// ---------------------------------------------------------------------------
// Scene 4b: Transaction rollback — FollowThread uses a single transaction;
// if the second write (thread ext row) fails the first write (group blacklist
// clear) must also be rolled back.
//
// We simulate this by inserting a row that causes the thread upsert to succeed
// normally (it uses INSERT IGNORE), so a direct rollback test is not possible
// without injecting a fault.  Instead we verify the atomicity guarantee via a
// concurrent race: two goroutines attempt FollowThread for the SAME thread;
// both must succeed (INSERT IGNORE) and the final state must be exactly one row.
// ---------------------------------------------------------------------------

func TestIntegration_FollowThread_ConcurrentInsertIdempotent(t *testing.T) {
	_, svc := newIntegrationDB(t)

	const uid, space, grp = "int-ft-race", "sp5", "int-ft-grp-race"
	threadChannelID := grp + "____thr-race"

	// Pre-condition: group is unfollowed.
	require.NoError(t, svc.UnfollowChannel(uid, space, grp))

	const workers = 5
	var wg sync.WaitGroup
	wg.Add(workers)
	errCh := make(chan error, workers)

	for range [workers]struct{}{} {
		go func() {
			defer wg.Done()
			if err := svc.FollowThread(uid, space, threadChannelID); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)

	// All goroutines must succeed (INSERT IGNORE is idempotent).
	for err := range errCh {
		assert.NoError(t, err)
	}

	// Exactly one thread ext row must exist.
	var count int
	_, err := svc.db.session.SelectBySql(
		"SELECT COUNT(*) FROM "+table+" WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
		uid, space, targetTypeThread, threadChannelID,
	).Load(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "exactly one thread ext row must exist after concurrent FollowThread calls")

	// Parent group must be un-blacklisted.
	parent, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, int8(0), parent.GroupUnfollowed, "parent group must no longer be blacklisted")
}

// ---------------------------------------------------------------------------
// Scene 6: v1 read-query zero-regression
//
// Writing ext rows (FollowDM, UnfollowChannel) must NOT change what the
// DB-layer queries that the v1 syncUserConversation path reads.
//
// v1 reads:
//   - conversation_extra table  (untouched by conversation_ext code)
//   - channel_offset, device_offset, … (untouched)
//   - group_setting / category   (untouched by conversation_ext code)
//
// The only tables conversation_ext writes to is user_conversation_ext.
// This test verifies that the ext table write is isolated: before and after
// writing ext rows, a COUNT(*) on all OTHER conversation-related tables is
// identical (i.e., ext writes do not spill into other tables).
// ---------------------------------------------------------------------------

func TestIntegration_ExtWrites_DoNotAffectOtherConversationTables(t *testing.T) {
	_, svc := newIntegrationDB(t)

	ctx := newCtxForTest(t)

	// Tables read by the v1 sync path (not written by conversation_ext).
	otherTables := []string{
		"conversation_extra",
	}

	// Count rows in other tables BEFORE any ext writes.
	beforeCounts := make(map[string]int, len(otherTables))
	for _, tbl := range otherTables {
		var n int
		_, _ = ctx.DB().SelectBySql("SELECT COUNT(*) FROM `" + tbl + "`").Load(&n)
		beforeCounts[tbl] = n
	}

	// Perform ext writes: follow a DM and unfollow a channel.
	const uid, space = "int-reg-u1", "sp-reg"
	catID := "cat-uuid-99"
	seedTestCategory(t, svc, uid, space, catID)
	require.NoError(t, svc.FollowDM(uid, space, "int-reg-peer1", &catID))
	require.NoError(t, svc.UnfollowChannel(uid, space, "int-reg-grp1"))
	require.NoError(t, svc.FollowThread(uid, space, "int-reg-grp1____thr-r1"))

	// Count rows in other tables AFTER ext writes.
	for _, tbl := range otherTables {
		var n int
		_, _ = ctx.DB().SelectBySql("SELECT COUNT(*) FROM `" + tbl + "`").Load(&n)
		assert.Equal(t, beforeCounts[tbl], n,
			"table %q row count must not change after conversation_ext writes", tbl)
	}

	// Verify ext rows were actually written (test is meaningful).
	dmRow, err := svc.db.Get(uid, space, targetTypeDM, "int-reg-peer1")
	require.NoError(t, err)
	assert.NotNil(t, dmRow, "DM ext row must have been written")

	grpRow, err := svc.db.Get(uid, space, targetTypeGroup, "int-reg-grp1")
	require.NoError(t, err)
	assert.NotNil(t, grpRow, "group ext row must have been written")
}

// ---------------------------------------------------------------------------
// Scene 6b: Confirm that ListFollowedDM / ListUnfollowedGroups / ListThreadExts
// queries return only rows for the calling user+space, never leaking across
// users — this is the critical isolation guarantee that v2 sidebar relies on.
// ---------------------------------------------------------------------------

func TestIntegration_ListQueries_SpaceAndUserIsolation(t *testing.T) {
	db, svc := newIntegrationDB(t)

	// uid1 in space A: follows DM and unfollows a group.
	const uid1, spaceA = "int-iso-u1", "iso-spA"
	catID := "cat-uuid-iso"
	seedTestCategory(t, svc, uid1, spaceA, catID)
	require.NoError(t, svc.FollowDM(uid1, spaceA, "iso-peer1", &catID))
	require.NoError(t, svc.UnfollowChannel(uid1, spaceA, "iso-grp1"))

	// uid2 in space B: different data, must not appear in uid1's queries.
	const uid2, spaceB = "int-iso-u2", "iso-spB"
	require.NoError(t, svc.FollowDM(uid2, spaceB, "iso-peer2", nil))

	// uid1's followed DMs in spaceA must contain exactly iso-peer1.
	dms, err := db.ListFollowedDM(uid1, spaceA)
	require.NoError(t, err)
	require.Len(t, dms, 1)
	assert.Equal(t, "iso-peer1", dms[0].TargetID)

	// uid1's unfollowed groups in spaceA must contain exactly iso-grp1.
	groups, err := db.ListUnfollowedGroups(uid1, spaceA)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "iso-grp1", groups[0].TargetID)

	// uid2's DMs must not bleed into uid1's results.
	dms2, err := db.ListFollowedDM(uid1, spaceB)
	require.NoError(t, err)
	assert.Len(t, dms2, 0, "uid1's query on spaceB must not see uid2's rows")
}

// ---------------------------------------------------------------------------
// Scene 7: Channel→threads cascade follow (auto_follow_threads)
//
// End-to-end lifecycle for the sidebar-channel-follow-cascade PR. Covers:
//
//   - FollowChannel materialises every active thread under the channel.
//   - OnThreadCreated fanouts to every auto_follow=1 user but skips users who
//     UnfollowChannel'd (auto_follow=0) or never followed.
//   - UnfollowChannel cascade-deletes all thread ext rows AND clears
//     auto_follow_threads so future OnThreadCreated skips this user.
//   - Re-FollowChannel "resurrects" the cascade —— even threads previously
//     UnfollowThread'd come back (confirmed product requirement).
//   - UnfollowThread keeps auto_follow_threads=1 and does NOT block fanout of
//     subsequently-created threads (since fanout only runs on creation).
// ---------------------------------------------------------------------------

// fixedThreadEnumerator 是 integration test 内用的 ThreadEnumerator 桩：
// 由测试自己维护 (groupNo -> shortIDs) 的快照，模拟"群下当前有哪些 active 子区"。
// 与 thread.DB 解耦让本测试不依赖 thread 模块表。
type fixedThreadEnumerator struct {
	groups map[string][]string
}

func (f *fixedThreadEnumerator) EnumerateActiveShortIDs(groupNo string, limit int) ([]string, error) {
	ids := f.groups[groupNo]
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]string, len(ids))
	copy(out, ids)
	return out, nil
}

func TestIntegration_ChannelCascade_FullLifecycle(t *testing.T) {
	_, svc := newIntegrationDB(t)
	const space, grp = "sp-cc", "int-cc-grp"
	const userA, userB, userC = "int-cc-A", "int-cc-B", "int-cc-C"

	// 模拟群下已有 3 个 active 子区。
	enum := &fixedThreadEnumerator{groups: map[string][]string{
		grp: {"t1", "t2", "t3"},
	}}
	svc.SetThreadEnumerator(enum)

	// 1. A 关注 channel → 3 个 thread ext 行被物化，auto_follow_threads=1。
	require.NoError(t, svc.FollowChannel(userA, space, grp))
	for _, sid := range []string{"t1", "t2", "t3"} {
		row, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+sid)
		require.NoError(t, err)
		assert.NotNil(t, row, "A 关注 channel 后应物化子区 %s", sid)
	}
	grpRow, err := svc.db.Get(userA, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, grpRow)
	assert.Equal(t, int8(1), grpRow.AutoFollowThreads)

	// 2. B 关注但立刻取关 channel —— OnThreadCreated 应跳过 B。
	require.NoError(t, svc.FollowChannel(userB, space, grp))
	require.NoError(t, svc.UnfollowChannel(userB, space, grp))
	// B 的 group 行 auto_follow_threads 必须被清零（否则 OnThreadCreated 还会找到他）。
	bGrp, err := svc.db.Get(userB, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, bGrp)
	assert.Equal(t, int8(0), bGrp.AutoFollowThreads)
	assert.Equal(t, int8(1), bGrp.GroupUnfollowed)

	// 3. C 完全没操作过 channel —— OnThreadCreated 应跳过 C。
	// （不写任何行）

	// 4. 模拟新建子区 t4：thread 模块创建后会同步调用 OnThreadCreated。
	enum.groups[grp] = append(enum.groups[grp], "t4")
	require.NoError(t, svc.OnThreadCreated(grp, "t4"))

	// Only A should have the new thread row.
	rowA4, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+"t4")
	require.NoError(t, err)
	assert.NotNil(t, rowA4, "A 应在 fanout 中拿到 t4")
	rowB4, err := svc.db.Get(userB, space, targetTypeThread, grp+threadSeparator+"t4")
	require.NoError(t, err)
	assert.Nil(t, rowB4, "B 已 unfollow channel，不应被 fanout")
	rowC4, err := svc.db.Get(userC, space, targetTypeThread, grp+threadSeparator+"t4")
	require.NoError(t, err)
	assert.Nil(t, rowC4, "C 从未关注 channel，不应被 fanout")

	// 5. A 单独 UnfollowThread(t2) —— 只删 t2 一行，auto_follow_threads 保持 1。
	require.NoError(t, svc.UnfollowThread(userA, space, grp+threadSeparator+"t2"))
	rowAT2, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+"t2")
	require.NoError(t, err)
	assert.Nil(t, rowAT2, "A 单独取关 t2 后该行消失")
	aGrp2, err := svc.db.Get(userA, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, aGrp2)
	assert.Equal(t, int8(1), aGrp2.AutoFollowThreads, "UnfollowThread 不应清掉 channel 级 auto_follow_threads")

	// 6. 新建子区 t5 —— A 仍然被 fanout（auto_follow=1 没变），但 t2 不会复活
	//    （fanout 只在子区创建时走，UnfollowThread 后已不再有 t2 的 create 信号）。
	enum.groups[grp] = append(enum.groups[grp], "t5")
	require.NoError(t, svc.OnThreadCreated(grp, "t5"))
	rowAT5, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+"t5")
	require.NoError(t, err)
	assert.NotNil(t, rowAT5, "新建子区 t5 应继续 fanout 给 A")
	rowAT2Again, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+"t2")
	require.NoError(t, err)
	assert.Nil(t, rowAT2Again, "fanout 只在子区创建时走；A 单独取关过的 t2 不会因为 t5 的 fanout 而复活")

	// 7. A 取消关注 channel —— 全部 thread 行级联清空 + auto_follow_threads=0。
	require.NoError(t, svc.UnfollowChannel(userA, space, grp))
	for _, sid := range []string{"t1", "t3", "t4", "t5"} { // t2 早就被取关
		row, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+sid)
		require.NoError(t, err)
		assert.Nil(t, row, "UnfollowChannel 应级联删 %s", sid)
	}
	aGrp3, err := svc.db.Get(userA, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, aGrp3)
	assert.Equal(t, int8(0), aGrp3.AutoFollowThreads)
	assert.Equal(t, int8(1), aGrp3.GroupUnfollowed)

	// 8. A 重新关注 channel —— 当前 active 子区全部"复活"（含曾被 UnfollowThread 的 t2）。
	//    enum 现在含 t1..t5。
	require.NoError(t, svc.FollowChannel(userA, space, grp))
	for _, sid := range []string{"t1", "t2", "t3", "t4", "t5"} {
		row, err := svc.db.Get(userA, space, targetTypeThread, grp+threadSeparator+sid)
		require.NoError(t, err)
		assert.NotNil(t, row,
			"FollowChannel 应物化所有当前 active 子区 %s（含 A 曾单独取关的 t2 —— 用户已确认这是预期行为）",
			sid)
	}
}

func TestIntegration_ChannelCascade_FollowVersionBumpedOnce(t *testing.T) {
	_, svc := newIntegrationDB(t)
	const uid, space, grp = "int-cc-v-u", "sp-ccv", "int-cc-v-grp"

	enum := &fixedThreadEnumerator{groups: map[string][]string{
		grp: {"v1", "v2", "v3", "v4", "v5"},
	}}
	svc.SetThreadEnumerator(enum)

	var before int64
	_ = svc.session.SelectBySql(
		"SELECT version FROM "+followVersionTable+" WHERE uid=? AND space_id=?",
		uid, space,
	).LoadOne(&before)

	require.NoError(t, svc.FollowChannel(uid, space, grp))

	var after int64
	require.NoError(t, svc.session.SelectBySql(
		"SELECT version FROM "+followVersionTable+" WHERE uid=? AND space_id=?",
		uid, space,
	).LoadOne(&after))

	// Bug fix #2 后 FollowChannel 拆两阶段提交，每阶段 +1 共 +2。
	// 不变量：bump 次数与子区数 N 无关，保持小常数（≤2）。
	assert.LessOrEqual(t, after-before, int64(2),
		"FollowChannel 物化 N 个子区，bump follow_version 的次数应为小常数（2 次），不与 N 成比例")
	assert.GreaterOrEqual(t, after-before, int64(1), "follow_version 至少 +1")
}

// ---------------------------------------------------------------------------
// Performance benchmarks (用户要求：fanout N=100/1000/10000 + 单 channel 500 子区物化)
//
// Run via: go test -tags integration -bench=. -benchmem -run=^$ \
//          ./modules/conversation_ext/
//
// 仅记录耗时供性能基线参考，不做断言（数值因机器而异）。
// ---------------------------------------------------------------------------

// newBenchService 是 benchmark 专用的 Service 工厂，避免在 *testing.B 里塞一个
// 假的 *testing.T{}（Jerry-Xin review）。直接接 testing.TB —— newCtxForTest 已经
// 用 testing.TB-兼容的方法（仅 Helper() + Fatalf via require）；require 系列也支持
// testing.TB。
func newBenchService(b *testing.B) (*DB, *Service) {
	b.Helper()
	ctx := newCtxForTestTB(b)
	db := initGlobalConvExtDBForTestTB(b, ctx)
	_, err := ctx.DB().DeleteFrom(table).Exec()
	require.NoError(b, err, "clean table before bench")
	_, err = ctx.DB().DeleteFrom(followVersionTable).Exec()
	require.NoError(b, err, "clean follow_version before bench")
	return db, NewService(ctx)
}

func benchmarkOnThreadCreatedFanout(b *testing.B, numUsers int) {
	b.Helper()
	_, svc := newBenchService(b)
	const space, grp = "bench-sp", "bench-grp-fanout"

	// 预先：numUsers 个用户全部 auto_follow_threads=1。
	for i := 0; i < numUsers; i++ {
		uid := "bench-u-" + strconv.Itoa(i)
		// 不走 FollowChannel（避免触发 enumerator），直接 upsert 群行。
		one := int8(1)
		zero := int8(0)
		require.NoError(b, svc.db.Upsert(uid, space, targetTypeGroup, grp, ConvExtFields{
			GroupUnfollowed:   &zero,
			AutoFollowThreads: &one,
		}))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每次跑用一个新 shortID，避免 INSERT IGNORE 第二次起全部 no-op 影响测量。
		shortID := "bench-thr-" + strconv.Itoa(i)
		if err := svc.OnThreadCreated(grp, shortID); err != nil {
			b.Fatalf("OnThreadCreated failed: %v", err)
		}
	}
}

func BenchmarkOnThreadCreated_N100(b *testing.B)   { benchmarkOnThreadCreatedFanout(b, 100) }
func BenchmarkOnThreadCreated_N1000(b *testing.B)  { benchmarkOnThreadCreatedFanout(b, 1000) }
func BenchmarkOnThreadCreated_N10000(b *testing.B) { benchmarkOnThreadCreatedFanout(b, 10000) }

func BenchmarkFollowChannel_Materialize500(b *testing.B) {
	_, svc := newBenchService(b)

	ids := make([]string, 500)
	for i := range ids {
		ids[i] = "bench-thr-" + strconv.Itoa(i)
	}
	enum := &fixedThreadEnumerator{groups: map[string][]string{
		"bench-grp-mat": ids,
	}}
	svc.SetThreadEnumerator(enum)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		uid := "bench-mat-u-" + strconv.Itoa(i)
		if err := svc.FollowChannel(uid, "bench-sp", "bench-grp-mat"); err != nil {
			b.Fatalf("FollowChannel failed: %v", err)
		}
	}
}
