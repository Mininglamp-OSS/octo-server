// modules/message/api_reminders_test.go
//
// Reminder fan-out matrix tests for the mention three-state rewrite
// (YUJ-1343 / Mininglamp-OSS/octo-server#94). The chokepoint rewrite
// (pkg/mentionrewrite.RewriteMention) double-writes legacy
// `mention.all=1` to `mention.humans=1`, and a new client may also set
// `mention.ais=1`. This file pins:
//
//  1. Message.getMention recognizes any of {humans, ais, all} = 1 as a
//     "broadcast" mention (channel-level reminder), and still pulls
//     per-user `uids` for the non-broadcast path.
//  2. Message.getReminders generates exactly the right shape of
//     reminder rows for each matrix cell — channel-level (UID="")
//     for broadcasts, per-uid rows for explicit uids, and zero rows
//     when the payload has no mention at all.
//
// These tests are pure helpers (no DB / no IM context) so they live
// next to the existing mention-shape suite in validation_test.go.
package message

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

// payloadJSON marshals the given map and re-decodes with UseNumber so
// the resulting map[string]interface{} mirrors what
// config.MessageResp.GetPayloadMap returns in production (UseNumber is
// the documented contract — see modules/message/validation_test.go for
// the same pattern).
func payloadJSON(t *testing.T, m map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestGetMention_ThreeStateMatrix locks the three-state read-side
// semantics established in YUJ-1343 / GH#94 §RFC 6.
func TestGetMention_ThreeStateMatrix(t *testing.T) {
	m := &Message{}

	cases := []struct {
		name       string
		mention    map[string]interface{}
		expectAll  bool
		expectUIDs []string
	}{
		{
			name:       "humans=1 alone → broadcast",
			mention:    map[string]interface{}{"humans": json.Number("1")},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			name:       "ais=1 alone → broadcast",
			mention:    map[string]interface{}{"ais": json.Number("1")},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			name: "humans=1 + ais=1 → broadcast",
			mention: map[string]interface{}{
				"humans": json.Number("1"),
				"ais":    json.Number("1"),
			},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			name: "all=1 (post-rewrite carries humans=1) → broadcast",
			mention: map[string]interface{}{
				"all":    json.Number("1"),
				"humans": json.Number("1"),
			},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			name: "legacy all=1 alone (no rewrite yet) → still broadcast (read-side resilience)",
			// This path SHOULDN'T happen in production once the
			// chokepoint runs, but if a listener somehow sees an
			// un-rewritten message (e.g. replay of historical data),
			// the reader must still emit a reminder.
			mention:    map[string]interface{}{"all": json.Number("1")},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			name:       "uids only → per-uid",
			mention:    map[string]interface{}{"uids": []interface{}{"u_alice", "u_bob"}},
			expectAll:  false,
			expectUIDs: []string{"u_alice", "u_bob"},
		},
		{
			name: "humans=1 + uids → broadcast wins (uids still parsed)",
			mention: map[string]interface{}{
				"humans": json.Number("1"),
				"uids":   []interface{}{"u_alice"},
			},
			expectAll:  true,
			expectUIDs: []string{"u_alice"},
		},
		{
			name:       "humans=0 + ais=0 + all=0 → no broadcast",
			mention:    map[string]interface{}{"humans": json.Number("0"), "ais": json.Number("0"), "all": json.Number("0")},
			expectAll:  false,
			expectUIDs: nil,
		},
		{
			name:       "empty mention map → no broadcast",
			mention:    map[string]interface{}{},
			expectAll:  false,
			expectUIDs: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]interface{}{"mention": tc.mention}
			// Round-trip through JSON+UseNumber so the test maps
			// match the production decoder shape.
			raw := payloadJSON(t, payload)
			var decoded map[string]interface{}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.UseNumber()
			if err := dec.Decode(&decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			gotAll, gotUIDs := m.getMention(decoded)
			assert.Equal(t, tc.expectAll, gotAll, "all-flag mismatch for %s", tc.name)
			assert.Equal(t, tc.expectUIDs, gotUIDs, "uids mismatch for %s", tc.name)
		})
	}
}

// newReminderTestMessage returns a Message whose reminder-version
// generator is a deterministic in-memory counter. Lets the matrix
// helpers exercise getReminders without standing up the seq table /
// MySQL / IM context. See Message.reminderSeqOverride in api.go.
func newReminderTestMessage(t *testing.T) *Message {
	t.Helper()
	var seq int64
	return &Message{
		reminderSeqOverride: func() (int64, error) {
			seq++
			return seq, nil
		},
	}
}

// TestGetReminders_FanoutMatrix asserts the SHAPE of reminders
// emitted for every cell of the three-state mention matrix. The fan-out
// behavior is:
//
//   - any of {humans, ais, all} = 1 → exactly ONE reminder per message
//     with UID="" (channel-level — clients filter by role in adapter)
//   - uids = [a, b]                 → one reminder PER uid, with
//     UID=<uid>
//   - no mention                    → zero reminders
//
// Role-aware delivery (humans-only vs bots-only) is intentionally a
// client-side adapter concern — see api_reminders.go:getMention for
// the rationale. This test pins the server contract.
func TestGetReminders_FanoutMatrix(t *testing.T) {
	m := newReminderTestMessage(t)

	cases := []struct {
		name            string
		mention         map[string]interface{}
		wantTotal       int
		wantBroadcast   int // reminders with UID==""
		wantPerUserUIDs []string
	}{
		{
			name:          "humans=1 → 1 channel-level reminder",
			mention:       map[string]interface{}{"humans": json.Number("1")},
			wantTotal:     1,
			wantBroadcast: 1,
		},
		{
			name:          "ais=1 → 1 channel-level reminder (bots fan-out)",
			mention:       map[string]interface{}{"ais": json.Number("1")},
			wantTotal:     1,
			wantBroadcast: 1,
		},
		{
			name: "humans+ais → still ONE channel-level reminder (client filters role)",
			mention: map[string]interface{}{
				"humans": json.Number("1"),
				"ais":    json.Number("1"),
			},
			wantTotal:     1,
			wantBroadcast: 1,
		},
		{
			name: "all=1 (rewrite double-wrote humans=1) → 1 channel-level reminder, humans-only semantics",
			mention: map[string]interface{}{
				"all":    json.Number("1"),
				"humans": json.Number("1"),
			},
			wantTotal:     1,
			wantBroadcast: 1,
		},
		{
			name:            "uids only → 2 per-user reminders",
			mention:         map[string]interface{}{"uids": []interface{}{"u_alice", "u_bob"}},
			wantTotal:       2,
			wantPerUserUIDs: []string{"u_alice", "u_bob"},
		},
		{
			name: "humans=1 + uids → broadcast wins (1 channel-level)",
			mention: map[string]interface{}{
				"humans": json.Number("1"),
				"uids":   []interface{}{"u_alice"},
			},
			wantTotal:     1,
			wantBroadcast: 1,
		},
		{
			name:      "no mention → 0 reminders",
			mention:   nil,
			wantTotal: 0,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]interface{}{"type": 1, "content": "msg"}
			if tc.mention != nil {
				payload["mention"] = tc.mention
			}
			msg := &config.MessageResp{
				ChannelID:   fmt.Sprintf("ch_%d", i),
				ChannelType: common.ChannelTypeGroup.Uint8(),
				FromUID:     "u_sender",
				MessageID:   int64(1000 + i),
				MessageSeq:  uint32(10 + i),
				ClientMsgNo: fmt.Sprintf("cmn_%d", i),
				Payload:     payloadJSON(t, payload),
			}
			got := m.getReminders([]*config.MessageResp{msg})
			assert.Equal(t, tc.wantTotal, len(got), "reminder count mismatch")
			if tc.wantTotal == 0 {
				return
			}
			var (
				broadcasts int
				perUserSet = map[string]bool{}
			)
			for _, r := range got {
				if r.UID == "" {
					broadcasts++
				} else {
					perUserSet[r.UID] = true
				}
				assert.Equal(t, ReminderTypeMentionMe, r.ReminderType)
				assert.Equal(t, msg.ChannelID, r.ChannelID)
				assert.Equal(t, msg.ChannelType, r.ChannelType)
				assert.Equal(t, msg.FromUID, r.Publisher)
				assert.Equal(t, fmt.Sprintf("%d", msg.MessageID), r.MessageID)
			}
			if tc.wantBroadcast > 0 {
				assert.Equal(t, tc.wantBroadcast, broadcasts, "broadcast count mismatch")
			}
			if len(tc.wantPerUserUIDs) > 0 {
				want := map[string]bool{}
				for _, u := range tc.wantPerUserUIDs {
					want[u] = true
				}
				assert.Equal(t, want, perUserSet, "per-user uid set mismatch")
			}
		})
	}
}

// TestGetReminders_AllAndHumans_RoundTripThroughRewrite is the
// end-to-end matrix cell that ties the chokepoint and the reader
// together: a legacy `mention.all=1` payload, after passing through
// the chokepoint rewrite, still produces exactly ONE channel-level
// reminder with humans-only semantics (Yu D1 — bots silent).
func TestGetReminders_AllAndHumans_RoundTripThroughRewrite(t *testing.T) {
	m := newReminderTestMessage(t)

	// Legacy inbound shape.
	inbound := map[string]interface{}{
		"type": 1,
		"mention": map[string]interface{}{
			"all": json.Number("1"),
		},
	}
	// Chokepoint rewrite.
	rewritten := RewriteMention(inbound)
	mention := rewritten["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"], "all preserved (outbound double-write)")
	assert.Equal(t, json.Number("1"), mention["humans"], "humans added by rewrite")

	// Reader sees the rewritten payload.
	msg := &config.MessageResp{
		ChannelID:   "ch_roundtrip",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     "u_sender",
		MessageID:   1,
		MessageSeq:  1,
		ClientMsgNo: "cmn_roundtrip",
		Payload:     payloadJSON(t, rewritten),
	}
	rems := m.getReminders([]*config.MessageResp{msg})
	assert.Len(t, rems, 1, "round-trip must produce exactly one broadcast reminder")
	assert.Equal(t, "", rems[0].UID, "broadcast reminder uses empty UID")
}
