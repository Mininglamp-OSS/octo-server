// Unit tests for the PR#82 review #2 R1 robot-ingress OBO key strip.
//
// These lock in the behavior Jerry-Xin's 2026-05-19 follow-up review
// required: a robot-message payload (the legacy
// `/v1/robots/:robot_id/:app_key/sendMessage` endpoint) with reserved
// `__obo_*` keys MUST have them removed before the message reaches the
// dispatcher (and therefore before the fan-out listener evaluates
// gate 3).
//
// Mirrors modules/message/sanitize_user_ingress_test.go one-to-one so
// the three-ingress contract (user / bot / robot) cannot drift apart.
package robot

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/obopayload"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// captureRobotLog records every (msg, fields) pair sent to logWarnFn
// so the strip's warn-logging contract can be asserted alongside the
// mutation. Named distinctly from the message-module `captureLog` to
// avoid any future test-binary symbol clash if helpers move.
type captureRobotLog struct {
	mu    sync.Mutex
	calls []capturedRobotCall
}

type capturedRobotCall struct {
	msg    string
	fields []zap.Field
}

func (cl *captureRobotLog) warn(msg string, fields ...zap.Field) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.calls = append(cl.calls, capturedRobotCall{msg: msg, fields: fields})
}

// TestRobotMessage_OBOReservedKeysStripped — primary regression guard
// for YUJ-1393 / PR#82 review #2 R1. A robot-message payload carrying
// `__obo_processed__` (the fan-out gate-3 marker) MUST be stripped
// before dispatch. Before the fix, the legacy robot endpoint was the
// only one of three ingress points (user / bot / robot) that let the
// marker through unmodified, allowing a malicious robot script to
// silently suppress its own persona-clone fan-out copy.
func TestRobotMessage_OBOReservedKeysStripped(t *testing.T) {
	cl := &captureRobotLog{}
	payload := map[string]interface{}{
		"type":              1,
		"content":           "hello from robot",
		"__obo_processed__": true,
	}

	stripped := sanitizeRobotIngressPayload(
		payload, "ch_group_abc", 2, "bot_alice", cl.warn,
	)

	// The marker MUST be gone from the dispatched payload — that's the
	// whole point of the fix.
	if _, present := payload["__obo_processed__"]; present {
		t.Fatalf("__obo_processed__ must be stripped from robot payload, got %v", payload)
	}
	// Non-reserved keys MUST be preserved — the strip is targeted, not
	// destructive.
	assert.Equal(t, 1, payload["type"], "type field must survive strip")
	assert.Equal(t, "hello from robot", payload["content"], "content field must survive strip")
	assert.Equal(t, 1, stripped, "exactly one __obo_* key was present")

	// And the strip MUST emit a single warn-log so SRE can spot abuse
	// attempts. We assert on the message text + the structured fields
	// (channel_id, channel_type, from_uid, stripped_count) so a quiet
	// regression that drops logging can be caught here.
	if len(cl.calls) != 1 {
		t.Fatalf("expected 1 warn log on strip, got %d", len(cl.calls))
	}
	got := cl.calls[0]
	assert.Contains(t, got.msg, "stripped reserved OBO keys")
	gotKeys := map[string]struct{}{}
	for _, f := range got.fields {
		gotKeys[f.Key] = struct{}{}
	}
	for _, want := range []string{"channel_id", "channel_type", "from_uid", "stripped_count"} {
		if _, ok := gotKeys[want]; !ok {
			t.Errorf("warn log missing field %q; got keys %v", want, gotKeys)
		}
	}
}

// TestRobotMessage_OBOReservedKeysStripped_MultipleKeys — the strip is
// namespace-wide (not marker-specific), so a robot payload that tries
// to spoof multiple `__obo_*` keys (e.g. anticipating future
// server-only OBO fields) is fully cleaned in one pass.
func TestRobotMessage_OBOReservedKeysStripped_MultipleKeys(t *testing.T) {
	cl := &captureRobotLog{}
	payload := map[string]interface{}{
		"content":             "hi",
		"__obo_processed__":   true,
		"__obo_actual_sender": "victim_bot",
		"__obo_anything_else": "x",
	}

	stripped := sanitizeRobotIngressPayload(
		payload, "ch", 2, "bot", cl.warn,
	)

	assert.Equal(t, 3, stripped)
	assert.Len(t, payload, 1, "only the non-reserved key should remain")
	assert.Equal(t, "hi", payload["content"])
	assert.Len(t, cl.calls, 1, "exactly one warn log even for multiple keys")
}

// TestRobotMessage_OBOReservedKeysStripped_NoOpWhenAbsent — the strip
// MUST be silent + no-op when the robot's payload is clean. Otherwise
// every legitimate robot message in production would log a warning.
func TestRobotMessage_OBOReservedKeysStripped_NoOpWhenAbsent(t *testing.T) {
	cl := &captureRobotLog{}
	payload := map[string]interface{}{
		"type":    1,
		"content": "hi",
	}
	stripped := sanitizeRobotIngressPayload(payload, "ch", 2, "bot", cl.warn)
	assert.Equal(t, 0, stripped)
	assert.Len(t, payload, 2, "clean payload must be untouched")
	assert.Empty(t, cl.calls, "no log on clean payload")
}

// TestRobotMessage_OBOReservedKeysStripped_NilPayload — defensive:
// the helper handles nil payloads (which can occur if a future caller
// forgets to default-init) without panicking. Returns 0, logs nothing.
func TestRobotMessage_OBOReservedKeysStripped_NilPayload(t *testing.T) {
	cl := &captureRobotLog{}
	stripped := sanitizeRobotIngressPayload(nil, "ch", 2, "bot", cl.warn)
	assert.Equal(t, 0, stripped)
	assert.Empty(t, cl.calls)
}

// TestRobotMessage_OBOReservedKeysStripped_LegacyKeyKept — the legacy
// (v0-shipped) `obo_processed` key is NOT in the reserved namespace
// (no double-underscore prefix) so the strip leaves it alone. This
// matches the user-API strip, the bot-API reject, and the gate-3
// marker check: only the `__obo_*` namespace is server-only.
func TestRobotMessage_OBOReservedKeysStripped_LegacyKeyKept(t *testing.T) {
	cl := &captureRobotLog{}
	payload := map[string]interface{}{
		"content":       "hi",
		"obo_processed": true, // legacy single-prefix, NOT reserved
	}
	stripped := sanitizeRobotIngressPayload(payload, "ch", 2, "bot", cl.warn)
	assert.Equal(t, 0, stripped)
	assert.Equal(t, true, payload["obo_processed"], "legacy key must survive")
	assert.Empty(t, cl.calls)
}

// TestRobotMessage_StripContract_PinnedToSharedPackage — meta-assertion
// matching the user-ingress meta-assertion: the robot strip MUST go
// through pkg/obopayload so the three ingresses + the fan-out listener
// can never drift apart on what counts as a reserved key.
func TestRobotMessage_StripContract_PinnedToSharedPackage(t *testing.T) {
	via := map[string]interface{}{"__obo_processed__": true, "type": 1}
	direct := map[string]interface{}{"__obo_processed__": true, "type": 1}

	sanitizeRobotIngressPayload(via, "ch", 2, "bot", nil)
	obopayload.StripReservedKeys(direct)

	assert.Equal(t, direct, via, "robot-module strip must match shared obopayload contract")
}
