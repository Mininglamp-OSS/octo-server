package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

// Invariant I2, exercised end to end against a real MySQL 8.0.
//
// I2 — for a group whose project_id is not the empty sentinel, every active
// group_member row belongs to an active member of that project (system bots
// exempted).
//
// There is NO read-path filter behind it. A uid with a group_member row sees the
// group in /v1/sidebar/sync, receives its messages over WuKongIM, and can post.
// So these cases are not about an error code being returned; they are about
// whether a row exists. Every assertion below reads group_member directly for
// that reason — an endpoint returning 400 while having written the row would
// pass a status-code assertion and fail the invariant.

func seedProject(t *testing.T, ctx *config.Context, projectID, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, '', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, spaceID, "p-"+projectID[:8],
	).Exec()
	require.NoError(t, err)
}

func seedProjectMember(t *testing.T, ctx *config.Context, projectID, spaceID, uid string, removing int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, 0, 1, ?, '', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, uid, spaceID, removing,
	).Exec()
	require.NoError(t, err)
}

func seedSpaceSeat(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO `space` (space_id, name, status, created_at, updated_at) "+
			"VALUES (?, ?, 1, NOW(), NOW())", spaceID, "s-"+spaceID).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) "+
			"VALUES (?, ?, 0, 1, NOW(), NOW())", spaceID, uid).Exec()
	require.NoError(t, err)
}

func seedGroupRow(t *testing.T, ctx *config.Context, groupNo, spaceID, projectID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, space_id, project_id) "+
			"VALUES (?, ?, '', 1, 1, ?, ?)", groupNo, "g", spaceID, projectID).Exec()
	require.NoError(t, err)
}

func activeMemberExists(t *testing.T, ctx *config.Context, groupNo, uid string) bool {
	t.Helper()
	var n int
	err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, uid).LoadOne(&n)
	require.NoError(t, err)
	return n > 0
}

// admitOne drives the single admission entry directly, in its own transaction,
// the way every converged path does.
func admitOne(t *testing.T, f *Group, groupNo, spaceID, projectID, uid, entry string) error {
	t.Helper()
	version, err := f.ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	tx, err := f.ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	if err := f.db.admitOrRestoreMembersTx(tx, groupNo, spaceID, projectID,
		[]MemberAdmission{{UID: uid, Version: version, Role: MemberRoleCommon, InviteUID: "op"}},
		entry); err != nil {
		return err
	}
	return tx.Commit()
}

// TestProjectGroupRefusesANonProjectMember is the core case: a uid who holds a
// Space seat but no project seat must not end up in a project group.
//
// Driven through the funnel with each entry-point label in turn. The acceptance
// requires every label to be emitted at least once by the suite, because a label
// that is never emitted is a path that is not enforcing — and the label is the
// only way to tell "this path refuses correctly" apart from "this path stopped
// calling the gate".
func TestProjectGroupRefusesANonProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "outsider")
	seedProject(t, ctx, projectID, spaceID)

	entries := []string{
		AdmissionEntryInviteConfirm,
		AdmissionEntryAddMembers,
		AdmissionEntryCreateGroup,
		AdmissionEntryCreateGroupBot,
		AdmissionEntryScanJoin,
		AdmissionEntryRegisterUser,
		AdmissionEntryOrgCreate,
		AdmissionEntryOrgEmployeeUpdate,
		AdmissionEntryPresetGroups,
		AdmissionEntryUnblacklist,
	}
	for _, entry := range entries {
		t.Run(entry, func(t *testing.T) {
			groupNo := util.GenerUUID()
			seedGroupRow(t, ctx, groupNo, spaceID, projectID)

			err := admitOne(t, f, groupNo, spaceID, projectID, "outsider", entry)
			require.Error(t, err, "a non-project-member must be refused")
			require.ErrorIs(t, err, ErrAdmissionRefused)
			require.False(t, activeMemberExists(t, ctx, groupNo, "outsider"),
				"refusal must mean NO row: there is no read-path filter behind I2")
		})
	}
}

// TestProjectGroupAdmitsAnActualProjectMember is the negative control. A gate
// that refuses everyone passes the case above and breaks the product.
func TestProjectGroupAdmitsAnActualProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "insider")
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, "insider", 0)

	groupNo := util.GenerUUID()
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	require.NoError(t, admitOne(t, f, groupNo, spaceID, projectID, "insider", AdmissionEntryAddMembers))
	require.True(t, activeMemberExists(t, ctx, groupNo, "insider"))
}

// TestGateAppliesToTheRestoreBranchToo.
//
// group_member rows are soft-deleted and carry a unique index on
// (group_no, uid), so re-joining is an UPDATE. A gate installed only in the
// insert primitive would cover first joins and miss every rejoin — and a rejoin
// is exactly how a removed member comes back.
func TestGateAppliesToTheRestoreBranchToo(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "rejoiner")
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, "rejoiner", 0)

	groupNo := util.GenerUUID()
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	// In, then out — leaving a soft-deleted row for the restore branch to find.
	require.NoError(t, admitOne(t, f, groupNo, spaceID, projectID, "rejoiner", AdmissionEntryAddMembers))
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?", groupNo, "rejoiner").Exec()
	require.NoError(t, err)

	// Now revoke the project seat and try to come back.
	_, err = ctx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET status=0 WHERE project_id=? AND uid=?",
		projectID, "rejoiner").Exec()
	require.NoError(t, err)

	err = admitOne(t, f, groupNo, spaceID, projectID, "rejoiner", AdmissionEntryAddMembers)
	require.ErrorIs(t, err, ErrAdmissionRefused, "the restore branch must be gated, not only the insert branch")
	require.False(t, activeMemberExists(t, ctx, groupNo, "rejoiner"))
}

// TestSeatBeingClosedIsNotAMember covers the `removing` half of D4: a seat with
// removing = 1 is still status = 1, and must NOT admit.
//
// This is what makes the two-phase removal safe. If the gate read only status,
// a member whose removal is in flight could be added to another of the project's
// groups while the cascade was tearing down the first one.
func TestSeatBeingClosedIsNotAMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "closing")
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, "closing", 1) // removing = 1

	groupNo := util.GenerUUID()
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	err := admitOne(t, f, groupNo, spaceID, projectID, "closing", AdmissionEntryAddMembers)
	require.ErrorIs(t, err, ErrAdmissionRefused,
		"a seat with removing=1 is not a member, even though status is still 1")
}

// TestCompositePredicateRefusesALostSpaceSeat covers P0's tolerated window.
//
// The Space→Project cascade is asynchronous: between a Space removal committing
// and that cascade running, octo_project_member still holds an ACTIVE row for a
// uid with no Space seat. A gate that asked only "is this an active project
// member?" would admit them for the whole of that window. The conjunction makes
// the window cost nothing, because the Space half fails first.
func TestCompositePredicateRefusesALostSpaceSeat(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "exspace")
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, "exspace", 0)

	// The Space seat goes away; the project seat has NOT been cascaded yet.
	_, err := ctx.DB().UpdateBySql(
		"UPDATE space_member SET status=0 WHERE space_id=? AND uid=?", spaceID, "exspace").Exec()
	require.NoError(t, err)

	groupNo := util.GenerUUID()
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	err = admitOne(t, f, groupNo, spaceID, projectID, "exspace", AdmissionEntryAddMembers)
	require.ErrorIs(t, err, ErrAdmissionRefused,
		"an active project seat must not admit someone who has lost their Space seat")
}

// TestSystemBotsAreExemptOrdinaryBotsAreNot.
//
// The platform adds its own bots to groups and there is nobody to grant them a
// project seat. An ordinary bot is different: someone invited it, and exempting
// it would make "invite a bot" a way to put a listener inside a project group
// without anyone granting it access.
func TestSystemBotsAreExemptOrdinaryBotsAreNot(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedProject(t, ctx, projectID, spaceID)

	groupNo := util.GenerUUID()
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	systemBot := "botfather"
	require.NoError(t, admitOne(t, f, groupNo, spaceID, projectID, systemBot, AdmissionEntryCreateGroupBot),
		"a whitelisted system bot must be admitted without a project seat")
	require.True(t, activeMemberExists(t, ctx, groupNo, systemBot))

	ordinaryBot := "bot_" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, ordinaryBot)
	err := admitOne(t, f, groupNo, spaceID, projectID, ordinaryBot, AdmissionEntryCreateGroupBot)
	require.ErrorIs(t, err, ErrAdmissionRefused, "an ordinary bot needs an explicit project seat")
}

// TestSpaceDirectGroupsAreUnaffected is the non-regression half (C1 and the
// "existing groups are unaffected" acceptance): with project_id at the empty
// sentinel, the gate short-circuits and admission behaves exactly as before.
func TestSpaceDirectGroupsAreUnaffected(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, "anyone")

	groupNo := util.GenerUUID()
	seedGroupRow(t, ctx, groupNo, spaceID, "") // Space-direct

	require.NoError(t, admitOne(t, f, groupNo, spaceID, "", "anyone", AdmissionEntryAddMembers),
		"a Space-direct group must admit exactly as it did before P1")
	require.True(t, activeMemberExists(t, ctx, groupNo, "anyone"))
}

// TestProjectIDColumnIsNotNull pins the schema decision (D2). Every predicate in
// this feature is written project_id <> ” or = ”, and a three-valued column
// turns each of them into a bug waiting for the first NULL row: `NULL <> ”` is
// NULL, not TRUE, so a NULL-attributed group would fall out of the I2 scan while
// still being a project group.
func TestProjectIDColumnIsNotNull(t *testing.T) {
	_, ctx := newTestServer(t)

	var nullable string
	rows, err := ctx.DB().SelectBySql(
		"SELECT IS_NULLABLE FROM information_schema.COLUMNS " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group' AND COLUMN_NAME = 'project_id'",
	).Load(&nullable)
	require.NoError(t, err)
	require.Equal(t, 1, rows, "group.project_id must exist")
	require.Equal(t, "NO", nullable, "group.project_id must be NOT NULL")

	// And an INSERT that omits it must get the sentinel, not NULL.
	groupNo := util.GenerUUID()
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`) VALUES (?, 'g', '', 1, 1)",
		groupNo).Exec()
	require.NoError(t, err)
	var got string
	err = ctx.DB().SelectBySql("SELECT project_id FROM `group` WHERE group_no=?", groupNo).LoadOne(&got)
	require.NoError(t, err)
	require.Equal(t, "", got, "omitting project_id must default to the empty sentinel")
}
