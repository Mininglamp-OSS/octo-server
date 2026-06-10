package robot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather/cmdmenu"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

// TestGetCommands_CmdMenuLocalization pins the #335 split on
// GET /v1/robot/commands: BotFather's menu is server-owned copy rendered in
// the request's negotiated language, while a user bot's commands are creator
// content served from the DB untouched whatever language the caller asks for.
func TestGetCommands_CmdMenuLocalization(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	creatorContent := `[{"command":"/deploy","description":"部署到生产"}]`
	for _, row := range []struct{ id, commands string }{
		{cmdmenu.BotFatherUID, cmdmenu.JSON("zh-CN")},
		{"user_bot", creatorContent},
	} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO robot (app_id, robot_id, username, token, version, status, creator_uid, description, bot_token, im_token_cache, bot_commands) VALUES ('app', ?, ?, 't', 1, 1, '', '', '', '', ?)",
			row.id, row.id, row.commands,
		).Exec()
		assert.NoError(t, err)
	}

	r := wkhttp.New()
	r.UseGin(octoi18n.EarlyMiddleware(octoi18n.MiddlewareOptions{DefaultLanguage: octoi18n.DefaultLanguage}))
	New(ctx).Route(r)

	cases := []struct {
		name           string
		robotID        string
		acceptLanguage string
		want           []cmdmenu.Command
	}{
		{"botfather en-US → English menu", cmdmenu.BotFatherUID, "en-US", cmdmenu.Commands("en-US")},
		{"botfather zh-CN → Chinese menu", cmdmenu.BotFatherUID, "zh-CN", cmdmenu.Commands("zh-CN")},
		{"botfather no header → deployment default", cmdmenu.BotFatherUID, "", cmdmenu.Commands(octoi18n.DefaultLanguage)},
		{"user bot stays creator content under en-US", "user_bot", "en-US", []cmdmenu.Command{{Command: "/deploy", Description: "部署到生产"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, err := http.NewRequest("GET", "/v1/robot/commands?robot_id="+tc.robotID, nil)
			assert.NoError(t, err)
			req.Header.Set("token", token)
			if tc.acceptLanguage != "" {
				req.Header.Set("Accept-Language", tc.acceptLanguage)
			}
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
			var got []cmdmenu.Command
			assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "body=%s", w.Body.String())
			assert.Equal(t, tc.want, got)
		})
	}
}
