package project

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetProjectMemberMatchesRosterAndFindsLaterPage(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv, "member-admin", "member-later")

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/member-admin/role", ownerToken,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	frontend := roleByName(t, catalog.Roles, "前端")
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/member-admin/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{frontend.RoleID}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The Owner sorts first and the promoted admin sorts second. With a one-row
	// page the target is therefore outside page one; a direct lookup must not
	// implement the tempting but incorrect "list then find" fallback.
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members?limit=1&page=1", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var firstPage []MemberResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &firstPage), "body: %s", w.Body.String())
	require.Len(t, firstPage, 1)
	assert.NotEqual(t, "member-admin", firstPage[0].UID)

	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members?limit=1&page=2", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var laterPage []MemberResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &laterPage), "body: %s", w.Body.String())
	require.Len(t, laterPage, 1)
	require.Equal(t, "member-admin", laterPage[0].UID)

	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/member-admin", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var direct MemberResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &direct), "body: %s", w.Body.String())
	assert.Equal(t, laterPage[0], direct,
		"single-member projection must be the same as the roster projection")
	assert.Equal(t, RoleAdmin, direct.Role)
	require.Len(t, direct.CollaborationRoles, 1)
	assert.Equal(t, frontend.RoleID, direct.CollaborationRoles[0].RoleID)
}

func TestGetProjectMemberProjectsAgentFieldsAndRole(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)
	seedAgent(t, spaceA, "agent-detail", "owner1", "octo_hosted")

	w := doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("agent-detail"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/agent-detail", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var direct MemberResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &direct), "body: %s", w.Body.String())
	assert.Equal(t, "agent-detail", direct.UID)
	assert.Equal(t, RoleCommon, direct.Role)
	assert.Equal(t, 1, direct.Robot)
	assert.Equal(t, "owner1", direct.OwnerUID)
	assert.NotNil(t, direct.CollaborationRoles)
	assert.Empty(t, direct.CollaborationRoles,
		"collaboration labels do not apply to agent seats")
}

func TestGetProjectMemberRejectsUnauthorizedCallersAndTargets(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, tokens, created := projectWithMembers(t, srv, "removed-target")

	// A Space member who is not a member of this Project cannot use a valid
	// target UID to turn the endpoint into a Project-membership oracle.
	strangerToken := seedUser(t, "project-stranger")
	seedSpaceMember(t, spaceA, "project-stranger", 0, 1)
	w := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/owner1", strangerToken, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	// A caller from another organization is denied by the resource's derived
	// Space check, even though the target is a valid member of this Project.
	seedSpace(t, spaceB, 1)
	foreignToken := seedUser(t, "foreign-caller")
	seedSpaceMember(t, spaceB, "foreign-caller", 0, 1)
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/owner1", foreignToken, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	// Unknown and removed targets have the same existing not-found semantics;
	// neither a DB miss nor an inactive seat is success with a zero-value DTO.
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/does-not-exist", ownerToken, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{"removed-target"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/removed-target", ownerToken, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	// Keep the target member's own session available to make the fixture's
	// intended distinction explicit: it is the removed target, not a caller
	// authentication failure, that is rejected here.
	assert.NotEmpty(t, tokens["removed-target"])
}
