// modules/message/api_reminders_test.go
//
// Reminder fan-out matrix tests for the mention three-state rewrite
// (YUJ-1343 / Mininglamp-OSS/octo-server#94) updated for Plan X
// (YUJ-1389). The chokepoint rewrite (pkg/mentionrewrite.RewriteMention)
// double-writes legacy `mention.all=1` to `mention.ais=1` so legacy
// `@所有人` traffic fans out to all AI bots without an SDK update on
// the sender side. A NEW field `mention.humans=1` is the explicit
// human-notification signal — it is the only way to produce a
// channel-level reminder (the "[有人@我]" red-dot). This file pins:
//
//  1. Message.getMention recognizes any of {humans, ais, all} = 1 as a
//     "broadcast" mention (so the caller knows to consider the
//     humans-gate), and still pulls per-user `uids` for the
//     non-broadcast path.
//  2. Message.getReminders emits a channel-level reminder ONLY when
//     `humans=1` is set on the payload. ais-only broadcasts (including
//     the legacy `all=1` shape after the chokepoint rewrite) produce
//     ZERO reminder rows — bots respond via the message delivery path,
//     so a "[有人@我]" for human members would be noise.
//  3. Explicit @uid mentions still produce per-uid reminder rows even
//     when the message ALSO carries a broadcast flag (`@所有人 + @alice`
//     must still ping @alice individually).
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
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
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
// semantics established in YUJ-1343 / GH#94 §RFC 6 and updated for
// Plan X (YUJ-1389): getMention still reports all=true for any of
// {humans, ais, all} = 1; the humans-gate that decides whether a
// channel-level reminder is actually emitted lives at the call site
// (see TestGetReminders_FanoutMatrix below).
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
			name:       "ais=1 alone → broadcast (read side sees it; emitter gates on humans)",
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
			name: "all=1 (Plan X post-rewrite carries ais=1) → broadcast",
			mention: map[string]interface{}{
				"all": json.Number("1"),
				"ais": json.Number("1"),
			},
			expectAll:  true,
			expectUIDs: nil,
		},
		{
			name: "legacy all=1 alone (no rewrite yet) → still broadcast (read-side resilience)",
			// This path SHOULDN'T happen in production once the
			// chokepoint runs, but if a listener somehow sees an
			// un-rewritten message (e.g. replay of historical data),
			// the reader must still recognize it as a broadcast.
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
			name: "humans=1 + uids → broadcast AND uids parsed",
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
// emitted for every cell of the Plan X (YUJ-1389) mention matrix.
// The fan-out behavior is:
//
//   - humans=1            → exactly ONE channel-level reminder
//     (UID="", "[有人@我]")
//   - ais=1 (only)        → ZERO reminders (bots respond via delivery)
//   - all=1 + ais=1       → ZERO reminders (post-rewrite ais-only)
//   - humans=1 + ais=1    → ONE channel-level reminder (humans visible,
//     bots fan out via delivery)
//   - uids = [a, b]       → one reminder PER uid, with UID=<uid>
//   - humans=1 + uids     → ONE channel-level reminder PLUS one
//     per-uid reminder for each uid (the broadcast and the explicit
//     mention coexist — `@所有人 + @alice` must still ping @alice)
//   - no mention          → zero reminders
//
// Role-aware delivery for bots is a downstream concern (bots subscribe
// to ais=1 messages directly through the message-delivery path). This
// test pins the server's reminder-emission contract.
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
			name:          "ais=1 only → 0 reminders (bots use delivery path)",
			mention:       map[string]interface{}{"ais": json.Number("1")},
			wantTotal:     0,
			wantBroadcast: 0,
		},
		{
			name: "humans=1 + ais=1 → 1 channel-level reminder (humans visible)",
			mention: map[string]interface{}{
				"humans": json.Number("1"),
				"ais":    json.Number("1"),
			},
			wantTotal:     1,
			wantBroadcast: 1,
		},
		{
			name: "all=1 + ais=1 (Plan X post-rewrite) → 0 reminders, ais-only semantics",
			mention: map[string]interface{}{
				"all": json.Number("1"),
				"ais": json.Number("1"),
			},
			wantTotal:     0,
			wantBroadcast: 0,
		},
		{
			name: "humans=1 + ais=1 + all=1 (legacy + new client double-tag) → 1 channel-level",
			mention: map[string]interface{}{
				"all":    json.Number("1"),
				"ais":    json.Number("1"),
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
			name: "humans=1 + uids → 1 channel-level + 1 per-user (broadcast and uid coexist)",
			mention: map[string]interface{}{
				"humans": json.Number("1"),
				"uids":   []interface{}{"u_alice"},
			},
			wantTotal:       2,
			wantBroadcast:   1,
			wantPerUserUIDs: []string{"u_alice"},
		},
		{
			name: "ais=1 + uids (post-rewrite ais-only with explicit @uid) → only per-user reminders",
			mention: map[string]interface{}{
				"ais":  json.Number("1"),
				"uids": []interface{}{"u_alice"},
			},
			wantTotal:       1,
			wantBroadcast:   0,
			wantPerUserUIDs: []string{"u_alice"},
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
			assert.Equal(t, tc.wantBroadcast, broadcasts, "broadcast count mismatch")
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

// TestGetReminders_LegacyAllRoundTripThroughRewrite is the end-to-end
// matrix cell that ties the chokepoint and the reader together. Under
// Mininglamp-OSS/octo-server#142 the chokepoint helper is now a strict
// pass-through (the historical Plan X / YUJ-1389 `all→ais` inference
// was removed because legacy `@所有人` must not auto-trigger bots), so
// the post-rewrite payload is still `{all:1}` with no `ais` and no
// `humans`. The reader-side outcome is unchanged: legacy `all=1` alone
// produces ZERO channel-level reminders (no humans gate trips), bots
// do NOT fan out (no explicit ais=1), and the legacy `all=1` field
// stays on the wire so old read-side clients keep rendering the
// `@所有人` pill.
func TestGetReminders_LegacyAllRoundTripThroughRewrite(t *testing.T) {
	m := newReminderTestMessage(t)

	// Legacy inbound shape.
	inbound := map[string]interface{}{
		"type": 1,
		"mention": map[string]interface{}{
			"all": json.Number("1"),
		},
	}
	// Chokepoint rewrite (post-#142: pass-through).
	rewritten := RewriteMention(inbound)
	mention := rewritten["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"],
		"all preserved verbatim (legacy read-side clients render @所有人 from it)")
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs,
		"#142: ais MUST NOT be inferred from legacy all=1 — bots only fire on explicit ais=1")
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans,
		"humans MUST NOT be auto-set by rewrite — only the sender may set it")

	// Reader sees the rewritten (pass-through) payload.
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
	assert.Len(t, rems, 0,
		"#142: legacy all=1 alone produces ZERO channel-level reminders (no humans gate trips)")
}

// TestGetReminders_HumansPlusAllRoundTrip — a client that wants BOTH
// the legacy `all=1` pill on old read-side clients AND a human-visible
// reminder sends `{all:1, humans:1}` inbound. Post-#142 the chokepoint
// is a pass-through, so the dispatched payload is still `{all:1,
// humans:1}` (no implicit `ais=1`). Reader must emit exactly ONE
// channel-level reminder (humans=1 is the gate).
func TestGetReminders_HumansPlusAllRoundTrip(t *testing.T) {
	m := newReminderTestMessage(t)

	inbound := map[string]interface{}{
		"type": 1,
		"mention": map[string]interface{}{
			"all":    json.Number("1"),
			"humans": json.Number("1"),
		},
	}
	rewritten := RewriteMention(inbound)
	mention := rewritten["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"])
	assert.Equal(t, json.Number("1"), mention["humans"])
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs,
		"#142: rewrite no longer infers ais=1 from legacy all=1")

	msg := &config.MessageResp{
		ChannelID:   "ch_humans_plus_all",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     "u_sender",
		MessageID:   2,
		MessageSeq:  2,
		ClientMsgNo: "cmn_humans_plus_all",
		Payload:     payloadJSON(t, rewritten),
	}
	rems := m.getReminders([]*config.MessageResp{msg})
	assert.Len(t, rems, 1, "humans=1 → exactly one channel-level reminder")
	assert.Equal(t, "", rems[0].UID, "broadcast reminder uses empty UID")
}

// TestFilterChannelLevelByPublisher pins YUJ-1377 / Mininglamp-OSS/
// octo-server#101 — the sender of an `@所有人` broadcast must NOT
// receive their own red-dot reminder. The fix lives in reminderSync,
// which calls filterChannelLevelByPublisher on the rows returned by
// remindersDB.sync; this test exercises the filter helper in
// isolation (pure data, no DB / IM context).
//
// Coverage matrix:
//   - channel-level reminder (UID="") authored by viewer → DROPPED
//   - channel-level reminder authored by someone else    → KEPT
//   - per-uid reminder addressed to viewer (e.g. @uid)   → KEPT
//   - per-uid apply-join-group reminder                  → KEPT
//     (this is the "do not break other reminder types"
//     guarantee from the issue description)
//   - empty/nil viewer UID                               → no-op
//   - empty slice                                        → no-op
//
// Plan X (YUJ-1389): channel-level reminders are now only created
// when `humans=1` is set, so this filter still applies to all rows
// that reach it — the gate just means fewer rows are produced in the
// first place. The filter contract is unchanged.
func TestFilterChannelLevelByPublisher(t *testing.T) {
	mk := func(uid, publisher string, reminderType int) *remindersDetailModel {
		return &remindersDetailModel{
			remindersModel: remindersModel{
				ChannelID:    "ch_team",
				ChannelType:  common.ChannelTypeGroup.Uint8(),
				UID:          uid,
				Publisher:    publisher,
				ReminderType: reminderType,
			},
		}
	}

	t.Run("drops channel-level reminder authored by viewer", func(t *testing.T) {
		input := []*remindersDetailModel{
			mk("", "u_sender", int(ReminderTypeMentionMe)),        // sender's own broadcast → drop
			mk("", "u_other", int(ReminderTypeMentionMe)),         // someone else's broadcast → keep
			mk("u_sender", "u_other", int(ReminderTypeMentionMe)), // @u_sender from u_other → keep
			mk("u_sender", "u_other", int(ReminderTypeApplyJoinGroup)),
		}
		got := filterChannelLevelByPublisher(input, "u_sender")
		assert.Len(t, got, 3, "exactly one row (the self-broadcast) must be dropped")
		for _, r := range got {
			if r.UID == "" {
				assert.NotEqual(t, "u_sender", r.Publisher,
					"no remaining channel-level reminder may be authored by the viewer")
			}
		}
	})

	t.Run("keeps everything when viewer is not the publisher", func(t *testing.T) {
		input := []*remindersDetailModel{
			mk("", "u_alice", int(ReminderTypeMentionMe)),
			mk("u_bob", "u_alice", int(ReminderTypeMentionMe)),
		}
		got := filterChannelLevelByPublisher(input, "u_bob")
		assert.Equal(t, input, got, "no-op path must return the input slice unchanged")
	})

	t.Run("preserves apply-join-group reminders addressed to viewer", func(t *testing.T) {
		// Apply-join-group reminders carry an explicit UID and have no
		// Publisher set by getReminders; the filter must never drop
		// them even if Publisher happens to coincide with the viewer.
		input := []*remindersDetailModel{
			mk("u_admin", "u_admin", int(ReminderTypeApplyJoinGroup)),
		}
		got := filterChannelLevelByPublisher(input, "u_admin")
		assert.Equal(t, input, got, "apply-join-group must pass through verbatim")
	})

	t.Run("no-op on empty viewer uid", func(t *testing.T) {
		input := []*remindersDetailModel{
			mk("", "u_alice", int(ReminderTypeMentionMe)),
		}
		got := filterChannelLevelByPublisher(input, "")
		assert.Equal(t, input, got)
	})

	t.Run("no-op on empty slice", func(t *testing.T) {
		got := filterChannelLevelByPublisher(nil, "u_sender")
		assert.Nil(t, got)
		got = filterChannelLevelByPublisher([]*remindersDetailModel{}, "u_sender")
		assert.Empty(t, got)
	})
}

// TestReminderSync_SenderExcludedFromBroadcast is the contract-level
// regression: the sender of `@所有人` is excluded from the reminder
// list returned by the read path. We exercise the filter step here
// (the DB layer is covered by integration tests); together with
// TestGetReminders_FanoutMatrix this pins both "row is created" and
// "row is hidden from the author on read".
func TestReminderSync_SenderExcludedFromBroadcast(t *testing.T) {
	// Simulate what remindersDB.sync would return for u_sender after
	// u_sender broadcast `@所有人` and an unrelated peer also did.
	rows := []*remindersDetailModel{
		{remindersModel: remindersModel{
			ChannelID: "ch_team", ChannelType: common.ChannelTypeGroup.Uint8(),
			UID: "", Publisher: "u_sender", ReminderType: int(ReminderTypeMentionMe),
			Text: "[有人@我]",
		}},
		{remindersModel: remindersModel{
			ChannelID: "ch_team", ChannelType: common.ChannelTypeGroup.Uint8(),
			UID: "", Publisher: "u_peer", ReminderType: int(ReminderTypeMentionMe),
			Text: "[有人@我]",
		}},
	}

	got := filterChannelLevelByPublisher(rows, "u_sender")
	assert.Len(t, got, 1, "sender must see only the peer's broadcast, not their own")
	assert.Equal(t, "u_peer", got[0].Publisher)

	// And a third party sees both.
	got = filterChannelLevelByPublisher(rows, "u_bystander")
	assert.Len(t, got, 2)
}

// TestGetReminders_AisExpansionDoesNotPolluteReminderRows is the
// PR#145 review-blocker regression (R3, YUJ-1810).
//
// Production flow we are pinning
// ==============================
//  1. sendMessage builds the in-memory `payload`.
//  2. wirePayload = CloneForExpansion(payload) →
//     ExpandAisToBotUIDs(wirePayload) stamps bot UIDs into
//     wirePayload["mention"]["uids"].
//  3. MsgSendReq.Payload = util.ToJson(wirePayload) — WuKongIM
//     persists the EXPANDED bytes.
//  4. WuKongIM webhooks back → listenerMessages → getReminders, which
//     decodes the persisted (expanded) bytes via
//     config.MessageResp.GetPayloadMap.
//
// So `getReminders` sees `mention.uids = [u_alice, bot_a, bot_b]` —
// NOT the original `[u_alice]`. CloneForExpansion alone is
// insufficient: it only protects the send-side in-memory map; the
// listener path doesn't touch that map at all.
//
// Required guard
// ==============
// getReminders must call robotService.ExistRobot for each UID and
// skip bots — that's the single source of truth for "no reminder
// rows for bots", and it covers both server-expanded ais UIDs AND
// user-typed explicit @bot_x mentions (bots never need a red-dot).
//
// What this test pins
// ===================
//  1. Wire bytes carry the expansion (legacy-adapter compat —
//     same assertion as before).
//  2. When the PERSISTED (= expanded) bytes are fed to getReminders
//     with a robotService that knows {bot_a, bot_b} are bots, the
//     emitted reminder set contains exactly ONE row for `u_alice`.
//     Zero rows for `bot_a`, zero for `bot_b`, zero channel-level
//     (no `humans=1`).
//
// If a future refactor removes the ExistRobot filter, this test
// fails on bot_a / bot_b leaking into the reminder set.
func TestGetReminders_AisExpansionDoesNotPolluteReminderRows(t *testing.T) {
	const channelID = "ch_team"

	// Caller-supplied payload: `@所有 AI + @alice` in a GROUP channel.
	// The explicit human mention (`u_alice`) MUST still get a per-user
	// reminder row; the broadcast (`ais=1`) MUST NOT — bots will
	// respond via the delivery path.
	original := map[string]interface{}{
		"type":    1,
		"content": "@所有 AI plus @alice",
		"mention": map[string]interface{}{
			"ais":  json.Number("1"),
			"uids": []interface{}{"u_alice"},
		},
	}

	// Simulate the chokepoint at sendMessage(): clone, expand on the
	// clone, serialize the clone for the wire. Same shape as the
	// production code at modules/message/api.go.
	wire := mentionrewrite.CloneForExpansion(original)
	wire = mentionrewrite.ExpandAisToBotUIDs(wire, common.ChannelTypeGroup.Uint8(), channelID,
		func(string) ([]string, error) {
			return []string{"bot_a", "bot_b"}, nil
		})

	// Assertion #1: wire bytes carry the expansion so legacy adapter
	// bots that only inspect `mention.uids` on the WuKongIM payload
	// still see the `@所有 AI` broadcast.
	wireMention := wire["mention"].(map[string]interface{})
	wireUIDs, _ := wireMention["uids"].([]interface{})
	assert.ElementsMatch(t,
		[]interface{}{"u_alice", "bot_a", "bot_b"},
		wireUIDs,
		"wire payload must include expanded bot UIDs for legacy adapter compat")

	// Assertion #2: feed the EXPANDED wire payload bytes into
	// getReminders (this is exactly what the listener path sees —
	// WuKongIM persists the expanded bytes and webhooks them back via
	// config.MessageResp.GetPayloadMap). The robotService recognizes
	// bot_a / bot_b as bots, so getReminders' new ExistRobot guard
	// must filter them out and emit ONLY the row for u_alice.
	m := newReminderTestMessage(t)
	m.robotService = &fakeExpandRobotService{
		exist: map[string]bool{
			"bot_a":   true,
			"bot_b":   true,
			"u_alice": false,
		},
	}
	msg := &config.MessageResp{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     "u_sender",
		MessageID:   2024,
		MessageSeq:  42,
		ClientMsgNo: "cmn_pr145",
		Payload:     payloadJSON(t, wire),
	}
	got := m.getReminders([]*config.MessageResp{msg})

	// Exactly one reminder, for the human `u_alice`. Zero reminders
	// for `bot_a` / `bot_b` / the channel-level broadcast (no
	// `humans=1`, so no `[有人@我]` red-dot either).
	assert.Len(t, got, 1, "exactly one reminder row, for the explicit human mention")
	assert.Equal(t, "u_alice", got[0].UID,
		"reminder must be for the explicit human UID, NOT a server-expanded bot UID")
	for _, r := range got {
		assert.NotEqual(t, "bot_a", r.UID, "bot_a must not receive a reminder row")
		assert.NotEqual(t, "bot_b", r.UID, "bot_b must not receive a reminder row")
		assert.NotEqual(t, "", r.UID, "no channel-level [有人@我] red-dot for ais-only broadcast")
	}
}
