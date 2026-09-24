package project

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateProjectDoesNotAutoCreateLinkedGroup checks the Project API,
// persisted Owner seat and associated-group list after explicit creation.
func TestCreateProjectDoesNotAutoCreateLinkedGroup(t *testing.T) {
	srv, _ := setup(t)

	const (
		spaceID = "no_auto_group_space"
		creator = "no_auto_group_creator"
	)
	seedSpace(t, spaceID, 1)
	seedSpaceMember(t, spaceID, creator, 2, 1)
	token := seedUser(t, creator)

	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceID+"/projects",
		token, map[string]any{"name": "显式创建不附带群"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Assert against the actual JSON response, not a typed Go response.
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	projectID, _ := resp["project_id"].(string)
	require.NotEmpty(t, projectID, "create must still return the new Project")
	assert.NotContains(t, resp, "all_member_group_no",
		"the create response must not carry a dedicated group pointer")

	// Creation must leave the Project's associated-group list empty.
	var linked int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `group` WHERE project_id = ?", projectID).LoadOne(&linked))
	assert.Zero(t, linked, "creating a Project must not create or link any group")

	// The relation list clients read must reflect the same fact.
	lw := doJSON(t, srv, http.MethodGet, "/v1/projects/"+projectID+"/groups", token, nil)
	require.Equal(t, http.StatusOK, lw.Code, "body: %s", lw.Body.String())
	assert.Equal(t, "0", lw.Header().Get("X-Total-Count"),
		"a brand-new Project has no linked group to count")
	assert.JSONEq(t, "[]", lw.Body.String())

	// The Project and its Owner seat remain active.
	var projects []struct {
		Status int `db:"status"`
	}
	_, err := testCtx.DB().SelectBySql(
		"SELECT status FROM `octo_project` WHERE project_id = ?", projectID).Load(&projects)
	require.NoError(t, err)
	require.Len(t, projects, 1, "the Project row must exist")
	assert.Equal(t, StatusNormal, projects[0].Status)

	var seats []struct {
		Status   int `db:"status"`
		Removing int `db:"removing"`
		Role     int `db:"role"`
	}
	_, err = testCtx.DB().SelectBySql(
		"SELECT status, removing, role FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ?", projectID, creator).Load(&seats)
	require.NoError(t, err)
	require.Len(t, seats, 1, "the creator's Project seat must exist")
	assert.Equal(t, MemberStatusActive, seats[0].Status)
	assert.Zero(t, seats[0].Removing)
	assert.Equal(t, RoleOwner, seats[0].Role)
}

// A new instance must coexist with old instances until they are drained.
// Module startup must not remove columns that old readers and writers still use.
func TestProjectStartupPreservesLegacyColumnsDuringRollout(t *testing.T) {
	setup(t)

	var rows []struct {
		GroupNo string `db:"all_member_group_no"`
	}
	_, err := testCtx.DB().SelectBySql(
		"SELECT `all_member_group_no` FROM `octo_project` LIMIT 1").Load(&rows)
	require.NoError(t, err, "an old Project reader must work while the new instance is running")

	_, err = testCtx.DB().Exec(
		"UPDATE `octo_project` SET `all_member_group_lease_until` = NULL WHERE `project_id` = ?",
		"rollout-no-such-project")
	require.NoError(t, err, "an old Project writer must work while the new instance is running")
}
