package workspace_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	workspacemod "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

var (
	workspaceTestServer  *server.Server
	workspaceTestContext *config.Context
)

func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		_ = os.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	}
	workspaceTestServer, workspaceTestContext = testutil.NewTestServer()
	bindWorkspaceTestDBPool(workspaceTestContext)
	workspaceTestServer.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	os.Exit(m.Run())
}

func bindWorkspaceTestDBPool(ctx *config.Context) {
	conn := ctx.DB()
	conn.SetMaxOpenConns(12)
	conn.SetMaxIdleConns(2)
	conn.SetConnMaxIdleTime(time.Second)
	conn.SetConnMaxLifetime(30 * time.Second)
}

func setupWorkspaceTest(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	require.NotNil(t, workspaceTestServer)
	require.NotNil(t, workspaceTestContext)
	require.NoError(t, testutil.CleanAllTables(workspaceTestContext))
	resetWorkspaceUIDRateLimit(t, workspaceTestContext)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(workspaceTestContext)
	})
	return workspaceTestServer, workspaceTestContext
}

func resetWorkspaceUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	cfg := ctx.GetConfig()
	client := redis.NewClient(&redis.Options{Addr: cfg.DB.RedisAddr, Password: cfg.DB.RedisPass})
	defer client.Close()
	keys, err := client.Keys("ratelimit:uid:*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, client.Del(keys...).Err())
	}
}

func seedWorkspaceUser(t *testing.T, ctx *config.Context, uid, name string) {
	t.Helper()
	shortNo := "ws-" + strings.ReplaceAll(uid, "_", "")
	if len(shortNo) > 40 {
		shortNo = shortNo[:40]
	}
	require.NoError(t, user.NewDB(ctx).Insert(&user.Model{UID: uid, Name: name, ShortNo: shortNo, Status: 1}))
}

func seedWorkspaceSpace(t *testing.T, ctx *config.Context, spaceID string, uids ...string) {
	t.Helper()
	require.NotEmpty(t, uids)
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Workspace test "+spaceID, uids[0], 1).Exec()
	require.NoError(t, err)
	for _, uid := range uids {
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(spaceID, uid, 0, 1).Exec()
		require.NoError(t, err)
	}
}

func workspaceToken(t *testing.T, ctx *config.Context, uid string) string {
	t.Helper()
	token := fmt.Sprintf("workspace-test-%s-%d", uid, time.Now().UnixNano())
	cfg := ctx.GetConfig()
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+token, uid+"@workspace-test"))
	return token
}

func doWorkspaceJSON(t *testing.T, handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	return doWorkspaceJSONWithHeaders(t, handler, method, path, token, body, nil)
}

func doWorkspaceJSONWithHeaders(t *testing.T, handler http.Handler, method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
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
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if token == "" {
		parts := strings.Fields(req.Header.Get("Authorization"))
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			req.Header.Set("token", parts[1])
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeWorkspaceError(t *testing.T, rec *httptest.ResponseRecorder) (string, int) {
	t.Helper()
	var body struct {
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.HTTPStatus
}

func createWorkspace(t *testing.T, ctx *config.Context, token, spaceID, name string) workspacemod.Workspace {
	t.Helper()
	rec := doWorkspaceJSON(t, workspaceTestServer.GetRoute(), http.MethodPost, "/v1/workspaces", token, map[string]any{
		"space_id": spaceID,
		"name":     name,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got workspacemod.Workspace
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotEmpty(t, got.WorkspaceID)
	require.Equal(t, spaceID, got.SpaceID)
	return got
}

func createWorkspaceService(t *testing.T, ctx *config.Context, ownerUID, spaceID, name string) *workspacemod.Workspace {
	t.Helper()
	svc := workspacemod.NewService(ctx)
	ws, err := svc.Create(workspacemod.Scope{UID: ownerUID}, workspacemod.CreateRequest{
		SpaceID: spaceID,
		Name:    name,
	})
	require.NoError(t, err)
	require.NotNil(t, ws)
	return ws
}

func workspaceService(t *testing.T, ctx *config.Context) *workspacemod.Service {
	t.Helper()
	return workspacemod.NewService(ctx)
}
