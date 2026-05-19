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
	gid, err := s.insertGrant(tGrantor, tBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable); err != nil {
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
		Payload:     []byte(`{"type":1,"content":"hello yu"}`),
	}
	got := ba.fanoutForMessage(msg)
	if got != 1 {
		t.Fatalf("expected 1 fan-out, got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 captured copy, got %d", len(fc.copies))
	}
	cp := fc.copies[0]
	if len(cp.Subscribers) != 1 || cp.Subscribers[0] != tBot {
		t.Fatalf("subscribers should be [%s], got %v", tBot, cp.Subscribers)
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
		Payload:     []byte(`{"type":1,"content":"forged","obo_processed":true}`),
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
		Payload:     []byte(`{"type":1,"content":"hello yu"}`),
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
		Payload:     []byte(`{"type":1,"content":"hello yu"}`),
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
	gid, err := s.insertGrant(tGrantor, tBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable); err != nil {
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
	if len(cp.Subscribers) != 1 || cp.Subscribers[0] != tBot {
		t.Fatalf("subscribers should be [%s], got %v", tBot, cp.Subscribers)
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
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
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
	gid, _ := s.insertGrant(tGrantor, tBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
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
		Payload:     []byte(`{"type":1,"content":"hi yu"}`),
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
	gidAlice, err := s.insertGrant(aliceUID, aliceBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant alice: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gidAlice, "", &enable); err != nil {
		t.Fatalf("updateGrant alice: %v", err)
	}
	if _, err := s.insertScope(gidAlice, peer, ct, 1); err != nil {
		t.Fatalf("insertScope alice: %v", err)
	}
	// Carol's grant + scope (peer=Bob) — the exploit setup. Carol and
	// Bob are friends so the per-grant access check WOULD permit this
	// grant absent the recipient filter.
	gidCarol, err := s.insertGrant(carolUID, carolBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant carol: %v", err)
	}
	if err := s.updateGrant(gidCarol, "", &enable); err != nil {
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
	if len(cp.Subscribers) != 1 || cp.Subscribers[0] != aliceBot {
		t.Fatalf("multi-grantor DM leak: subscriber should be [%s], got %v", aliceBot, cp.Subscribers)
	}
	// Explicitly assert Carol's bot is NOT a subscriber on any copy —
	// the regression we're guarding against.
	for _, c := range fc.copies {
		for _, sub := range c.Subscribers {
			if sub == carolBot {
				t.Fatalf("CROSS-USER DM LEAK: carol's bot (%s) received fan-out of bob→alice DM", carolBot)
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
	gid, err := s.insertGrant(tGrantor, tBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable); err != nil {
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
		Payload:     []byte(`{"type":1,"content":"hello yu"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("single-grantor DM happy path: expected 1 fan-out, got %d", n)
	}
	if len(fc.copies) != 1 || fc.copies[0].Subscribers[0] != tBot {
		t.Fatalf("single-grantor DM happy path: wrong dispatch, copies=%+v", fc.copies)
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
	gidCarol, err := s.insertGrant(carolUID, carolBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant carol: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gidCarol, "", &enable); err != nil {
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
