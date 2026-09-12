package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/require"
)

func TestDedicatedAdmissionFailureDoesNotSpawnRetryJobs(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)
	stub := newGroupIMStub(t, ctx)
	suffix := util.GenerUUID()[:8]
	spaceID, projectID, groupNo, uid := "retry-space-"+suffix, "retry-project-"+suffix, "retry-group-"+suffix, "retry-user-"+suffix
	seedSpaceSeat(t, ctx, spaceID, uid)
	seedProjectForGroupTest(t, ctx, projectID, spaceID, uid)
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, uid)
	_, err := ctx.DB().InsertBySql("INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)", uid, uid, uid).Exec()
	require.NoError(t, err)
	// Admission is also invoked by the durable projection worker. Its failures
	// must stay with that caller instead of recursively creating more jobs.
	for range 3 {
		stub.failNextSubscriberAdd()
		require.ErrorIs(t, g.admitToAllMemberGroup(ctx, spaceID, groupNo, uid), projectpkg.ErrAdmittedButNotSubscribed)
	}
	var pending int
	_, err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM space_member_removal_cleanup WHERE space_id=? AND uid=?", spaceID, uid).Load(&pending)
	require.NoError(t, err)
	require.Zero(t, pending, "repeated failures must not spawn a second retry lifecycle")
	require.True(t, activeMemberExists(t, ctx, groupNo, uid))
}

func TestDedicatedOwnerProjectionSurvivesMixedCollation(t *testing.T) {
	_, base := newTestServer(t)
	defer testutil.CleanAllTables(base)
	ctx := newAllMemberGuardCollationContext(t, base)
	var source string
	require.NoError(t, base.DB().SelectBySql("SELECT DATABASE()").LoadOne(&source))
	for _, table := range []string{"seq", "space", "space_member", "user", "octo_project_member", "group_member"} {
		_, err := ctx.DB().Exec("CREATE TABLE `" + table + "` LIKE `" + source + "`.`" + table + "`")
		require.NoError(t, err)
	}
	for _, table := range []string{"space", "space_member", "user"} {
		_, err := ctx.DB().Exec("ALTER TABLE `" + table + "` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		require.NoError(t, err)
	}
	g := New(ctx)
	newGroupIMStub(t, ctx)
	const spaceID, projectID, groupNo, uid = "mixed-owner-space", "mixed-owner-project", "mixed-owner-group", "mixed-owner"
	seedSpaceSeat(t, ctx, spaceID, uid)
	seedProjectForGroupTest(t, ctx, projectID, spaceID, uid)
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, uid)
	seedGroupMemberRow(t, ctx, groupNo, uid, MemberRoleCommon)
	_, err := ctx.DB().Exec("INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)", uid, uid, uid)
	require.NoError(t, err)
	require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo))
	var role int
	require.NoError(t, ctx.DB().SelectBySql("SELECT role FROM group_member WHERE group_no=? AND uid=?", groupNo, uid).LoadOne(&role))
	require.Equal(t, MemberRoleCreator, role)
}
