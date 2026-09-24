package project_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func TestProjectMembershipOwnerAndNameDoNotMutateLinkedNativeGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)
	t.Cleanup(func() { require.NoError(t, testutil.CleanAllTables(ctx)) })

	const (
		spaceID   = "project_native_independence_space"
		projectID = "project_native_independence_project"
		groupNo   = "project_native_independence_group"
		owner     = "project_native_independence_owner"
		member    = "project_native_independence_member"
		successor = "project_native_independence_successor"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)", spaceID, spaceID, owner)
	for _, uid := range []string{owner, member, successor} {
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no, status) VALUES (?, ?, ?, 1)", uid, uid, uid)
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
	}
	exec(t, ctx, "INSERT INTO `octo_project` (project_id, space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, NOW(), NOW())", projectID, spaceID, "Project before rename", owner)
	for _, seat := range []struct {
		uid  string
		role int
	}{{owner, 2}, {member, 0}, {successor, 0}} {
		exec(t, ctx, "INSERT INTO octo_project_member (project_id, uid, space_id, role, status, removing, invite_uid, created_at, joined_at, updated_at) VALUES (?, ?, ?, ?, 1, 0, ?, NOW(), NOW(), NOW())", projectID, seat.uid, spaceID, seat.role, owner)
	}
	exec(t, ctx, "INSERT INTO `group` (group_no, name, creator, status, version, space_id, project_id) VALUES (?, ?, ?, 1, 1, ?, ?)", groupNo, "Native group", owner, spaceID, projectID)
	exec(t, ctx, "INSERT INTO group_member (group_no, uid, role, is_deleted, status, version) VALUES (?, ?, 0, 0, 1, 1)", groupNo, member)

	token := seedToken(t, ctx, owner)
	w := writeProjectE2E(t, srv, http.MethodPut, "/v1/projects/"+projectID, token, map[string]any{"name": "Project after rename"})
	require.Equal(t, http.StatusOK, w.Code, "rename: %s", w.Body.String())
	w = writeProjectE2E(t, srv, http.MethodPut, "/v1/projects/"+projectID+"/owner", token, map[string]any{"uid": successor})
	require.Equal(t, http.StatusOK, w.Code, "Owner transfer: %s", w.Body.String())
	var ownerUID string
	require.NoError(t, ctx.DB().SelectBySql("SELECT uid FROM octo_project_member WHERE project_id = ? AND status = 1 AND role = 2", projectID).LoadOne(&ownerUID))
	require.Equal(t, successor, ownerUID)
	w = writeProjectE2E(t, srv, http.MethodPost, "/v1/projects/"+projectID+"/members/remove", token, map[string]any{"uids": []string{member}})
	require.Equal(t, http.StatusOK, w.Code, "member removal: %s", w.Body.String())
	require.Eventually(t, func() bool {
		status, removing, found := projectSeatStateE2E(ctx, projectID, member)
		return found && status == 0 && removing == 0
	}, 15*time.Second, 100*time.Millisecond, "Project removal worker must close the member seat")

	require.True(t, activeGroupMemberE2E(ctx, groupNo, member), "Project member removal must preserve the native group seat")
	var groups []struct {
		Name      string `db:"name"`
		Creator   string `db:"creator"`
		ProjectID string `db:"project_id"`
	}
	_, err := ctx.DB().SelectBySql("SELECT name, creator, project_id FROM `group` WHERE group_no = ?", groupNo).Load(&groups)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "Native group", groups[0].Name)
	require.Equal(t, owner, groups[0].Creator)
	require.Equal(t, projectID, groups[0].ProjectID)
}
