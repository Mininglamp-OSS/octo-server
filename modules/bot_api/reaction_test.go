package bot_api

// Tests for POST /v1/bot/message/reaction (as-bot add/remove).
//
// Layered coverage rationale:
//   - The full reaction validation chain (membership / visibility / text-only /
//     thread / disband / DM-Space) lives in message.ReactionService.WriteReaction
//     and is exercised end-to-end by modules/message's reaction integration tests
//     (the bot endpoint delegates to the SAME authority).
//   - The idempotent add/remove DB primitive is unit-tested in
//     modules/message/db_message_reaction_test.go (setReaction, sqlmock).
//   - Here we cover the bot-endpoint-specific glue: action parsing and the
//     route+auth+delegation+error-mapping wiring via rejection paths that do NOT
//     require seeding a target message (they return before queryMessageByID or
//     before WriteReaction), so no WuKongIM dispatch is involved.
//
// The App-Bot DM-only gate uses the same getBotKindFromContext(c)==BotKindApp
// check groups.go already covers; seeding an app_bot here would require the
// registry/table wiring that bot_api tests deliberately avoid (prod→test import
// cycle, see incoming_webhook_test.go), so it is not re-tested at HTTP level.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reactionBotID    = "bot_reaction_ep"
	reactionBotToken = "bf_reaction_ep_token"
)

// TestParseReactionAction pins the wire→semantic mapping: empty defaults to add
// (the common "react before reply" case), remove/delete map to remove, unknown
// is rejected. Bot reactions are always explicit — never toggle.
func TestParseReactionAction(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   message.ReactionAction
		wantOK bool
	}{
		{"add", message.ReactionAdd, true},
		{"", message.ReactionAdd, true},
		{"  ADD ", message.ReactionAdd, true},
		{"remove", message.ReactionRemove, true},
		{"delete", message.ReactionRemove, true},
		{"Remove", message.ReactionRemove, true},
		{"frobnicate", message.ReactionAdd, false},
	} {
		got, ok := parseReactionAction(tc.in)
		assert.Equal(t, tc.wantOK, ok, "ok for %q", tc.in)
		if tc.wantOK {
			assert.Equal(t, tc.want, got, "action for %q", tc.in)
		}
	}

	assert.Equal(t, "add", normalizeReactionActionLabel(""))
	assert.Equal(t, "add", normalizeReactionActionLabel("add"))
	assert.Equal(t, "remove", normalizeReactionActionLabel("remove"))
	assert.Equal(t, "remove", normalizeReactionActionLabel("DELETE"))
	assert.Equal(t, "add", normalizeReactionActionLabel("frobnicate"))
}

// newReactionTestServer wires the i18n error renderer (testutil.NewTestServer
// omits it) so error responses carry the v2 error.code block and tests can
// assert on the code rather than brittle message text.
func newReactionTestServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return s, ctx
}

func seedReactionBotRow(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", reactionBotID, reactionBotToken).Exec()
	require.NoError(t, err)
}

func seedReactionGroup(t *testing.T, ctx *config.Context, groupNo string, withBotMember bool) {
	t.Helper()
	gdb := group.NewDB(ctx)
	require.NoError(t, gdb.Insert(&group.Model{
		GroupNo: groupNo,
		Name:    "reaction-ep-test",
		Creator: testutil.UID,
		Status:  group.GroupStatusNormal,
		Version: 1,
	}))
	if withBotMember {
		require.NoError(t, gdb.InsertMember(&group.MemberModel{
			GroupNo: groupNo,
			UID:     reactionBotID,
			Role:    group.MemberRoleCommon,
			Status:  int(common.GroupMemberStatusNormal),
			Version: 1,
		}))
	}
}

func postBotReaction(t *testing.T, s *server.Server, token string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/bot/message/reaction", bytes.NewReader([]byte(util.ToJson(body))))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// errCodeOf decodes the localized error envelope and returns error.code.
func errCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &e), w.Body.String())
	return e.Error.Code
}

// TestBotReactionRejectsNonMember: a registered bot that is NOT a member of the
// target group is denied before any message lookup (channel-access-denied).
func TestBotReactionRejectsNonMember(t *testing.T) {
	s, ctx := newReactionTestServer(t)
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedReactionBotRow(t, ctx)
	const groupNo = "grp_reaction_nonmember"
	seedReactionGroup(t, ctx, groupNo, false)

	w := postBotReaction(t, s, reactionBotToken, map[string]interface{}{
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"message_id":   "910001",
		"emoji":        "👀",
		"action":       "add",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String()) // D14 pinned 400
	assert.Equal(t, errcode.ErrMessageChannelAccessDenied.ID, errCodeOf(t, w), w.Body.String())
}

// (The message-not-found path — bot IS a member but the target message does not
// exist → 404-mapped ErrMessageNotFound — is covered by modules/message's
// reaction integration tests against WriteReaction, which the bot endpoint
// delegates to. It is not re-tested here because seeding a target message row
// depends on the sharded `message{N}` table layout, whose non-zero shards are not
// provisioned in the default test DB.)

// TestBotReactionRejectsInvalidAction: an unknown action value is a request-shape
// error (field=action), rejected before the reaction write path.
func TestBotReactionRejectsInvalidAction(t *testing.T) {
	s, ctx := newReactionTestServer(t)
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedReactionBotRow(t, ctx)

	w := postBotReaction(t, s, reactionBotToken, map[string]interface{}{
		"channel_id":   "grp_reaction_action",
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"message_id":   "910001",
		"emoji":        "👀",
		"action":       "frobnicate",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, errcode.ErrBotAPIRequestInvalid.ID, errCodeOf(t, w), w.Body.String())
}
