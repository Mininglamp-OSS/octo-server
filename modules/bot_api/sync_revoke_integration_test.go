package bot_api

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSanitizeRevokedSyncResp_Integration exercises the real message_extra
// SELECT the bot history-pull path (/v1/bot/messages/sync) runs before it
// sanitizes revoked messages. It feeds a hand-built IM sync response (WuKongIM
// is not involved) so it needs only MySQL. Covers WS-168 acceptance:
//
//	(1) a revoked message → revoke=1 + placeholder body (no original text)
//	(3) a non-revoked message in the same page → body intact (no误伤)
//
// The channel/sync consistency case (2) is anchored structurally in
// modules/message TestRevokeSanitizeConsistency (both paths share revokedPayload).
func TestSanitizeRevokedSyncResp_Integration(t *testing.T) {
	_, ctx := newUpstreamTestServer(t)
	defer testutil.CleanAllTables(ctx)

	ba := &BotAPI{ctx: ctx, Log: log.NewTLog("bot-sync-revoke-test")}

	// message 111 is revoked (revoke=1, revoker=carol); 222 is a normal message
	// with no message_extra row at all.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id, channel_id, channel_type, `revoke`, revoker) VALUES (?, ?, ?, 1, ?)",
		"111", "c1", 2, "carol",
	).Exec()
	require.NoError(t, err)

	resp := &config.SyncChannelMessageResp{
		StartMessageSeq: 1,
		EndMessageSeq:   2,
		Messages: []*config.MessageResp{
			{MessageID: 111, FromUID: "alice", Timestamp: 1700,
				Payload: []byte(`{"type":1,"content":"revoked secret"}`),
				Streams: []*config.StreamItemResp{{StreamSeq: 1, Blob: []byte("blob")}}},
			{MessageID: 222, FromUID: "bob", Timestamp: 1701,
				Payload: []byte(`{"type":1,"content":"still visible"}`)},
		},
	}

	out, err := ba.sanitizeRevokedSyncResp(resp)
	require.NoError(t, err)
	require.Len(t, out.Messages, 2)

	// (1) revoked → placeholder, no original text, revoke flag + revoker present.
	revoked := out.Messages[0]
	assert.Equal(t, 1, revoked.Revoke)
	assert.Equal(t, "carol", revoked.Revoker)
	assert.Nil(t, revoked.Streams)
	assert.NotContains(t, string(revoked.Payload), "revoked secret")
	var pm map[string]interface{}
	require.NoError(t, json.Unmarshal(revoked.Payload, &pm))
	assert.EqualValues(t, 1, mustInt(pm["type"]))
	_, hasContent := pm["content"]
	assert.False(t, hasContent)

	// (3) non-revoked → intact, not误伤.
	kept := out.Messages[1]
	assert.Equal(t, 0, kept.Revoke)
	assert.Contains(t, string(kept.Payload), "still visible")
}

func mustInt(v interface{}) int64 {
	switch t := v.(type) {
	case json.Number:
		i, _ := t.Int64()
		return i
	case float64:
		return int64(t)
	}
	return -1
}
