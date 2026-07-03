//go:build integration

package message

// Regression guard for the default-Space DM conversation-preview leak observed
// on the deployed test env (main) via
//   POST /v1/conversation/sync?space_id=:space_id
//
// Scenario (mirrors the real capture): one physical DM channel whose history is
//   - an UNTAGGED message  (no payload.space_id → belongs to the DEFAULT space)
//   - a message TAGGED with a NON-default space (the globally-latest one)
//
// Behaviour after the fix (fillPersonSpaceUnread/findSpaceLastMessage now take
// defaultSpaceID and treat an untagged DM message as a default-Space message):
//   1. Query the NON-default space → space_last_message is that space's message
//      (explicitly tagged); the untagged default message never leaks in.
//   2. Query the DEFAULT space     → space_last_message is the untagged default
//      message (NOT the other space's globally-latest one). Before the fix this
//      was nil, so the client fell back to recents[last] and leaked a wrong-space
//      preview.
//
// This drives the REAL handler (syncUserConversation) through the registered
// /v1/conversation route (which mounts spacepkg.SpaceMiddleware, reading
// ?space_id=). WuKongIM is the shared httptest fake from
// conversation_recent_filter_e2e_test.go (same package/build tag).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dmMsgRepro builds one raw DM message with the given seq/timestamp/payload JSON.
func dmMsgRepro(channelID string, seq uint32, ts int64, payloadJSON string) *config.MessageResp {
	return &config.MessageResp{
		MessageID:   int64(seq)*1000 + 1,
		MessageSeq:  seq,
		ClientMsgNo: channelID + "-" + strconv.FormatUint(uint64(seq), 10),
		FromUID:     channelID,
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   int32(ts),
		Setting:     0, // not a signal message → payload parsed into the map
		IsDeleted:   0,
		Payload:     []byte(payloadJSON),
	}
}

// dmIMConvMulti builds a DM conversation (as IMSyncUserConversation returns it)
// carrying multiple recent messages; LastMsgSeq tracks the newest.
func dmIMConvMulti(channelID string, ts, version int64, msgs []*config.MessageResp) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Unread:      0,
		Timestamp:   ts,
		LastMsgSeq:  int64(msgs[len(msgs)-1].MessageSeq),
		Version:     version,
		Recents:     msgs,
	}
}

// seedSpaceMemberRepro inserts a space + membership for uid with an explicit
// created_at (so GetUserDefaultSpaceIDE — earliest membership — is deterministic)
// and clears the Redis membership cache so the middleware re-checks the DB.
func seedSpaceMemberRepro(t *testing.T, ctx *config.Context, spaceID, uid, createdAt string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status, created_at, updated_at) VALUES (?,?,?,1,?,?)",
		spaceID, spaceID, uid, createdAt, createdAt).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `space_member` (space_id, uid, role, status, created_at, updated_at) VALUES (?,?,0,1,?,?)",
		spaceID, uid, createdAt, createdAt).Exec()
	require.NoError(t, err)
	_ = ctx.GetRedisConn().Del("space:member:" + spaceID + ":" + uid)
}

// callConvSyncSpace POSTs to the real route with ?space_id= and returns the full
// decoded conversation objects (so we can inspect space_last_message / recents).
func callConvSyncSpace(t *testing.T, s *server.Server, spaceID string) []*SyncUserConversationResp {
	t.Helper()
	body := `{"msg_count":50,"device_uuid":"dev-repro-` + spaceID + `"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/conversation/sync?space_id="+spaceID, strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var wrap struct {
		Conversations []*SyncUserConversationResp `json:"conversations"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wrap))
	return wrap.Conversations
}

func findConv(convs []*SyncUserConversationResp, channelID string) *SyncUserConversationResp {
	for _, c := range convs {
		if c.ChannelID == channelID {
			return c
		}
	}
	return nil
}

// TestFix_SpaceLastMessage_DefaultSpaceUsesUntagged verifies the fix end-to-end
// through POST /v1/conversation/sync?space_id=.
func TestFix_SpaceLastMessage_DefaultSpaceUsesUntagged(t *testing.T) {
	const (
		dmChannel   = "dm-peer-repro"
		spaceB      = "space-b-repro"       // a non-default space
		spaceDefaul = "space-default-repro" // earliest membership → default space
	)

	now := time.Now()
	// History: seq 11 = UNTAGGED (default-space message), seq 12 = tagged spaceB
	// (globally latest). Matches the real capture where the latest msg is 668cc9.
	untagged := dmMsgRepro(dmChannel, 11, now.Add(-2*time.Hour).Unix(),
		`{"type":1,"content":"default-space-hello"}`)
	taggedB := dmMsgRepro(dmChannel, 12, now.Add(-1*time.Hour).Unix(),
		`{"type":1,"content":"1111","space_id":"`+spaceB+`"}`)

	convs := []*config.SyncUserConversationResp{
		dmIMConvMulti(dmChannel, now.Add(-1*time.Hour).Unix(), 100, []*config.MessageResp{untagged, taggedB}),
	}

	s, ctx := setupConvSyncE2E(t, convs)
	// Default space MUST have the earlier created_at (GetUserDefaultSpaceIDE =
	// earliest membership by created_at ASC).
	seedSpaceMemberRepro(t, ctx, spaceDefaul, testutil.UID, "2020-01-01 00:00:00")
	seedSpaceMemberRepro(t, ctx, spaceB, testutil.UID, "2020-06-01 00:00:00")

	// ---- (1) NON-default space: preview is correct (explicitly tagged) ----
	convB := findConv(callConvSyncSpace(t, s, spaceB), dmChannel)
	require.NotNil(t, convB, "DM must be visible in its tagged (non-default) space")
	require.NotNil(t, convB.SpaceLastMessage,
		"non-default space: space_last_message present (tagged message found)")
	assert.Equal(t, "1111", convB.SpaceLastMessage.Payload["content"],
		"non-default space: preview is the spaceB-tagged message")
	assert.Equal(t, spaceB, convB.SpaceLastMessage.Payload["space_id"])

	// ---- (2) DEFAULT space: preview is now the untagged default message ----
	convD := findConv(callConvSyncSpace(t, s, spaceDefaul), dmChannel)
	require.NotNil(t, convD, "DM is visible in the default space")

	// Fixed: the untagged message counts as a default-Space message, so the
	// per-Space preview resolves to it instead of leaking the spaceB message.
	require.NotNil(t, convD.SpaceLastMessage,
		"default-space space_last_message is now populated from the untagged default message")
	assert.Equal(t, "default-space-hello", convD.SpaceLastMessage.Payload["content"],
		"default-space preview is the untagged (default) message, not the spaceB one")
	_, hasSpaceID := convD.SpaceLastMessage.Payload["space_id"]
	assert.False(t, hasSpaceID,
		"the default-space preview message carries no space_id (it is the untagged one)")
}

// TestFix_SystemBotUntaggedNotInDefaultPreview is the system-bot parity regression
// guard (PR #532 review by yujiawei/mochashanyao/Jerry-Xin/OctoBoooot): a system-bot
// DM (botfather) whose default-Space history is untagged must NOT surface a
// default-Space space_last_message or unread badge — mirroring the history filter's
// rule-4 drop, so preview/badge can't show what /v1/message/channel/sync hides.
func TestFix_SystemBotUntaggedNotInDefaultPreview(t *testing.T) {
	const (
		botChannel  = "botfather" // spacepkg.IsSystemBot(botfather) == true
		spaceDefaul = "space-default-bot"
	)
	now := time.Now()
	// Untagged system-bot history (default-Space by the regular-DM convention, but
	// rule 4 drops it for system bots).
	m1 := dmMsgRepro(botChannel, 11, now.Add(-2*time.Hour).Unix(), `{"type":1,"content":"bot-a"}`)
	m2 := dmMsgRepro(botChannel, 12, now.Add(-1*time.Hour).Unix(), `{"type":1,"content":"bot-b"}`)
	conv := dmIMConvMulti(botChannel, now.Add(-1*time.Hour).Unix(), 100, []*config.MessageResp{m1, m2})
	conv.Unread = 2 // exercise the unread-count path
	convs := []*config.SyncUserConversationResp{conv}

	s, ctx := setupConvSyncE2E(t, convs)
	seedSpaceMemberRepro(t, ctx, spaceDefaul, testutil.UID, "2020-01-01 00:00:00")

	convD := findConv(callConvSyncSpace(t, s, spaceDefaul), botChannel)
	require.NotNil(t, convD, "system-bot DM stays visible in the conversation list")
	assert.Nil(t, convD.SpaceLastMessage,
		"untagged system-bot messages must not become the default-space preview (rule-4 parity)")
	if convD.SpaceUnread != nil {
		assert.Equal(t, 0, *convD.SpaceUnread,
			"untagged system-bot messages must not be counted as default-space unread")
	}
}

// TestFix_FindSpaceLastMessage_DefaultMatchesUntagged pins the root-cause fix at
// the helper level: findSpaceLastMessage now takes defaultSpaceID and treats an
// untagged DM message as a default-Space message (the same predicate the 200-msg
// fallback uses), while non-default queries stay strict.
func TestFix_FindSpaceLastMessage_DefaultMatchesUntagged(t *testing.T) {
	const (
		spaceB       = "space-b-repro"
		spaceDefault = "space-default-repro"
	)
	recents := []*MsgSyncResp{
		{MessageSeq: 11, Payload: map[string]interface{}{"type": float64(1), "content": "default-space-hello"}}, // UNTAGGED
		{MessageSeq: 12, Payload: map[string]interface{}{"type": float64(1), "content": "1111", "space_id": spaceB}},
	}

	// Non-default space: the tagged message is found; the untagged one never leaks.
	got := findSpaceLastMessage(recents, spaceB, spaceDefault, false)
	require.NotNil(t, got)
	assert.Equal(t, uint32(12), got.MessageSeq)

	// Default space: the untagged default message is now matched (was nil before).
	got = findSpaceLastMessage(recents, spaceDefault, spaceDefault, false)
	require.NotNil(t, got, "untagged default-space message is now resolved")
	assert.Equal(t, uint32(11), got.MessageSeq)

	// Guard: with no default configured (defaultSpaceID==""), the untagged=default
	// convention is off and a non-default query stays strict.
	assert.Nil(t, findSpaceLastMessage(recents, spaceDefault, "", false))
}
