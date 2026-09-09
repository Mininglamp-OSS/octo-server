package project

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/stretchr/testify/require"
)

func TestDigitalAvatarRequiresProjectAdminAndProjectSeat(t *testing.T) {
	srv, p := setup(t)
	spaceID := "avatar_project_space"
	owner, admin, ordinary := "avatar_project_owner", "avatar_project_admin", "avatar_project_member"
	avatarID := "avatar_project_" + util.GenerUUID()[:8]
	seedSpace(t, spaceID, 1)
	ownerToken := seedUser(t, owner)
	seedUser(t, admin)
	seedUser(t, ordinary)
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,username,short_no,status,robot,is_destroy) VALUES (?,?,?,?,1,1,0)",
		avatarID, "Digital employee", avatarID, util.GenerUUID()[:8]).Exec()
	require.NoError(t, err)
	for _, uid := range []string{owner, admin, ordinary, avatarID} {
		seedSpaceMember(t, spaceID, uid, 0, 1)
	}
	_, err = testCtx.DB().InsertBySql(`INSERT INTO robot
		(robot_id,status,creator_uid,auto_approve,kind,management_scope,management_space_id,
		 created_by,publication_state,lifecycle_pending)
		VALUES (?,1,'',1,'avatar','space',?,'manager','published',0)`, avatarID, spaceID).Exec()
	require.NoError(t, err)

	created := createProjectVia(t, srv, spaceID, ownerToken, "Avatar project")
	_, err = p.addOneMember(created.ProjectID, spaceID, owner, admin)
	require.NoError(t, err)
	_, err = p.addOneMember(created.ProjectID, spaceID, owner, ordinary)
	require.NoError(t, err)
	_, err = testCtx.DB().Update("octo_project_member").Set("role", RoleAdmin).
		Where("project_id=? AND uid=?", created.ProjectID, admin).Exec()
	require.NoError(t, err)

	results, err := p.addMembers(created.ProjectID, spaceID, admin, []string{avatarID})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	require.True(t, results[0].Admitted)
	var projectGroup string
	require.NoError(t, testCtx.DB().Select("group_no").From("`group`").
		Where("project_id=? AND status=1", created.ProjectID).Limit(1).LoadOne(&projectGroup))
	allowed, err := botpolicy.CanAccessChannel(testCtx.DB(), avatarID, projectGroup, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.False(t, allowed, "a project seat does not put an Avatar in a project group")

	// The administrator who added the Avatar is not its owner. Removing that
	// administrator must not cascade the organization identity out of the project.
	_, err = p.removeMember(created.ProjectID, spaceID, owner, admin)
	require.NoError(t, err)
	var active int
	require.NoError(t, testCtx.DB().SelectBySql(`SELECT COUNT(*) FROM octo_project_member
		WHERE project_id=? AND uid=? AND status=1 AND removing=0`, created.ProjectID, avatarID).LoadOne(&active))
	require.Equal(t, 1, active)

	removed, err := p.removeMember(created.ProjectID, spaceID, owner, avatarID)
	require.NoError(t, err)
	require.True(t, removed)
	allowed, err = botpolicy.CanAccessChannel(testCtx.DB(), avatarID, projectGroup, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.False(t, allowed, "removing=1 closes the project-group gate immediately")

	_, err = p.addOneMember(created.ProjectID, spaceID, ordinary, avatarID)
	require.ErrorIs(t, err, errPermissionDenied)
}
