package ai_team_test

// Contract test between ai_team (which CREATES the hidden AI session container)
// and bot_api (which DECIDES whether the container's bot may answer without an
// @mention). Both halves are exercised over real HTTP against a real MySQL /
// Redis / WuKongIM stack, because the bug this guards against lives exactly in
// the seam: bot_api's unit tests hand-write the `group` row, so they cannot tell
// whether a container built by the real provisioning path actually carries the
// purpose and the bot membership the decision depends on.
//
// The user-visible failure being pinned: in an AI session the adapter's mention
// gate (openclaw-channel-octo, whose isGroup covers CommunityTopic) files every
// non-@ message as history context instead of replying. Delivery inside a
// container is already resolved from ai_team_agent rather than from the payload,
// so the @ carries no information — requiring one just makes the AI look dead.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mentionPrefResponse is the adapter-facing shape of
// GET /v1/bot/groups/:group_no/mention_pref.
type mentionPrefResponse struct {
	NoMention           int  `json:"no_mention"`
	GroupAllowNoMention int  `json:"group_allow_no_mention"`
	Effective           bool `json:"effective"`
}

// botRequest issues a bot-token (adapter) request, as opposed to the user-session
// `request` helper in api_test.go.
func botRequest(t *testing.T, botToken, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(method, path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+botToken)
	w := httptest.NewRecorder()
	testServer.GetRoute().ServeHTTP(w, req)
	return w
}

// provisionContainerSession drives the real provisioning path: add the AI
// (which eagerly creates the container group), then create its first thread and
// WuKongIM topic channel. Returns the container group_no and thread channel id.
func provisionContainerSession(t *testing.T, f fixture, idem string) (groupNo, channelID string) {
	t.Helper()
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", idem,
		map[string]string{"name": "no-mention contract"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var session struct {
		SessionID string `json:"session_id"`
		GroupNo   string `json:"group_no"`
		ChannelID string `json:"channel_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &session), "body=%s", w.Body.String())
	require.NotEmpty(t, session.GroupNo)
	require.NotEmpty(t, session.ChannelID)
	return session.GroupNo, session.ChannelID
}

// grantBotToken gives the fixture's bot a usable bot_token so the adapter-facing
// endpoints can authenticate as it (seedFixture only inserts the robot row).
func grantBotToken(t *testing.T, f fixture) string {
	t.Helper()
	botToken := "bf_" + util.GenerUUID()[:12]
	_, err := testContext.DB().Update("robot").Set("bot_token", botToken).
		Where("robot_id=?", f.botID).Exec()
	require.NoError(t, err)
	return botToken
}

// TestAIContainerAnswersWithoutMention is the regression for the whole feature:
// a container provisioned through the real API must report 免@ to its bot with no
// preference row anywhere.
func TestAIContainerAnswersWithoutMention(t *testing.T) {
	f := seedFixture(t)
	botToken := grantBotToken(t, f)
	groupNo, _ := provisionContainerSession(t, f, "idem-no-mention-1")

	// Premise 1 — the decision keys on `purpose`, so the real provisioning path
	// must actually stamp it. (bot_api's own tests hand-write this column.)
	var purpose string
	require.NoError(t, testContext.DB().Select("IFNULL(purpose,'')").From("`group`").
		Where("group_no=?", groupNo).LoadOne(&purpose))
	require.Equal(t, aiteampkg.GroupPurpose, purpose, "container must be stamped with the AI purpose")

	// Premise 2 — the endpoint's membership gate runs BEFORE the decision, so a
	// bot that is not admitted to its own container would 403 and never reach it.
	var botMembers int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("group_member").
		Where("group_no=? AND uid=? AND is_deleted=0", groupNo, f.botID).LoadOne(&botMembers))
	require.Equal(t, 1, botMembers, "bot must be a member of its own container")

	// Premise 3 — no preference row exists, and none can be created (see
	// TestAIContainerOwnerEndpointStaysProtected). The 免@ therefore CANNOT come
	// from seeded data; only the server-side purpose rule can produce it.
	var prefRows int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("bot_mention_pref").
		Where("group_no=?", groupNo).LoadOne(&prefRows))
	require.Equal(t, 0, prefRows, "container must not rely on a backfilled bot_mention_pref row")

	// The actual adapter call.
	w := botRequest(t, botToken, http.MethodGet, "/v1/bot/groups/"+groupNo+"/mention_pref")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body mentionPrefResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body=%s", w.Body.String())

	assert.True(t, body.Effective, "container must be 免@ so the adapter's mention gate lets the message through")
	assert.Equal(t, 1, body.NoMention, "legacy adapters that read only no_mention must see 免@ too")
	assert.Equal(t, 1, body.GroupAllowNoMention, "containers are provisioned with allow_no_mention=1")
}

// TestOrdinaryGroupStillRequiresMention bounds the blast radius: the same bot in
// an ordinary group still has to be @mentioned. Without this, "AI containers are
// 免@" could silently become "every group this bot is in is 免@".
func TestOrdinaryGroupStillRequiresMention(t *testing.T) {
	f := seedFixture(t)
	botToken := grantBotToken(t, f)

	ordinaryGroupNo := "grp_ordinary_" + util.GenerUUID()[:8]
	_, err := testContext.DB().InsertBySql(
		"INSERT INTO `group` (group_no,name,creator,status,version,allow_no_mention) VALUES (?,?,?,1,1,1)",
		ordinaryGroupNo, "ordinary group", f.uid).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO group_member (group_no,uid,vercode,is_deleted,status,version) VALUES (?,?,?,0,1,1)",
		ordinaryGroupNo, f.botID, util.GenerUUID()).Exec()
	require.NoError(t, err)

	w := botRequest(t, botToken, http.MethodGet, "/v1/bot/groups/"+ordinaryGroupNo+"/mention_pref")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body mentionPrefResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body=%s", w.Body.String())

	assert.False(t, body.Effective, "an ordinary group with no owner preference must still require an @")
	assert.Equal(t, 0, body.NoMention)
	assert.Equal(t, 1, body.GroupAllowNoMention)
}

// TestAIContainerOwnerEndpointStaysProtected pins Premise 3 of the test above and
// the reason the rule has to live server-side at all: the owner cannot configure
// a container's mention preference, so "just let the owner switch 免@ on" is not
// an available answer.
func TestAIContainerOwnerEndpointStaysProtected(t *testing.T) {
	f := seedFixture(t)
	groupNo, _ := provisionContainerSession(t, f, "idem-no-mention-2")

	for _, tc := range []struct {
		name   string
		method string
		body   any
	}{
		{"read", http.MethodGet, nil},
		{"write", http.MethodPut, map[string]int{"no_mention": 1}},
		{"delete", http.MethodDelete, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, f, tc.method,
				"/v1/robot/"+f.botID+"/groups/"+groupNo+"/mention_pref", "", tc.body)
			assert.NotEqual(t, http.StatusOK, w.Code,
				"owner must not be able to %s a container's mention preference: %s", tc.name, w.Body.String())
		})
	}
}
