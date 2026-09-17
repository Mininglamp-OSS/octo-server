package bot_api

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildBotSyncResp_RevokeSanitize covers the pure transformation the bot
// history-pull path (/v1/bot/messages/sync) applies once message_extra has been
// consulted. Full send→revoke→sync HTTP coverage lives in the integration test
// (needs MySQL/Redis/WuKongIM); this locks the sanitize/no-touch decision.
func TestBuildBotSyncResp_RevokeSanitize(t *testing.T) {
	payloadOf := func(t *testing.T, m *botSyncMessage) map[string]interface{} {
		t.Helper()
		var pm map[string]interface{}
		require.NoError(t, json.Unmarshal(m.Payload, &pm))
		return pm
	}

	newResp := func() *config.SyncChannelMessageResp {
		return &config.SyncChannelMessageResp{
			StartMessageSeq: 10,
			EndMessageSeq:   20,
			PullMode:        config.PullModeDown,
			Messages: []*config.MessageResp{
				{MessageID: 111, FromUID: "alice", Timestamp: 1700,
					Payload: []byte(`{"type":1,"content":"revoked secret"}`),
					Streams: []*config.StreamItemResp{{StreamSeq: 1, Blob: []byte("blob")}}},
				{MessageID: 222, FromUID: "bob", Timestamp: 1701,
					Payload: []byte(`{"type":1,"content":"still visible"}`)},
			},
		}
	}

	t.Run("revoked message → placeholder body + revoke flag; non-revoked untouched", func(t *testing.T) {
		resp := newResp()
		out := buildBotSyncResp(resp, map[string]string{"111": "carol"})

		// top-level shape preserved
		assert.Equal(t, uint32(10), out.StartMessageSeq)
		assert.Equal(t, uint32(20), out.EndMessageSeq)
		require.Len(t, out.Messages, 2)

		// Case 1: revoked → revoke=1/revoker, body stripped, streams cleared.
		revoked := out.Messages[0]
		assert.Equal(t, 1, revoked.Revoke)
		assert.Equal(t, "carol", revoked.Revoker)
		assert.Nil(t, revoked.Streams)
		pm := payloadOf(t, revoked)
		assert.Equal(t, float64(common.Text.Int()), pm["type"])
		_, hasContent := pm["content"]
		assert.False(t, hasContent, "revoked content must not leak")

		// Case 3: non-revoked → intact, no revoke flag (不误伤).
		kept := out.Messages[1]
		assert.Equal(t, 0, kept.Revoke)
		assert.Empty(t, kept.Revoker)
		assert.Equal(t, "still visible", payloadOf(t, kept)["content"])
	})

	t.Run("empty revoked set leaves every message intact", func(t *testing.T) {
		resp := newResp()
		out := buildBotSyncResp(resp, nil)
		require.Len(t, out.Messages, 2)
		for _, m := range out.Messages {
			assert.Equal(t, 0, m.Revoke)
			assert.Contains(t, string(m.Payload), "content")
		}
	})
}

// TestBotSyncMessage_JSONShapeIsAdditive guards the wire-compat guarantee: the
// bot sync response must keep every field the raw config.MessageResp exposed
// (existing adapters parse `payload` as base64 bytes) and only ADD revoke/revoker.
func TestBotSyncMessage_JSONShapeIsAdditive(t *testing.T) {
	m := &botSyncMessage{
		MessageResp: &config.MessageResp{MessageID: 7, FromUID: "u", Payload: []byte(`{"type":1}`)},
		Revoke:      1,
		Revoker:     "r",
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &got))

	// embedded MessageResp fields promoted to the top level (unchanged names)
	assert.Contains(t, got, "message_id")
	assert.Contains(t, got, "from_uid")
	assert.Contains(t, got, "payload")
	// additive revoke signal
	assert.Equal(t, float64(1), got["revoke"])
	assert.Equal(t, "r", got["revoker"])
}
