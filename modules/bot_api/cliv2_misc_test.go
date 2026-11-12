package bot_api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex.mlamp.cn/dmwork/octo-auth-sdk/contract"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/stretchr/testify/require"
)

const (
	botCLIV2ServiceSecret = "cliv2-bot-test-service-secret-at-least-32-bytes"
	botCLIV2BusinessToken = "cliv2-bot-business-token"
)

type botCLIV2Harness struct {
	target     *wkhttp.WKHttp
	limiterUID string
}

func newBotCLIV2Harness(t *testing.T, source *wkhttp.WKHttp, spaceID, botID string, actions ...string) *botCLIV2Harness {
	t.Helper()
	bindings := make([]contract.BotBinding, 0)
	if botID != "" {
		bindings = append(bindings, contract.BotBinding{BotID: botID, BotName: "Bound Bot"})
	}
	groups := make([]contract.PermissionGroup, 0, len(actions))
	for _, action := range actions {
		groups = append(groups, contract.PermissionGroup{
			Domain: action[:strings.LastIndex(action, ":")], Actions: []string{action},
		})
	}
	access := contract.AccessContext{
		Principal: contract.Principal{
			TokenID: "bot-cli-token", SpaceID: spaceID, OwnerID: testutil.UID,
			OwnerName: "Token User", SubjectType: "user", SubjectID: testutil.UID,
		},
		Permissions: groups,
		BotBindings: bindings,
	}
	h := &botCLIV2Harness{}
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(contract.Data[contract.AccessContext]{Data: access})
	}))
	t.Cleanup(authServer.Close)

	var err error
	h.target, err = cliv2.NewRouter(authServer.URL, botCLIV2ServiceSecret)
	require.NoError(t, err)
	require.NoError(t, cliv2.SetUIDRateLimiter(h.target, func(c *wkhttp.Context) {
		h.limiterUID = c.GetLoginUID()
		if h.limiterUID == "" {
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		c.Next()
	}))
	require.NoError(t, cliv2.Mount(source, h.target))
	return h
}

func newBotCLIV2Source(ctx *config.Context) *wkhttp.WKHttp {
	source := wkhttp.New()
	user.New(ctx).Route(source)
	file.New(ctx).Route(source)
	NewBotAPI(ctx).registerCLIMiscRoutes(source)
	return source
}

func botCLIV2Request(t *testing.T, h http.Handler, method, path, contentType string, body *bytes.Buffer) *httptest.ResponseRecorder {
	return botCLIV2RequestWithHeaders(t, h, method, path, contentType, body, nil)
}

func botCLIV2RequestWithHeaders(t *testing.T, h http.Handler, method, path, contentType string, body *bytes.Buffer, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body.Bytes())
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+botCLIV2BusinessToken)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	h.ServeHTTP(recorder, req)
	return recorder
}

func TestCLIV2IdentityReturnsTokenUserAndBoundBot(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	source := newBotCLIV2Source(ctx)
	h := newBotCLIV2Harness(t, source, "sp_cli_identity", "bot_cli_identity", "im.identity:read")

	w := botCLIV2Request(t, h.target, http.MethodGet, "/v2/cli/identity", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got struct {
		Data struct {
			UserID   string `json:"user_id"`
			UserName string `json:"user_name"`
			BotID    string `json:"bot_id"`
			SpaceID  string `json:"space_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, testutil.UID, got.Data.UserID)
	require.Equal(t, "Token User", got.Data.UserName)
	require.Equal(t, "bot_cli_identity", got.Data.BotID)
	require.Equal(t, "bot_cli_identity", h.limiterUID)
}

func TestCLIV2SpaceMembersCannotOverrideBoundSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{UID: testutil.UID, Name: "Token User"}))
	for _, spaceID := range []string{"sp_cli_bound", "sp_cli_other"} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO space (space_id, name, creator, status, version) VALUES (?,?,?,1,1)",
			spaceID, spaceID, testutil.UID,
		).Exec()
		require.NoError(t, err)
	}
	for _, member := range []struct{ spaceID, uid string }{
		{"sp_cli_bound", testutil.UID}, {"sp_cli_bound", "ut_cli_member_a"},
		{"sp_cli_bound", "bot_cli_bound"}, {"sp_cli_other", "ut_cli_member_b"},
	} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status, version) VALUES (?,?,0,1,1)",
			member.spaceID, member.uid,
		).Exec()
		require.NoError(t, err)
	}

	source := newBotCLIV2Source(ctx)
	h := newBotCLIV2Harness(t, source, "sp_cli_bound", "bot_cli_bound", "im.spaces:read")
	w := botCLIV2Request(t, h.target, http.MethodGet,
		"/v2/cli/space/members?space_id=sp_cli_other&page=1&page_size=100", "", nil)
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
	require.Equal(t, 3, got.Pagination.Total)
	require.Equal(t, 1, got.Pagination.Page)
	require.Equal(t, 100, got.Pagination.PageSize)
	members := make(map[string]bool, len(got.Data))
	for _, member := range got.Data {
		members[member.UID] = true
	}
	require.True(t, members[testutil.UID])
	require.True(t, members["bot_cli_bound"])
	require.True(t, members["ut_cli_member_a"])
	require.False(t, members["ut_cli_member_b"], "the caller-supplied Space must not override the authenticated binding")
}
func TestCLIV2DownloadRetainsLegacyRedirect(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().FileService = "seaweedfs"
	ctx.GetConfig().Seaweed.URL = "https://storage.example"

	for _, botID := range []string{"", "bot_cli_files"} {
		t.Run("actor="+botID, func(t *testing.T) {
			source := newBotCLIV2Source(ctx)
			h := newBotCLIV2Harness(t, source, "sp_cli_files", botID, "im.files:read")
			w := botCLIV2Request(t, h.target, http.MethodGet,
				"/v2/cli/files/_download?path=chat/2026/report.pdf", "", nil)
			require.Equal(t, http.StatusFound, w.Code, w.Body.String())
			require.Equal(t, "https://storage.example/chat/2026/report.pdf", w.Header().Get("Location"))
			require.Contains(t, w.Header().Get("Content-Type"), "text/html")

			w = botCLIV2Request(t, h.target, http.MethodGet,
				"/v2/cli/files/_download?path=..%2Fsecret", "", nil)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}

	source := newBotCLIV2Source(ctx)
	h := newBotCLIV2Harness(t, source, "sp_cli_files", "")
	w := botCLIV2Request(t, h.target, http.MethodGet,
		"/v2/cli/files/_download?path=chat/2026/report.pdf", "", nil)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
func TestCLIV2DownloadIgnoresForeignSpaceSelectors(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().FileService = "seaweedfs"
	ctx.GetConfig().Seaweed.URL = "https://storage.example"

	const foreignSpaceID = "sp_cli_foreign_download"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, version) VALUES (?,?,?,1,1)",
		foreignSpaceID, foreignSpaceID, testutil.UID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, version) VALUES (?,?,0,1,1)",
		foreignSpaceID, "ut_cli_other_download",
	).Exec()
	require.NoError(t, err)

	source := newBotCLIV2Source(ctx)
	h := newBotCLIV2Harness(t, source, "sp_cli_bound_download", "bot_cli_download", "im.files:read")
	validPath := "/v2/cli/files/_download?path=chat/2026/report.pdf"
	wantLocation := "https://storage.example/chat/2026/report.pdf"
	for _, tc := range []struct {
		name           string
		path           string
		headers        map[string]string
		invalidPath    string
		invalidHeaders map[string]string
	}{
		{
			name:        "query space_id",
			path:        validPath + "&space_id=" + foreignSpaceID,
			invalidPath: "/v2/cli/files/_download?path=..%2Fsecret&space_id=" + foreignSpaceID,
		},
		{
			name:           "X-Space-ID header",
			path:           validPath,
			headers:        map[string]string{"X-Space-ID": foreignSpaceID},
			invalidPath:    "/v2/cli/files/_download?path=..%2Fsecret",
			invalidHeaders: map[string]string{"X-Space-ID": foreignSpaceID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := botCLIV2RequestWithHeaders(t, h.target, http.MethodGet, tc.path, "", nil, tc.headers)
			require.Equal(t, http.StatusFound, w.Code, w.Body.String())
			require.Equal(t, wantLocation, w.Header().Get("Location"))
			require.Contains(t, w.Header().Get("Content-Type"), "text/html")

			w = botCLIV2RequestWithHeaders(t, h.target, http.MethodGet, tc.invalidPath, "", nil, tc.invalidHeaders)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Contains(t, w.Header().Get("Content-Type"), "application/json")
			var got struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
				LegacyMessage string `json:"msg"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			require.Equal(t, "VALIDATION_ERROR", got.Error.Code)
			require.Empty(t, got.LegacyMessage, "v2 download errors must not use the legacy raw envelope")
		})
	}
}

func TestCLIV2BotFileOperationsRejectCallerSelectedKeys(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	source := newBotCLIV2Source(ctx)
	h := newBotCLIV2Harness(t, source, "sp_cli_files", "bot_cli_files", "im.files:write")

	t.Run("multipart upload", func(t *testing.T) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		require.NoError(t, form.Close())
		w := botCLIV2Request(t, h.target, http.MethodPost,
			"/v2/cli/files?path=chat/sp_other/known-object", form.FormDataContentType(), &body)
		requireBotCLIV2ObjectKeyRejected(t, w)
	})
	t.Run("STS credentials", func(t *testing.T) {
		w := botCLIV2Request(t, h.target, http.MethodGet,
			"/v2/cli/uploads/credentials?filename=report.pdf&path=chat/sp_other/known-object", "", nil)
		requireBotCLIV2ObjectKeyRejected(t, w)
	})
	t.Run("presigned upload", func(t *testing.T) {
		w := botCLIV2Request(t, h.target, http.MethodGet,
			"/v2/cli/uploads/_presigned?filename=report.pdf&file_size=1&path=chat/sp_other/known-object", "", nil)
		requireBotCLIV2ObjectKeyRejected(t, w)
	})
	t.Run("User file route also rejects caller-selected object keys", func(t *testing.T) {
		userSource := newBotCLIV2Source(ctx)
		userHarness := newBotCLIV2Harness(t, userSource, "sp_cli_files", "", "im.files:write")
		w := botCLIV2Request(t, userHarness.target, http.MethodGet,
			"/v2/cli/uploads/credentials?filename=report.pdf&path=chat/sp_other/known-object", "", nil)
		requireBotCLIV2ObjectKeyRejected(t, w)
		require.Equal(t, testutil.UID, userHarness.limiterUID)
	})
}

func requireBotCLIV2ObjectKeyRejected(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, "VALIDATION_ERROR", got.Error.Code)
	require.Equal(t, "Caller-provided object keys are not supported.", got.Error.Message)
}
