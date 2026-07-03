//go:build integration

package message

// Reproduction for the default-Space DM conversation-preview leak observed on
// the deployed test env (main) via
//   POST /v1/conversation/sync?space_id=:space_id
//
// Scenario (mirrors the real capture): one physical DM channel whose history is
//   - an UNTAGGED message  (no payload.space_id → belongs to the DEFAULT space)
//   - a message TAGGED with a NON-default space (the globally-latest one)
//
// Hypothesis under test:
//   1. Query the NON-default space  → space_last_message is that space's message
//      (correct — it is explicitly tagged).
//   2. Query the DEFAULT space      → space_last_message is ABSENT (nil), because
//      findSpaceLastMessage matches strictly on payload.space_id == filterSpaceID
//      and the default-space messages are UNTAGGED, so nothing matches. The client
//      then falls back to recents[last] = the channel's GLOBAL last message, which
//      is tagged for the OTHER space → the preview leaks a wrong-space message.
//
// This drives the REAL handler (syncUserConversation) through the registered
// /v1/conversation route (which mounts spacepkg.SpaceMiddleware, reading
// ?space_id=). WuKongIM is the shared httptest fake from
// conversation_recent_filter_e2e_test.go (same package/build tag): it returns
// our canned conversation for /conversation/sync and {} for /channel/messagesync
// (so the findSpaceLastMessageFallback path also finds no default-tagged message
// — same strict-match blind spot, exercised below as a pure-function check).

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

// TestRepro_SpaceLastMessage_DefaultSpaceLeak confirms the hypothesis end-to-end
// through POST /v1/conversation/sync?space_id=.
func TestRepro_SpaceLastMessage_DefaultSpaceLeak(t *testing.T) {
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

	// ---- (2) DEFAULT space: preview is WRONG (leaks the spaceB message) ----
	convD := findConv(callConvSyncSpace(t, s, spaceDefaul), dmChannel)
	require.NotNil(t, convD, "DM is (correctly) visible in the default space via catch-all")

	// The bug: no per-space preview could be computed for the default space,
	// because its messages are UNTAGGED and findSpaceLastMessage matches strictly
	// on space_id == defaultSpaceID.
	assert.Nil(t, convD.SpaceLastMessage,
		"BUG: default-space space_last_message is nil — untagged default messages never match the strict space_id filter")

	// With space_last_message absent, the client falls back to recents[last] =
	// the channel's global-last message, which belongs to the OTHER space.
	require.NotEmpty(t, convD.Recents, "recents present")
	lastRecent := convD.Recents[len(convD.Recents)-1]
	assert.Equal(t, "1111", lastRecent.Payload["content"],
		"LEAK: default-space fallback preview is the spaceB message content")
	assert.Equal(t, spaceB, lastRecent.Payload["space_id"],
		"LEAK: default-space fallback preview carries the WRONG space_id (spaceB, not the default space)")
}

// TestRepro_FindSpaceLastMessage_StrictMatchBlindSpot isolates the root cause:
// findSpaceLastMessage / the fallback both match strictly on payload.space_id ==
// spaceID, so for the DEFAULT space (whose DM messages are UNTAGGED) they return
// nil even when the default-space message IS in the scanned set. This is why the
// 200-message fallback in findSpaceLastMessageFallback cannot rescue it either.
func TestRepro_FindSpaceLastMessage_StrictMatchBlindSpot(t *testing.T) {
	const spaceB = "space-b-repro"
	recents := []*MsgSyncResp{
		{MessageSeq: 11, Payload: map[string]interface{}{"type": float64(1), "content": "default-space-hello"}}, // UNTAGGED
		{MessageSeq: 12, Payload: map[string]interface{}{"type": float64(1), "content": "1111", "space_id": spaceB}},
	}

	// Non-default space: the tagged message is found.
	got := findSpaceLastMessage(recents, spaceB)
	require.NotNil(t, got)
	assert.Equal(t, uint32(12), got.MessageSeq)

	// Default space: the untagged default message exists in the set but is NOT
	// matched (no space_id key) → nil. Same strict predicate the fallback uses.
	assert.Nil(t, findSpaceLastMessage(recents, "space-default-repro"),
		"strict space_id match cannot find the untagged default-space message")
}
