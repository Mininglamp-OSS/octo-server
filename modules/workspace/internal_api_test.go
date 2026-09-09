package workspace_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	workspacemod "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

const (
	workspaceInternalLoopToken  = "loop-internal-token-012345678901234567890"
	workspaceInternalDriveToken = "drive-internal-token-012345678901234567890"
)

func internalWorkspaceRouter(t *testing.T, ctx *config.Context) *libwkhttp.WKHttp {
	t.Helper()
	r := libwkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	workspacemod.New(ctx).Route(r)
	return r
}

func resetWorkspaceInternalRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer client.Close()
	keys, err := client.Keys("ratelimit:strict:workspace_internal:*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, client.Del(keys...).Err())
	}
}

func doInternalWorkspaceRequest(t *testing.T, handler http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestWorkspaceInternalAuthenticationMatrix(t *testing.T) {
	cases := []struct {
		name       string
		loopToken  string
		driveToken string
		request    string
		wantStatus int
		wantCode   string
	}{
		{name: "loop", loopToken: workspaceInternalLoopToken, driveToken: workspaceInternalDriveToken, request: workspaceInternalLoopToken, wantStatus: http.StatusOK},
		{name: "drive", loopToken: workspaceInternalLoopToken, driveToken: workspaceInternalDriveToken, request: workspaceInternalDriveToken, wantStatus: http.StatusOK},
		{name: "wrong", loopToken: workspaceInternalLoopToken, driveToken: workspaceInternalDriveToken, request: "wrong-token", wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
		{name: "missing", loopToken: workspaceInternalLoopToken, driveToken: workspaceInternalDriveToken, request: "", wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
		{name: "both-unset", loopToken: "", driveToken: "", request: workspaceInternalLoopToken, wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
		{name: "loop-short", loopToken: "short", driveToken: workspaceInternalDriveToken, request: "short", wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
		{name: "drive-short", loopToken: workspaceInternalLoopToken, driveToken: "short", request: "short", wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := setupWorkspaceTest(t)
			t.Setenv(workspacemod.LoopInternalTokenEnv, tc.loopToken)
			t.Setenv(workspacemod.DriveInternalTokenEnv, tc.driveToken)
			resetWorkspaceInternalRateLimit(t, ctx)
			r := internalWorkspaceRouter(t, ctx)
			rec := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces", tc.request)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantCode != "" {
				code, semantic := decodeWorkspaceError(t, rec)
				require.Equal(t, tc.wantCode, code)
				require.Equal(t, tc.wantStatus, semantic)
			}
		})
	}
}

func TestWorkspaceInternalNotFoundPrivacyAndDTO(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "internal-owner", "Internal owner")
	seedWorkspaceSpace(t, ctx, "internal-space", "internal-owner")
	ws := createWorkspaceService(t, ctx, "internal-owner", "internal-space", "Internal workspace")
	_, err := ctx.DB().Update("octo_workspace").Set("status", workspacemod.WorkspaceStatusArchived).Where("workspace_id=?", ws.WorkspaceID).Exec()
	require.NoError(t, err)

	t.Setenv(workspacemod.LoopInternalTokenEnv, workspaceInternalLoopToken)
	t.Setenv(workspacemod.DriveInternalTokenEnv, workspaceInternalDriveToken)
	resetWorkspaceInternalRateLimit(t, ctx)
	r := internalWorkspaceRouter(t, ctx)
	for _, workspaceID := range []string{ws.WorkspaceID, "unknown-internal-workspace"} {
		for _, suffix := range []string{"", "/members"} {
			rec := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces/"+workspaceID+suffix, workspaceInternalLoopToken)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			code, semantic := decodeWorkspaceError(t, rec)
			require.Equal(t, "err.shared.not_found", code)
			require.Equal(t, http.StatusNotFound, semantic)
		}
	}

	_, ctx = setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "internal-owner-dto", "Internal DTO owner")
	seedWorkspaceSpace(t, ctx, "internal-dto-space", "internal-owner-dto")
	ws = createWorkspaceService(t, ctx, "internal-owner-dto", "internal-dto-space", "Internal DTO workspace")
	t.Setenv(workspacemod.LoopInternalTokenEnv, workspaceInternalLoopToken)
	t.Setenv(workspacemod.DriveInternalTokenEnv, workspaceInternalDriveToken)
	resetWorkspaceInternalRateLimit(t, ctx)
	r = internalWorkspaceRouter(t, ctx)
	rec := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces/"+ws.WorkspaceID, workspaceInternalLoopToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dto map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dto))
	require.NotContains(t, dto, "workspace_role")
	require.Equal(t, ws.WorkspaceID, dto["workspace_id"])
	require.Equal(t, "internal-dto-space", dto["space_id"])
	require.EqualValues(t, 1, dto["member_count"])
}

func TestWorkspaceInternalEnumerationSpaceFilterAndPagination(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, uid := range []string{"enum-owner-a", "enum-owner-b", "enum-owner-c"} {
		seedWorkspaceUser(t, ctx, uid, uid+" name")
	}
	seedWorkspaceSpace(t, ctx, "enum-space-a", "enum-owner-a")
	seedWorkspaceSpace(t, ctx, "enum-space-b", "enum-owner-b")
	seedWorkspaceSpace(t, ctx, "enum-space-c", "enum-owner-c")
	wsA1 := createWorkspaceService(t, ctx, "enum-owner-a", "enum-space-a", "A1")
	wsA2 := createWorkspaceService(t, ctx, "enum-owner-a", "enum-space-a", "A2")
	wsB := createWorkspaceService(t, ctx, "enum-owner-b", "enum-space-b", "B1")
	wsC := createWorkspaceService(t, ctx, "enum-owner-c", "enum-space-c", "C1")

	t.Setenv(workspacemod.LoopInternalTokenEnv, workspaceInternalLoopToken)
	t.Setenv(workspacemod.DriveInternalTokenEnv, workspaceInternalDriveToken)
	resetWorkspaceInternalRateLimit(t, ctx)
	r := internalWorkspaceRouter(t, ctx)

	page1 := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces?space_id=enum-space-a&page_index=1&page_size=1", workspaceInternalDriveToken)
	require.Equal(t, http.StatusOK, page1.Code, page1.Body.String())
	var gotPage1 struct {
		Count int64                            `json:"count"`
		List  []workspacemod.InternalWorkspace `json:"list"`
	}
	require.NoError(t, json.Unmarshal(page1.Body.Bytes(), &gotPage1))
	require.EqualValues(t, 2, gotPage1.Count)
	require.Len(t, gotPage1.List, 1)

	page2 := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces?space_id=enum-space-a&page_index=2&page_size=1", workspaceInternalLoopToken)
	require.Equal(t, http.StatusOK, page2.Code, page2.Body.String())
	var gotPage2 struct {
		Count int64                            `json:"count"`
		List  []workspacemod.InternalWorkspace `json:"list"`
	}
	require.NoError(t, json.Unmarshal(page2.Body.Bytes(), &gotPage2))
	require.EqualValues(t, 2, gotPage2.Count)
	require.Len(t, gotPage2.List, 1)
	require.NotEqual(t, gotPage1.List[0].WorkspaceID, gotPage2.List[0].WorkspaceID)
	require.ElementsMatch(t, []string{wsA1.WorkspaceID, wsA2.WorkspaceID}, []string{gotPage1.List[0].WorkspaceID, gotPage2.List[0].WorkspaceID})

	all := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces?page_index=1&page_size=20", workspaceInternalLoopToken)
	require.Equal(t, http.StatusOK, all.Code, all.Body.String())
	var gotAll struct {
		Count int64                            `json:"count"`
		List  []workspacemod.InternalWorkspace `json:"list"`
	}
	require.NoError(t, json.Unmarshal(all.Body.Bytes(), &gotAll))
	require.EqualValues(t, 4, gotAll.Count)
	require.Len(t, gotAll.List, 4)
	require.ElementsMatch(t, []string{wsA1.WorkspaceID, wsA2.WorkspaceID, wsB.WorkspaceID, wsC.WorkspaceID}, func() []string {
		ids := make([]string, 0, len(gotAll.List))
		for _, item := range gotAll.List {
			ids = append(ids, item.WorkspaceID)
		}
		return ids
	}())
}

func TestWorkspaceInternalMembersPaginationAndFilters(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, uid := range []string{"member-owner", "member-admin", "member-one", "member-two"} {
		seedWorkspaceUser(t, ctx, uid, uid+" name")
	}
	seedWorkspaceSpace(t, ctx, "member-space", "member-owner", "member-admin", "member-one", "member-two")
	ws := createWorkspaceService(t, ctx, "member-owner", "member-space", "Members")
	svc := workspacemod.NewService(ctx)
	_, err := svc.AddMembers(workspacemod.Scope{UID: "member-owner"}, ws.WorkspaceID, []workspacemod.MemberInput{
		{UID: "member-admin", WorkspaceRole: workspacemod.WorkspaceRoleAdmin},
		{UID: "member-one", WorkspaceRole: workspacemod.WorkspaceRoleMember},
		{UID: "member-two", WorkspaceRole: workspacemod.WorkspaceRoleMember},
	})
	require.NoError(t, err)
	_, err = ctx.DB().Update("octo_workspace_member").Set("status", workspacemod.MemberStatusInactive).Where("workspace_id=? AND uid=?", ws.WorkspaceID, "member-two").Exec()
	require.NoError(t, err)

	t.Setenv(workspacemod.LoopInternalTokenEnv, workspaceInternalLoopToken)
	t.Setenv(workspacemod.DriveInternalTokenEnv, workspaceInternalDriveToken)
	resetWorkspaceInternalRateLimit(t, ctx)
	r := internalWorkspaceRouter(t, ctx)

	active := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces/"+ws.WorkspaceID+"/members?page_index=1&page_size=2", workspaceInternalLoopToken)
	require.Equal(t, http.StatusOK, active.Code, active.Body.String())
	var activePage struct {
		Count int64                 `json:"count"`
		List  []workspacemod.Member `json:"list"`
	}
	require.NoError(t, json.Unmarshal(active.Body.Bytes(), &activePage))
	require.EqualValues(t, 3, activePage.Count)
	require.Len(t, activePage.List, 2)
	for _, member := range activePage.List {
		require.NotEqual(t, workspacemod.MemberStatusInactive, member.Status)
	}

	admin := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces/"+ws.WorkspaceID+"/members?workspace_role=admin", workspaceInternalLoopToken)
	require.Equal(t, http.StatusOK, admin.Code, admin.Body.String())
	var adminPage struct {
		Count int64                 `json:"count"`
		List  []workspacemod.Member `json:"list"`
	}
	require.NoError(t, json.Unmarshal(admin.Body.Bytes(), &adminPage))
	require.EqualValues(t, 1, adminPage.Count)
	require.Len(t, adminPage.List, 1)
	require.Equal(t, "member-admin", adminPage.List[0].UID)
	require.Equal(t, workspacemod.WorkspaceRoleAdmin, adminPage.List[0].WorkspaceRole)

	inactive := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces/"+ws.WorkspaceID+"/members?status=0", workspaceInternalDriveToken)
	require.Equal(t, http.StatusOK, inactive.Code, inactive.Body.String())
	var inactivePage struct {
		Count int64                 `json:"count"`
		List  []workspacemod.Member `json:"list"`
	}
	require.NoError(t, json.Unmarshal(inactive.Body.Bytes(), &inactivePage))
	require.EqualValues(t, 1, inactivePage.Count)
	require.Len(t, inactivePage.List, 1)
	require.Equal(t, "member-two", inactivePage.List[0].UID)
	require.Equal(t, workspacemod.MemberStatusInactive, inactivePage.List[0].Status)
}

func TestWorkspaceInternalRateLimit(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	t.Setenv(workspacemod.LoopInternalTokenEnv, workspaceInternalLoopToken)
	t.Setenv(workspacemod.DriveInternalTokenEnv, workspaceInternalDriveToken)
	t.Setenv("DM_WORKSPACE_INTERNAL_IP_RPS", "1")
	t.Setenv("DM_WORKSPACE_INTERNAL_IP_BURST", "1")
	resetWorkspaceInternalRateLimit(t, ctx)
	r := internalWorkspaceRouter(t, ctx)

	first := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces", workspaceInternalLoopToken)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	second := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces", workspaceInternalLoopToken)
	require.Equal(t, http.StatusTooManyRequests, second.Code, second.Body.String())
	code, semantic := decodeWorkspaceError(t, second)
	require.Equal(t, "err.shared.rate.limited", code)
	require.Equal(t, http.StatusTooManyRequests, semantic)
}

func TestWorkspaceInternalTokenValuesNeverLeakInErrors(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	badLoop := strings.Repeat("l", 31)
	badDrive := strings.Repeat("d", 31)
	t.Setenv(workspacemod.LoopInternalTokenEnv, badLoop)
	t.Setenv(workspacemod.DriveInternalTokenEnv, badDrive)
	resetWorkspaceInternalRateLimit(t, ctx)
	r := internalWorkspaceRouter(t, ctx)
	rec := doInternalWorkspaceRequest(t, r, "/v1/internal/workspaces", badLoop)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotContains(t, rec.Body.String(), badLoop)
	require.NotContains(t, rec.Body.String(), badDrive)
	require.NotContains(t, rec.Body.String(), os.Getenv(workspacemod.LoopInternalTokenEnv))
}
