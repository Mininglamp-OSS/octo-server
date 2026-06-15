package app_bot

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppBotPlatformRouteScope_E2E exercises the cross-tenant scope guard against
// the FULL server + real MySQL — NewTestServer runs the app_bot migrations and
// the real AuthMiddleware, unlike the sqlmock route harness. It proves
// end-to-end that an admin cannot reveal a SPACE bot's token through the platform
// route, while a genuine PLATFORM bot's token IS revealed (the guard does not
// over-block the intended delegation).
func TestAppBotPlatformRouteScope_E2E(t *testing.T) {
	// The full server boots every module; common's app-config init refuses to
	// start without a master key.
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	// space-scoped bot (belongs to space "X") + platform bot, seeded directly.
	_, err := ctx.DB().InsertInto("app_bot").
		Columns("id", "uid", "display_name", "token", "created_by", "scope", "space_id", "status").
		Values("space-bot-e2e", "app_space-bot-e2e_bot", "SpaceBot", "space-secret-token-e2e", "creator", "space", "X", 1).
		Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("app_bot").
		Columns("id", "uid", "display_name", "token", "created_by", "scope", "space_id", "status").
		Values("plat-bot-e2e", "app_plat-bot-e2e_bot", "PlatBot", "plat-token-e2e", "creator", "platform", "", 1).
		Exec()
	require.NoError(t, err)

	// Log in as a system admin. The test server wires no RoleResolver, so the
	// token's baked role is authoritative.
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.Admin)))

	reveal := func(id string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/v1/admin/app_bot/"+id+"/token/reveal", nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// IDOR: admin must NOT reveal a space bot's token via the platform route.
	wSpace := reveal("space-bot-e2e")
	assert.Equal(t, http.StatusNotFound, wSpace.Code, "platform route must reject a space bot")
	assert.NotContains(t, wSpace.Body.String(), "space-secret-token-e2e",
		"a space bot's token must never be revealed cross-tenant")

	// Positive: admin CAN reveal a genuine platform bot's token.
	wPlat := reveal("plat-bot-e2e")
	assert.Equal(t, http.StatusOK, wPlat.Code, "admin may reveal a platform bot")
	assert.Contains(t, wPlat.Body.String(), "plat-token-e2e", "platform bot token is returned")
}
