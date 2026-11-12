package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex.mlamp.cn/dmwork/octo-auth-sdk/contract"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/stretchr/testify/require"
)

const (
	userCLIV2ServiceSecret = "cliv2-user-test-service-secret-at-least-32-bytes"
	userCLIV2BusinessToken = "cliv2-user-business-token"
)

func userCLIV2Router(t *testing.T, u *User, spaceID string, actions ...string) *wkhttp.WKHttp {
	return userCLIV2RouterWithBindings(t, u, spaceID, nil, actions...)
}

func userCLIV2RouterWithBindings(t *testing.T, u *User, spaceID string, botBindings []contract.BotBinding, actions ...string) *wkhttp.WKHttp {
	t.Helper()
	source := wkhttp.New()
	u.registerCLIV2Routes(source)
	if botBindings == nil {
		botBindings = make([]contract.BotBinding, 0)
	}
	access := contract.AccessContext{
		Principal: contract.Principal{
			TokenID: "user-cli-token", SpaceID: spaceID, OwnerID: testutil.UID,
			OwnerName: "User One", SubjectType: "user", SubjectID: testutil.UID,
		},
		Permissions: []contract.PermissionGroup{{Domain: "im.users", Actions: []string{"im.users:read"}}},
		BotBindings: botBindings,
	}
	for _, action := range actions {
		if !strings.HasPrefix(action, "im.users:") {
			access.Permissions = append(access.Permissions, contract.PermissionGroup{Domain: action[:strings.LastIndex(action, ":")], Actions: []string{action}})
		}
	}
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.Data[contract.AccessContext]{Data: access})
	}))
	t.Cleanup(authServer.Close)

	target, err := cliv2.NewRouter(authServer.URL, userCLIV2ServiceSecret)
	require.NoError(t, err)
	require.NoError(t, cliv2.SetUIDRateLimiter(target, func(c *wkhttp.Context) {
		if c.GetLoginUID() == "" {
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		c.Next()
	}))
	require.NoError(t, cliv2.Mount(source, target))
	return target
}

func userCLIV2Get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+userCLIV2BusinessToken)
	h.ServeHTTP(recorder, req)
	return recorder
}

func TestCLIV2UserDetailHidesTargetOutsideBoundSpace(t *testing.T) {
	_, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	require.NoError(t, NewService(ctx).AddUser(&AddUserReq{UID: "ut_cli_foreign", Name: "Foreign"}))
	seedSpace(t, ctx, "sp_cli_owner", 1)
	seedSpaceMemberRow(t, ctx, "sp_cli_owner", testutil.UID)
	seedSpace(t, ctx, "sp_cli_foreign", 1)
	seedSpaceMemberRow(t, ctx, "sp_cli_foreign", "ut_cli_foreign")

	h := userCLIV2Router(t, New(ctx), "sp_cli_owner", "im.users:read")
	w := userCLIV2Get(t, h, "/v2/cli/users/ut_cli_foreign")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, "NOT_FOUND", got.Error.Code)
}

func TestCLIV2SpaceMembersUsesBoundSpaceAndOffsetPagination(t *testing.T) {
	_, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	require.NoError(t, NewService(ctx).AddUser(&AddUserReq{UID: "ut_cli_space_a", Name: "Space A"}))
	require.NoError(t, NewService(ctx).AddUser(&AddUserReq{UID: "ut_cli_space_b", Name: "Space B"}))
	seedSpace(t, ctx, "sp_cli_owner", 1)
	seedSpaceMemberRow(t, ctx, "sp_cli_owner", testutil.UID)
	seedSpaceMemberRow(t, ctx, "sp_cli_owner", "ut_cli_space_a")
	seedSpace(t, ctx, "sp_cli_other", 1)
	seedSpaceMemberRow(t, ctx, "sp_cli_other", "ut_cli_space_b")

	h := userCLIV2Router(t, New(ctx), "sp_cli_owner", "im.spaces:read")
	w := userCLIV2Get(t, h, "/v2/cli/space/members?space_id=sp_cli_other&page=1&page_size=1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got struct {
		Data []struct {
			UID string `json:"uid"`
		} `json:"data"`
		Pagination struct {
			Total    int `json:"total"`
			Page     int `json:"page"`
			PageSize int `json:"page_size"`
		} `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Data, 1)
	require.Equal(t, 2, got.Pagination.Total, "total must count only active members in the bound Space")
	require.Equal(t, 1, got.Pagination.Page)
	require.Equal(t, 1, got.Pagination.PageSize)
	firstUID := got.Data[0].UID

	w = userCLIV2Get(t, h, "/v2/cli/space/members?space_id=sp_cli_other&page=2&page_size=1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Data, 1)
	require.NotEqual(t, firstUID, got.Data[0].UID)
	require.Equal(t, 2, got.Pagination.Total)
	require.Equal(t, 2, got.Pagination.Page)
	require.Equal(t, 1, got.Pagination.PageSize)

	w = userCLIV2Get(t, h, "/v2/cli/space/members?space_id=sp_cli_other")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Data, 2)
	require.Equal(t, 1, got.Pagination.Page)
	require.Equal(t, 20, got.Pagination.PageSize)

	w = userCLIV2Get(t, h, "/v2/cli/space/members?page_size=101")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
func TestCLIV2SpaceMembersRequireTokenOwnerSpaceMembership(t *testing.T) {
	_, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	const (
		spaceID  = "sp_cli_owner_revoked_members"
		botID    = "bot_cli_owner_revoked_members"
		memberID = "ut_cli_owner_revoked_member"
	)
	require.NoError(t, NewService(ctx).AddUser(&AddUserReq{UID: memberID, Name: "Current member"}))
	seedCLIV2OwnerRevokedSpace(t, ctx, spaceID, botID, memberID)
	h := userCLIV2RouterWithBindings(t, New(ctx), spaceID,
		[]contract.BotBinding{{BotID: botID, BotName: "Bound Bot"}},
		"im.spaces:read",
	)

	w := userCLIV2Get(t, h, "/v2/cli/space/members")
	requireUserCLIV2Error(t, w, http.StatusForbidden, "FORBIDDEN")
}

func TestCLIV2UserDetailRequiresTokenOwnerSpaceMembership(t *testing.T) {
	_, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	const (
		spaceID  = "sp_cli_owner_revoked_user"
		botID    = "bot_cli_owner_revoked_user"
		memberID = "ut_cli_owner_revoked_user"
	)
	require.NoError(t, NewService(ctx).AddUser(&AddUserReq{UID: memberID, Name: "Current member"}))
	seedCLIV2OwnerRevokedSpace(t, ctx, spaceID, botID, memberID)
	h := userCLIV2RouterWithBindings(t, New(ctx), spaceID,
		[]contract.BotBinding{{BotID: botID, BotName: "Bound Bot"}},
		"im.users:read",
	)

	// The target remains a Space member, so this specifically guards the
	// revoked Token-owner boundary rather than target-UID hiding.
	w := userCLIV2Get(t, h, "/v2/cli/users/"+memberID)
	requireUserCLIV2Error(t, w, http.StatusForbidden, "FORBIDDEN")
}

func seedCLIV2OwnerRevokedSpace(t *testing.T, ctx *config.Context, spaceID, botID, memberID string) {
	t.Helper()
	seedSpace(t, ctx, spaceID, 1)
	seedSpaceMemberRow(t, ctx, spaceID, botID)
	seedSpaceMemberRow(t, ctx, spaceID, memberID)
	seedRevokedSpaceMemberRow(t, ctx, spaceID, testutil.UID)
}

func seedRevokedSpaceMemberRow(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, version) VALUES (?,?,0,0,2)", spaceID, uid,
	).Exec()
	require.NoError(t, err)
}

func requireUserCLIV2Error(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, w.Code, w.Body.String())
	var got struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, code, got.Error.Code)
	require.NotEmpty(t, got.Error.Message)
	require.NotNil(t, got.Error.Details)
}
