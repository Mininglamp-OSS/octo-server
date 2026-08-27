//go:build pilote2e

// Reproduction for symptom 1 of the cross-Space bot/notification report:
//
//	"我在 A 空间找平台 bot 聊天可以回复，但是在 B 空间找，回复消息落到 A 空间可见了"
//
// Drives the REAL handlers end to end against the REAL stack (MySQL 3306 /
// Redis 6379 / WuKongIM 5001), with no stubs anywhere on the path:
//
//	POST /v1/message/send        (X-Space-ID: B)   — user asks the bot from Space B
//	POST /v1/bot/sendMessage     (platform App Bot) — bot answers
//	POST /v1/message/channel/sync (X-Space-ID: B)  — user reads Space B
//	POST /v1/message/channel/sync (X-Space-ID: A)  — user reads Space A
//
// Run:
//
//	go test -tags pilote2e ./pilote2e/ -run TestReproCrossSpace_PlatformAppBotReply -v
package pilote2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReproCrossSpace_PlatformAppBotReply_LosesConversationSpace proves the
// defect and pins down exactly which hop drops the Space:
//
//	Step 1 — the user's question carries payload.space_id = Space B. The send
//	         path is CORRECT: SpaceMiddleware validates X-Space-ID and
//	         modules/message.sendMessage stamps it authoritatively.
//	Step 2 — the platform App Bot's reply carries NO payload.space_id at all.
//	         bot_api.resolveBotActiveSpaceID resolves the Space from the BOT's
//	         identity, not from the conversation: a scope=platform App Bot has
//	         no space_member row (modules/app_bot/db.go insertAppBot never
//	         writes one), so the DB fallback returns dbr.ErrNotFound and
//	         enrichBotPayloadWithResolvedSpaceID fail-closed STRIPS the tag.
//	Step 3 — the read filter (personSpaceAllows, modules/message/space_filter.go)
//	         then splits the one conversation across two Spaces: rule 3 drops
//	         the untagged reply in non-default Space B, rule 2 keeps it in the
//	         user's default Space A.
//
// Net effect for the user: Space B shows the question with no answer, Space A
// shows an answer with no question.
func TestReproCrossSpace_PlatformAppBotReply_LosesConversationSpace(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	cfg := ctx.GetConfig()
	// /v1/message/send is the proxy-send ingress; the deployment gate is off by
	// default in the test config.
	cfg.Message.SendMessageOn = true
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"))

	// WuKongIM persists its channel log in ~/.wukong across runs (only MySQL is
	// truncated), so every identity is unique per run — the DM channel then
	// holds exactly this run's two messages.
	stamp := time.Now().UnixNano()
	var (
		spaceA   = fmt.Sprintf("spc_xs_a_%d", stamp) // joined FIRST → user's default Space
		spaceB   = fmt.Sprintf("spc_xs_b_%d", stamp) // non-default
		botUID   = fmt.Sprintf("app_xs_%d_bot", stamp)
		botToken = fmt.Sprintf("app_xs_token_%d", stamp) // "app_" prefix → authAppBot
		question = fmt.Sprintf("question-asked-in-space-B-%d", stamp)
		answer   = fmt.Sprintf("answer-from-platform-bot-%d", stamp)
	)

	xsSeedSpace(t, ctx, spaceA, "2020-01-01 00:00:00")
	xsSeedSpace(t, ctx, spaceB, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceA, testutil.UID, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceB, testutil.UID, "2021-01-01 00:00:00")
	require.Equal(t, spaceA, xsDefaultSpaceOf(t, ctx, testutil.UID),
		"Space A must be the user's default Space (earliest space_member.created_at) for this repro")

	// A published PLATFORM App Bot: an app_bot row and nothing else. This
	// mirrors production exactly — createBot writes app_bot only.
	xsSeedPlatformAppBot(t, ctx, botUID, botToken)
	xsSeedFriend(t, ctx, testutil.UID, botUID)
	xsSeedFriend(t, ctx, botUID, testutil.UID)
	require.Zero(t, xsSpaceMemberRows(t, ctx, botUID),
		"a platform App Bot has NO space_member row — that is the precondition of the bug")

	xsResetLimits(t, ctx, testutil.UID, spaceA, spaceB)

	// ---- Step 1: the user asks the bot FROM Space B -----------------------
	code, body := xsPost(s, "/v1/message/send",
		map[string]string{"token": testutil.Token, "X-Space-ID": spaceB},
		map[string]interface{}{
			"token":                testutil.Token,
			"receive_channel_id":   botUID,
			"receive_channel_type": common.ChannelTypePerson.Uint8(),
			"payload":              map[string]interface{}{"type": 1, "content": question},
		})
	require.Equal(t, http.StatusOK, code, "user send from Space B must succeed: %s", truncate(body))

	// ---- Step 2: the platform App Bot replies ----------------------------
	// No X-Space-ID header — the bot has no way to know one is expected: the
	// /v1/bot/events envelope has no top-level space_id field and no published
	// contract asks the bot to echo one back.
	code, body = xsPost(s, "/v1/bot/sendMessage",
		map[string]string{"Authorization": "Bearer " + botToken},
		map[string]interface{}{
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"payload":      map[string]interface{}{"type": 1, "content": answer},
		})
	require.Equal(t, http.StatusOK, code, "platform App Bot reply must be accepted: %s", truncate(body))

	// ---- What actually went on the wire ----------------------------------
	onWire := xsWaitForPayloads(t, cfg.WuKongIM.APIURL, testutil.UID, botUID, question, answer)

	qPayload := onWire[question]
	require.NotNil(t, qPayload, "the user's question must be persisted in WuKongIM")
	assert.Equal(t, spaceB, qPayload["space_id"],
		"SEND PATH IS CORRECT: the user's question is stamped with the conversation's Space")

	aPayload := onWire[answer]
	require.NotNil(t, aPayload, "the bot's reply must be persisted in WuKongIM")
	gotSpace, hasSpace := aPayload["space_id"]
	assert.False(t, hasSpace,
		"DEFECT (root cause): the platform App Bot reply carries NO payload.space_id "+
			"(resolveBotActiveSpaceID → querySpaceIDsByRobotID → ErrNotFound → fail-closed strip); got %v", gotSpace)

	// ---- Step 3: what the user sees in each Space ------------------------
	inB := xsChannelSyncContents(t, s, botUID, spaceB)
	inA := xsChannelSyncContents(t, s, botUID, spaceA)
	t.Logf("Space B (conversation's Space) sees: %v", inB)
	t.Logf("Space A (user's default Space)  sees: %v", inA)

	assert.Contains(t, inB, question, "Space B keeps the question (exact space_id match — rule 1)")
	assert.NotContains(t, inB, answer,
		"DEFECT (user-visible symptom): the reply is INVISIBLE in Space B, "+
			"where the conversation actually happened (untagged + non-default → personSpaceAllows rule 3)")

	assert.Contains(t, inA, answer,
		"DEFECT (user-visible symptom): the reply surfaces in Space A, the user's DEFAULT Space, "+
			"which they never used for this conversation (untagged + default → personSpaceAllows rule 2)")
	assert.NotContains(t, inA, question,
		"Space A does not have the question (tagged with Space B → rule 5), so A shows an answer with no question")
}

// TestReproCrossSpace_PlatformAppBotReply_XSpaceIDHeaderIsTheOnlyWayOut is the
// control arm: the exact same platform App Bot, replying to the exact same
// conversation, lands in the right Space the moment it sends X-Space-ID.
//
// This isolates the defect to the RESOLUTION step rather than the authorization
// step: isBotSpaceAuthorized (modules/bot_api/db.go) already recognises platform
// App Bots in every active Space, so the server is willing to accept Space B —
// it simply has no way to derive it on its own. Any fix therefore has to supply
// the Space (server-side conversation lookup) or make the bot supply it
// (contract change), not loosen a permission.
func TestReproCrossSpace_PlatformAppBotReply_XSpaceIDHeaderIsTheOnlyWayOut(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	cfg := ctx.GetConfig()
	cfg.Message.SendMessageOn = true
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"))

	stamp := time.Now().UnixNano()
	var (
		spaceA   = fmt.Sprintf("spc_xsh_a_%d", stamp)
		spaceB   = fmt.Sprintf("spc_xsh_b_%d", stamp)
		botUID   = fmt.Sprintf("app_xsh_%d_bot", stamp)
		botToken = fmt.Sprintf("app_xsh_token_%d", stamp)
		answer   = fmt.Sprintf("answer-with-header-%d", stamp)
	)

	xsSeedSpace(t, ctx, spaceA, "2020-01-01 00:00:00")
	xsSeedSpace(t, ctx, spaceB, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceA, testutil.UID, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceB, testutil.UID, "2021-01-01 00:00:00")
	xsSeedPlatformAppBot(t, ctx, botUID, botToken)
	xsSeedFriend(t, ctx, testutil.UID, botUID)
	xsSeedFriend(t, ctx, botUID, testutil.UID)
	xsResetLimits(t, ctx, testutil.UID, spaceA, spaceB)

	code, body := xsPost(s, "/v1/bot/sendMessage",
		map[string]string{"Authorization": "Bearer " + botToken, "X-Space-ID": spaceB},
		map[string]interface{}{
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"payload":      map[string]interface{}{"type": 1, "content": answer},
		})
	require.Equal(t, http.StatusOK, code, "reply must be accepted: %s", truncate(body))

	onWire := xsWaitForPayloads(t, cfg.WuKongIM.APIURL, testutil.UID, botUID, answer)
	require.NotNil(t, onWire[answer], "the reply must be persisted in WuKongIM")
	assert.Equal(t, spaceB, onWire[answer]["space_id"],
		"with X-Space-ID the SAME platform App Bot is authorized for Space B and the tag survives — "+
			"the defect is Space RESOLUTION, not authorization")

	inB := xsChannelSyncContents(t, s, botUID, spaceB)
	inA := xsChannelSyncContents(t, s, botUID, spaceA)
	assert.Contains(t, inB, answer, "correctly tagged reply is visible in the conversation's Space")
	assert.NotContains(t, inA, answer, "and correctly absent from the unrelated default Space")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func xsSeedSpace(t *testing.T, ctx *config.Context, spaceID, createdAt string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)",
		spaceID, spaceID, testutil.UID, createdAt, createdAt,
	).Exec()
	require.NoError(t, err)
}

func xsSeedSpaceMemberAt(t *testing.T, ctx *config.Context, spaceID, uid, createdAt string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, ?, ?)",
		spaceID, uid, createdAt, createdAt,
	).Exec()
	require.NoError(t, err)
}

// xsSeedPlatformAppBot mirrors modules/app_bot.insertAppBot for scope=platform:
// an app_bot row plus the user row that makes it a robot. Deliberately NO
// space_member row — that omission is production behaviour and the bug's
// precondition.
func xsSeedPlatformAppBot(t *testing.T, ctx *config.Context, botUID, token string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, username, robot) VALUES (?, ?, ?, 1)",
		botUID, "平台 Bot", botUID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO app_bot (id, uid, display_name, scope, space_id, status, token, created_by) "+
			"VALUES (?, ?, ?, 'platform', '', 1, ?, ?)",
		botUID, botUID, "平台 Bot", token, testutil.UID,
	).Exec()
	require.NoError(t, err)
}

func xsSeedFriend(t *testing.T, ctx *config.Context, uid, toUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO friend (uid, to_uid, is_deleted, version) VALUES (?, ?, 0, 1)", uid, toUID,
	).Exec()
	require.NoError(t, err)
}

func xsSpaceMemberRows(t *testing.T, ctx *config.Context, uid string) int {
	t.Helper()
	var n int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE uid=? AND status=1", uid).LoadOne(&n))
	return n
}

// xsDefaultSpaceOf mirrors modules/space.GetUserDefaultSpaceID: earliest joined.
func xsDefaultSpaceOf(t *testing.T, ctx *config.Context, uid string) string {
	t.Helper()
	var spaceID string
	_, err := ctx.DB().SelectBySql(
		"SELECT space_id FROM space_member WHERE uid=? AND status=1 ORDER BY created_at ASC LIMIT 1", uid,
	).Load(&spaceID)
	require.NoError(t, err)
	return spaceID
}

// xsResetLimits clears the shared per-UID rate-limit bucket and the Space
// membership cache, neither of which CleanAllTables touches.
func xsResetLimits(t *testing.T, ctx *config.Context, uid string, spaceIDs ...string) {
	t.Helper()
	r := ctx.GetRedisConn()
	_ = r.Del("ratelimit:uid:" + uid)
	for _, sp := range spaceIDs {
		_ = r.Del("space:member:" + sp + ":" + uid)
	}
}

func xsPost(s *server.Server, path string, headers map[string]string, body interface{}) (int, string) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// xsWaitForPayloads reads the DM channel straight out of WuKongIM and returns
// the decoded payload of each message whose `content` matches one of `contents`.
// Polls because /message/send returns before the channel log has the message.
func xsWaitForPayloads(t *testing.T, apiURL, loginUID, peerUID string, contents ...string) map[string]map[string]interface{} {
	t.Helper()
	found := make(map[string]map[string]interface{}, len(contents))
	for attempt := 0; attempt < 40; attempt++ {
		for _, m := range channelMessageSync(t, apiURL, loginUID, peerUID) {
			payload := decodePayload(m)
			if payload == nil {
				continue
			}
			c, _ := payload["content"].(string)
			for _, want := range contents {
				if c == want {
					found[want] = payload
				}
			}
		}
		if len(found) == len(contents) {
			return found
		}
		time.Sleep(250 * time.Millisecond)
	}
	return found
}

// xsChannelSyncContents drives the REAL POST /v1/message/channel/sync as the
// logged-in user with X-Space-ID set, and returns the `content` of every
// message that survived the server-side Space filter.
func xsChannelSyncContents(t *testing.T, s *server.Server, peerUID, spaceID string) []string {
	t.Helper()
	code, body := xsPost(s, "/v1/message/channel/sync",
		map[string]string{"token": testutil.Token, "X-Space-ID": spaceID},
		map[string]interface{}{
			"channel_id":        peerUID,
			"channel_type":      common.ChannelTypePerson.Uint8(),
			"start_message_seq": 0,
			"end_message_seq":   0,
			"limit":             100,
			"pull_mode":         1,
		})
	require.Equal(t, http.StatusOK, code, "channel/sync in %s must succeed: %s", spaceID, truncate(body))

	var resp struct {
		Messages []struct {
			Payload map[string]interface{} `json:"payload"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp), "channel/sync response must decode: %s", truncate(body))

	contents := make([]string, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		if c, ok := m.Payload["content"].(string); ok {
			contents = append(contents, c)
		}
	}
	return contents
}
