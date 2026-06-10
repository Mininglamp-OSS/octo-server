package botfather

// End-to-end coverage for #335: BotFather's command menu is server-owned copy,
// stored once in the deployment default language (registerBotFatherCommands)
// and re-rendered per request on the menu-bearing read endpoints. These tests
// live in the botfather package — not in channel/user — because this is the
// only package that may import every involved module: user cannot blank-import
// botfather's migrations (botfather → user would cycle), yet GetUserDetail's
// robot-details query needs botfather-owned columns (agent_platform, ...).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather/cmdmenu"
	"github.com/Mininglamp-OSS/octo-server/modules/channel"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

// setupCmdMenuE2E builds the full server and normalizes the BotFather rows the
// module bootstrap created during module.Setup: user.robot=1 (set by the
// deployment seed in production, not by initBotFatherUser) and a deterministic
// zh-CN blob in robot.bot_commands (the bootstrap renders whatever
// OCTO_DEFAULT_LANGUAGE was at setup time). Also creates the login user.
func setupCmdMenuE2E(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()

	svc := user.NewService(ctx)
	assert.NoError(t, svc.AddUser(&user.AddUserReq{UID: testutil.UID, Name: "Login User"}))
	_, err := ctx.DB().UpdateBySql("UPDATE user SET robot=1 WHERE uid=?", BotFatherUID).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().UpdateBySql("UPDATE robot SET bot_commands=? WHERE robot_id=?",
		cmdmenu.JSON("zh-CN"), BotFatherUID).Exec()
	assert.NoError(t, err)

	return s, ctx
}

// channelExtraBotCommands extracts extra.bot_commands from a ChannelResp body.
func channelExtraBotCommands(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Extra map[string]interface{} `json:"extra"`
	}
	assert.NoError(t, json.Unmarshal(body, &resp), "body=%s", body)
	v, _ := resp.Extra["bot_commands"].(string)
	return v
}

// TestChannelGet_BotFatherMenuFollowsDeploymentDefault pins the #335 floor on
// the fully wired route set: with no negotiated request language (testutil's
// router mounts no EarlyMiddleware, mirroring a context-less caller) the
// override renders OCTO_DEFAULT_LANGUAGE — so an en-US deployment no longer
// serves the stored zh-CN blob.
func TestChannelGet_BotFatherMenuFollowsDeploymentDefault(t *testing.T) {
	s, ctx := setupCmdMenuE2E(t)
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	t.Setenv(octoi18n.EnvDefaultLanguage, "en-US")

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/channels/"+BotFatherUID+"/1", nil)
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, cmdmenu.JSON("en-US"), channelExtraBotCommands(t, w.Body.Bytes()),
		"en-US deployment must see the English menu even though the stored blob is zh-CN")
}

// TestChannelGet_BotFatherMenuFollowsRequestLanguage mounts the production
// EarlyMiddleware on a fresh router (testutil's route set omits it) and pins
// the per-request property #335's per-language-storage option was assumed to
// require: two users of ONE deployment each see the menu in their own
// negotiated language, with no schema change.
func TestChannelGet_BotFatherMenuFollowsRequestLanguage(t *testing.T) {
	_, ctx := setupCmdMenuE2E(t)
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	r := wkhttp.New()
	r.UseGin(octoi18n.EarlyMiddleware(octoi18n.MiddlewareOptions{DefaultLanguage: octoi18n.DefaultLanguage}))
	channel.New(ctx).Route(r)

	cases := []struct {
		acceptLanguage string
		want           string
	}{
		{"en-US", cmdmenu.JSON("en-US")},
		{"zh-CN", cmdmenu.JSON("zh-CN")},
		{"", cmdmenu.JSON(octoi18n.DefaultLanguage)},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req, err := http.NewRequest("GET", "/v1/channels/"+BotFatherUID+"/1", nil)
		assert.NoError(t, err)
		req.Header.Set("token", testutil.Token)
		if tc.acceptLanguage != "" {
			req.Header.Set("Accept-Language", tc.acceptLanguage)
		}
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code, "Accept-Language=%q body=%s", tc.acceptLanguage, w.Body.String())
		assert.Equal(t, tc.want, channelExtraBotCommands(t, w.Body.Bytes()), "Accept-Language=%q", tc.acceptLanguage)
	}
}

// TestUserGet_BotFatherMenuLocalized pins the #335 override on
// GET /v1/users/botfather: the response carries bot_commands rendered in the
// resolved language, not the stored zh-CN blob.
func TestUserGet_BotFatherMenuLocalized(t *testing.T) {
	s, ctx := setupCmdMenuE2E(t)
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	fetch := func() string {
		w := httptest.NewRecorder()
		req, err := http.NewRequest("GET", "/v1/users/"+BotFatherUID, nil)
		assert.NoError(t, err)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
		var resp struct {
			BotCommands string `json:"bot_commands"`
		}
		assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		return resp.BotCommands
	}

	t.Setenv(octoi18n.EnvDefaultLanguage, "en-US")
	assert.Equal(t, cmdmenu.JSON("en-US"), fetch(),
		"en-US deployment must see the English menu even though the stored blob is zh-CN")

	t.Setenv(octoi18n.EnvDefaultLanguage, "zh-CN")
	assert.Equal(t, cmdmenu.JSON("zh-CN"), fetch())
}
