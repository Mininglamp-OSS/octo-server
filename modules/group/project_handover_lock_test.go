package group

// PR #844 review round 2 — the two group-side blockers, each pinned by the
// behaviour it broke rather than by the shape of the fix.
//
// Both are instances of the same thing: a statement whose WHERE carries
// `is_deleted = 0` and whose caller never asked whether it matched anything. One
// silently loses a write (the handover), the other silently gains one (the
// poller).

import (
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/stretchr/testify/require"
)

// seedGroupMemberRow inserts one member row directly, so a case can build the
// exact starting state it needs without going through the funnel.
func seedGroupMemberRow(t *testing.T, ctx *config.Context, groupNo, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, remark, role, `version`, status, vercode, "+
			"is_deleted, invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
			"VALUES (?, ?, '', ?, 1, ?, ?, 0, '', 0, 0, 0, '', NOW())",
		groupNo, uid, role, int(common.GroupMemberStatusNormal), util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

func softDeleteGroupMemberRow(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted = 1 WHERE group_no = ? AND uid = ?", groupNo, uid).Exec()
	require.NoError(t, err)
}

func memberIsDeleted(t *testing.T, ctx *config.Context, groupNo, uid string) int {
	t.Helper()
	var v []int
	_, err := ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no = ? AND uid = ?", groupNo, uid).Load(&v)
	require.NoError(t, err)
	require.Len(t, v, 1)
	return v[0]
}

func activeCreatorsOf(t *testing.T, ctx *config.Context, groupNo string) []string {
	t.Helper()
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND role = ? AND is_deleted = 0",
		groupNo, MemberRoleCreator).Load(&uids)
	require.NoError(t, err)
	return uids
}

// projectGroupFixture builds a project group with a creator and, optionally,
// other members — all of them active members of the project.
func projectGroupFixture(t *testing.T, ctx *config.Context, creator string, others ...string) (groupNo, spaceID, projectID string) {
	t.Helper()
	spaceID = "sp_" + util.GenerUUID()[:8]
	projectID = util.GenerUUID()
	groupNo = util.GenerUUID()

	seedSpaceSeat(t, ctx, spaceID, creator)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, creator, 0)
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	seedGroupMemberRow(t, ctx, groupNo, creator, MemberRoleCreator)

	for _, uid := range others {
		seedSpaceSeat(t, ctx, spaceID, uid)
		seedProjectMember(t, ctx, projectID, spaceID, uid, 0)
		seedGroupMemberRow(t, ctx, groupNo, uid, MemberRoleCommon)
	}
	return groupNo, spaceID, projectID
}

// ---------- blocker 1: the handover ----------

// TestTheProjectGroupSuccessorIsPickedUnderARowLock.
//
// The defect: querySuccessorForProjectGroupTx read its candidate without a lock,
// and the promotion that follows it carries `is_deleted = 0`. A removal of the
// successor committing in between made the promotion affect zero rows and return
// no error, while the demotion of the outgoing creator affected one — leaving the
// group with no creator at all and the cascade logging a successful handover.
//
// This is the lock, asserted the only way a lock can be: a writer that must wait
// for it. The Space-side cascade's querySecondOldestNonBotMemberTx carries the
// same clause and the same comment, having learned it the same way.
func TestTheProjectGroupSuccessorIsPickedUnderARowLock(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	groupNo, _, projectID := projectGroupFixture(t, ctx, "creator1", "successor1")

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()

	successor, err := f.db.querySuccessorForProjectGroupTx(tx, groupNo, projectID, "creator1")
	require.NoError(t, err)
	require.Equal(t, "successor1", successor)

	// A concurrent removal of exactly that uid, on its own connection.
	var wg sync.WaitGroup
	done := make(chan struct{})
	var removeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, removeErr = ctx.DB().UpdateBySql(
			"UPDATE group_member SET is_deleted = 1 WHERE group_no = ? AND uid = ?",
			groupNo, "successor1").Exec()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the successor row was deleted while the handover transaction held it: " +
			"querySuccessorForProjectGroupTx is not locking its pick")
	case <-time.After(2 * time.Second):
		// Blocked, which is the assertion.
	}

	require.NoError(t, tx.Commit())
	wg.Wait()
	require.NoError(t, removeErr, "the removal must proceed once the handover commits, not fail")
}

// TestAPromotionThatMatchesNoLiveRowIsAnError.
//
// The second half of the fix, and the one that survives a future change to the
// first: even if the lock were dropped, a promotion that matched nothing must not
// fall through to the demotion. updateMemberRoleIfLiveTx reports what
// UpdateMemberRoleTx swallowed.
func TestAPromotionThatMatchesNoLiveRowIsAnError(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	groupNo, _, _ := projectGroupFixture(t, ctx, "creator1", "successor1")

	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()

	changed, err := f.db.updateMemberRoleIfLiveTx(tx, groupNo, "successor1", MemberRoleCreator, version)
	require.NoError(t, err)
	require.True(t, changed, "a live member must promote")
	require.NoError(t, tx.Commit())

	softDeleteGroupMemberRow(t, ctx, groupNo, "successor1")

	version, err = ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	tx2, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx2.RollbackUnlessCommitted()
	changed, err = f.db.updateMemberRoleIfLiveTx(tx2, groupNo, "successor1", MemberRoleCreator, version)
	require.NoError(t, err)
	require.False(t, changed, "a removed member must NOT promote, and must be reported as not promoted")
	require.NoError(t, tx2.Commit())
}

// TestTheHandoverLeavesExactlyOneCreator is the end state the two fixes exist
// for, asserted on group_member rather than on a return value.
func TestTheHandoverLeavesExactlyOneCreator(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	groupNo, spaceID, projectID := projectGroupFixture(t, ctx, "creator1", "successor1")

	handedOver, detached, err := f.handOverProjectGroupIfCreator(groupNo, projectmod.MemberRemoval{
		ProjectID:   projectID,
		UID:         "creator1",
		SpaceID:     spaceID,
		OperatorUID: "creator1",
		Reason:      "left",
	})
	require.NoError(t, err)
	require.True(t, handedOver)
	require.False(t, detached)

	require.Equal(t, []string{"successor1"}, activeCreatorsOf(t, ctx, groupNo),
		"the group must have exactly one creator, and it must be the successor")
}

// ---------- blocker 2: the poller ----------

// TestUpdateMemberCannotResurrectARemovedRow.
//
// UpdateMember writes is_deleted from a caller-supplied model. All four callers
// read a live member first and write the whole model back, so a row removed
// between the read and the write came back ACTIVE, with its pre-removal role,
// having never passed admitOrRestoreMembersTx. CheckForbiddenLoop is where the
// window is seconds wide: it reads a batch of up to 100 and then does a GenSeq
// and two IM calls per row before writing each one back.
//
// For a project group whose seat has closed in that window the resurrected row
// is a permanent I2 violation: the reconcile scan reports it and nothing repairs
// it, because the removal job is already terminal.
func TestUpdateMemberCannotResurrectARemovedRow(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	groupNo, _, _ := projectGroupFixture(t, ctx, "creator1", "muted1")

	// What the poller holds: a snapshot taken while the member was live.
	stale := &MemberModel{
		GroupNo:            groupNo,
		UID:                "muted1",
		Role:               MemberRoleCommon,
		IsDeleted:          0,
		ForbiddenExpirTime: 0,
	}

	// The member leaves — their project seat closed, an admin kicked them, it
	// does not matter which.
	softDeleteGroupMemberRow(t, ctx, groupNo, "muted1")

	stale.Version, _ = ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, f.db.UpdateMember(stale),
		"writing back a stale snapshot is a no-op, not an error: the member is simply gone")

	require.Equal(t, 1, memberIsDeleted(t, ctx, groupNo, "muted1"),
		"a removed member must stay removed; UpdateMember must not put them back in the group")
	require.False(t, activeMemberExists(t, ctx, groupNo, "muted1"))
}

// TestUpdateMemberStillUpdatesALiveMember is the other side of the same
// predicate: narrowing the WHERE must not break the four paths that use it.
func TestUpdateMemberStillUpdatesALiveMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	groupNo, _, _ := projectGroupFixture(t, ctx, "creator1", "muted1")

	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	require.NoError(t, f.db.UpdateMember(&MemberModel{
		GroupNo:            groupNo,
		UID:                "muted1",
		Remark:             "renamed",
		Role:               MemberRoleCommon,
		Version:            version,
		IsDeleted:          0,
		ForbiddenExpirTime: 4242,
	}))

	type row struct {
		Remark             string `db:"remark"`
		ForbiddenExpirTime int64  `db:"forbidden_expir_time"`
	}
	var rows []row
	_, err = ctx.DB().SelectBySql(
		"SELECT remark, forbidden_expir_time FROM group_member WHERE group_no = ? AND uid = ?",
		groupNo, "muted1").Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "renamed", rows[0].Remark)
	require.EqualValues(t, 4242, rows[0].ForbiddenExpirTime)
}
