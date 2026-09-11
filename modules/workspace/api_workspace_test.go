package workspace_test

import (
	"encoding/json"
	"fmt"
	workspacemod "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/stretchr/testify/require"
	"net/http"
	"strings"
	"testing"
)

func TestWorkspaceHTTPCreateListDetailAndHeaderAssertion(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "http-crud-space", "10000")
	token := workspaceToken(t, ctx, "10000")

	conflictingAssertion := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodPost, "/v1/workspaces", token, map[string]any{
		"space_id": "http-crud-space",
		"name":     "conflicting assertion",
	}, map[string]string{"X-Space-Id": "other-space"})
	require.Equal(t, http.StatusBadRequest, conflictingAssertion.Code, conflictingAssertion.Body.String())
	conflictCode, _ := decodeWorkspaceError(t, conflictingAssertion)
	require.Equal(t, "err.server.workspace.space_required", conflictCode)
	missingBodySpace := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodPost, "/v1/workspaces", token, map[string]any{
		"name": "missing body space",
	}, map[string]string{"X-Space-Id": "http-crud-space"})
	require.Equal(t, http.StatusBadRequest, missingBodySpace.Code, missingBodySpace.Body.String())
	missingBodyCode, _ := decodeWorkspaceError(t, missingBodySpace)
	require.Equal(t, "err.server.workspace.space_required", missingBodyCode)

	create := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPost, "/v1/workspaces", token, map[string]any{
		"space_id":    "http-crud-space",
		"name":        "  协作空间  ",
		"description": "initial description",
		"logo":        "https://example.invalid/logo.png",
		// Read-only and unknown fields must not become part of the authority.
		"owner_uid": "attacker",
		"status":    0,
		"unknown":   "ignored",
	})
	require.Equal(t, http.StatusCreated, create.Code, create.Body.String())
	var ws workspacemod.Workspace
	require.NoError(t, json.Unmarshal(create.Body.Bytes(), &ws))
	require.Equal(t, "协作空间", ws.Name)
	require.Equal(t, "initial description", ws.Description)
	require.Equal(t, "https://example.invalid/logo.png", ws.Logo)
	require.Equal(t, "10000", ws.OwnerUID)
	require.Equal(t, workspacemod.WorkspaceStatusActive, ws.Status)
	require.Equal(t, workspacemod.WorkspaceRoleOwner, ws.WorkspaceRole)
	var topLevel map[string]any
	require.NoError(t, json.Unmarshal(create.Body.Bytes(), &topLevel))
	require.NotContains(t, topLevel, "data", "Workspace success responses are direct DTOs")

	list := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces?space_id=http-crud-space&keyword=协作&page_index=1&page_size=15", token, nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var listed struct {
		Count int64                    `json:"count"`
		List  []workspacemod.Workspace `json:"list"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listed))
	require.EqualValues(t, 1, listed.Count)
	require.NotNil(t, listed.List)
	require.Len(t, listed.List, 1)
	require.Equal(t, ws.WorkspaceID, listed.List[0].WorkspaceID)

	detail := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet, "/v1/workspaces/"+ws.WorkspaceID, token, nil)
	require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
	var got workspacemod.Workspace
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &got))
	require.Equal(t, ws.WorkspaceID, got.WorkspaceID)
	require.Equal(t, ws.SpaceID, got.SpaceID)

	longLogo := "https://example.invalid/" + strings.Repeat("x", 220)
	longLogoUpdate := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPut,
		"/v1/workspaces/"+ws.WorkspaceID, token, map[string]string{"logo": longLogo})
	require.Equal(t, http.StatusOK, longLogoUpdate.Code, longLogoUpdate.Body.String())
	var longLogoWorkspace workspacemod.Workspace
	require.NoError(t, json.Unmarshal(longLogoUpdate.Body.Bytes(), &longLogoWorkspace))
	require.Equal(t, longLogo, longLogoWorkspace.Logo)

	withMatchingHeader := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, token, nil, map[string]string{"X-Space-Id": "http-crud-space"})
	require.Equal(t, http.StatusOK, withMatchingHeader.Code, withMatchingHeader.Body.String())
	withWrongHeader := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, token, nil, map[string]string{"X-Space-Id": "other-space"})
	require.Equal(t, http.StatusBadRequest, withWrongHeader.Code, withWrongHeader.Body.String())
	code, semantic := decodeWorkspaceError(t, withWrongHeader)
	require.Equal(t, "err.server.workspace.space_required", code)
	require.Equal(t, http.StatusBadRequest, semantic)
}

func TestWorkspaceHTTPAuthHeaderPrecedenceAndUnknownResourcePrivacy(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-foreign", "Foreign user"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "http-auth-space", "10000")
	seedWorkspaceSpace(t, ctx, "http-foreign-space", "ws-foreign")
	ownerToken := workspaceToken(t, ctx, "10000")
	foreignToken := workspaceToken(t, ctx, "ws-foreign")
	ws := createWorkspace(t, ctx, ownerToken, "http-auth-space", "Private")

	// A non-empty token header wins; an attacker-controlled Authorization header
	// cannot replace it or turn a failed alternate credential into a success.
	precedence := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, ownerToken, nil,
		map[string]string{"Authorization": "Bearer definitely-invalid"})
	require.Equal(t, http.StatusOK, precedence.Code, precedence.Body.String())

	bearer := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, "", nil,
		map[string]string{"Authorization": "Bearer " + ownerToken})
	require.Equal(t, http.StatusOK, bearer.Code, bearer.Body.String())

	for _, tc := range []struct {
		name    string
		headers map[string]string
		path    string
	}{
		{"basic", map[string]string{"Authorization": "Basic " + ownerToken}, "/v1/workspaces/" + ws.WorkspaceID},
		{"multi_segment_bearer", map[string]string{"Authorization": "Bearer " + ownerToken + " extra"}, "/v1/workspaces/" + ws.WorkspaceID},
		{"query_credential", nil, "/v1/workspaces/" + ws.WorkspaceID + "?token=" + ownerToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doWorkspaceJSONWithHeaders(t, srv.GetRoute(), http.MethodGet, tc.path, "", nil, tc.headers)
			require.NotEqual(t, http.StatusOK, rec.Code, "query/non-Bearer credentials must not authenticate")
		})
	}

	// A known Workspace in a foreign Space and an unknown Workspace have the
	// same not-found result. The middleware must not reveal resource existence
	// through a forbidden-vs-not-found distinction.
	knownForeign := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, foreignToken, nil)
	unknown := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/workspace-does-not-exist", foreignToken, nil)
	require.Equal(t, http.StatusBadRequest, knownForeign.Code, knownForeign.Body.String())
	require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
	knownCode, knownStatus := decodeWorkspaceError(t, knownForeign)
	unknownCode, unknownStatus := decodeWorkspaceError(t, unknown)
	require.Equal(t, "err.server.workspace.not_found", knownCode)
	require.Equal(t, knownCode, unknownCode)
	require.Equal(t, http.StatusNotFound, knownStatus)
	require.Equal(t, knownStatus, unknownStatus)

	missingSpace := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet, "/v1/workspaces", ownerToken, nil)
	require.Equal(t, http.StatusBadRequest, missingSpace.Code, missingSpace.Body.String())
	missingCode, _ := decodeWorkspaceError(t, missingSpace)
	require.Equal(t, "err.server.workspace.space_required", missingCode)
}

func TestWorkspaceHTTPMembersPaginationAndExitedMemberProjection(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-admin", "Workspace admin"},
		{"ws-member", "Workspace member"},
		{"ws-exited", "Workspace exited"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "http-members-space", "10000", "ws-admin", "ws-member", "ws-exited")
	ownerToken := workspaceToken(t, ctx, "10000")
	exitedToken := workspaceToken(t, ctx, "ws-exited")
	ws := createWorkspace(t, ctx, ownerToken, "http-members-space", "Members")

	add := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPost,
		"/v1/workspaces/"+ws.WorkspaceID+"/members", ownerToken, map[string]any{
			"members": []map[string]string{
				{"uid": "ws-admin", "workspace_role": "admin"},
				{"uid": "ws-member", "workspace_role": "member"},
				{"uid": "ws-exited", "workspace_role": "member"},
			},
		})
	require.Equal(t, http.StatusOK, add.Code, add.Body.String())
	var addBody struct {
		WorkspaceID string                `json:"workspace_id"`
		Members     []workspacemod.Member `json:"members"`
	}
	require.NoError(t, json.Unmarshal(add.Body.Bytes(), &addBody))
	require.Equal(t, ws.WorkspaceID, addBody.WorkspaceID)
	require.Len(t, addBody.Members, 3)

	leave := doWorkspaceJSON(t, srv.GetRoute(), http.MethodDelete,
		"/v1/workspaces/"+ws.WorkspaceID+"/members/me", exitedToken, nil)
	require.Equal(t, http.StatusOK, leave.Code, leave.Body.String())

	// Historical member relationships stay queryable by an authorized owner;
	// the single-member projection must expose status=0 instead of collapsing
	// an exited relation to not-found.
	exited := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID+"/members/ws-exited", ownerToken, nil)
	require.Equal(t, http.StatusOK, exited.Code, exited.Body.String())
	var exitedMember workspacemod.Member
	require.NoError(t, json.Unmarshal(exited.Body.Bytes(), &exitedMember))
	require.Equal(t, "ws-exited", exitedMember.UID)
	require.Equal(t, workspacemod.MemberStatusInactive, exitedMember.Status)
	require.Equal(t, workspacemod.WorkspaceRoleMember, exitedMember.WorkspaceRole)

	active := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID+"/members?workspace_role=owner,member&status=1&page_index=1&page_size=2", ownerToken, nil)
	require.Equal(t, http.StatusOK, active.Code, active.Body.String())
	var activeBody struct {
		Count int64                 `json:"count"`
		List  []workspacemod.Member `json:"list"`
	}
	require.NoError(t, json.Unmarshal(active.Body.Bytes(), &activeBody))
	require.EqualValues(t, 2, activeBody.Count)
	require.NotNil(t, activeBody.List)
	require.Len(t, activeBody.List, 2)

	exitedList := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID+"/members?status=0&page_index=1&page_size=15", ownerToken, nil)
	require.Equal(t, http.StatusOK, exitedList.Code, exitedList.Body.String())
	var exitedBody struct {
		Count int64                 `json:"count"`
		List  []workspacemod.Member `json:"list"`
	}
	require.NoError(t, json.Unmarshal(exitedList.Body.Bytes(), &exitedBody))
	require.EqualValues(t, 1, exitedBody.Count)
	require.Len(t, exitedBody.List, 1)
	require.Equal(t, "ws-exited", exitedBody.List[0].UID)

	overflow := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID+"/members?status=1&page_index=999999999999999999999999999999999999&page_size=200", ownerToken, nil)
	require.Equal(t, http.StatusOK, overflow.Code, overflow.Body.String())
	var overflowBody struct {
		Count int64                 `json:"count"`
		List  []workspacemod.Member `json:"list"`
	}
	require.NoError(t, json.Unmarshal(overflow.Body.Bytes(), &overflowBody))
	require.EqualValues(t, 3, overflowBody.Count)
	require.NotNil(t, overflowBody.List)
	require.Empty(t, overflowBody.List, "a huge page must not wrap and return page one")

	invalidRole := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPost,
		"/v1/workspaces/"+ws.WorkspaceID+"/members", ownerToken, map[string]any{
			"members": []map[string]string{{"uid": "ws-member", "workspace_role": "owner"}},
		})
	require.Equal(t, http.StatusBadRequest, invalidRole.Code, invalidRole.Body.String())
	invalidCode, invalidStatus := decodeWorkspaceError(t, invalidRole)
	require.Equal(t, "err.server.workspace.workspace_role_invalid", invalidCode)
	require.Equal(t, http.StatusUnprocessableEntity, invalidStatus)

	statusJunk := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID+"/members?status=1junk", ownerToken, nil)
	require.Equal(t, http.StatusBadRequest, statusJunk.Code, statusJunk.Body.String())
	statusJunkCode, statusJunkSemantic := decodeWorkspaceError(t, statusJunk)
	require.Equal(t, "err.server.workspace.request_invalid", statusJunkCode)
	require.Equal(t, http.StatusBadRequest, statusJunkSemantic)
}

func TestWorkspaceHTTPOwnerTransferLeaveAndOrganizationRevocation(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-new-owner", "New owner"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "http-owner-space", "10000", "ws-new-owner")
	ownerToken := workspaceToken(t, ctx, "10000")
	newOwnerToken := workspaceToken(t, ctx, "ws-new-owner")
	ws := createWorkspace(t, ctx, ownerToken, "http-owner-space", "Lifecycle")

	add := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPost,
		"/v1/workspaces/"+ws.WorkspaceID+"/members", ownerToken, map[string]any{
			"members": []map[string]string{{"uid": "ws-new-owner", "workspace_role": "member"}},
		})
	require.Equal(t, http.StatusOK, add.Code, add.Body.String())
	transfer := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPut,
		"/v1/workspaces/"+ws.WorkspaceID+"/owner", ownerToken, map[string]string{"uid": "ws-new-owner"})
	require.Equal(t, http.StatusOK, transfer.Code, transfer.Body.String())
	var transferred workspacemod.Workspace
	require.NoError(t, json.Unmarshal(transfer.Body.Bytes(), &transferred))
	require.Equal(t, "ws-new-owner", transferred.OwnerUID)
	require.Equal(t, workspacemod.WorkspaceRoleAdmin, transferred.WorkspaceRole)

	oldOwnerLeave := doWorkspaceJSON(t, srv.GetRoute(), http.MethodDelete,
		"/v1/workspaces/"+ws.WorkspaceID+"/members/me", ownerToken, nil)
	require.Equal(t, http.StatusOK, oldOwnerLeave.Code, oldOwnerLeave.Body.String())
	newOwnerView := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, newOwnerToken, nil)
	require.Equal(t, http.StatusOK, newOwnerView.Code, newOwnerView.Body.String())
	var ownerView workspacemod.Workspace
	require.NoError(t, json.Unmarshal(newOwnerView.Body.Bytes(), &ownerView))
	require.Equal(t, workspacemod.WorkspaceRoleOwner, ownerView.WorkspaceRole)
}

func TestWorkspaceHTTPDeleteWorkspaceIsUnregisteredAndPreservesStatus(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "http-delete-space", "10000")
	ownerToken := workspaceToken(t, ctx, "10000")
	ws := createWorkspace(t, ctx, ownerToken, "http-delete-space", "Delete is unsupported")

	deleted := doWorkspaceJSON(t, srv.GetRoute(), http.MethodDelete,
		"/v1/workspaces/"+ws.WorkspaceID, ownerToken, nil)
	require.Equal(t, http.StatusNotFound, deleted.Code, deleted.Body.String())

	var status int
	_, err := ctx.DB().Select("status").From("octo_workspace").
		Where("workspace_id=?", ws.WorkspaceID).Load(&status)
	require.NoError(t, err)
	require.Equal(t, workspacemod.WorkspaceStatusActive, status)
}

func TestWorkspaceHTTPRevokedMemberAndBannedSpaceFailClosed(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "http-revoke-space", "10000")
	ownerToken := workspaceToken(t, ctx, "10000")
	ws := createWorkspace(t, ctx, ownerToken, "http-revoke-space", "Revocation")

	_, err := ctx.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", "http-revoke-space", "10000").Exec()
	require.NoError(t, err)
	revoked := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces/"+ws.WorkspaceID, ownerToken, nil)
	require.Equal(t, http.StatusBadRequest, revoked.Code, revoked.Body.String())
	revokedCode, revokedStatus := decodeWorkspaceError(t, revoked)
	require.Equal(t, "err.server.workspace.not_found", revokedCode)
	require.Equal(t, http.StatusNotFound, revokedStatus)

	_, err = ctx.DB().Update("space_member").Set("status", 1).
		Where("space_id=? AND uid=?", "http-revoke-space", "10000").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("space").Set("status", 2).
		Where("space_id=?", "http-revoke-space").Exec()
	require.NoError(t, err)
	banned := doWorkspaceJSON(t, srv.GetRoute(), http.MethodGet,
		"/v1/workspaces?space_id=http-revoke-space", ownerToken, nil)
	require.Equal(t, http.StatusBadRequest, banned.Code, banned.Body.String())
	bannedCode, bannedStatus := decodeWorkspaceError(t, banned)
	require.Equal(t, "err.server.workspace.not_found", bannedCode)
	require.Equal(t, http.StatusNotFound, bannedStatus)
}
func TestWorkspaceHTTPAddMembersOverBatchReturnsSemantic422WithoutWrite(t *testing.T) {
	srv, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	candidateUIDs := make([]string, 101)
	for i := range candidateUIDs {
		candidateUIDs[i] = fmt.Sprintf("ws-overflow-%03d", i)
		seedWorkspaceUser(t, ctx, candidateUIDs[i], "Workspace overflow candidate")
	}
	spaceMembers := append([]string{"10000"}, candidateUIDs...)
	seedWorkspaceSpace(t, ctx, "http-batch-overflow-space", spaceMembers...)
	ownerToken := workspaceToken(t, ctx, "10000")
	ws := createWorkspace(t, ctx, ownerToken, "http-batch-overflow-space", "Batch overflow")

	members := make([]map[string]string, len(candidateUIDs))
	for i, uid := range candidateUIDs {
		members[i] = map[string]string{
			"uid":            uid,
			"workspace_role": workspacemod.WorkspaceRoleMember,
		}
	}
	overflow := doWorkspaceJSON(t, srv.GetRoute(), http.MethodPost,
		"/v1/workspaces/"+ws.WorkspaceID+"/members", ownerToken, map[string]any{"members": members})
	require.Equal(t, http.StatusBadRequest, overflow.Code, overflow.Body.String())
	code, semanticStatus := decodeWorkspaceError(t, overflow)
	require.Equal(t, "err.server.workspace.candidate_ineligible", code)
	require.Equal(t, http.StatusUnprocessableEntity, semanticStatus)

	var memberCount int64
	_, err := ctx.DB().Select("COUNT(*)").From("octo_workspace_member").
		Where("workspace_id=?", ws.WorkspaceID).Load(&memberCount)
	require.NoError(t, err)
	require.EqualValues(t, 1, memberCount, "rejected overflow batch must not write membership rows")
}
