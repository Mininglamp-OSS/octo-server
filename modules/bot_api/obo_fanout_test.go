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
// obo_processed=true (set by sendMessage when on_behalf_of was honored).
// This is the loop guard that breaks the cycle "bot replies → reply is
// observed → bot replies again". The marker must be respected even if
// the FromUID looks like a random user (since FromUID = grantor when OBO
// fires — already covered by gate 2 — but also defensive against future
// callers that set the marker without flipping FromUID).
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
		Payload:     []byte(`{"type":1,"content":"bot reply","obo_processed":true}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("gate 3 (obo_processed marker) violated: dispatched %d", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
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
// rather than the surrounding fan-out plumbing.
func TestHasOBOProcessedMarker_Variants(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"empty", "", false},
		{"non-json", "not json at all", false},
		{"json no marker", `{"type":1}`, false},
		{"marker true", `{"obo_processed":true}`, true},
		{"marker false", `{"obo_processed":false}`, false},
		{"marker not bool", `{"obo_processed":"yes"}`, false},
		{"marker mixed in", `{"type":1,"content":"hi","obo_processed":true}`, true},
	}
	for _, tc := range cases {
		got := hasOBOProcessedMarker([]byte(tc.payload))
		if got != tc.want {
			t.Errorf("%s: hasOBOProcessedMarker(%q) = %v, want %v", tc.name, tc.payload, got, tc.want)
		}
	}
}
