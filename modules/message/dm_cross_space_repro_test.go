//go:build integration

package message

// Post-fix end-to-end coverage for issue #484 — DM per-Space isolation.
//
// Originally a reproduction (DMs leaked across Spaces / mutually hid between
// Spaces); now flipped to assert the fix, driven through the REAL registered
// handlers against MySQL/Redis with WuKongIM mocked:
//
//   - Symptom 2 (was: mutual hide): conversation visibility now comes from the
//     authoritative dm_space_presence index, written at the REAL WuKongIM
//     message webhook (POST /v1/webhook/message/notify) and read by
//     /v1/conversation/sync — so a DM is visible in EVERY Space it has messages
//     in, simultaneously, independent of the shared Recents window.
//   - Symptom 1 (was: history leak): /v1/message/channel/sync keeps an untagged
//     DM message only in the user's default Space.
//
// Build-tagged `integration` (NOT compiled in the default CI `go test` job).
// Run targeted (order-fragile like every e2e file here — register.GetModules
// memoizes modules under a sync.Once, so handlers bind to the FIRST
// NewTestServer's ctx/config in the process):
//
//	go test -tags=integration ./modules/message/ -run TestRepro484 -v

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reproPeerUID = "peer_contact_777"
const reproWebhookSecret = "repro-webhook-secret-484"

// Three Spaces the test user belongs to. spaceDefault is joined first so it is
// the user's default Space (GetUserDefaultSpaceID = earliest created_at). spaceB
// and spaceC are both NON-default, which isolates the per-Space behaviour from
// the "default Space always shows" special-case.
const (
	reproSpaceDefault = "spaceDefault"
	reproSpaceB       = "spaceB"
	reproSpaceC       = "spaceC"
)

// One long-lived fake WuKongIM for this file, serving both IM endpoints the two
// handlers hit, off mutable response vars. Tests run sequentially (no
// t.Parallel), so the shared vars need no lock.
var (
	reproIMOnce  sync.Once
	reproIMSrv   *httptest.Server
	reproIMConv  *config.SyncUserConversationResp   // /conversation/sync payload (single DM)
	reproIMConvs []*config.SyncUserConversationResp // /conversation/sync payload (multi-DM); takes precedence over reproIMConv
	reproIMMsgs  []*config.MessageResp              // /channel/messagesync payload
)

func reproFakeIM() *httptest.Server {
	reproIMOnce.Do(func() {
		reproIMSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/conversation/sync"):
				convs := []*config.SyncUserConversationResp{}
				switch {
				case reproIMConvs != nil:
					convs = reproIMConvs
				case reproIMConv != nil:
					convs = append(convs, reproIMConv)
				}
				_, _ = w.Write([]byte(util.ToJson(convs)))
			case strings.HasSuffix(r.URL.Path, "/channel/messagesync"):
				_, _ = w.Write([]byte(util.ToJson(&config.SyncChannelMessageResp{
					StartMessageSeq: 1,
					EndMessageSeq:   uint32(len(reproIMMsgs)),
					Messages:        reproIMMsgs,
				})))
			default:
				_, _ = w.Write([]byte("{}"))
			}
		}))
	})
	return reproIMSrv
}

// reproMsg builds an IM message with the given content marker and (optional)
// payload.space_id tag. spaceID=="" → an UNTAGGED message.
func reproMsg(seq uint32, content, spaceID string) *config.MessageResp {
	payload := map[string]interface{}{"type": 1, "content": content}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	return &config.MessageResp{
		MessageID:   int64(seq),
		MessageSeq:  seq,
		ClientMsgNo: content,
		FromUID:     reproPeerUID,
		ChannelID:   reproPeerUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000000 + int32(seq),
		IsDeleted:   0,
		Payload:     []byte(util.ToJson(payload)),
	}
}

// reproDMConv wraps a single UNTAGGED recent message into a DM conversation as
// IMSyncUserConversation would return it. Untagged Recents means the legacy
// window-scan OR-term never matches spaceB/spaceC, so conv visibility in those
// Spaces is driven purely by the authoritative dm_space_presence index.
func reproDMConv() *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   reproPeerUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Unread:      0,
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents:     []*config.MessageResp{reproMsg(1, "untagged-recent", "")},
	}
}

// reproMsgFor is reproMsg for an arbitrary peer (multi-DM scenarios).
func reproMsgFor(peer string, seq uint32, content, spaceID string) *config.MessageResp {
	payload := map[string]interface{}{"type": 1, "content": content}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	return &config.MessageResp{
		MessageID:   int64(seq),
		MessageSeq:  seq,
		ClientMsgNo: content,
		FromUID:     peer,
		ChannelID:   peer,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000000 + int32(seq),
		Payload:     []byte(util.ToJson(payload)),
	}
}

// reproDMConvFor builds a DM conversation for `peer` whose single recent message
// is tagged with `spaceID` (or untagged when spaceID==""). Used to assemble a
// multi-DM /conversation/sync payload mirroring the production snapshot.
func reproDMConvFor(peer, content, spaceID string) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   peer,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents:     []*config.MessageResp{reproMsgFor(peer, 1, content, spaceID)},
	}
}

func reproSeedSpace(t *testing.T, ctx *config.Context, spaceID, createdAt string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)",
		spaceID, spaceID, testutil.UID, createdAt, createdAt,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, ?, ?)",
		spaceID, testutil.UID, createdAt, createdAt,
	).Exec()
	require.NoError(t, err)
}

// reproSeedGroup inserts a row into `group` with the given space_id. An empty
// spaceID models a spaceless group whose Space cannot be resolved, so the
// server's group filter fails open.
func reproSeedGroup(t *testing.T, ctx *config.Context, groupNo, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, space_id) VALUES (?, ?, 1, ?)",
		groupNo, groupNo, spaceID,
	).Exec()
	require.NoError(t, err)
}

// reproSeedGroupMember makes the login user an active member of groupNo.
// conversation/sync gates group conversations behind ExistMembers
// (api_conversation.go:474), so a group is dropped entirely unless the caller is
// a member — orthogonal to the Space filter under test.
func reproSeedGroupMember(t *testing.T, ctx *config.Context, groupNo string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, is_deleted, version) VALUES (?, ?, 0, 1, 0, 1)",
		groupNo, testutil.UID,
	).Exec()
	require.NoError(t, err)
}

// reproGroupConv builds a GROUP conversation (bare group_no, conv-level SpaceID
// empty) as IMSyncUserConversation returns it; the server resolves its Space from
// the `group` table (GetGroups → groupSpaceMap).
func reproGroupConv(groupNo, content string) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents: []*config.MessageResp{{
			MessageID:   1,
			MessageSeq:  1,
			ClientMsgNo: content,
			FromUID:     testutil.UID,
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Timestamp:   1700000001,
			Payload:     []byte(util.ToJson(map[string]interface{}{"type": 1, "content": content})),
		}},
	}
}

// reproSetup wires a fresh test server with the fake IM, seeds the three Space
// memberships, configures the webhook HMAC secret, and clears Redis state so
// each test is deterministic.
func reproSetup(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()

	// Reset the shared fake-IM response vars so each test is independent
	// regardless of run order (single-conv and multi-conv tests share these).
	reproIMConv = nil
	reproIMConvs = nil
	reproIMMsgs = nil

	// module.Setup (common module) refuses to start without a master key.
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	// Webhook reads its HMAC secret from env at module construction.
	t.Setenv("TS_WEBHOOK_SECRET_KEY", reproWebhookSecret)

	imURL := reproFakeIM().URL
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	cfg := ctx.GetConfig()
	cfg.MessageSaveAcrossDevice = false
	cfg.WuKongIM.APIURL = imURL

	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"))

	// spaceDefault joined first → user's default Space. B and C are non-default.
	reproSeedSpace(t, ctx, reproSpaceDefault, "2020-01-01 00:00:00")
	reproSeedSpace(t, ctx, reproSpaceB, "2021-01-01 00:00:00")
	reproSeedSpace(t, ctx, reproSpaceC, "2022-01-01 00:00:00")
	reproEnsureAppBotTable(t, ctx)

	r := ctx.GetRedisConn()
	_ = r.Del("ratelimit:uid:" + testutil.UID)
	_ = r.Del("userMaxVersion:" + testutil.UID)
	for _, sp := range []string{reproSpaceDefault, reproSpaceB, reproSpaceC} {
		_ = r.Del("space:member:" + sp + ":" + testutil.UID)
	}
	return s, ctx
}

// reproIngestPersonMsg drives the REAL WuKongIM message webhook for an inbound
// Person message (fromUID → channelID) tagged with spaceID, populating
// dm_space_presence. The HMAC signature satisfies verifyRequestSignature.
// Payload is a []byte JSON field → base64-encoded on the wire (encoding/json
// round-trips []byte). The webhook keys presence by
// common.GetFakeChannelIDWith(fromUID, channelID), which is symmetric — so it
// matches the conversation read side GetFakeChannelIDWith(loginUID, peer).
func reproIngestPersonMsg(t *testing.T, s *server.Server, fromUID, channelID, spaceID string, seq uint32) {
	t.Helper()
	payload := fmt.Sprintf(`{"type":1,"content":"m%d","space_id":%q}`, seq, spaceID)
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	body := fmt.Sprintf(
		`[{"message_id":%d,"message_seq":%d,"from_uid":%q,"channel_id":%q,"channel_type":1,"timestamp":%d,"payload":%q}]`,
		seq, seq, fromUID, channelID, 1700000000+int(seq), b64,
	)
	mac := hmac.New(sha256.New, []byte(reproWebhookSecret))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/webhook/message/notify", strings.NewReader(body))
	req.Header.Set("X-Signature-256", sig)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// reproIngestDM ingests an inbound DM from the peer contact to the login user.
func reproIngestDM(t *testing.T, s *server.Server, spaceID string, seq uint32) {
	t.Helper()
	reproIngestPersonMsg(t, s, reproPeerUID, testutil.UID, spaceID, seq)
}

// reproSeedBotUser marks botUID as a regular bot (user.robot=1) so GetBotUIDs
// recognizes it and the conversation filter routes it through the bot branch.
func reproSeedBotUser(t *testing.T, ctx *config.Context, botUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, robot) VALUES (?, 1)", botUID,
	).Exec()
	require.NoError(t, err)
}

// reproEnsureAppBotTable stubs an EMPTY app_bot table. The bot in these tests is
// a robot (user.robot=1, identified by GetBotUIDs); app_bot is a SEPARATE
// mechanism (platform/space-scoped bots) and is left empty here. But
// CheckBotsInSpace (pkg/space) always also queries app_bot — and the message test
// binary doesn't load the app_bot module, so that table is absent and the query
// errors → resolveBotFilter fail-opens (skipBotFilter=true) and shows every bot in
// every Space, masking the membership logic. Creating the empty table lets the
// robot membership path (space_member) run for real. Only the queried columns are
// needed (uid/status/scope/space_id); nothing inserts into it here.
func reproEnsureAppBotTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"CREATE TABLE IF NOT EXISTS app_bot (" +
			"uid VARCHAR(40) NOT NULL, status TINYINT NOT NULL DEFAULT 0, " +
			"scope VARCHAR(20) NOT NULL DEFAULT 'platform', space_id VARCHAR(40) DEFAULT NULL)",
	).Exec()
	require.NoError(t, err)
}

// reproSeedBotMember makes botUID a member of spaceID (CheckBotsInSpace → in-space).
func reproSeedBotMember(t *testing.T, ctx *config.Context, spaceID, botUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, botUID,
	).Exec()
	require.NoError(t, err)
}

// reproCallConvSync drives POST /v1/conversation/sync with X-Space-ID and
// returns the channel IDs present in the response conversation list.
func reproCallConvSync(t *testing.T, s *server.Server, spaceID string) []string {
	t.Helper()
	body := `{"version":0,"msg_count":50,"device_uuid":"dev-repro","recent_filter":false}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/conversation/sync", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	// spaceID=="" models a request with NO space context (no X-Space-ID, no
	// ?space_id) — SpaceMiddleware then passes through without setting space_id.
	if spaceID != "" {
		req.Header.Set("X-Space-ID", spaceID)
	}
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var wrap struct {
		Conversations []struct {
			ChannelID string `json:"channel_id"`
		} `json:"conversations"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wrap))
	ids := make([]string, 0, len(wrap.Conversations))
	for _, c := range wrap.Conversations {
		ids = append(ids, c.ChannelID)
	}
	return ids
}

// reproCallConvSyncNoSpace drives POST /v1/conversation/sync with NO space
// context at all (no ?space_id, no X-Space-ID) — the production "client forgot
// to send space_id" request. The handler then SKIPS FilterConversationsBySpace.
func reproCallConvSyncNoSpace(t *testing.T, s *server.Server) []string {
	t.Helper()
	return reproCallConvSync(t, s, "")
}

// reproCallChannelSync drives POST /v1/message/channel/sync for the DM channel
// with X-Space-ID and returns the content markers of the returned messages.
func reproCallChannelSync(t *testing.T, s *server.Server, spaceID string) []string {
	t.Helper()
	body := `{"channel_id":"` + reproPeerUID + `","channel_type":1,"limit":100,"pull_mode":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/message/channel/sync", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("X-Space-ID", spaceID)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Messages []struct {
			Payload map[string]interface{} `json:"payload"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	contents := make([]string, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		if c, ok := m.Payload["content"].(string); ok {
			contents = append(contents, c)
		}
	}
	return contents
}

func reproContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestRepro484_Symptom2_DMVisibleInEverySpaceItHasMessages asserts the fix for
// symptom 2: a single DM that has messages in BOTH spaceB and spaceC is visible
// in BOTH simultaneously — the shared Recents window no longer makes them
// mutually exclusive. Presence is written via the REAL message webhook.
func TestRepro484_Symptom2_DMVisibleInEverySpaceItHasMessages(t *testing.T) {
	s, _ := reproSetup(t)
	reproIMConv = reproDMConv() // Recents are UNTAGGED → visibility is presence-driven

	// The contact was chatted with in spaceB AND spaceC (durable presence rows).
	reproIngestDM(t, s, reproSpaceB, 1)
	reproIngestDM(t, s, reproSpaceC, 2)

	inB := reproCallConvSync(t, s, reproSpaceB)
	inC := reproCallConvSync(t, s, reproSpaceC)

	assert.True(t, reproContains(inB, reproPeerUID),
		"DM visible in spaceB (authoritative presence), independent of the Recents window")
	assert.True(t, reproContains(inC, reproPeerUID),
		"FIXED symptom 2: the SAME DM is also visible in spaceC at the same time — no mutual hide")
}

// TestRepro484_Symptom2_IsolationPreserved asserts we did not over-correct: a DM
// with messages only in spaceB is visible in spaceB but NOT in spaceC (which it
// genuinely has no messages in).
func TestRepro484_Symptom2_IsolationPreserved(t *testing.T) {
	s, _ := reproSetup(t)
	reproIMConv = reproDMConv()

	reproIngestDM(t, s, reproSpaceB, 1) // only spaceB

	inB := reproCallConvSync(t, s, reproSpaceB)
	inC := reproCallConvSync(t, s, reproSpaceC)

	assert.True(t, reproContains(inB, reproPeerUID), "DM visible in the Space it has messages in")
	assert.False(t, reproContains(inC, reproPeerUID),
		"isolation preserved: DM absent from a Space it has no messages in")
}

// TestRepro484_DefaultSpaceListsOtherSpaceOnlyDM confirms CURRENT (known)
// behavior, NOT a desired end-state: the default-Space catch-all
// (space_filter.go:305-309 decideConvKeepInSpace) lists EVERY bare DM in the
// user's default Space, including a DM whose messages belong only to a
// non-default Space. So `POST /v1/conversation/sync` with space_id = the user's
// default Space surfaces other-Space DMs as entries. This is an open product
// question (issue #484 follow-up), tracked here as a reproduction. By contrast a
// non-default Space the DM has no messages in correctly hides it (the #484 fix).
func TestRepro484_DefaultSpaceListsOtherSpaceOnlyDM(t *testing.T) {
	s, _ := reproSetup(t)
	// A DM whose ONLY recent message is spaceB-tagged (no untagged / no
	// default-tagged content), so any default-Space visibility can ONLY come from
	// the catch-all branch — not from legacy/untagged history.
	reproIMConv = &config.SyncUserConversationResp{
		ChannelID:   reproPeerUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents:     []*config.MessageResp{reproMsg(1, "only-spaceB", reproSpaceB)},
	}

	inDefault := reproCallConvSync(t, s, reproSpaceDefault)
	assert.True(t, reproContains(inDefault, reproPeerUID),
		"KNOWN: default-Space catch-all lists a DM whose messages are only in non-default spaceB")

	inC := reproCallConvSync(t, s, reproSpaceC)
	assert.False(t, reproContains(inC, reproPeerUID),
		"non-default spaceC (DM has no messages there) correctly hides it — the #484 fix")
}

// TestRepro484_Symptom1_UntaggedHistoryOnlyInDefaultSpace asserts the fix for
// symptom 1: an untagged DM message is kept only in the user's default Space,
// no longer leaking into every Space.
func TestRepro484_Symptom1_UntaggedHistoryOnlyInDefaultSpace(t *testing.T) {
	s, _ := reproSetup(t)

	// One DM history: a spaceB-tagged msg, an UNTAGGED msg, a spaceC-tagged msg.
	reproIMMsgs = []*config.MessageResp{
		reproMsg(1, "msg-tagged-B", reproSpaceB),
		reproMsg(2, "msg-UNTAGGED", ""),
		reproMsg(3, "msg-tagged-C", reproSpaceC),
	}

	// Non-default Space (spaceB): only its own tagged message; untagged dropped.
	inB := reproCallChannelSync(t, s, reproSpaceB)
	assert.True(t, reproContains(inB, "msg-tagged-B"), "spaceB sees its own tagged msg")
	assert.False(t, reproContains(inB, "msg-tagged-C"), "spaceB does NOT see spaceC's tagged msg")
	assert.False(t, reproContains(inB, "msg-UNTAGGED"),
		"FIXED symptom 1: untagged msg no longer leaks into non-default spaceB")

	// Default Space: the untagged message is retained (forward-compat).
	inDefault := reproCallChannelSync(t, s, reproSpaceDefault)
	assert.True(t, reproContains(inDefault, "msg-UNTAGGED"), "untagged msg retained in default Space")
	assert.False(t, reproContains(inDefault, "msg-tagged-B"), "default Space does NOT see spaceB's tagged msg")
}

// TestRepro484_Bot_SystemBotVisibleInEverySpace locks the contract that a system
// bot DM (here botfather) is present in the conversation list of EVERY Space —
// the #484 fix must not change this. Visibility holds via the SystemBots branch
// in decideConvKeepInSpace (space_filter.go) and/or the EnsureSystemBotsPresent
// fallback injection.
func TestRepro484_Bot_SystemBotVisibleInEverySpace(t *testing.T) {
	s, _ := reproSetup(t)
	// botfather DM with a spaceB-tagged recent — irrelevant to a system bot, which
	// is visible regardless of Space/messages.
	reproIMConv = &config.SyncUserConversationResp{
		ChannelID:   "botfather",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents:     []*config.MessageResp{reproMsg(1, "bot-hi", reproSpaceB)},
	}

	inDefault := reproCallConvSync(t, s, reproSpaceDefault)
	inC := reproCallConvSync(t, s, reproSpaceC)

	assert.True(t, reproContains(inDefault, "botfather"), "system bot visible in default Space")
	assert.True(t, reproContains(inC, "botfather"), "system bot visible in non-default spaceC too")
}

// TestRepro484_Bot_RegularBotRespectsSpaceMembership verifies a regular bot
// (user.robot=1) is visible only in Spaces it is a member of — and crucially that
// its dm_space_presence row (written by the webhook for ANY Person message with a
// space_id, bots included) is IGNORED: the presence/Recents check is gated behind
// `if !botSet[channelID]`, so a bot that messaged in spaceC but is NOT a spaceC
// member must stay hidden there. This locks the !botSet gate (space_filter.go:321).
func TestRepro484_Bot_RegularBotRespectsSpaceMembership(t *testing.T) {
	s, ctx := reproSetup(t)
	const botX = "bot_x_888"
	reproSeedBotUser(t, ctx, botX)                // recognized as a bot
	reproSeedBotMember(t, ctx, reproSpaceB, botX) // member of spaceB only

	// The bot sent a spaceC-tagged message → webhook writes presence(pair, spaceC).
	// The bot is NOT a spaceC member, so this row must NOT make it visible there.
	reproIngestPersonMsg(t, s, botX, testutil.UID, reproSpaceC, 1)

	reproIMConv = &config.SyncUserConversationResp{
		ChannelID:   botX,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents:     []*config.MessageResp{reproMsg(1, "bot-msg", reproSpaceC)},
	}

	inB := reproCallConvSync(t, s, reproSpaceB)
	inC := reproCallConvSync(t, s, reproSpaceC)

	assert.True(t, reproContains(inB, botX), "regular bot visible in spaceB (it is a member)")
	assert.False(t, reproContains(inC, botX),
		"regular bot HIDDEN in spaceC: not a member, and its dm_space_presence row is correctly ignored for bots")
}
