package app_bot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/botutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConnectInfo_DefaultAndOverride pins the value contract of the connect
// object: plugin_package is the backend default (overridable by env) and api_url
// is the configured public Bot API entry — never the admin origin.
func TestConnectInfo_DefaultAndOverride(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.External.BaseURL = "https://bot-api.example.com"
	ab := &AppBot{ctx: config.NewContext(cfg)}

	info := ab.connectInfo()
	assert.Equal(t, "create-openclaw-octo", info["plugin_package"], "default plugin package")
	assert.Equal(t, "https://bot-api.example.com", info["api_url"], "api_url is the configured Bot API entry")
	// connect carries data only — no token / secret keys.
	assert.Len(t, info, 2, "connect must contain exactly plugin_package + api_url")

	t.Setenv(botutil.PluginPackageEnv, "create-openclaw-canary")
	info = ab.connectInfo()
	assert.Equal(t, "create-openclaw-canary", info["plugin_package"], "env override flows through")
}

// serveGetBotDetail mounts the platform admin route with a published platform bot
// stubbed in sqlmock and returns the recorded GET /v1/admin/app_bot/{id} response.
func serveGetBotDetail(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	route, mock, cleanup := newPlatformGateRouteWithMock(t, string(wkhttp.Admin))
	t.Cleanup(cleanup)

	// No WithArgs: dbr may interpolate the WHERE value into the SQL (0 bound
	// args) rather than bind it, depending on process state; this test asserts
	// the connect payload, not the WHERE clause, so match on the query shape only.
	mock.ExpectQuery("app_bot").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "uid", "display_name", "description", "scope", "space_id", "status", "token", "welcome_msg", "created_by"}).
			AddRow(id, "app_"+id+"_bot", "My Bot", "desc", "platform", "", 1, "super-secret-token", "", "creator-1"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/app_bot/"+id, nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	return w
}

// TestGetBotDetail_IncludesConnect is the read-path proof: getBotDetail backs
// both GET /v1/admin/app_bot/:id and GET /v1/space/:space_id/app_bot/:id, and
// must return a connect object that carries no secret — and, via the env
// override, that the package name is server-configured (the core of #446). The
// create path (POST) is pinned separately by TestCreatePlatformBot_IncludesConnect_E2E.
func TestGetBotDetail_IncludesConnect(t *testing.T) {
	w := serveGetBotDetail(t, "bot-1")
	require.Equal(t, http.StatusOK, w.Code, "admin reaches the bot detail")

	var resp struct {
		Token   string         `json:"token"`
		Connect map[string]any `json:"connect"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.NotNil(t, resp.Connect, "response carries a connect object")
	assert.Equal(t, "create-openclaw-octo", resp.Connect["plugin_package"])
	assert.NotEmpty(t, resp.Connect["api_url"], "api_url resolves to the public Bot API entry")
	// No secret leaks into connect: exactly the two public keys, and the raw
	// token never appears there (the top-level token stays masked).
	assert.Len(t, resp.Connect, 2)
	assert.NotContains(t, resp.Connect, "token")
	assert.NotEqual(t, "super-secret-token", resp.Connect["api_url"])
}

func TestGetBotDetail_ConnectHonorsPackageOverride(t *testing.T) {
	t.Setenv(botutil.PluginPackageEnv, "create-openclaw-canary")
	w := serveGetBotDetail(t, "bot-2")
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Connect map[string]any `json:"connect"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "create-openclaw-canary", resp.Connect["plugin_package"],
		"a configured package name reaches the client without a frontend change")
}

// TestCreatePlatformBot_IncludesConnect_E2E pins the create-side wire contract
// (POST /v1/admin/app_bot) against the full server + real MySQL/IM
// (NewTestServer): a successful create returns the same connect object, data
// only, with the freshly-minted token never leaking into connect. Complements
// the read-path coverage in TestGetBotDetail_IncludesConnect.
func TestCreatePlatformBot_IncludesConnect_E2E(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	// Log in as a system admin (the test server wires no RoleResolver, so the
	// token's baked role is authoritative).
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.Admin)))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/admin/app_bot",
		strings.NewReader(`{"id":"connect-create-e2e","display_name":"Connect Create"}`))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "admin create should succeed: %s", w.Body.String())

	var resp struct {
		Token   string         `json:"token"`
		Connect map[string]any `json:"connect"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Connect, "create response carries a connect object")
	assert.Equal(t, "create-openclaw-octo", resp.Connect["plugin_package"])
	assert.NotEmpty(t, resp.Connect["api_url"], "api_url resolves to the public Bot API entry")
	// Data only: exactly the two public keys, and the freshly-minted token
	// (returned at top level on create) must never appear inside connect.
	assert.Len(t, resp.Connect, 2)
	assert.NotContains(t, resp.Connect, "token")
	assert.NotEqual(t, resp.Token, resp.Connect["api_url"])
	assert.NotEqual(t, resp.Token, resp.Connect["plugin_package"])
}
