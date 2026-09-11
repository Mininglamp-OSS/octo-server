package workspace

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/require"
)

func TestSpaceMemberRemovalCleanupIsRegistered(t *testing.T) {
	require.Contains(t, space.MemberRemovalCleanupStepNames(), spaceMemberRemovalStepName)
}

func TestSpaceMemberRemovalDeactivatesWorkspaceMembership(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		owner   = "ws-cascade-owner"
		admin   = "ws-cascade-admin"
		spaceID = "ws-cascade-space"
	)
	require.NoError(t, user.NewDB(ctx).Insert(&user.Model{UID: owner, Name: "Cascade owner", ShortNo: "cascade-owner", Status: 1}))
	require.NoError(t, user.NewDB(ctx).Insert(&user.Model{UID: admin, Name: "Cascade admin", ShortNo: "cascade-admin", Status: 1}))
	_, err := ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Cascade space", owner, 1).Exec()
	require.NoError(t, err)
	for _, uid := range []string{owner, admin} {
		_, err = ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
			Values(spaceID, uid, 0, 1).Exec()
		require.NoError(t, err)
	}

	svc := NewService(ctx)
	ws, err := svc.Create(Scope{UID: owner}, CreateRequest{SpaceID: spaceID, Name: "Cascade"})
	require.NoError(t, err)
	_, err = svc.AddMembers(Scope{UID: owner}, ws.WorkspaceID, []MemberInput{{
		UID: admin, WorkspaceRole: WorkspaceRoleAdmin,
	}})
	require.NoError(t, err)
	_, err = ctx.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", spaceID, admin).Exec()
	require.NoError(t, err)

	require.NoError(t, cleanupSpaceMemberWorkspaces(ctx, space.MemberRemoval{
		SpaceID: spaceID,
		UID:     admin,
		Reason:  space.MemberRemoveReasonKicked,
	}))
	require.NoError(t, cleanupSpaceMemberWorkspaces(ctx, space.MemberRemoval{
		SpaceID: spaceID,
		UID:     admin,
		Reason:  space.MemberRemoveReasonKicked,
	}))

	var status int
	require.NoError(t, ctx.DB().Select("status").From("octo_workspace_member").
		Where("workspace_id=? AND uid=?", ws.WorkspaceID, admin).LoadOne(&status))
	require.Equal(t, MemberStatusInactive, status)

	_, err = ctx.DB().Update("space_member").Set("status", 1).
		Where("space_id=? AND uid=?", spaceID, admin).Exec()
	require.NoError(t, err)
	_, err = svc.Get(Scope{UID: admin}, ws.WorkspaceID)
	require.ErrorIs(t, err, ErrForbidden)
}
