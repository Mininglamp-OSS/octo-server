// Package bot_api · YUJ-1166 — Unit tests for the fan-out listener.
//
// Each of the three loop-protection gates (RFC §5.3) has a dedicated test
// asserting it short-circuits BEFORE dispatching to the grantee bot, plus
// a happy-path test confirming a regular inbound is fanned out.
//
// Test surface: fanoutForMessage (single-message entry) + oboFanoutDispatch
// hook (captures the constructed copies for assertions).
package bot_api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// fanoutCapture collects every MsgSendReq the fan-out path would have
// dispatched. Used by all tests below.
type fanoutCapture struct {
	mu     sync.Mutex
	copies []*config.MsgSendReq
}

func (fc *fanoutCapture) hook(req *config.MsgSendReq) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	cp := *req
	if req.Payload != nil {
		buf := make([]byte, len(req.Payload))
		copy(buf, req.Payload)
		cp.Payload = buf
	}
	fc.copies = append(fc.copies, &cp)
	return nil
}

// seedGrantWithScope is the shared setup: yu has an active grant to
// bot_clone, scoped to the test channel.
func seedGrantWithScope(t *testing.T, ch string, ct uint8) *fakeOBOStore {
	t.Helper()
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	if _, err := s.insertScope(gid, ch, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	return s
}

func newBAforFanout(s *fakeOBOStore, fc *fanoutCapture) *BotAPI {
	return &BotAPI{
		Log:               log.NewTLog("BotAPI-fanout-test"),
		oboStoreOverride:  s,
		oboFanoutDispatch: fc.hook,
		// PR#82 round-2 P1-A — fanoutForMessage now re-checks the
		// grantor's live channel access per grant. Default the test
		// override to "always allowed" so existing happy/gate/no-grants
		// tests stay focused on what they were written to cover. The
		// TOCTOU regression test installs a denying override.
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}
}

// TestFanout_Happy — a non-bot, non-grantor user sends into a scoped
// channel. The bot receives exactly one fan-out copy with Subscribers
// limited to it and the original payload preserved.
func TestFanout_Happy(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice", // some random sender, NOT bot, NOT grantor
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello yu","mention":{"uids":["user_yu"]}}`),
	}
	got := ba.fanoutForMessage(msg)
	if got != 1 {
		t.Fatalf("expected 1 fan-out, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if err := assertFanoutDispatchContract(cp); err != nil {
		t.Fatalf("fan-out contract violated: %v (req=%+v)", err, cp)
	}
	if cp.ChannelID != tBot {
		t.Fatalf("channel_id should be bot mailbox %q, got %q", tBot, cp.ChannelID)
	}
	if len(cp.Subscribers) != 0 {
		t.Fatalf("subscribers must be omitted (channel_id/subscribers are mutually exclusive on /message/send), got %v", cp.Subscribers)
	}
	if cp.Header.NoPersist != 1 || cp.Header.RedDot != 0 {
		t.Fatalf("fan-out must be silent (NoPersist=1, RedDot=0), got %+v", cp.Header)
	}
	// Sanity-check augmented payload preserved original keys.
	var got2 map[string]interface{}
	_ = json.Unmarshal(cp.Payload, &got2)
	if got2["content"] != "hello yu" {
		t.Fatalf("payload content lost: %v", got2)
	}
	if v, _ := got2["obo_fanout"].(bool); !v {
		t.Fatalf("payload should be marked obo_fanout=true: %v", got2)
	}
	// Origin channel context preserved so downstream consumers can route.
	if got2["obo_origin_channel_id"] != ch {
		t.Fatalf("obo_origin_channel_id should be %q, got %v", ch, got2["obo_origin_channel_id"])
	}
}

// TestFanout_Gate1_BotSelfSent — a message whose FromUID == grantee bot
// must NOT be fanned back to that same bot (loop guard #1).
//
// Note: this is distinct from gate #3 (the obo_processed marker). Gate 1
// covers cases where the bot sends WITHOUT going through OBO (e.g. bot
// posts a status update as itself in a channel that has an active grant).
func TestFanout_Gate1_BotSelfSent(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     tBot, // bot sent it itself
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"bot status update"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("gate 1 (bot self-sent) violated: dispatched %d copies", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_Gate2_GrantorOwnOutbound — the grantor sent the message
// (from any of their devices). The bot must NOT see it (loop guard #2),
// otherwise the bot would observe "I said X" and might autoreply.
func TestFanout_Gate2_GrantorOwnOutbound(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     tGrantor, // yu typing on his own phone
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hi everyone"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("gate 2 (grantor outbound) violated: dispatched %d copies", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_Gate3_AlreadyOBOProcessed — message_extra has
// __obo_processed__=true (set by sendMessage when on_behalf_of was honored).
// This is the loop guard that breaks the cycle "bot replies → reply is
// observed → bot replies again". The marker must be respected even if
// the FromUID looks like a random user (since FromUID = grantor when OBO
// fires — already covered by gate 2 — but also defensive against future
// callers that set the marker without flipping FromUID).
//
// PR#82 review #2 P1-2 — marker key migrated from `obo_processed` to the
// reserved-namespace `__obo_processed__` so a malicious bot can't suppress
// its own fan-out via a hand-crafted payload. The inbound payload
// validator rejects any `__obo_*` key on /v1/bot/sendMessage, leaving the
// marker as server-only state.
func TestFanout_Gate3_AlreadyOBOProcessed(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// FromUID intentionally NOT the bot and NOT the grantor — only the
	// marker should keep this from fanning out.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"bot reply","__obo_processed__":true}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("gate 3 (__obo_processed__ marker) violated: dispatched %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_Gate3_LegacyMarkerIgnored — the v0-era `obo_processed` key
// (no underscores) is NOT recognized as a marker after the PR#82 hardening
// — the gate only honors the reserved-namespace `__obo_processed__` key.
// Confirms that a bot crafting the legacy field on its own payload no
// longer suppresses fan-out.
func TestFanout_Gate3_LegacyMarkerIgnored(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"forged","obo_processed":true,"mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("legacy obo_processed marker must NOT short-circuit gate 3, want fan-out=1 got %d", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("captured %d copies, expected 1", len(fc.copies))
	}
}

// TestFanout_NoGrantsForChannel — channel has no scope row → no DB JOIN
// match → 0 dispatches. This is the common case on most messages.
func TestFanout_NoGrantsForChannel(t *testing.T) {
	s := newFakeOBOStore()
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   "unrelated_channel",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte(`{"type":1,"content":"hi"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("unscoped channel should not fan out, got %d", n)
	}
}

// TestFanout_NilOrEmptyMessage — defensive: nil or empty channel → no-op.
func TestFanout_NilOrEmptyMessage(t *testing.T) {
	ba := newBAforFanout(newFakeOBOStore(), &fanoutCapture{})
	if n := ba.fanoutForMessage(nil); n != 0 {
		t.Fatalf("nil message should be no-op, got %d", n)
	}
	if n := ba.fanoutForMessage(&config.MessageResp{}); n != 0 {
		t.Fatalf("empty-channel message should be no-op, got %d", n)
	}
}

// TestHasOBOProcessedMarker_Variants — exercises the JSON decode path
// shared by gate 3 directly so failures here pinpoint the marker logic
// rather than the surrounding fan-out plumbing. Marker key is the
// reserved-namespace `__obo_processed__` (PR#82 review #2 P1-2).
func TestHasOBOProcessedMarker_Variants(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"empty", "", false},
		{"non-json", "not json at all", false},
		{"json no marker", `{"type":1}`, false},
		{"marker true", `{"__obo_processed__":true}`, true},
		{"marker false", `{"__obo_processed__":false}`, false},
		{"marker not bool", `{"__obo_processed__":"yes"}`, false},
		{"marker mixed in", `{"type":1,"content":"hi","__obo_processed__":true}`, true},
		{"legacy key ignored", `{"obo_processed":true}`, false},
	}
	for _, tc := range cases {
		got := hasOBOProcessedMarker([]byte(tc.payload))
		if got != tc.want {
			t.Errorf("%s: hasOBOProcessedMarker(%q) = %v, want %v", tc.name, tc.payload, got, tc.want)
		}
	}
}

// TestPayloadHasReservedOBOKey — direct unit test for the inbound-payload
// validator that rejects `__obo_*` keys on /v1/bot/sendMessage. Mirrors
// the gate-3 marker move: the inbound side strips off anything in the
// reserved namespace so a bot can't forge server-only OBO state.
func TestPayloadHasReservedOBOKey(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    bool
	}{
		{"empty", map[string]interface{}{}, false},
		{"plain", map[string]interface{}{"type": 1, "content": "hi"}, false},
		{"single underscore not reserved", map[string]interface{}{"_obo_internal": true}, false},
		{"legacy obo_processed not reserved", map[string]interface{}{"obo_processed": true}, false},
		{"the marker itself", map[string]interface{}{"__obo_processed__": true}, true},
		{"any double-underscore obo key", map[string]interface{}{"__obo_anything__": "x"}, true},
		{"mixed in", map[string]interface{}{"type": 1, "__obo_marker": false}, true},
		// PR#121 R3 — actual_sender_uid is server-only (no `obo_`
		// prefix) so the bot-API ingress MUST reject a bot client
		// that tries to spoof the real-bot-behind-OBO identity.
		{"actual_sender_uid rejected", map[string]interface{}{"actual_sender_uid": "bot_admin"}, true},
		{"actual_sender_uid mixed in", map[string]interface{}{"type": 1, "content": "hi", "actual_sender_uid": "bot_x"}, true},
		// Anti-overreach: adjacent client field names must not
		// trip the reject (otherwise we'd break unrelated bot
		// schemas that happen to ship `sender_uid` / `actual_sender`).
		{"adjacent sender_uid passes", map[string]interface{}{"sender_uid": "u"}, false},
		{"adjacent actual_sender passes", map[string]interface{}{"actual_sender": "u"}, false},
	}
	for _, tc := range cases {
		got := payloadHasReservedOBOKey(tc.payload)
		if got != tc.want {
			t.Errorf("%s: payloadHasReservedOBOKey(%v) = %v, want %v", tc.name, tc.payload, got, tc.want)
		}
	}
}

// TestFanout_GrantorMembershipRevoked_SkipsCopy — PR#82 round-2 P1-A.
// Grant + scope are in place and a normal inbound (not from bot, not
// from grantor) arrives in the scoped channel. But the grantor was
// kicked from `group_42` after the scope was installed, so the live
// channel-access check denies — fan-out must NOT dispatch a copy to the
// grantee bot.
//
// Without the re-check the bot would keep harvesting messages from a
// channel the grantor no longer has eyes on, defeating the kick at the
// IM layer (kicked-from-group is one of the standard ways admins cut
// off a misbehaving user).
func TestFanout_GrantorMembershipRevoked_SkipsCopy(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Grantor lost membership → access check denies.
	calls := 0
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		calls++
		if uid != tGrantor || channelID != ch || channelType != ct {
			t.Errorf("unexpected access override args: uid=%q chan=%q type=%d", uid, channelID, channelType)
		}
		return false, nil
	}

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello yu","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("grantor lost access, expected 0 fan-out copies, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
	}
	if calls != 1 {
		t.Fatalf("expected the re-check to fire once per grant, got %d", calls)
	}

	// Sanity: same setup, access restored → fan-out resumes.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return true, nil
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("access restored, expected 1 fan-out, got %d", n)
	}
}

// TestFanout_GrantorMembershipRevoked_DBErrorSkipsCopy — defensive: a DB
// error on the access re-check must fail closed (skip the copy) so a
// transient blip can never leak otherwise-denied traffic. The grant is
// dropped from this listener invocation; the next message will re-try
// (no persistent state).
func TestFanout_GrantorMembershipRevoked_DBErrorSkipsCopy(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	boom := errors.New("connection refused")
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return false, boom
	}

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello yu","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("DB error on access re-check must fail closed, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0 on DB error", len(fc.copies))
	}
}

// TestFanout_DMPeerToGrantor_MatchesScope — PR#82 round-2 P1-B.
// Alice (grantor) installs an OBO scope for DM peer Bob. When Bob sends
// Alice a DM, the listener sees ChannelID=Alice (receiver) and
// FromUID=Bob (sender). The pre-fix code looked up scopes by ChannelID
// (= Alice) and missed Alice's scope row entirely, silently dropping
// every inbound DM. The fix normalizes the lookup to FromUID for DMs
// (the peer = grantor's frame of reference, matching how scopes are
// stored).
//
// Happy path: one fan-out copy delivered to the grantee bot, with the
// peer's payload preserved and gate-2 NOT firing.
func TestFanout_DMPeerToGrantor_MatchesScope(t *testing.T) {
	const peer = "bob"
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	// Scope row uses the grantor's perspective: channel_id = peer uid.
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Grantor still has access (still friends with Bob).
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		// The fan-out hot path must use the GRANTOR frame of reference
		// (channel_id = peer). Assert that here so a regression to the
		// raw m.ChannelID lookup is caught.
		if uid != tGrantor || channelID != peer || channelType != ct {
			t.Errorf("access check called with wrong frame: uid=%q chan=%q type=%d (want grantor=%q peer=%q)", uid, channelID, channelType, tGrantor, peer)
		}
		return true, nil
	}

	// Listener-emitted DM: ChannelID = receiver (= grantor), FromUID = peer.
	// See modules/webhook/api.go:248-279 + toConfigMessageResp.
	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   tGrantor,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hey yu"}`),
	}
	got := ba.fanoutForMessage(msg)
	if got != 1 {
		t.Fatalf("expected 1 fan-out, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if err := assertFanoutDispatchContract(cp); err != nil {
		t.Fatalf("fan-out contract violated: %v (req=%+v)", err, cp)
	}
	if cp.ChannelID != tBot {
		t.Fatalf("channel_id should be bot mailbox %q, got %q", tBot, cp.ChannelID)
	}
	if len(cp.Subscribers) != 0 {
		t.Fatalf("subscribers must be omitted on the fan-out copy, got %v", cp.Subscribers)
	}
	// Payload integrity: the bot must see the original sender and content.
	var p map[string]interface{}
	_ = json.Unmarshal(cp.Payload, &p)
	if p["content"] != "hey yu" {
		t.Fatalf("payload content lost: %v", p)
	}
	if v, _ := p["obo_origin_from_uid"].(string); v != peer {
		t.Fatalf("obo_origin_from_uid should be %q, got %q", peer, v)
	}
	// Origin channel context: DM receiver is the grantor.
	if v, _ := p["obo_origin_channel_id"].(string); v != tGrantor {
		t.Fatalf("obo_origin_channel_id should be grantor %q, got %q", tGrantor, v)
	}
}

// TestFanout_DMGrantorToPeer_DoesNotEcho — PR#82 round-2 P1-B, gate-2
// invariant under the new DM lookup. When the grantor types on their
// own device to a DM peer, the listener sees ChannelID=peer and
// FromUID=grantor. The new lookup-by-FromUID gives us scope-row matches
// keyed by grantor's uid — which the grantor's own scope row (keyed by
// peer) will never match. Result: 0 fan-out, no echo to the bot.
//
// Gate 2 (g.GrantorUID == m.FromUID) is the historical defense for this
// case and still acts as belt-and-braces if a future code path falls
// back to the verbatim m.ChannelID lookup — but with the P1-B fix, the
// lookup itself returns nothing, so gate 2 never even fires.
func TestFanout_DMGrantorToPeer_DoesNotEcho(t *testing.T) {
	const peer = "bob"
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// If the access check fires here, the lookup leaked through —
	// surface that as a failure rather than a quiet "0 dispatches".
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		t.Errorf("grantor-to-peer DM should not even reach the access check; called with uid=%q chan=%q", uid, channelID)
		return true, nil
	}

	// Grantor typing on own device: FromUID=grantor, ChannelID=peer.
	msg := &config.MessageResp{
		FromUID:     tGrantor,
		ChannelID:   peer,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hi bob"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("grantor's own DM outbound must not echo to bot, got %d copies", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_DMUnrelatedPeer_NoMatch — defensive cousin of P1-B. A DM
// from some third party Eve to the grantor must NOT fan out when the
// grantor's scope is for Bob, not Eve. With the new lookup-by-FromUID,
// scope (channel_id = Bob) and lookup (FromUID = Eve) do not match.
func TestFanout_DMUnrelatedPeer_NoMatch(t *testing.T) {
	const scopedPeer, otherPeer = "bob", "eve"
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	if _, err := s.insertScope(gid, scopedPeer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		t.Errorf("unrelated peer DM should not reach access check; uid=%q chan=%q", uid, channelID)
		return true, nil
	}

	msg := &config.MessageResp{
		FromUID:     otherPeer,
		ChannelID:   tGrantor,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hi yu","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("unscoped DM peer must not fan out, got %d", n)
	}
}

// TestFanout_DMMultiGrantor_OnlyRecipientReceives — PR#82 round-3 P1.
// Cross-user DM privacy leak in fan-out: two grantors (Alice and Carol)
// each install an OBO grant + scope `(peer=Bob)` for their own clone
// bots. When Bob DMs Alice, the listener sees ChannelID=Alice (the
// recipient), FromUID=Bob. findActiveGrantsForChannel(Bob, Person)
// returns BOTH Alice's grant AND Carol's grant — both scoped that peer
// — and the per-grant grantor-access re-check accepts Carol because she
// is also friends with Bob and so can read DMs with him. Without the
// recipient filter, Carol's clone bot would receive a copy of Bob's
// private message to Alice.
//
// The fix is a per-grant filter inside fanoutForMessage's ChannelType
// Person branch: skip any grant whose grantor is not the actual DM
// recipient (= m.ChannelID under the listener's frame of reference).
// This test asserts exactly one fan-out (to Alice's bot), with Bob's
// payload preserved.
func TestFanout_DMMultiGrantor_OnlyRecipientReceives(t *testing.T) {
	const (
		peer     = "bob"
		aliceUID = "user_alice"
		aliceBot = "bot_alice_clone"
		carolUID = "user_carol"
		carolBot = "bot_carol_clone"
	)
	ct := common.ChannelTypePerson.Uint8()

	s := newFakeOBOStore()
	// Alice's grant + scope (peer=Bob).
	gidAlice, err := s.insertGrant(aliceUID, aliceBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant alice: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gidAlice, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant alice: %v", err)
	}
	if _, err := s.insertScope(gidAlice, peer, ct, 1); err != nil {
		t.Fatalf("insertScope alice: %v", err)
	}
	// Carol's grant + scope (peer=Bob) — the exploit setup. Carol and
	// Bob are friends so the per-grant access check WOULD permit this
	// grant absent the recipient filter.
	gidCarol, err := s.insertGrant(carolUID, carolBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant carol: %v", err)
	}
	if err := s.updateGrant(gidCarol, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant carol: %v", err)
	}
	if _, err := s.insertScope(gidCarol, peer, ct, 1); err != nil {
		t.Fatalf("insertScope carol: %v", err)
	}

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Both grantors are friends with Bob → both pass the per-grant
	// access re-check. The recipient filter is the ONLY thing keeping
	// Carol's bot off the dispatch list.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		if channelID != peer || channelType != ct {
			t.Errorf("access check called with wrong DM frame: uid=%q chan=%q (want peer=%q)", uid, channelID, peer)
		}
		if uid != aliceUID && uid != carolUID {
			t.Errorf("unexpected grantor in access check: %q", uid)
		}
		return true, nil
	}

	// Bob → Alice DM.
	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   aliceUID,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"private note for alice"}`),
	}
	got := ba.fanoutForMessage(msg)
	if got != 1 {
		t.Fatalf("multi-grantor DM: expected exactly 1 fan-out (to alice's bot), got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("multi-grantor DM: expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if err := assertFanoutDispatchContract(cp); err != nil {
		t.Fatalf("fan-out contract violated: %v (req=%+v)", err, cp)
	}
	if cp.ChannelID != aliceBot {
		t.Fatalf("multi-grantor DM leak: channel_id should be alice's bot mailbox %q, got %q", aliceBot, cp.ChannelID)
	}
	if len(cp.Subscribers) != 0 {
		t.Fatalf("subscribers must be omitted on the fan-out copy, got %v", cp.Subscribers)
	}
	// Explicitly assert Carol's bot is NOT the addressed mailbox on any
	// copy — the regression we're guarding against.
	for _, c := range fc.copies {
		if c.ChannelID == carolBot {
			t.Fatalf("CROSS-USER DM LEAK: carol's bot mailbox (%s) received fan-out of bob→alice DM", carolBot)
		}
		for _, sub := range c.Subscribers {
			if sub == carolBot {
				t.Fatalf("CROSS-USER DM LEAK: carol's bot (%s) listed in Subscribers (and Subscribers should be empty)", carolBot)
			}
		}
	}
	var p map[string]interface{}
	_ = json.Unmarshal(cp.Payload, &p)
	if p["content"] != "private note for alice" {
		t.Fatalf("payload content lost: %v", p)
	}
}

// TestFanout_DMSingleGrantor_RecipientReceives — the happy path under
// the new recipient filter still works: exactly one grantor (Alice) has
// a scope for peer Bob; Bob → Alice DM fans out to Alice's bot. Mirrors
// TestFanout_DMPeerToGrantor_MatchesScope but explicitly named in the
// R3 regression set so future readers see the multi-grantor and
// single-grantor cases side by side.
func TestFanout_DMSingleGrantor_RecipientReceives(t *testing.T) {
	const peer = "bob"
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return true, nil
	}

	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   tGrantor,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello yu","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("single-grantor DM happy path: expected 1 fan-out, got %d", n)
	}
	if len(fc.copies) != 1 || fc.copies[0].ChannelID != tBot {
		t.Fatalf("single-grantor DM happy path: wrong dispatch, copies=%+v", fc.copies)
	}
	if err := assertFanoutDispatchContract(fc.copies[0]); err != nil {
		t.Fatalf("fan-out contract violated: %v (req=%+v)", err, fc.copies[0])
	}
}

// TestFanout_DMNonRecipient_NoLeak — edge case for the R3 recipient
// filter: a grant exists whose grantor is NOT the DM recipient, but
// access re-check would otherwise allow it. The filter must drop the
// non-recipient grant BEFORE the access check fires, so the access
// override is intentionally rigged to fail the test if it gets called
// for the wrong grantor.
//
// Setup: Carol scopes peer Bob (Carol ↔ Bob are friends). Bob then
// DMs Alice (a different user, who has NO grant). The fan-out lookup
// returns Carol's grant (scope is keyed by peer=Bob). The filter must
// drop it because Carol is not the recipient (Alice is, and Alice
// doesn't even have a grant).
func TestFanout_DMNonRecipient_NoLeak(t *testing.T) {
	const (
		peer     = "bob"
		aliceUID = "user_alice_no_grant"
		carolUID = "user_carol"
		carolBot = "bot_carol_clone"
	)
	ct := common.ChannelTypePerson.Uint8()

	s := newFakeOBOStore()
	gidCarol, err := s.insertGrant(carolUID, carolBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant carol: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gidCarol, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant carol: %v", err)
	}
	if _, err := s.insertScope(gidCarol, peer, ct, 1); err != nil {
		t.Fatalf("insertScope carol: %v", err)
	}

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// If the access check fires here, the recipient filter leaked —
	// surface that as a hard failure rather than a silent "0 dispatches"
	// (which could mask a regression where a later gate happens to
	// catch it).
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		t.Errorf("non-recipient grant reached the access check; filter must drop earlier (uid=%q chan=%q)", uid, channelID)
		return true, nil
	}

	// Bob → Alice DM (recipient = Alice, who has NO grant).
	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   aliceUID,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"for alice only"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("non-recipient grant must not fan out, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("non-recipient grant leaked: captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_OriginalSenderHasNoConversationLeak — PR#82 R6 P0 Bug A.
// im-test 2026-05-19 surfaced that bob (the peer of an original DM
// admin↔bob conversation) saw james (admin's persona-clone bot) appear
// in his conversation list with the message he sent to admin. james
// MUST be invisible to bob — that is the whole point of "managed
// persona" (bob only ever sees admin as the counterparty).
//
// Root cause: the v0 fan-out copy used `FromUID=m.FromUID` (= bob), so
// WuKongIM observed a (FromUID=bob, ChannelID=james) PERSONAL message
// and synced the `bob ↔ james` conversation pair to bob's client.
//
// Fix: FromUID is the GRANTOR (admin), not the original sender. The
// regression assertion is that NO fan-out copy ever carries
// FromUID == original sender for any channel type. This locks the
// invariant across the DM peer→grantor case (the actual reported bug)
// AND every other channel type — a regression where group / topic
// fan-outs use m.FromUID would similarly leak the bot into the original
// sender's contacts via the same WuKongIM sync.
func TestFanout_OriginalSenderHasNoConversationLeak(t *testing.T) {
	cases := []struct {
		name        string
		ct          uint8
		fromUID     string // original sender on the inbound message
		channelID   string // ChannelID on the inbound message (listener view)
		scope       string // OBO scope.channel_id (grantor frame of reference)
		setupAccess func(ba *BotAPI)
	}{
		{
			name:      "dm_peer_to_grantor",
			ct:        common.ChannelTypePerson.Uint8(),
			fromUID:   "u_bob",
			channelID: tGrantor,
			scope:     "u_bob", // for DM, scope.channel_id = peer uid
		},
		{
			name:      "group",
			ct:        common.ChannelTypeGroup.Uint8(),
			fromUID:   "u_bob",
			channelID: "group_42",
			scope:     "group_42",
		},
		{
			name:      "community_topic",
			ct:        common.ChannelTypeCommunityTopic.Uint8(),
			fromUID:   "u_bob",
			channelID: "group_42____topic_99",
			scope:     "group_42____topic_99",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := seedGrantWithScope(t, tc.scope, tc.ct)
			fc := &fanoutCapture{}
			ba := newBAforFanout(s, fc)

			msg := &config.MessageResp{
				FromUID:     tc.fromUID,
				ChannelID:   tc.channelID,
				ChannelType: tc.ct,
				Payload:     []byte(`{"type":1,"content":"private to admin","mention":{"uids":["user_yu"]}}`),
			}
			if n := ba.fanoutForMessage(msg); n != 1 {
				t.Fatalf("expected 1 dispatch, got %d", n)
			}
			if len(fc.copies) != 1 {
				t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
			}
			cp := fc.copies[0]

			// The bug-A assertion. If FromUID == original sender, WuKongIM
			// will sync a `<sender> ↔ <granteeBot>` conversation pair to
			// the sender's client and the bot leaks into the sender's
			// contact list.
			if cp.FromUID == tc.fromUID {
				t.Fatalf("R6 P0 LEAK: fan-out FromUID == original sender %q — WuKongIM will surface the persona-clone bot %q in the sender's conversation list", tc.fromUID, tBot)
			}
			// Belt-and-braces: FromUID should be the grantor, which keeps
			// the only synced conversation pair (`<grantor> ↔ <bot>`)
			// scoped to a party who legitimately owns the bot.
			if cp.FromUID != tGrantor {
				t.Fatalf("fan-out FromUID should be grantor %q, got %q", tGrantor, cp.FromUID)
			}
			// And the bot still gets the original sender's uid via the
			// payload field so it can address its reply to the right
			// real user.
			var p map[string]interface{}
			_ = json.Unmarshal(cp.Payload, &p)
			if got := p["obo_origin_from_uid"]; got != tc.fromUID {
				t.Fatalf("obo_origin_from_uid must preserve the original sender %q for reply addressing, got %v", tc.fromUID, got)
			}
		})
	}
}


// invariant: a MsgSendReq must carry EXACTLY ONE of (channel_id set with
// empty subscribers) OR (empty channel_id with subscribers set). Setting
// both triggers the production rejection observed in PR#82 R5 P0:
//
//	【message】channelId和subscribers不能同时存在！
//
// The OBO fan-out path picks the channel_id branch (bot's own mailbox).
// This helper is used by every dispatch-contract regression test so any
// future driver of buildFanoutCopyReq that re-introduces the conflict is
// caught at the unit-test layer, before im-test ever sees it.
func assertFanoutDispatchContract(req *config.MsgSendReq) error {
	if req == nil {
		return errors.New("dispatched a nil MsgSendReq")
	}
	hasChannelID := strings.TrimSpace(req.ChannelID) != ""
	hasSubscribers := len(req.Subscribers) > 0
	if hasChannelID && hasSubscribers {
		return fmt.Errorf("MsgSendReq sets BOTH channel_id=%q AND subscribers=%v — WuKongIM rejects this combination", req.ChannelID, req.Subscribers)
	}
	if !hasChannelID && !hasSubscribers {
		return errors.New("MsgSendReq has neither channel_id nor subscribers — WuKongIM cannot route the message")
	}
	return nil
}

// TestFanout_DispatchReq_NoConflict_ChannelOrSubscribers — PR#82 R5 P0
// regression. The v0 buildFanoutCopyReq set BOTH ChannelID (origin
// conversation) AND Subscribers ([granteeBot]); WuKongIM /message/send
// rejected every fan-out with "channelId和subscribers不能同时存在", and
// the bot consequently never received the copy → the persona never
// replied.
//
// This test exercises every channel-type / sender-role combination the
// fan-out hot path can see (DM peer→grantor, group, community topic) and
// asserts the dispatched MsgSendReq always satisfies the mutex contract.
// It does NOT touch the access-check or recipient-filter logic — those
// have their own dedicated tests — so any future regression in
// buildFanoutCopyReq alone surfaces here.
func TestFanout_DispatchReq_NoConflict_ChannelOrSubscribers(t *testing.T) {
	cases := []struct {
		name       string
		ct         uint8
		setupMsg   func(scope string) *config.MessageResp
		setupScope func() (string, *fakeOBOStore)
	}{
		{
			name: "group",
			ct:   common.ChannelTypeGroup.Uint8(),
			setupScope: func() (string, *fakeOBOStore) {
				ch := "group_42"
				return ch, seedGrantWithScope(t, ch, common.ChannelTypeGroup.Uint8())
			},
			setupMsg: func(scope string) *config.MessageResp {
				return &config.MessageResp{
					FromUID:     "alice",
					ChannelID:   scope,
					ChannelType: common.ChannelTypeGroup.Uint8(),
					Payload:     []byte(`{"type":1,"content":"hi group","mention":{"uids":["user_yu"]}}`),
				}
			},
		},
		{
			name: "dm_peer_to_grantor",
			ct:   common.ChannelTypePerson.Uint8(),
			setupScope: func() (string, *fakeOBOStore) {
				const peer = "bob"
				ct := common.ChannelTypePerson.Uint8()
				s := newFakeOBOStore()
				gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
				enable := 1
				_ = s.updateGrant(gid, "", &enable, nil)
				if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
					t.Fatalf("insertScope: %v", err)
				}
				return peer, s
			},
			setupMsg: func(peer string) *config.MessageResp {
				return &config.MessageResp{
					FromUID:     peer,
					ChannelID:   tGrantor,
					ChannelType: common.ChannelTypePerson.Uint8(),
					Payload:     []byte(`{"type":1,"content":"hey yu"}`),
				}
			},
		},
		{
			name: "community_topic",
			ct:   common.ChannelTypeCommunityTopic.Uint8(),
			setupScope: func() (string, *fakeOBOStore) {
				ch := "topic_99"
				return ch, seedGrantWithScope(t, ch, common.ChannelTypeCommunityTopic.Uint8())
			},
			setupMsg: func(scope string) *config.MessageResp {
				return &config.MessageResp{
					FromUID:     "alice",
					ChannelID:   scope,
					ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
					Payload:     []byte(`{"type":1,"content":"hi topic","mention":{"uids":["user_yu"]}}`),
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, s := tc.setupScope()
			fc := &fanoutCapture{}
			ba := newBAforFanout(s, fc)
			msg := tc.setupMsg(scope)
			n := ba.fanoutForMessage(msg)
			if n != 1 {
				t.Fatalf("expected 1 dispatch, got %d (copies=%+v)", n, fc.copies)
			}
			if len(fc.copies) != 1 {
				t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
			}
			cp := fc.copies[0]
			if err := assertFanoutDispatchContract(cp); err != nil {
				t.Fatalf("WuKongIM mutex contract violated: %v\n  channel_id=%q\n  subscribers=%v\n  channel_type=%d\n  from_uid=%q",
					err, cp.ChannelID, cp.Subscribers, cp.ChannelType, cp.FromUID)
			}
		})
	}
}

// TestFanout_DispatchReq_BotReceivesViaOwnChannel — PR#82 R5 P0. Locks
// in option-3 routing decision: the fan-out copy is delivered via the
// grantee bot's OWN Person mailbox (ChannelID=granteeBotUID,
// ChannelType=Person), not via the origin conversation's channel_id with
// a Subscribers filter. This is the contract that satisfies the
// WuKongIM mutex AND keeps the copy out of the origin channel's
// subscriber pipeline (so no real user sees it even if NoPersist were
// to ever regress).
//
// Asserts (per fan-out copy):
//   - ChannelID == granteeBotUID
//   - ChannelType == ChannelTypePerson (set by NewPersonalMsgSendReq)
//   - Subscribers is empty
//   - FromUID == GRANTOR uid (PR#82 R6 P0 — NOT the original sender;
//     the bot learns the real speaker via obo_origin_from_uid)
//   - obo_origin_* payload fields preserve the routing context
func TestFanout_DispatchReq_BotReceivesViaOwnChannel(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:      "u_bob",
		ChannelID:    ch,
		ChannelType:  ct,
		MessageIDStr: "origin_msg_42",
		Payload:      []byte(`{"type":1,"content":"hi yu","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("expected 1 dispatch, got %d", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if err := assertFanoutDispatchContract(cp); err != nil {
		t.Fatalf("contract violated: %v", err)
	}
	if cp.ChannelID != tBot {
		t.Fatalf("ChannelID should be grantee bot mailbox %q, got %q", tBot, cp.ChannelID)
	}
	if cp.ChannelType != common.ChannelTypePerson.Uint8() {
		t.Fatalf("ChannelType should be Person (%d), got %d", common.ChannelTypePerson.Uint8(), cp.ChannelType)
	}
	if len(cp.Subscribers) != 0 {
		t.Fatalf("Subscribers must be omitted (mutex with channel_id), got %v", cp.Subscribers)
	}
	// PR#82 R6 P0 — FromUID must be the GRANTOR, not the original sender.
	// Using the original sender (m.FromUID = u_bob) made WuKongIM sync a
	// u_bob ↔ bot_clone conversation entry to bob's client, leaking the
	// persona-clone bot into bob's contact list. The grantor (admin) is
	// the bot's owner and the only party who should see the bot.
	if cp.FromUID != tGrantor {
		t.Fatalf("FromUID should be grantor %q (PR#82 R6 P0 — NOT the original sender), got %q", tGrantor, cp.FromUID)
	}

	var p map[string]interface{}
	if err := json.Unmarshal(cp.Payload, &p); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if got := p["obo_origin_channel_id"]; got != ch {
		t.Fatalf("obo_origin_channel_id should be origin channel %q, got %v", ch, got)
	}
	// JSON numbers decode as float64.
	if got, _ := p["obo_origin_channel_type"].(float64); uint8(got) != ct {
		t.Fatalf("obo_origin_channel_type should be %d, got %v", ct, p["obo_origin_channel_type"])
	}
	// obo_origin_from_uid carries the ORIGINAL sender so the bot can
	// address replies / reactions to the right user — this is the
	// substitute for the now-rewritten FromUID.
	if got := p["obo_origin_from_uid"]; got != "u_bob" {
		t.Fatalf("obo_origin_from_uid should be %q, got %v", "u_bob", got)
	}
	if got := p["obo_origin_message_idstr"]; got != "origin_msg_42" {
		t.Fatalf("obo_origin_message_idstr should be %q, got %v", "origin_msg_42", got)
	}
	// Original content preserved.
	if got := p["content"]; got != "hi yu" {
		t.Fatalf("original content lost: got %v", got)
	}
	// Fan-out marker present.
	if v, _ := p["obo_fanout"].(bool); !v {
		t.Fatalf("obo_fanout marker missing")
	}
	// And — defense in depth — the gate-3 marker MUST NOT be on the
	// fan-out copy. The bot's own reply (a separate send via
	// /v1/bot/sendMessage) is what carries the __obo_processed__ marker.
	if _, present := p["__obo_processed__"]; present {
		t.Fatalf("fan-out copy must not carry the gate-3 __obo_processed__ marker; that key is set only on the bot's own outbound: %v", p)
	}
}

// TestFanout_DispatchReq_RealDispatcher_ContractCheck — PR#82 R5 P0. Test
// gap closure: every existing fan-out test mocks oboFanoutDispatch with
// the fanoutCapture hook, which means WuKongIM-shape rejections (like the
// channel_id/subscribers mutex) are invisible to the unit suite. The
// production v0 path silently fed conflicting requests to WuKongIM and
// only im-test surfaced the bug.
//
// This test installs a "fake WuKongIM" dispatcher that performs the same
// mutex check the real /message/send endpoint does, then runs the
// fan-out happy path and asserts the dispatcher accepts the request (zero
// rejections, exactly one delivery). A future regression that re-
// introduces the conflict — by, say, copying buildFanoutCopyReq into a
// new code path — will trip this fake and fail.
func TestFanout_DispatchReq_RealDispatcher_ContractCheck(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)

	var (
		dispatched int
		rejected   []string
	)
	fakeWK := func(req *config.MsgSendReq) error {
		// Mirror the WuKongIM /message/send precondition: channel_id and
		// subscribers are mutually exclusive.
		if strings.TrimSpace(req.ChannelID) != "" && len(req.Subscribers) > 0 {
			msg := fmt.Sprintf("【message】channelId和subscribers不能同时存在！ (channel_id=%q subscribers=%v)", req.ChannelID, req.Subscribers)
			rejected = append(rejected, msg)
			return errors.New(msg)
		}
		if strings.TrimSpace(req.ChannelID) == "" && len(req.Subscribers) == 0 {
			msg := "【message】channelId和subscribers至少需要一个！"
			rejected = append(rejected, msg)
			return errors.New(msg)
		}
		dispatched++
		return nil
	}
	ba := &BotAPI{
		Log:               log.NewTLog("BotAPI-fanout-test"),
		oboStoreOverride:  s,
		oboFanoutDispatch: fakeWK,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}

	msg := &config.MessageResp{
		FromUID:     "u_bob",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hi yu","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("fan-out should succeed against contract-respecting WuKongIM, got %d (rejections=%v)", n, rejected)
	}
	if dispatched != 1 {
		t.Fatalf("fake WuKongIM should have accepted 1 dispatch, accepted %d", dispatched)
	}
	if len(rejected) != 0 {
		t.Fatalf("WuKongIM contract violations: %v", rejected)
	}
}

// TestFanout_ImplicitScope_GrantorMember_BotNotMember — PR#121 perf
// optimization. With global_enabled=1 and NO explicit scope row, the
// implicit-scope SQL feeder is the source of truth for "grantor IS in
// group AND bot is NOT in group". A group message into such a channel
// must produce one fan-out copy without the dispatch loop ever calling
// `grantorCanReadChannel` or `userIsGroupMember(bot)` again — the SQL
// already established both predicates.
func TestFanout_ImplicitScope_GrantorMember_BotNotMember(t *testing.T) {
	const groupNo = "group_implicit_42"
	ct := common.ChannelTypeGroup.Uint8()

	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	// Grantor is a member; bot is intentionally NOT a member.
	s.seedGroupMember(groupNo, tGrantor)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// If the access override fires for an implicit-scope grant, the SQL
	// pre-validation got bypassed — surface that as a hard failure so a
	// regression that re-introduces the per-grant Go check is caught.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		t.Errorf("implicit-scope grant must NOT trigger grantorCanReadChannel (uid=%q chan=%q)", uid, channelID)
		return true, nil
	}

	msg := &config.MessageResp{
		FromUID:     "u_alice", // not bot, not grantor
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello implicit-scope","mention":{"uids":["user_yu"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("implicit-scope grantor-member happy path: expected 1 fan-out, got %d", n)
	}
	if len(fc.copies) != 1 || fc.copies[0].ChannelID != tBot {
		t.Fatalf("implicit-scope grantor-member happy path: wrong dispatch, copies=%+v", fc.copies)
	}
}

// TestFanout_ImplicitScope_GrantorNotMember_NoFanout — when the grantor
// is NOT a current group member the SQL JOIN's `INNER JOIN group_member
// gm_grantor` excludes the grant entirely. No fan-out copy must be
// dispatched, and the access override must not fire (the candidate set
// is empty, so the dispatch loop has nothing to re-check).
func TestFanout_ImplicitScope_GrantorNotMember_NoFanout(t *testing.T) {
	const groupNo = "group_implicit_43"
	ct := common.ChannelTypeGroup.Uint8()

	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	// Intentionally do NOT seed grantor as group member.

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		t.Errorf("non-member grantor should never reach the access check (uid=%q)", uid)
		return true, nil
	}

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"should be dropped"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("non-member grantor must not fan out, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("non-member grantor leaked: captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_ImplicitScope_BotAlsoMember_NoFanout — Gate 4 in SQL. When
// the grantee bot is itself in the group it already receives messages
// directly via the WuKongIM subscriber pipeline; a fan-out copy would
// double-process. The new SQL `LEFT JOIN ... gm_bot.uid IS NULL` filter
// excludes these grants without the dispatch loop having to call
// `userIsGroupMember(bot)`.
func TestFanout_ImplicitScope_BotAlsoMember_NoFanout(t *testing.T) {
	const groupNo = "group_implicit_44"
	ct := common.ChannelTypeGroup.Uint8()

	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	// Both grantor AND bot are members → Gate 4 must drop this candidate.
	s.seedGroupMember(groupNo, tGrantor)
	s.seedGroupMember(groupNo, tBot)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"bot is in group already"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("bot-also-member must not fan out (Gate 4), got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("Gate 4 leaked: captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_ImplicitScope_ExplicitScopeWins — when an explicit scope row
// exists (even with enabled=0) the implicit-scope feeder must NOT return
// the grant. The explicit-scope feeder (`findActiveGrantsForChannel`)
// requires `enabled=1`, so an `enabled=0` row produces zero fan-out
// either way — exactly the "admin disabled this channel" semantics.
func TestFanout_ImplicitScope_ExplicitScopeWins(t *testing.T) {
	const groupNo = "group_implicit_45"
	ct := common.ChannelTypeGroup.Uint8()

	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	// Grantor is a member, bot is not — would normally trigger implicit
	// scope. But an explicitly DISABLED scope row exists for this channel.
	s.seedGroupMember(groupNo, tGrantor)
	if _, err := s.insertScope(gid, groupNo, ct, 0); err != nil {
		t.Fatalf("insertScope (disabled): %v", err)
	}

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "u_alice",
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"admin disabled this channel"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("explicitly disabled scope must suppress implicit-scope, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("explicit-disabled scope leaked: captured %d copies", len(fc.copies))
	}
}

// TestFanout_PR121R6_ImplicitScopeRespectsMentionFilter — PR#121 R6 / B2
// (Jerry-Xin + lml2468 2026-05-22 blocking) regression guard. With two
// global-enabled grantors in the same group, an inbound message that
// @-mentions only ONE of them must summon only that grantor's persona;
// the implicit-scope feeder must NOT silently pull in the un-mentioned
// grantor's clone. Pre-fix the implicit-scope branch appended every
// candidate `findGlobalGrantsWithoutScope` returned (which is
// mention-agnostic by design), so a message mentioning Alice would
// also dispatch a fan-out copy to Bob's clone — a direct violation
// of the documented v2 mention contract: `mention.uids` summons only
// the mentioned grantor(s); only `mention.all` summons everyone.
func TestFanout_PR121R6_ImplicitScopeRespectsMentionFilter(t *testing.T) {
	const (
		groupNo   = "group_pr121_r6"
		grantorA  = "user_alice"
		botCloneA = "bot_clone_alice"
		grantorB  = "user_bob"
		botCloneB = "bot_clone_bob"
	)
	ct := common.ChannelTypeGroup.Uint8()

	s := newFakeOBOStore()
	// Two global-enabled grants, no explicit scope rows — both are
	// implicit-scope candidates for this group.
	gidA, err := s.insertGrant(grantorA, botCloneA, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant A: %v", err)
	}
	gidB, err := s.insertGrant(grantorB, botCloneB, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant B: %v", err)
	}
	enable := 1
	_ = s.updateGrant(gidA, "", &enable, nil)
	_ = s.updateGrant(gidB, "", &enable, nil)
	// Both grantors are in the group; neither bot is.
	s.seedGroupMember(groupNo, grantorA)
	s.seedGroupMember(groupNo, grantorB)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// Inbound mentions ONLY Alice.
	msg := &config.MessageResp{
		FromUID:     "u_carol", // not bot, not grantor
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hi alice","mention":{"uids":["` + grantorA + `"]}}`),
	}
	n := ba.fanoutForMessage(msg)
	if n != 1 {
		t.Fatalf("expected exactly 1 fan-out (Alice only), got %d (copies=%+v)", n, fc.copies)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected exactly 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if cp.ChannelID != botCloneA {
		t.Fatalf("fan-out must address Alice's bot mailbox (%q), got %q — implicit-scope leaked an un-mentioned grantor", botCloneA, cp.ChannelID)
	}
	if cp.FromUID != grantorA {
		t.Fatalf("fan-out FromUID must be Alice (%q), got %q", grantorA, cp.FromUID)
	}
	// Defensive: explicitly confirm Bob's clone was NOT addressed.
	for _, c := range fc.copies {
		if c.ChannelID == botCloneB {
			t.Fatalf("implicit-scope leaked: Bob's clone %q was un-mentioned but received a fan-out copy", botCloneB)
		}
	}
}

// TestFanout_PR121R6_ImplicitScopeMentionAllSummonsEveryone — companion
// to the filter test above. `mention.all=1` (= `@所有人`) summons every
// grantor in the channel, so BOTH implicit-scope candidates must fan
// out. This locks the symmetry: filter when `mention.uids` is the only
// signal, pass through when `mention.all` is set.
func TestFanout_PR121R6_ImplicitScopeMentionAllSummonsEveryone(t *testing.T) {
	const (
		groupNo   = "group_pr121_r6_all"
		grantorA  = "user_alice"
		botCloneA = "bot_clone_alice"
		grantorB  = "user_bob"
		botCloneB = "bot_clone_bob"
	)
	ct := common.ChannelTypeGroup.Uint8()

	s := newFakeOBOStore()
	gidA, _ := s.insertGrant(grantorA, botCloneA, "auto", "")
	gidB, _ := s.insertGrant(grantorB, botCloneB, "auto", "")
	enable := 1
	_ = s.updateGrant(gidA, "", &enable, nil)
	_ = s.updateGrant(gidB, "", &enable, nil)
	s.seedGroupMember(groupNo, grantorA)
	s.seedGroupMember(groupNo, grantorB)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "u_carol",
		ChannelID:   groupNo,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@everyone","mention":{"all":1}}`),
	}
	n := ba.fanoutForMessage(msg)
	if n != 2 {
		t.Fatalf("mention.all must summon both grantors, got %d (copies=%+v)", n, fc.copies)
	}
	seen := map[string]bool{}
	for _, c := range fc.copies {
		seen[c.ChannelID] = true
	}
	if !seen[botCloneA] || !seen[botCloneB] {
		t.Fatalf("mention.all must reach both bot mailboxes, got %+v", seen)
	}
}

// TestFanout_Gate4_CommunityTopic_BotInParentGroup_NoFanout — PR#121 R8
// (Jerry-Xin + lml2468 blocking). Gate 4 originally only fired for
// ChannelTypeGroup; for CommunityTopic, when the grantee bot was already
// a parent-group member it received BOTH the direct WuKongIM delivery
// AND the OBO fan-out copy — exactly the duplicate case Gate 4 prevents.
//
// Setup: grantor + grantee bot are BOTH members of the parent group.
// An explicit scope row covers the topic channel (`<parent>____<short>`).
// A third party (`u_bob`) posts to the topic with a mention summoning
// the grantor.
//
// Expected: ZERO fan-out copies. The bot already receives the topic
// message directly via the parent-group subscriber pipeline; an OBO copy
// would be a strict duplicate (double processing / double reply).
func TestFanout_Gate4_CommunityTopic_BotInParentGroup_NoFanout(t *testing.T) {
	const parentGroup = "group_topic_parent_88"
	const topicChan = parentGroup + "____topic_99"
	ct := common.ChannelTypeCommunityTopic.Uint8()

	s := seedGrantWithScope(t, topicChan, ct)
	// Both grantor and grantee bot are in the parent group — the bot's
	// direct delivery covers this topic, so Gate 4 must skip the fan-out.
	s.seedGroupMember(parentGroup, tGrantor)
	s.seedGroupMember(parentGroup, tBot)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Route the Gate 4 membership lookup through the fake store so the
	// dispatch-loop check observes the seeded membership without a live
	// DB.
	ba.oboGroupMemberOverride = func(uid, groupNo string) (bool, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.groupMembers[groupNo][uid], nil
	}

	msg := &config.MessageResp{
		FromUID:     "u_bob",
		ChannelID:   topicChan,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu look at this","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("CommunityTopic Gate 4: bot in parent group must not fan out, got %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("CommunityTopic Gate 4 leaked: captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_Gate4_CommunityTopic_BotNotInParentGroup_FansOut — control
// case for the test above. Same setup EXCEPT the bot is NOT a parent-
// group member. The OBO fan-out is the ONLY delivery path to the bot
// (it isn't a direct topic subscriber), so Gate 4 must NOT fire and the
// bot must receive exactly one fan-out copy.
func TestFanout_Gate4_CommunityTopic_BotNotInParentGroup_FansOut(t *testing.T) {
	const parentGroup = "group_topic_parent_89"
	const topicChan = parentGroup + "____topic_100"
	ct := common.ChannelTypeCommunityTopic.Uint8()

	s := seedGrantWithScope(t, topicChan, ct)
	// Grantor is in the parent group; bot is NOT. Bot only sees the
	// message via OBO fan-out, so Gate 4 must NOT fire.
	s.seedGroupMember(parentGroup, tGrantor)

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	// Same DB-free membership shim as the sibling test — proves the
	// Gate 4 branch correctly answers "bot NOT a parent-group member"
	// and lets the fan-out through.
	ba.oboGroupMemberOverride = func(uid, groupNo string) (bool, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.groupMembers[groupNo][uid], nil
	}

	msg := &config.MessageResp{
		FromUID:     "u_bob",
		ChannelID:   topicChan,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu look at this","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("CommunityTopic with bot outside parent group must fan out exactly 1, got %d", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("CommunityTopic control: expected 1 captured copy, got %d", len(fc.copies))
	}
}
