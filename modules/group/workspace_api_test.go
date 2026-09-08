package group

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	workspace "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

func setupGroupWorkspaceContract(t *testing.T) (*server.Server, *config.Context, *Group) {
	t.Helper()
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	clearGroupWorkspaceUIDBuckets(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		clearGroupWorkspaceUIDBuckets(ctx)
	})
	newGroupIMStub(t, ctx)
	g := New(ctx)
	return s, ctx, g
}

func clearGroupWorkspaceUIDBuckets(ctx *config.Context) {
	if ctx == nil {
		return
	}
	cfg := ctx.GetConfig()
	client := redis.NewClient(&redis.Options{Addr: cfg.DB.RedisAddr, Password: cfg.DB.RedisPass})
	defer client.Close()
	keys, err := client.Keys("ratelimit:uid:*").Result()
	if err == nil && len(keys) > 0 {
		_ = client.Del(keys...).Err()
	}
}

func groupWorkspaceContractToken(t *testing.T, ctx *config.Context, uid string) string {
	token := fmt.Sprintf("group-workspace-%s-%d", uid, time.Now().UnixNano())
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+token, uid+"@group-workspace-test"))
	return token
}

func groupWorkspaceContractJSON(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(data))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func seedGroupWorkspaceUsers(t *testing.T, g *Group, uids ...string) {
	t.Helper()
	insertTestUsers(t, g.userDB, uids...)
	for _, uid := range uids {
		_, err := g.ctx.DB().Update("user").Set("status", 1).Where("uid=?", uid).Exec()
		require.NoError(t, err)
	}
}

func createGroupWorkspace(t *testing.T, g *Group, creator, spaceID string, members ...string) string {
	t.Helper()
	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: creator,
		Members: members,
		Name:    "Workspace contract group",
		SpaceID: spaceID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp)
	require.NotEmpty(t, resp.GroupNo)
	return resp.GroupNo
}

func createContractWorkspace(t *testing.T, ctx *config.Context, owner, spaceID, name string) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewService(ctx).Create(workspace.Scope{UID: owner}, workspace.CreateRequest{
		SpaceID: spaceID,
		Name:    name,
	})
	require.NoError(t, err)
	require.NotNil(t, ws)
	return ws
}

func addContractWorkspaceMembers(t *testing.T, ctx *config.Context, owner, workspaceID string, uids ...string) {
	t.Helper()
	inputs := make([]workspace.MemberInput, 0, len(uids))
	for _, uid := range uids {
		inputs = append(inputs, workspace.MemberInput{UID: uid, WorkspaceRole: workspace.WorkspaceRoleMember})
	}
	_, err := workspace.NewService(ctx).AddMembers(workspace.Scope{UID: owner}, workspaceID, inputs)
	require.NoError(t, err)
}

func TestGroupWorkspaceRelationMetadataAuthRebindAndUnbind(t *testing.T) {
	s, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-owner"
		wsOnly  = "gw-workspace-only"
		native  = "gw-native-member"
		manager = "gw-native-manager"
	)
	seedGroupWorkspaceUsers(t, g, owner, wsOnly, native, manager)
	seedSpaceWithMembers(t, ctx, "gw-space-a", owner, wsOnly, native, manager)
	seedSpaceWithMembers(t, ctx, "gw-space-b", owner)
	wsA := createContractWorkspace(t, ctx, owner, "gw-space-a", "A")
	addContractWorkspaceMembers(t, ctx, owner, wsA.WorkspaceID, wsOnly, manager)
	wsB := createContractWorkspace(t, ctx, owner, "gw-space-b", "B")
	wsC := createContractWorkspace(t, ctx, owner, "gw-space-a", "C")
	groupNo := createGroupWorkspace(t, g, owner, "gw-space-a", native, manager)
	ownerToken := groupWorkspaceContractToken(t, ctx, owner)
	wsOnlyToken := groupWorkspaceContractToken(t, ctx, wsOnly)
	nativeToken := groupWorkspaceContractToken(t, ctx, native)
	managerToken := groupWorkspaceContractToken(t, ctx, manager)

	bind := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodPut,
		"/v1/groups/"+groupNo+"/workspace", ownerToken, map[string]string{"workspace_id": wsA.WorkspaceID})
	require.Equal(t, http.StatusOK, bind.Code, bind.Body.String())
	var relation GroupWorkspace
	require.NoError(t, json.Unmarshal(bind.Body.Bytes(), &relation))
	require.NotNil(t, relation.WorkspaceID)
	require.Equal(t, wsA.WorkspaceID, *relation.WorkspaceID)
	require.NotNil(t, relation.LinkedBy)
	require.Equal(t, owner, *relation.LinkedBy)

	// Workspace membership grants only restricted relation metadata. It never
	// substitutes for native group membership when reading group content.
	workspaceOnlyRelation := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/groups/"+groupNo+"/workspace", wsOnlyToken, nil)
	require.Equal(t, http.StatusOK, workspaceOnlyRelation.Code, workspaceOnlyRelation.Body.String())
	groupContent := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/groups/"+groupNo, wsOnlyToken, nil)
	require.NotEqual(t, http.StatusOK, groupContent.Code, "relation metadata must not grant group content access")
	nativeOnlyRelation := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/groups/"+groupNo+"/workspace", nativeToken, nil)
	require.Equal(t, http.StatusBadRequest, nativeOnlyRelation.Code, nativeOnlyRelation.Body.String())
	nativeErr := decodeEnvelope(t, nativeOnlyRelation.Body.Bytes())
	require.Equal(t, "err.server.workspace.forbidden", nativeErr.Error.Code)
	require.Equal(t, http.StatusForbidden, nativeErr.Error.HTTPStatus)
	require.Equal(t, http.StatusBadRequest, nativeErr.Status)

	list := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/groups?workspace_id="+wsA.WorkspaceID+"&page_index=1&page_size=15", wsOnlyToken, nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var listed struct {
		Count int64            `json:"count"`
		List  []GroupWorkspace `json:"list"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listed))
	require.EqualValues(t, 1, listed.Count)
	require.Len(t, listed.List, 1)
	require.Equal(t, groupNo, listed.List[0].GroupNo)

	// Same-target PUT from a different native manager is an idempotent no-op;
	// it must preserve the original linked_by actor.
	_, err := ctx.DB().Update("group_member").Set("role", MemberRoleManager).
		Where("group_no=? AND uid=?", groupNo, manager).Exec()
	require.NoError(t, err)
	sameTarget := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodPut,
		"/v1/groups/"+groupNo+"/workspace", managerToken, map[string]string{"workspace_id": wsA.WorkspaceID})
	require.Equal(t, http.StatusOK, sameTarget.Code, sameTarget.Body.String())
	require.NoError(t, json.Unmarshal(sameTarget.Body.Bytes(), &relation))
	require.NotNil(t, relation.LinkedBy)
	require.Equal(t, owner, *relation.LinkedBy)

	crossSpace := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodPut,
		"/v1/groups/"+groupNo+"/workspace", ownerToken, map[string]string{"workspace_id": wsB.WorkspaceID})
	require.Equal(t, http.StatusBadRequest, crossSpace.Code, crossSpace.Body.String())
	conflictErr := decodeEnvelope(t, crossSpace.Body.Bytes())
	require.Equal(t, "err.server.group.workspace_conflict", conflictErr.Error.Code)
	require.Equal(t, http.StatusConflict, conflictErr.Error.HTTPStatus)
	require.Equal(t, http.StatusBadRequest, conflictErr.Status)

	rebind := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodPut,
		"/v1/groups/"+groupNo+"/workspace", ownerToken, map[string]string{"workspace_id": wsC.WorkspaceID})
	require.Equal(t, http.StatusOK, rebind.Code, rebind.Body.String())
	require.NoError(t, json.Unmarshal(rebind.Body.Bytes(), &relation))
	require.NotNil(t, relation.WorkspaceID)
	require.Equal(t, wsC.WorkspaceID, *relation.WorkspaceID)
	require.Equal(t, owner, *relation.LinkedBy)

	beforeMembers, err := g.db.QueryMembersFirstNine(groupNo)
	require.NoError(t, err)
	unbind := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodDelete,
		"/v1/groups/"+groupNo+"/workspace", ownerToken, nil)
	require.Equal(t, http.StatusOK, unbind.Code, unbind.Body.String())
	require.NoError(t, json.Unmarshal(unbind.Body.Bytes(), &relation))
	require.Nil(t, relation.WorkspaceID)
	require.Nil(t, relation.LinkedBy)
	afterMembers, err := g.db.QueryMembersFirstNine(groupNo)
	require.NoError(t, err)
	require.Len(t, afterMembers, len(beforeMembers), "relation unbind must preserve native group membership")

	// Ordinary GroupResp remains a separate contract and does not leak relation
	// metadata into the legacy /group/my response.
	my := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/group/my?space_id=gw-space-a", ownerToken, nil)
	require.Equal(t, http.StatusOK, my.Code, my.Body.String())
	var myRows []map[string]any
	require.NoError(t, json.Unmarshal(my.Body.Bytes(), &myRows))
	for _, row := range myRows {
		require.NotContains(t, row, "workspace_id")
		require.NotContains(t, row, "linked_by")
	}
}

func TestGroupWorkspaceMyModesUseNumericNativeRoles(t *testing.T) {
	s, ctx, g := setupGroupWorkspaceContract(t)
	const (
		requester = "gw-my-requester"
		creatorA  = "gw-my-owner"
		creatorB  = "gw-my-admin-creator"
		creatorC  = "gw-my-member-creator"
		spaceID   = "gw-my-space"
	)
	seedGroupWorkspaceUsers(t, g, requester, creatorA, creatorB, creatorC)
	seedSpaceWithMembers(t, ctx, spaceID, requester, creatorA, creatorB, creatorC)
	requesterToken := groupWorkspaceContractToken(t, ctx, requester)
	ownerGroup := createGroupWorkspace(t, g, requester, spaceID, creatorB)
	adminGroup := createGroupWorkspace(t, g, creatorB, spaceID, requester)
	memberGroup := createGroupWorkspace(t, g, creatorC, spaceID, requester)
	_, err := ctx.DB().Update("group_member").Set("role", MemberRoleManager).
		Where("group_no=? AND uid=?", adminGroup, requester).Exec()
	require.NoError(t, err)

	spaceMode := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/group/my?space_id="+spaceID, requesterToken, nil)
	require.Equal(t, http.StatusOK, spaceMode.Code, spaceMode.Body.String())
	var rows []struct {
		GroupNo string `json:"group_no"`
		Role    int    `json:"role"`
		SpaceID string `json:"space_id"`
	}
	require.NoError(t, json.Unmarshal(spaceMode.Body.Bytes(), &rows))
	roles := make(map[string]int, len(rows))
	for _, row := range rows {
		roles[row.GroupNo] = row.Role
		require.Equal(t, spaceID, row.SpaceID)
	}
	require.Equal(t, 1, roles[ownerGroup])
	require.Equal(t, 2, roles[adminGroup])
	require.Equal(t, 0, roles[memberGroup])

	managed := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/group/my?role=owner,admin&space_id="+spaceID, requesterToken, nil)
	require.Equal(t, http.StatusOK, managed.Code, managed.Body.String())
	var managedRows []struct {
		GroupNo string `json:"group_no"`
		Role    int    `json:"role"`
	}
	require.NoError(t, json.Unmarshal(managed.Body.Bytes(), &managedRows))
	require.Len(t, managedRows, 2)
	managedRoles := make(map[string]int, len(managedRows))
	for _, row := range managedRows {
		managedRoles[row.GroupNo] = row.Role
	}
	require.Equal(t, 1, managedRoles[ownerGroup])
	require.Equal(t, 2, managedRoles[adminGroup])
	require.NotContains(t, managedRoles, memberGroup)

	invalid := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/group/my?role=owner,,admin&space_id="+spaceID, requesterToken, nil)
	require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())

	for _, groupNo := range []string{ownerGroup, adminGroup, memberGroup} {
		_, err = ctx.DB().InsertInto("group_setting").
			Columns("group_no", "uid", "save", "version").
			Values(groupNo, requester, 1, 1).Exec()
		require.NoError(t, err)
	}
	saved := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/group/my", requesterToken, nil)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	var savedRows []struct {
		GroupNo string `json:"group_no"`
		Role    int    `json:"role"`
	}
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &savedRows))
	require.Len(t, savedRows, 3)
	savedRoles := make(map[string]int, len(savedRows))
	for _, row := range savedRows {
		savedRoles[row.GroupNo] = row.Role
	}
	require.Equal(t, 1, savedRoles[ownerGroup])
	require.Equal(t, 2, savedRoles[adminGroup])
	require.Equal(t, 0, savedRoles[memberGroup])

}

func TestGroupWorkspaceCreateSnapshotsUnionOnceAndSystemPolicy(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner    = "gw-snapshot-owner"
		activeA  = "gw-snapshot-a"
		activeB  = "gw-snapshot-b"
		explicit = "gw-snapshot-explicit"
		later    = "gw-snapshot-later"
		spaceID  = "gw-snapshot-space"
	)
	systemUID := strings.TrimSpace(ctx.GetConfig().Account.FileHelperUID)
	require.NotEmpty(t, systemUID)
	seedGroupWorkspaceUsers(t, g, owner, activeA, activeB, explicit, later, systemUID)
	seedSpaceWithMembers(t, ctx, spaceID, owner, activeA, activeB, explicit, later, systemUID)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, 1)").Exec()
	require.NoError(t, err)
	ws := createContractWorkspace(t, ctx, owner, spaceID, "Snapshot")
	addContractWorkspaceMembers(t, ctx, owner, ws.WorkspaceID, activeA, activeB)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     owner,
		Members:     []string{activeA, explicit, activeA},
		Name:        "Snapshot union",
		WorkspaceID: ws.WorkspaceID,
	})
	require.NoError(t, err)
	row, err := g.db.QueryWithGroupNo(resp.GroupNo)
	require.NoError(t, err)
	require.NotNil(t, row.WorkspaceID)
	require.Equal(t, ws.WorkspaceID, *row.WorkspaceID)
	require.NotNil(t, row.WorkspaceLinkedBy)
	require.Equal(t, owner, *row.WorkspaceLinkedBy)
	require.Equal(t, spaceID, row.SpaceID)

	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	seen := make(map[string]int, len(members))
	for _, member := range members {
		seen[member.UID]++
	}
	require.Equal(t, map[string]int{owner: 1, activeA: 1, activeB: 1, explicit: 1}, seen)

	// Snapshot is one-time: later Workspace membership changes neither add nor
	// remove native group rows.
	addContractWorkspaceMembers(t, ctx, owner, ws.WorkspaceID, later)
	_, err = ctx.DB().Update("octo_workspace_member").Set("status", 0).
		Where("workspace_id=? AND uid=?", ws.WorkspaceID, activeA).Exec()
	require.NoError(t, err)
	members, err = g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	seen = make(map[string]int, len(members))
	for _, member := range members {
		seen[member.UID]++
	}
	require.Equal(t, map[string]int{owner: 1, activeA: 1, activeB: 1, explicit: 1}, seen)
	addContractWorkspaceMembers(t, ctx, owner, ws.WorkspaceID, systemUID)

	var beforeRejected int64
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("`group`").LoadOne(&beforeRejected))
	_, err = g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     owner,
		Members:     []string{"gw-snapshot-missing"},
		WorkspaceID: ws.WorkspaceID,
	})
	require.ErrorIs(t, err, workspace.ErrCandidateIneligible)
	var afterRejected int64
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("`group`").LoadOne(&afterRejected))
	require.Equal(t, beforeRejected, afterRejected, "missing explicit union members must reject before any group row is written")

	_, err = ctx.DB().Update("app_config").Set("invite_system_account_join_group_on", 0).Exec()
	require.NoError(t, err)
	var before int64
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("`group`").LoadOne(&before))
	_, err = g.groupService.CreateGroup(&CreateGroupServiceReq{Creator: owner, WorkspaceID: ws.WorkspaceID})
	require.Error(t, err, "system accounts discovered in a Workspace snapshot must obey the group policy")
	var after int64
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("`group`").LoadOne(&after))
	require.Equal(t, before, after)
}
