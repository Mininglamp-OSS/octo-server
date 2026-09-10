package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

type noopGroupEvent struct{}

func (noopGroupEvent) Begin(*wkevent.Data, *dbr.Tx) (int64, error) {
	return 1, nil
}

func (noopGroupEvent) Commit(int64) {}

// Native group admission is independent from Project membership.
//
// Project-linked groups still carry a relation used by Project resource
// authorization, but native group member rows are governed by the Group's own
// role, bot, user, Space, and allow_external policies. These fixtures exercise
// that boundary against a real MySQL 8.0 database.

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

// admitOne drives the native admission primitive directly in its own
// transaction. projectID and entry are retained in this test helper's call
// shape for fixtures that describe the linked group; the production primitive
// intentionally does not inspect either value.
func admitOne(t *testing.T, f *Group, groupNo, spaceID, projectID, uid, entry string) error {
	t.Helper()
	version, err := f.ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	tx, err := f.ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	if err := f.db.admitOrRestoreMembersTx(tx, groupNo,
		[]MemberAdmission{{UID: uid, Version: version, Role: MemberRoleCommon, InviteUID: "op"}}); err != nil {
		return err
	}
	return tx.Commit()
}

// seedGroupMemberRow adds an active native group member for manager-policy
// fixtures that do not need the full Service create flow.
func seedGroupMemberRow(t *testing.T, ctx *config.Context, groupNo, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, `version`, status, vercode, is_deleted, "+
			"invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
			"VALUES (?, ?, ?, 1, 1, ?, 0, '', 0, 0, 0, '', NOW())",
		groupNo, uid, role, util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

// TestProjectLinkedGroupNativeInvitationAdmitsNonProjectMember verifies the
// important separation directly through the existing invitation transaction.
// The target has a valid native Space/user context but no Project seat; the
// linked group's native manager may still invite it.
func TestProjectLinkedGroupNativeInvitationAdmitsNonProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	ctx.Event = noopGroupEvent{}
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo := util.GenerUUID()
	operator := "native-operator-" + util.GenerUUID()[:8]
	target := "native-target-" + util.GenerUUID()[:8]
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: operator, Name: "Native operator", ShortNo: operator,
	}))
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: target, Name: "Native target", ShortNo: target,
	}))
	seedSpaceSeat(t, ctx, spaceID, operator)
	seedSpaceSeat(t, ctx, spaceID, target)
	seedProject(t, ctx, projectID, spaceID)
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	seedGroupMemberRow(t, ctx, groupNo, operator, MemberRoleCreator)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	commitCallback, err := f.addMembersTxWithSpace(
		[]string{target}, groupNo, operator, "Native operator", "", tx,
	)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	if commitCallback != nil {
		commitCallback()
	}
	require.True(t, activeMemberExists(t, ctx, groupNo, target),
		"a Project seat is not required for native invitation into a linked group")
}

// TestProjectIDColumnIsNotNull pins the relation sentinel used by Project
// resource queries. Every group row uses an empty string for no Project and a
// concrete id for a linked Project; NULL would make that distinction ambiguous.
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
