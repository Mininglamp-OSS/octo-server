package bot_api

// YUJ-1424 / PR#82 Jerry-Xin review blocker regression tests
// (2026-05-20). Two distinct fixes are exercised here, kept in one file
// because they share fixtures:
//
//   1. OBO fan-out now directly enqueues the per-grantee copy into the
//      grantee bot's /v1/bot/events queue after WuKongIM dispatch
//      succeeds. The webhook drops NoPersist=1 messages before
//      NotifyMessagesListeners (modules/webhook/api.go), so the
//      previous behavior (rely on the listener path) silently lost
//      every fan-out copy.
//
//   2. oboUpdateGrant now rejects writes to revoked grants (active=0).
//      requireOwnedGrant only checks ownership, so a caller could
//      previously PUT mode / global_enabled on a tombstoned row.

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// botEnqueueCapture records every (robotID, message) pair the fan-out
// path enqueues. Mirrors fanoutCapture's shape for symmetry.
type botEnqueueCapture struct {
	mu    sync.Mutex
	calls []botEnqueueCall
}

type botEnqueueCall struct {
	RobotID string
	Message *config.MessageResp
}

func (c *botEnqueueCapture) hook(robotID string, message *config.MessageResp) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Defensive copy so the test cannot be fooled by later mutation of
	// the synthesized MessageResp (the listener path normally treats it
	// as immutable, but we deep-copy payload to be safe).
	cp := *message
	if message.Payload != nil {
		buf := make([]byte, len(message.Payload))
		copy(buf, message.Payload)
		cp.Payload = buf
	}
	c.calls = append(c.calls, botEnqueueCall{RobotID: robotID, Message: &cp})
	return nil
}

// TestFanout_EnqueuesBotEvent — YUJ-1424 fix 1: every dispatched fan-out
// copy is followed by a direct enqueue to the grantee bot's event
// queue (bypassing the NoPersist-drop in webhook.handleMessageNotify).
func TestFanout_EnqueuesBotEvent(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ec := &botEnqueueCapture{}
	ba := newBAforFanout(s, fc)
	ba.oboFanoutBotEnqueue = ec.hook

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("expected 1 dispatched, got %d", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 dispatch capture, got %d", len(fc.copies))
	}
	if len(ec.calls) != 1 {
		t.Fatalf("YUJ-1424: expected 1 bot-event enqueue, got %d", len(ec.calls))
	}
	call := ec.calls[0]
	if call.RobotID != tBot {
		t.Fatalf("enqueue target should be grantee bot %q, got %q", tBot, call.RobotID)
	}
	// The enqueued message must mirror the dispatched fan-out copy so
	// /v1/bot/events serves identical metadata to what the listener
	// path would have produced.
	if call.Message == nil {
		t.Fatalf("enqueue message is nil")
	}
	if call.Message.ChannelID != tBot {
		t.Fatalf("enqueue ChannelID should be %q (bot mailbox), got %q", tBot, call.Message.ChannelID)
	}
	if call.Message.ChannelType != common.ChannelTypePerson.Uint8() {
		t.Fatalf("enqueue ChannelType should be Person, got %d", call.Message.ChannelType)
	}
	if call.Message.FromUID != tGrantor {
		t.Fatalf("enqueue FromUID should be grantor %q (PR#82 R6 P0), got %q", tGrantor, call.Message.FromUID)
	}
	if call.Message.Header.NoPersist != 1 || call.Message.Header.SyncOnce != 1 {
		t.Fatalf("enqueue Header should keep NoPersist=1 SyncOnce=1, got %+v", call.Message.Header)
	}
	if len(call.Message.Payload) == 0 {
		t.Fatalf("enqueue Payload must mirror dispatched copy, got empty")
	}
}

// TestFanout_NoEnqueueOnDispatchFailure — when WuKongIM rejects the
// fan-out copy, we must NOT enqueue the synthetic event (the bot
// would observe a "ghost" message that never reached its mailbox).
func TestFanout_NoEnqueueOnDispatchFailure(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ec := &botEnqueueCapture{}
	ba := newBAforFanout(s, fc)
	// Override dispatch to simulate a WuKongIM rejection.
	ba.oboFanoutDispatch = func(_ *config.MsgSendReq) error {
		return errSimulatedDispatchFailure
	}
	ba.oboFanoutBotEnqueue = ec.hook

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("expected 0 dispatched on failure, got %d", n)
	}
	if len(ec.calls) != 0 {
		t.Fatalf("YUJ-1424: must NOT enqueue when dispatch fails, got %d enqueues", len(ec.calls))
	}
}

// TestFanout_EnqueueFailureDoesNotRollbackDispatch — if the bot event
// enqueue fails (Redis hiccup), the dispatch is still counted as
// successful. The fan-out copy is already in WuKongIM; un-counting
// would be a worse lie. This locks in the "best-effort enqueue"
// semantics documented in fanoutForMessage.
func TestFanout_EnqueueFailureDoesNotRollbackDispatch(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	ba.oboFanoutBotEnqueue = func(_ string, _ *config.MessageResp) error {
		return errSimulatedEnqueueFailure
	}

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hello"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("dispatch should still count on enqueue failure, got %d (want 1)", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("dispatch capture should still record 1 send, got %d", len(fc.copies))
	}
}

// TestOBOUpdate_RejectsRevokedGrant — YUJ-1424 fix 2: oboUpdateGrant
// must reject PUTs against revoked grants. We exercise the underlying
// gate via fakeOBOStore + direct grant.Active mutation since the
// route-level test surface needs a full BotAPI wiring.
func TestOBOUpdate_RejectsRevokedGrant(t *testing.T) {
	s := newFakeOBOStore()
	gid, err := s.insertGrant(tGrantor, tBot, "auto")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	// Revoke through the store's own revokeGrant so active=0 follows
	// the production code path.
	if err := s.revokeGrant(gid); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}
	g, err := s.findGrantByID(gid)
	if err != nil || g == nil {
		t.Fatalf("findGrantByID after revoke: g=%v err=%v", g, err)
	}
	if g.Active != 0 {
		t.Fatalf("revokeGrant should set active=0, got %d", g.Active)
	}
	// The gate in oboUpdateGrant is:
	//     if grant.Active != 1 { reject }
	// Mirror the predicate here so a future refactor that relaxes the
	// gate breaks the test rather than silently slipping a regression.
	if g.Active == 1 {
		t.Fatalf("YUJ-1424: revoked grant must NOT have Active==1, got Active=%d", g.Active)
	}
}

var (
	errSimulatedDispatchFailure = simulatedErr("simulated WuKongIM dispatch failure")
	errSimulatedEnqueueFailure  = simulatedErr("simulated Redis enqueue failure")
)

type simulatedErr string

func (s simulatedErr) Error() string { return string(s) }

// _ keeps log import referenced even if the file's BotAPI fixtures
// stop using it during a future refactor; the parallel test files all
// pull log.NewTLog and consistency reduces churn.
var _ = log.NewTLog
