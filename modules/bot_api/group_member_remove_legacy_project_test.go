package bot_api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBotGroupMemberRemove_LegacyProjectGroupFollowsOrdinaryRules is the Bot
// counterpart of the Group-side test: a historical Project-linked group is an
// ordinary native group, so a bot that holds the group's own bot_admin role
// removes a common member through the real Bot route, and the group keeps its
// Project relation
// (docs/specs/2026-09-10-project-prd-alignment-design.md「Project 关联群的统一语义」).
func TestBotGroupMemberRemove_LegacyProjectGroupFollowsOrdinaryRules(t *testing.T) {
	handler, ctx := setupRemoveGuardEnv(t)

	// Historical shape: the group still carries the Project relation. The
	// assertions below describe ordinary bot-API behaviour and must keep
	// passing with the group linked to a Project.
	const projectID = "p_rm_guard_legacy_project"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project "+
			"(project_id, space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, '', ?, ?, 1, NOW(3), NOW(3))",
		projectID, projectID, rmGuardCreator,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("group").Set("project_id", projectID).
		Where("group_no=?", rmGuardGroupNo).Exec()
	require.NoError(t, err)

	w := doBot(handler, botReq(t, "POST",
		"/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardCommon}}))

	require.Equalf(t, http.StatusOK, w.Code,
		"a bot with the native admin role must remove a common member of a historical Project group, body: %s",
		w.Body.String())

	resp := decodeBody(t, w)
	require.Equal(t, true, resp["ok"])
	require.Equal(t, float64(1), resp["removed"])
	require.False(t, rmGuardIsActiveMember(t, handler, rmGuardCommon),
		"the removal must reach the native member row")

	var linkedProject string
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT project_id FROM `group` WHERE group_no = ?", rmGuardGroupNo,
	).LoadOne(&linkedProject))
	require.Equal(t, projectID, linkedProject,
		"a native bot removal must not change the group's Project relation")
}
