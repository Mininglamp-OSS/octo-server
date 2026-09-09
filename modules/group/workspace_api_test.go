package group

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
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

func TestGroupWorkspaceUnboundGetRechecksSpaceAccessAfterMiddleware(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Context, string, string) error
	}{
		{
			name: "account disabled",
			mutate: func(ctx *config.Context, owner, _ string) error {
				_, err := ctx.DB().Update("user").Set("status", 0).Where("uid=?", owner).Exec()
				return err
			},
		},
		{
			name: "Space disabled",
			mutate: func(ctx *config.Context, _, spaceID string) error {
				_, err := ctx.DB().Update("space").Set("status", 2).Where("space_id=?", spaceID).Exec()
				return err
			},
		},
		{
			name: "seat revoked",
			mutate: func(ctx *config.Context, owner, spaceID string) error {
				_, err := ctx.DB().Update("space_member").Set("status", 0).
					Where("space_id=? AND uid=?", spaceID, owner).Exec()
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx, g := setupGroupWorkspaceContract(t)
			owner := "gw-ub-owner"
			member := "gw-ub-member"
			spaceID := "gw-ub-space"
			seedGroupWorkspaceUsers(t, g, owner, member)
			seedSpaceWithMembers(t, ctx, spaceID, owner, member)
			groupNo := createGroupWorkspace(t, g, owner, spaceID, member)
			token := groupWorkspaceContractToken(t, ctx, owner)

			entered := make(chan struct{})
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()

			route := wkhttp.New()
			route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
			route.GET("/v1/groups/:group_no/workspace",
				g.ctx.AuthMiddleware(route),
				workspace.VerifiedSpaceMiddleware(g.ctx, g.resolveGroupWorkspaceSpace),
				func(c *wkhttp.Context) {
					close(entered)
					<-release
					c.Next()
				},
				g.groupWorkspaceGet,
			)

			response := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response <- groupWorkspaceContractJSON(t, route, http.MethodGet,
					"/v1/groups/"+groupNo+"/workspace", token, nil)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach the post-middleware barrier")
			}

			require.NoError(t, tc.mutate(ctx, owner, spaceID))
			close(release)
			released = true

			var rec *httptest.ResponseRecorder
			select {
			case rec = <-response:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not finish")
			}
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			errResp := decodeEnvelope(t, rec.Body.Bytes())
			require.Equal(t, "err.server.workspace.forbidden", errResp.Error.Code)
		})
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
func createNamedBoundGroup(t *testing.T, g *Group, creator, spaceID, workspaceID, name string, members ...string) string {
	t.Helper()
	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: creator,
		Members: members,
		Name:    name,
		SpaceID: spaceID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp)
	require.NotEmpty(t, resp.GroupNo)
	_, err = g.ctx.DB().Update("group").
		Set("workspace_id", workspaceID).
		Set("workspace_linked_by", creator).
		Where("group_no=?", resp.GroupNo).
		Exec()
	require.NoError(t, err)
	return resp.GroupNo
}

func TestWorkspaceGroupLikeEscapesLiteralWildcards(t *testing.T) {
	require.Equal(t, `%literal!!!%!_\\value%`, workspaceGroupLike(`literal!%_\\value`))
}

func TestGroupWorkspaceListMatchesLiteralKeywordCountAndList(t *testing.T) {
	s, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-like-owner"
		member  = "gw-like-member"
		spaceID = "gw-like-space"
	)
	seedGroupWorkspaceUsers(t, g, owner, member)
	seedSpaceWithMembers(t, ctx, spaceID, owner, member)
	ws := createContractWorkspace(t, ctx, owner, spaceID, "Like workspace")
	for _, name := range []string{"literal % group", "literal _ group", "literal ! group", `literal \ group`} {
		createNamedBoundGroup(t, g, owner, spaceID, ws.WorkspaceID, name, member)
	}
	token := groupWorkspaceContractToken(t, ctx, owner)
	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)
	var originalMode string
	require.NoError(t, ctx.DB().SelectBySql("SELECT @@SESSION.sql_mode").LoadOne(&originalMode))
	t.Cleanup(func() {
		_, restoreErr := ctx.DB().Exec("SET SESSION sql_mode = ?", originalMode)
		require.NoError(t, restoreErr)
		bindTestDBPool(ctx)
	})
	modeParts := make([]string, 0)
	for _, part := range strings.Split(originalMode, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !strings.EqualFold(part, "NO_BACKSLASH_ESCAPES") {
			modeParts = append(modeParts, part)
		}
	}
	defaultMode := strings.Join(modeParts, ",")
	noBackslashMode := defaultMode
	if noBackslashMode != "" {
		noBackslashMode += ","
	}
	noBackslashMode += "NO_BACKSLASH_ESCAPES"
	setMode := func(mode string) {
		_, err := ctx.DB().Exec("SET SESSION sql_mode = ?", mode)
		require.NoError(t, err)
	}
	for _, mode := range []string{defaultMode, noBackslashMode} {
		setMode(mode)
		for _, keyword := range []string{"%", "_", "!", `\`} {
			path := "/v1/groups?workspace_id=" + url.QueryEscape(ws.WorkspaceID) +
				"&keyword=" + url.QueryEscape(keyword) + "&page_index=1&page_size=15"
			resp := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet, path, token, nil)
			require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
			var listed struct {
				Count int64            `json:"count"`
				List  []GroupWorkspace `json:"list"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &listed))
			require.EqualValues(t, 1, listed.Count, "mode %q keyword %q", mode, keyword)
			require.Len(t, listed.List, 1, "mode %q keyword %q count/list mismatch", mode, keyword)
			require.Contains(t, listed.List[0].Name, keyword)
		}
	}
}

func TestGroupWorkspaceGetAndListUseOneReadConnection(t *testing.T) {
	s, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-one-connection-owner"
		member  = "gw-one-connection-member"
		spaceID = "gw-one-connection-space"
	)
	seedGroupWorkspaceUsers(t, g, owner, member)
	seedSpaceWithMembers(t, ctx, spaceID, owner, member)
	ws := createContractWorkspace(t, ctx, owner, spaceID, "One connection workspace")
	groupNo := createNamedBoundGroup(t, g, owner, spaceID, ws.WorkspaceID, "One connection group", member)
	token := groupWorkspaceContractToken(t, ctx, owner)
	ctx.DB().SetMaxOpenConns(1)

	get := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/groups/"+groupNo+"/workspace", token, nil)
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	list := groupWorkspaceContractJSON(t, s.GetRoute(), http.MethodGet,
		"/v1/groups?workspace_id="+url.QueryEscape(ws.WorkspaceID)+"&page_index=1&page_size=15",
		token, nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
}

func TestGroupMyMemberCountFailureStillReturnsRows(t *testing.T) {
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawDB.Close() })
	conn := &dbr.Connection{
		DB:            rawDB,
		EventReceiver: &dbr.NullEventReceiver{},
		Dialect:       dialect.MySQL,
	}
	g := &Group{
		db:  &DB{session: conn.NewSession(nil)},
		Log: log.NewTLog("Group"),
	}
	mock.ExpectQuery("SELECT group_no, role").
		WillReturnRows(sqlmock.NewRows([]string{"group_no", "role"}).AddRow("group-1", MemberRoleCommon))
	mock.ExpectQuery("SELECT group_no, COUNT").
		WillReturnError(errors.New("count query failed"))

	wk := wkhttp.New()
	wk.GET("/group-my", func(c *wkhttp.Context) {
		g.respondGroupMyModels(c, "uid", []*Model{{GroupNo: "group-1", Name: "group"}}, true, false)
	})
	req := httptest.NewRequest(http.MethodGet, "/group-my", nil)
	rec := httptest.NewRecorder()
	wk.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []struct {
		GroupNo     string `json:"group_no"`
		MemberCount int    `json:"member_count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "group-1", rows[0].GroupNo)
	require.Zero(t, rows[0].MemberCount)
	require.NoError(t, mock.ExpectationsWereMet())
}
