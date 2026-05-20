// Package bot_api · YUJ-1356 / Mininglamp-OSS/octo-server#96 — regression
// coverage for the OBO fan-out content-type contract.
//
// Background (E2E T03.3, 2026-05-19): the Persona Clone end-to-end suite
// reported text / image / voice inbound messages correctly triggered the
// OBO fan-out hook, but file (and video) inbound messages did NOT. Code
// audit of fanoutForMessage confirmed the listener path does not filter
// by content type — every gate is keyed on channel id, channel type, the
// sender uid, or the `__obo_processed__` payload marker. The early-out
// conditions only check that ChannelID is non-empty and (for DMs) FromUID
// is non-empty; both hold for every real client inbound regardless of
// payload type.
//
// This test file locks in that audit by exercising fanoutForMessage with
// every content type the production clients send today (octo-lib
// common.{Text,Image,GIF,Voice,Video,Location,Card,File,...}) plus the
// custom T03.3 type=9 the E2E suite happened to use. Each case must
// produce exactly one fan-out dispatch — otherwise a future regression
// that quietly adds a content-type filter (e.g. "skip CMD / system
// messages") to fanoutForMessage or any upstream listener-shim layer
// will fail here instead of slipping into prod.
//
// What this test does NOT do (intentional scope split):
//   - It does not test webhook handleMessageNotify. The webhook layer
//     gates on Header.SyncOnce==1 || Header.NoPersist==1 (cmd /
//     ephemeral messages do not reach listeners by design), but does not
//     filter by content type either. A separate integration test would
//     be needed to cover that wiring end-to-end with a real DB.
//   - It does not test WuKongIM's own webhook config (octo-deployment).
//     If WuKongIM is configured to suppress msg.notify for certain
//     payload types, no listener — fan-out or otherwise — will see them.
//     That's outside octo-server's reach; this file proves octo-server
//     itself is not adding any content-type filter.
package bot_api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
)

// TestFanout_ContentTypeAgnostic exercises every content type the Persona
// Clone client matrix can produce, including the T03.3 file (type=9 in
// the test harness, common.File=8 in octo-lib) and video (common.Video=5)
// cases the E2E report flagged as silently dropping fan-out copies.
//
// Each sub-case sends a single non-grantor / non-bot user message into a
// scoped GROUP channel and asserts:
//   - fanoutForMessage returns 1 (the gate cascade let it through)
//   - the capture hook recorded exactly one MsgSendReq
//   - the dispatch contract holds (channel_id XOR subscribers, see
//     assertFanoutDispatchContract for the WuKongIM mutex rationale)
//   - the fan-out copy's payload carries `obo_origin_channel_id` and
//     the marker `obo_fanout=true` so downstream consumers can route
//
// We intentionally use a GROUP channel for the matrix so the fan-out
// lookup (`fanoutLookupChannelID` → m.ChannelID for non-DM) is a single
// hop and the test focuses on the content-type axis. DM and
// community-topic fan-out are covered by the existing
// TestFanout_DispatchReq_NoConflict_ChannelOrSubscribers matrix.
func TestFanout_ContentTypeAgnostic(t *testing.T) {
	// Cover every octo-lib chat content type the bot API exposes, plus
	// the YUJ-1356 "client type=9" report value (some clients map file
	// to 9 instead of 8 — fan-out must not care about the numeric value).
	cases := []struct {
		name    string
		payload string
	}{
		{name: "text_type_1", payload: `{"type":1,"content":"hi"}`},
		{name: "image_type_2", payload: `{"type":2,"url":"https://cdn/img.png","width":100,"height":100}`},
		{name: "gif_type_3", payload: `{"type":3,"url":"https://cdn/a.gif","width":120,"height":80}`},
		{name: "voice_type_4", payload: `{"type":4,"url":"https://cdn/v.amr","duration":3}`},
		{name: "video_type_5", payload: `{"type":5,"url":"https://cdn/v.mp4","width":1280,"height":720,"duration":12}`},
		{name: "location_type_6", payload: `{"type":6,"latitude":31.2304,"longitude":121.4737,"address":"Shanghai"}`},
		{name: "card_type_7", payload: `{"type":7,"uid":"u_carol","name":"Carol"}`},
		// YUJ-1356 / octo-server#96 — octo-lib common.File.
		{name: "file_type_8", payload: `{"type":8,"url":"https://cdn/report.pdf","name":"report.pdf","size":12345}`},
		// YUJ-1356 — the literal "type=9" value the E2E report flagged.
		// Some client codepaths emit 9 for file instead of 8; either way
		// fan-out MUST fire because the listener does not consult `type`.
		{name: "file_type_9_e2e_report_value", payload: `{"type":9,"url":"https://cdn/q3-report.pdf","name":"q3-report.pdf","size":67890}`},
		{name: "multipleforward_type_11", payload: `{"type":11,"messages":[{"type":1,"content":"a"},{"type":1,"content":"b"}]}`},
		{name: "vector_sticker_type_12", payload: `{"type":12,"url":"https://cdn/sticker.json"}`},
		{name: "emoji_sticker_type_13", payload: `{"type":13,"url":"https://cdn/emoji.png"}`},
		{name: "rich_text_type_14", payload: `{"type":14,"content":[{"type":"text","text":"hello"}]}`},
		// Defensive: an inbound message with no `type` field at all
		// must STILL fan out — the fan-out listener has no business
		// rejecting payloads that fail the bot-side payload validator;
		// that's a sendMessage-ingress concern, not a relay concern.
		{name: "no_type_field", payload: `{"content":"some content"}`},
		// Defensive: non-JSON payload (e.g. a raw signal-encrypted blob
		// in some future code path). buildFanoutCopyReq falls back to a
		// `{"raw":..., "type":0}` envelope; fan-out must still dispatch.
		{name: "non_json_payload", payload: `binary-blob-not-json`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
			s := seedGrantWithScope(t, ch, ct)
			fc := &fanoutCapture{}
			ba := newBAforFanout(s, fc)

			msg := &config.MessageResp{
				FromUID:      "alice", // non-bot, non-grantor user
				ChannelID:    ch,
				ChannelType:  ct,
				MessageIDStr: fmt.Sprintf("origin_%s", tc.name),
				Payload:      []byte(tc.payload),
			}
			got := ba.fanoutForMessage(msg)
			if got != 1 {
				t.Fatalf("expected exactly 1 fan-out dispatch for %s payload, got %d", tc.name, got)
			}
			if len(fc.copies) != 1 {
				t.Fatalf("expected exactly 1 captured fan-out copy for %s payload, got %d", tc.name, len(fc.copies))
			}
			cp := fc.copies[0]
			if err := assertFanoutDispatchContract(cp); err != nil {
				t.Fatalf("dispatch contract violated for %s: %v (req=%+v)", tc.name, err, cp)
			}
			// The fan-out copy must always preserve origin routing context
			// regardless of payload shape. For non-JSON the buildFanoutCopyReq
			// fallback wraps the bytes under `raw` and still augments
			// obo_origin_*; for JSON it overlays the keys directly.
			var p map[string]interface{}
			if err := json.Unmarshal(cp.Payload, &p); err != nil {
				t.Fatalf("dispatched payload is not JSON for %s: %v (payload=%s)", tc.name, err, string(cp.Payload))
			}
			if p["obo_origin_channel_id"] != ch {
				t.Fatalf("%s: obo_origin_channel_id should be %q, got %v", tc.name, ch, p["obo_origin_channel_id"])
			}
			if v, _ := p["obo_fanout"].(bool); !v {
				t.Fatalf("%s: obo_fanout marker missing in payload %v", tc.name, p)
			}
		})
	}
}

// TestFanout_OBOMessagesListen_BatchAllContentTypes asserts that the
// MessagesListener entry point (the function actually wired into
// ctx.AddMessagesListener) fans out every message in a mixed-type batch.
// This catches a class of regressions where someone might add a batch-level
// `if anyOf(types) is unsupported { skip whole batch }` shortcut to
// oboMessagesListen — which would silently break file/video/etc dispatch
// the same way the E2E reported.
func TestFanout_OBOMessagesListen_BatchAllContentTypes(t *testing.T) {
	ch, ct := "group_77", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// Heterogeneous batch: every payload type the issue/E2E mentioned.
	batch := []*config.MessageResp{
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":1,"content":"hi"}`)},
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":2,"url":"https://cdn/i.png"}`)},
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":3,"url":"https://cdn/a.gif"}`)},
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":4,"url":"https://cdn/v.amr","duration":2}`)},
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":5,"url":"https://cdn/v.mp4","duration":10}`)},
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":8,"url":"https://cdn/r.pdf","name":"r.pdf","size":1024}`)},
		{FromUID: "alice", ChannelID: ch, ChannelType: ct, Payload: []byte(`{"type":9,"url":"https://cdn/f9","name":"file9.bin","size":2048}`)},
	}

	ba.oboMessagesListen(batch)

	if len(fc.copies) != len(batch) {
		// Build a per-type diagnostic so a regression points at exactly
		// which payload type was silently dropped.
		seen := map[string]int{}
		for _, cp := range fc.copies {
			var p map[string]interface{}
			_ = json.Unmarshal(cp.Payload, &p)
			typeKey := fmt.Sprintf("%v", p["type"])
			seen[typeKey]++
		}
		t.Fatalf("expected %d fan-out copies (one per batch message), got %d; per-type counts=%v",
			len(batch), len(fc.copies), seen)
	}
}
