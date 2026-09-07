package group

// A group row that cannot be read is not "a group with no project".
//
// PR #846's review found addMembersTxWithSpace passing empty space_id/project_id
// to the admission funnel whenever QueryWithGroupNo returned no row. Empty
// project_id is the funnel's assertion "this is not a project group", so the
// whole batch was admitted without an I2 check — a fail-OPEN whose trigger is a
// failed read, which is exactly the shortcut preset_group_admission.go refuses in
// its own doc comment.
//
// The path is reachable without corrupting anything: the operator's membership
// is checked against group_member, the attribution against `group`. A group
// disbanded (row deleted) between an invite being sent and confirmed leaves the
// first true and the second empty.

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

func seedBareMemberRow(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, remark, role, `version`, status, vercode, "+
			"is_deleted, invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
			"VALUES (?, ?, '', 1, 1, 1, ?, 0, '', 0, 0, 0, '', NOW())",
		groupNo, uid, util.GenerUUID()).Exec()
	require.NoError(t, err)
}

func TestAddMembersRefusesWhenTheGroupRowIsGone(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	// group_member says the operator is in it; `group` has no row at all.
	groupNo := util.GenerUUID()
	seedBareMemberRow(t, ctx, groupNo, "op_missing_group")
	// A real invitee, so the batch is refused by the missing-group check rather
	// than falling out earlier on "this user does not exist".
	for _, uid := range []string{"op_missing_group", "invitee_missing_group"} {
		_, err := ctx.DB().Exec(
			"INSERT INTO `user` (uid, name, short_no, created_at, updated_at) "+
				"VALUES (?, ?, ?, NOW(), NOW())",
			uid, uid, util.GenerUUID()[:12])
		require.NoError(t, err)
	}

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()

	_, err = f.addMembersTx([]string{"invitee_missing_group"}, groupNo,
		"op_missing_group", "op", tx)
	require.Error(t, err,
		"with no group row there is no attribution to check the admission against; "+
			"continuing admits the batch under an empty project_id, which the funnel "+
			"reads as 'not a project group'")

	require.False(t, activeMemberExists(t, ctx, groupNo, "invitee_missing_group"),
		"nothing may be written on the way to that error")
}
