package project

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time RED: the production wire type does not exist before this task.
// Once it does, the HTTP cases below remain the behavioural contract.
var _ CollaborationRoleResp

func TestCollaborationRoleEpochOnlyEverIncrements(t *testing.T) {
	found := false
	assignment := regexp.MustCompile(`\bcollaboration_role_epoch\b\s*=`)
	increment := regexp.MustCompile(`\bcollaboration_role_epoch\b\s*=\s*\bcollaboration_role_epoch\b\s*\+\s*1`)
	for _, file := range moduleSourceFiles(t) {
		cleaned := readStripped(t, file)
		for _, match := range assignment.FindAllStringIndex(cleaned, -1) {
			window := cleaned[match[0]:min(match[1]+90, len(cleaned))]
			if !increment.MatchString(window) {
				t.Errorf("modules/project/%s assigns collaboration_role_epoch non-monotonically: %q",
					file, strings.TrimSpace(window))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no collaboration_role_epoch increment statement found")
	}
}

type collaborationRoleWire struct {
	RoleID     string `json:"role_id"`
	BuiltinKey string `json:"builtin_key"`
	Name       string `json:"name"`
	Source     string `json:"source"`
}

type collaborationRoleCatalogWire struct {
	CollaborationRoleEpoch int64                   `json:"collaboration_role_epoch"`
	Roles                  []collaborationRoleWire `json:"roles"`
}

type collaborationRoleMemberWire struct {
	UID                string                  `json:"uid"`
	Role               int                     `json:"role"`
	CollaborationRoles []collaborationRoleWire `json:"collaboration_roles"`
}

func getCollaborationRoleCatalog(t *testing.T, projectID, token string) collaborationRoleCatalogWire {
	t.Helper()
	// Kept as a local decode helper so the test asserts the wire contract rather
	// than reaching into the production response type.
	w := doJSON(t, testSrv, http.MethodGet,
		"/v1/projects/"+projectID+"/collaboration-roles", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var got collaborationRoleCatalogWire
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	return got
}

func roleByName(t *testing.T, roles []collaborationRoleWire, name string) collaborationRoleWire {
	t.Helper()
	for _, role := range roles {
		if role.Name == name {
			return role
		}
	}
	t.Fatalf("role %q not found in %+v", name, roles)
	return collaborationRoleWire{}
}

func rosterMember(t *testing.T, projectID, token, uid string) collaborationRoleMemberWire {
	t.Helper()
	w := doJSON(t, testSrv, http.MethodGet, "/v1/projects/"+projectID+"/members", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var rows []collaborationRoleMemberWire
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	for _, row := range rows {
		if row.UID == uid {
			return row
		}
	}
	t.Fatalf("member %q not found in roster", uid)
	return collaborationRoleMemberWire{}
}

func createCollaborationRole(t *testing.T, projectID, token, name string) collaborationRoleWire {
	t.Helper()
	w := doJSON(t, testSrv, http.MethodPost,
		"/v1/projects/"+projectID+"/collaboration-roles", token,
		map[string]any{"name": name})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var got collaborationRoleWire
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.NotEmpty(t, got.RoleID)
	return got
}

func renameCollaborationRole(t *testing.T, projectID, roleID, token, name string) collaborationRoleWire {
	t.Helper()
	w := doJSON(t, testSrv, http.MethodPut,
		"/v1/projects/"+projectID+"/collaboration-roles/"+roleID, token,
		map[string]any{"name": name})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var got collaborationRoleWire
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	return got
}

func TestCollaborationRoleLifecycleDoesNotChangeProjectAuthorization(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, tokens, created := projectWithMembers(t, srv, "m1")
	memberEpoch := epochOf(t, created.ProjectID)

	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	require.Len(t, catalog.Roles, 4)
	for _, name := range []string{"产品", "前端", "后端", "HR"} {
		role := roleByName(t, catalog.Roles, name)
		assert.Equal(t, "builtin", role.Source)
		assert.NotEmpty(t, role.BuiltinKey)
	}

	designer := createCollaborationRole(t, created.ProjectID, ownerToken, "设计")
	assert.Equal(t, "custom", designer.Source)
	assert.Empty(t, designer.BuiltinKey)

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{
			roleByName(t, catalog.Roles, "前端").RoleID,
			designer.RoleID,
		}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	member := rosterMember(t, created.ProjectID, ownerToken, "m1")
	assert.Equal(t, RoleCommon, member.Role, "collaboration labels must not alter authorization")
	require.Len(t, member.CollaborationRoles, 2)
	assert.Equal(t, memberEpoch, epochOf(t, created.ProjectID),
		"collaboration labels must not move member_epoch")

	boundCatalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{designer.RoleID, roleByName(t, catalog.Roles, "前端").RoleID}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	idempotentCatalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	assert.Equal(t, boundCatalog.CollaborationRoleEpoch, idempotentCatalog.CollaborationRoleEpoch,
		"replacing with the same set must be an epoch no-op")

	renamed := renameCollaborationRole(t, created.ProjectID, designer.RoleID, ownerToken, "交互设计")
	assert.Equal(t, "交互设计", renamed.Name)
	member = rosterMember(t, created.ProjectID, ownerToken, "m1")
	assert.Equal(t, "交互设计", roleByName(t, member.CollaborationRoles, "交互设计").Name)

	w = doJSON(t, srv, http.MethodDelete,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles/"+designer.RoleID, ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	member = rosterMember(t, created.ProjectID, ownerToken, "m1")
	assert.Len(t, member.CollaborationRoles, 1, "deleting a definition must remove its bindings")

	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, rosterMember(t, created.ProjectID, ownerToken, "m1").CollaborationRoles)

	// The labelled ordinary member still has ordinary-member capabilities.
	detail := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, tokens["m1"], nil)
	require.Equal(t, http.StatusOK, detail.Code)
	project := decodeResp(t, detail)
	assert.Equal(t, RoleCommon, project.MyRole)
	assert.False(t, project.Capabilities.CanManageMember)
	assert.False(t, project.Capabilities.CanChangeRole)

	after := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	assert.Greater(t, after.CollaborationRoleEpoch, catalog.CollaborationRoleEpoch)
}

func TestCollaborationRolePermissionMatrixAndBuiltinProtection(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, tokens, created := projectWithMembers(t, srv, "admin1", "member1")
	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/admin1/role", ownerToken,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	frontend := roleByName(t, catalog.Roles, "前端")

	// Admins may bind an existing role.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/member1/collaboration-roles", tokens["admin1"],
		map[string]any{"role_ids": []string{frontend.RoleID}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Admins may not change the role catalog.
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles", tokens["admin1"],
		map[string]any{"name": "设计"})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	// Ordinary members may neither self-assign nor assign another member.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/member1/collaboration-roles", tokens["member1"],
		map[string]any{"role_ids": []string{frontend.RoleID}})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	// Built-ins are selectable but immutable.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles/"+frontend.RoleID, ownerToken,
		map[string]any{"name": "客户端"})
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_invalid")
	w = doJSON(t, srv, http.MethodDelete,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles/"+frontend.RoleID, ownerToken, nil)
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_invalid")
}

func TestCollaborationRoleDuplicateIsNormalizedWithinProject(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)
	createCollaborationRole(t, created.ProjectID, ownerToken, "  Designer  ")

	w := doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles", ownerToken,
		map[string]any{"name": "designer"})
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_duplicated")
}

func TestCollaborationRoleCaseOnlyRenameUpdatesDisplayName(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)
	designer := createCollaborationRole(t, created.ProjectID, ownerToken, "designer")
	before := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)

	renamed := renameCollaborationRole(t, created.ProjectID, designer.RoleID, ownerToken, "Designer")
	assert.Equal(t, "Designer", renamed.Name)

	after := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	assert.Equal(t, "Designer", roleByName(t, after.Roles, "Designer").Name)
	assert.Equal(t, before.CollaborationRoleEpoch+1, after.CollaborationRoleEpoch,
		"a case-only display-name rename is a real catalog change")
}

func TestCollaborationRoleNormalizedNameKeepsAccentsDistinct(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)

	plain := createCollaborationRole(t, created.ProjectID, ownerToken, "resume")
	accented := createCollaborationRole(t, created.ProjectID, ownerToken, "résumé")

	assert.NotEqual(t, plain.RoleID, accented.RoleID)
	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	assert.Equal(t, plain.RoleID, roleByName(t, catalog.Roles, "resume").RoleID)
	assert.Equal(t, accented.RoleID, roleByName(t, catalog.Roles, "résumé").RoleID)
}

func TestCollaborationRolesAreClearedWhenMemberLeavesAndNotRestored(t *testing.T) {
	srv, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv, "m1")
	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	product := roleByName(t, catalog.Roles, "产品")

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{product.RoleID}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	drainRemovalCascade(t, p)

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	member := rosterMember(t, created.ProjectID, ownerToken, "m1")
	assert.Empty(t, member.CollaborationRoles,
		"re-admission must not restore labels from the previous seat lifecycle")
}

func TestCollaborationRoleRejectsInvalidSetsAndAgents(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv, "m1")
	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	frontend := roleByName(t, catalog.Roles, "前端")

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{frontend.RoleID, frontend.RoleID}})
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_invalid")

	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{"missing-role"}})
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_invalid")

	seedAgent(t, spaceA, "agent1", "owner1", "octo_hosted")
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{"agent1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/agent1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{frontend.RoleID}})
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_target_invalid")
	assert.Empty(t, rosterMember(t, created.ProjectID, ownerToken, "agent1").CollaborationRoles)
}

func TestCollaborationRoleWriteFlagDoesNotHideReads(t *testing.T) {
	_, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, testSrv)
	p.cfg.CollaborationRoleEnabled = false
	r := mountProject(t, p)

	w := doOn(t, r, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles", ownerToken,
		map[string]any{"name": "设计"})
	assertProjectErrorCode(t, w, "err.server.project.collaboration_role_disabled")
}

func TestCollaborationRoleQuotasAreConfigBacked(t *testing.T) {
	_, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, testSrv, "m1")
	p.cfg.CollaborationRoleMaxPerProject = len(builtinCollaborationRoles) + 1
	p.cfg.CollaborationRoleMaxPerMember = 1
	r := mountProject(t, p)

	first := doOn(t, r, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles", ownerToken,
		map[string]any{"name": "设计"})
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())
	var custom collaborationRoleWire
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &custom))

	w := doOn(t, r, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/collaboration-roles", ownerToken,
		map[string]any{"name": "测试"})
	assertProjectErrorCode(t, w, "err.server.project.quota_collaboration_roles")

	catalog := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	w = doOn(t, r, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{custom.RoleID, roleByName(t, catalog.Roles, "产品").RoleID}})
	assertProjectErrorCode(t, w, "err.server.project.quota_member_collaboration_roles")
}

func TestConcurrentCollaborationRoleCreateKeepsNormalizedNameUnique(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)

	responses := make(chan *struct {
		code int
		body string
	}, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"Designer", " designer "} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			w := doJSON(t, srv, http.MethodPost,
				"/v1/projects/"+created.ProjectID+"/collaboration-roles", ownerToken,
				map[string]any{"name": name})
			responses <- &struct {
				code int
				body string
			}{code: w.Code, body: w.Body.String()}
		}(name)
	}
	wg.Wait()
	close(responses)

	ok, conflict := 0, 0
	for response := range responses {
		switch response.code {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(response.body), &envelope))
			if envelope.Error.Code == "err.server.project.collaboration_role_duplicated" {
				conflict++
			}
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, conflict)
}

func TestBuiltinCollaborationRoleBackfillIsBoundedAndIdempotent(t *testing.T) {
	_, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, testSrv)
	before := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	product := roleByName(t, before.Roles, "产品")

	_, err := testCtx.DB().DeleteFrom("octo_project_collaboration_role").
		Where("project_id = ? AND role_id = ?", created.ProjectID, product.RoleID).
		Exec()
	require.NoError(t, err)

	p.cfg.ReconcileLimit = 1
	collaborationRoleBackfillCursor.Store(0)
	p.backfillBuiltinCollaborationRoles()
	after := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	require.Len(t, after.Roles, len(builtinCollaborationRoles))
	assert.Equal(t, before.CollaborationRoleEpoch+1, after.CollaborationRoleEpoch)

	p.backfillBuiltinCollaborationRoles()
	idempotent := getCollaborationRoleCatalog(t, created.ProjectID, ownerToken)
	assert.Equal(t, after.CollaborationRoleEpoch, idempotent.CollaborationRoleEpoch)
}

func TestCollaborationRoleIntegrityPageBoundsRowsBeforeFiltering(t *testing.T) {
	srv, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv, "m1")
	frontend := roleByName(t,
		getCollaborationRoleCatalog(t, created.ProjectID, ownerToken).Roles, "前端")

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{frontend.RoleID}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Sort the invalid row after the valid m1 row. A bounded scan with limit=1
	// must inspect only m1 on the first page; filtering violations before LIMIT
	// would skip m1 and scan ahead until it found zz_missing_member.
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project_member_collaboration_role` "+
			"(project_id, uid, role_id, created_at) VALUES (?, ?, ?, NOW(3))",
		created.ProjectID, "zz_missing_member", frontend.RoleID,
	).Exec()
	require.NoError(t, err)

	first, err := p.db.collaborationRoleIntegrityPage("", "", "", 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, "m1", first[0].UID)
	assert.False(t, first[0].Violating)

	second, err := p.db.collaborationRoleIntegrityPage(
		first[0].ProjectID, first[0].UID, first[0].RoleID, 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "zz_missing_member", second[0].UID)
	assert.True(t, second[0].Violating)

	// The scheduled scan keeps the composite cursor and running total across
	// ticks, then resets only after a complete rotation. This prevents a fixed
	// first page from hiding violations later in the keyspace.
	collaborationRoleIntegrityState.save("", "", "", 0, true)
	p.cfg.ReconcileLimit = 1
	p.scanCollaborationRoleIntegrity()
	projectID, uid, roleID, total := collaborationRoleIntegrityState.resume()
	assert.Equal(t, created.ProjectID, projectID)
	assert.Equal(t, "m1", uid)
	assert.NotEmpty(t, roleID)
	assert.Zero(t, total)

	p.scanCollaborationRoleIntegrity()
	projectID, uid, roleID, total = collaborationRoleIntegrityState.resume()
	assert.Equal(t, created.ProjectID, projectID)
	assert.Equal(t, "zz_missing_member", uid)
	assert.NotEmpty(t, roleID)
	assert.Equal(t, 1, total)

	p.scanCollaborationRoleIntegrity()
	projectID, uid, roleID, total = collaborationRoleIntegrityState.resume()
	assert.Empty(t, projectID)
	assert.Empty(t, uid)
	assert.Empty(t, roleID)
	assert.Zero(t, total)
}
