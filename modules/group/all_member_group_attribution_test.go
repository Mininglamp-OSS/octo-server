package group

// P2-3：两个 hook 的写要以**权威快照**判断群归属，而不是项目侧那次事务外的读。
//
// admitToAllMemberGroup 早就这么做了（Q13：事务内重读群行，闸门用重读那一份）。
// 另外两个没有：renameAllMemberGroup 只看 status，然后按 group_no 改名；
// ensureAllMemberGroupOwner 按 group_no 读 group_member，从头到尾没问过归属。
// 项目侧的 queryAllMemberGroupNo 确实两半都验，但它是一次独立的无锁读——这中间
// P1 的 detach 可以把群变回 Space 直属，于是一次项目改名、或一次群主变更，会落在
// 一个已经不属于该项目的群上。改群主尤其重，那是在改"谁控制这个群"。

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOwnerSyncRefusesAGroupThatIsNoLongerTheProjects pins the attribution re-read.
//
// The setup is the post-detach state: the project still points at the group (P1's
// detach lives in modules/group and cannot write octo_project), but the group's own
// project_id has been cleared. That is the snapshot the project side's unlocked read
// can hand over, and acting on it changes the ownership of a group that now belongs
// to the Space, not to this project.
func TestOwnerSyncRefusesAGroupThatIsNoLongerTheProjects(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_attr", "s_attr", "grp_attr"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_stale")
	// The detach: group.project_id goes back to Space-direct, the project's pointer
	// does not (nothing in modules/group may write octo_project).
	_, err := ctx.DB().UpdateBySql(
		"UPDATE `group` SET project_id = '' WHERE group_no = ?", groupNo).Exec()
	require.NoError(t, err)

	// A state the sync would otherwise repair: a sitting creator who is not a project
	// owner, and an active project owner sitting in the group. Without the
	// attribution check the sync promotes u_owner and demotes u_stale.
	seedGroupMemberRole(t, ctx, groupNo, "u_stale", MemberRoleCreator, 10)
	seedGroupMemberRole(t, ctx, groupNo, "u_owner", MemberRoleCommon, 5)

	require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo),
		"a detached group is nothing to do, not an error — retrying would not change it")

	assert.Equal(t, []string{"u_stale"}, creatorsOf(t, ctx, groupNo),
		"the sync must not touch a group that is no longer this project's. It reached here "+
			"through the project side's UNLOCKED pointer read, and a detach landing in that "+
			"window is exactly the case the in-transaction re-read exists for — changing the "+
			"creator here changes who controls a group that now belongs to the Space")
}

// TestOwnerSyncStillActsOnTheProjectsOwnGroup is the other half: with the group
// still attributed to the project, the same setup converges.
//
// Without this the test above passes just as well against a sync that does nothing
// at all — which is the vacuity this PR has been caught on twice.
func TestOwnerSyncStillActsOnTheProjectsOwnGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_attr2", "s_attr2", "grp_attr2"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_stale")
	seedGroupMemberRole(t, ctx, groupNo, "u_stale", MemberRoleCreator, 10)
	seedGroupMemberRole(t, ctx, groupNo, "u_owner", MemberRoleCommon, 5)

	require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo))

	assert.Equal(t, []string{"u_owner"}, creatorsOf(t, ctx, groupNo),
		"same setup, attribution intact: the sync must converge")
}

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
