package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenameRefusesAGroupThatIsNoLongerTheProjects pins the rename half.
//
// The rename cannot use an in-transaction re-read, because the transaction belongs
// to UpdateGroupInfo. It uses a FENCE instead — project_id in the UPDATE's own WHERE
// — which is strictly stronger: read and write are one statement, so there is no
// window at all rather than a narrowed one.
func TestRenameRefusesAGroupThatIsNoLongerTheProjects(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_ren", "s_ren", "grp_ren"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_owner")
	_, err := ctx.DB().UpdateBySql(
		"UPDATE `group` SET project_id = ? WHERE group_no = ?", "p_somebody_else", groupNo).Exec()
	require.NoError(t, err)

	require.NoError(t, g.renameAllMemberGroup(ctx, projectID, groupNo, "renamed-by-the-wrong-project"))

	var name string
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT name FROM `group` WHERE group_no=?", groupNo).LoadOne(&name))
	assert.Equal(t, "all-"+projectID, name,
		"a project must not rename a group that is no longer its own — and the rename also "+
			"pushes a version bump and a group-info change to every member, so the write is "+
			"not the only observable effect")
}

// TestRenameFenceIsWhatStopsTheWrite drives the fence directly, so the guard is
// pinned at the layer that actually enforces it.
//
// The early return in renameAllMemberGroup reads the group outside any transaction;
// deleting it leaves the fence, and the rename is still refused. Deleting the FENCE
// is what this case catches — and the previous case would not, because its detach
// happens before the call rather than inside the window.
func TestRenameFenceIsWhatStopsTheWrite(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_fence", "s_fence", "grp_fence"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_owner")

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()

	wrong := "not-this-project"
	name := "fenced-out"
	affected, err := g.db.UpdateNameNoticeTx(groupNo, &name, nil, 7, wrong, tx)
	require.NoError(t, err)
	assert.Zero(t, affected,
		"the fence must reject a write aimed at a group that belongs to another project")

	affected, err = g.db.UpdateNameNoticeTx(groupNo, &name, nil, 8, projectID, tx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, affected,
		"and must let the project's own group through, or the fence would be a permanent "+
			"off switch on D8's rename")
	require.NoError(t, tx.Commit())
}
