// Unit tests for the PR#82 R8 user-ingress OBO key strip.
//
// These lock the behavior Jerry-Xin's review on head 244fe9fa
// required: a user-message payload with reserved `__obo_*` keys MUST
// have them removed before the message reaches the dispatcher (and
// therefore before the fan-out listener evaluates gate 3).
package message

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/obopayload"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// captureLog records every (msg, fields) pair sent to logWarnFn so the
// strip's warn-logging contract can be asserted alongside the mutation.
type captureLog struct {
	mu    sync.Mutex
	calls []capturedCall
}

type capturedCall struct {
	msg    string
	fields []zap.Field
}

func (cl *captureLog) warn(msg string, fields ...zap.Field) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.calls = append(cl.calls, capturedCall{msg: msg, fields: fields})
}

// TestUserMessage_OBOReservedKeysStripped — primary regression guard
// for PR#82 R8. A user-message payload carrying `__obo_processed__`
// (the fan-out gate-3 marker) MUST be stripped before dispatch. The
// previous behavior on head 244fe9fa passed the payload through
// untouched, letting any user suppress fan-out by forging the marker.
func TestUserMessage_OBOReservedKeysStripped(t *testing.T) {
	cl := &captureLog{}
	payload := map[string]interface{}{
		"type":              1,
		"content":           "hello",
		"__obo_processed__": true,
	}

	stripped := sanitizeUserIngressPayload(
		payload, "ch_abc", 1, "u_alice", cl.warn,
	)

	// The marker MUST be gone from the dispatched payload — that's
	// the whole point of the fix.
	if _, present := payload["__obo_processed__"]; present {
		t.Fatalf("__obo_processed__ must be stripped from user payload, got %v", payload)
	}
	// Non-reserved keys MUST be preserved — the strip is targeted, not
	// destructive.
	assert.Equal(t, 1, payload["type"], "type field must survive strip")
	assert.Equal(t, "hello", payload["content"], "content field must survive strip")
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

// (Helper removed — earlier draft used a stringifier we no longer
// need; we only assert keys are present, not their values.)

// TestUserMessage_OBOReservedKeysStripped_MultipleKeys — the strip is
// namespace-wide (not marker-specific), so a user payload that tries
// to spoof multiple `__obo_*` keys (e.g. anticipating future
// server-only OBO fields) is fully cleaned in one pass.
func TestUserMessage_OBOReservedKeysStripped_MultipleKeys(t *testing.T) {
	cl := &captureLog{}
	payload := map[string]interface{}{
		"content":             "hi",
		"__obo_processed__":   true,
		"__obo_actual_sender": "victim_bot",
		"__obo_anything_else": "x",
	}

	stripped := sanitizeUserIngressPayload(
		payload, "ch", 1, "u", cl.warn,
	)

	assert.Equal(t, 3, stripped)
	assert.Len(t, payload, 1, "only the non-reserved key should remain")
	assert.Equal(t, "hi", payload["content"])
	assert.Len(t, cl.calls, 1, "exactly one warn log even for multiple keys")
}

// TestUserMessage_OBOReservedKeysStripped_NoOpWhenAbsent — the strip
// MUST be silent + no-op when the user's payload is clean. Otherwise
// every legitimate message in production would log a warning.
func TestUserMessage_OBOReservedKeysStripped_NoOpWhenAbsent(t *testing.T) {
	cl := &captureLog{}
	payload := map[string]interface{}{
		"type":    1,
		"content": "hi",
	}
	stripped := sanitizeUserIngressPayload(payload, "ch", 1, "u", cl.warn)
	assert.Equal(t, 0, stripped)
	assert.Len(t, payload, 2, "clean payload must be untouched")
	assert.Empty(t, cl.calls, "no log on clean payload")
}

// TestUserMessage_OBOReservedKeysStripped_NilPayload — defensive: the
// helper handles nil payloads (which can occur if a future caller
// forgets to default-init) without panicking. Returns 0, logs nothing.
func TestUserMessage_OBOReservedKeysStripped_NilPayload(t *testing.T) {
	cl := &captureLog{}
	stripped := sanitizeUserIngressPayload(nil, "ch", 1, "u", cl.warn)
	assert.Equal(t, 0, stripped)
	assert.Empty(t, cl.calls)
}

// TestUserMessage_OBOReservedKeysStripped_LegacyKeyKept — the legacy
// (v0-shipped) `obo_processed` key is NOT in the reserved namespace
// (no double-underscore prefix) so the strip leaves it alone. This
// matches the bot-API reject and the gate-3 marker check: only the
// `__obo_*` namespace is server-only. Locking this in protects
// downstream consumers that might still inspect the legacy field for
// debugging.
func TestUserMessage_OBOReservedKeysStripped_LegacyKeyKept(t *testing.T) {
	cl := &captureLog{}
	payload := map[string]interface{}{
		"content":       "hi",
		"obo_processed": true, // legacy single-prefix, NOT in reserved namespace
	}
	stripped := sanitizeUserIngressPayload(payload, "ch", 1, "u", cl.warn)
	assert.Equal(t, 0, stripped)
	assert.Equal(t, true, payload["obo_processed"], "legacy key must survive")
	assert.Empty(t, cl.calls)
}

// TestUserMessage_StripContract_PinnedToSharedPackage — meta-assertion:
// the strip MUST go through pkg/obopayload so the user ingress and the
// bot ingress + the fan-out listener can never drift apart on what
// counts as a reserved key. We verify by calling the shared helper
// directly and comparing against the message-module helper for the
// same input.
func TestUserMessage_StripContract_PinnedToSharedPackage(t *testing.T) {
	via := map[string]interface{}{"__obo_processed__": true, "type": 1}
	direct := map[string]interface{}{"__obo_processed__": true, "type": 1}

	sanitizeUserIngressPayload(via, "ch", 1, "u", nil)
	obopayload.StripReservedKeys(direct)

	assert.Equal(t, direct, via, "message-module strip must match shared obopayload contract")
}
