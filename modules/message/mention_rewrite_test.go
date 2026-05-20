// Colocated message-package tests for the RewriteMention helper. The
// authoritative contract suite lives at pkg/mentionrewrite/rewrite_test.go
// (where the helper itself is defined). These tests assert the
// message-package shim is wired correctly — a future refactor that
// turns the shim into a no-op stub will trip these.
//
// We deliberately keep these in `package message` (not _test) so the
// tests reach the unqualified `RewriteMention` symbol the
// message-package callers use; this is the same pattern
// sanitize_user_ingress_test.go uses for the obopayload strip shim.
package message

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMessagePackage_RewriteMention_AllRewrittenToAIsDoubleWrite is
// the canonical regression guard for the message-package shim under
// Plan X (YUJ-1389): inbound `mention.all=1` must be rewritten to
// also carry `mention.ais=1` (so legacy `@所有人` auto-fans-out to
// all AI bots without an SDK update), while preserving the legacy
// `all=1` on the outbound payload for old read-side clients. humans
// MUST NOT be auto-set — humans is the explicit human-notification
// signal and only the sender may set it. If someone deletes the
// import or breaks the wiring this test fails.
func TestMessagePackage_RewriteMention_AllRewrittenToAIsDoubleWrite(t *testing.T) {
	payload := map[string]interface{}{
		"type":    1,
		"content": "@所有人 ping",
		"mention": map[string]interface{}{
			"all": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"],
		"all=1 outbound double-write must be preserved")
	assert.Equal(t, json.Number("1"), mention["ais"],
		"Plan X: all=1 inbound must rewrite to add ais=1 (auto-fan-out to bots)")
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans,
		"humans MUST NOT be auto-set — only the sender may set the human-notification signal")
}

// TestMessagePackage_RewriteMention_HumansPassthrough — message-package
// shim must NOT short-circuit on the humans-only shape (the helper
// preserves it untouched). ais MUST NOT be inferred from humans.
func TestMessagePackage_RewriteMention_HumansPassthrough(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"humans": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["humans"])
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs)
	_, hasAll := mention["all"]
	assert.False(t, hasAll)
}

// TestMessagePackage_RewriteMention_AisPassthrough — message-package
// shim must NOT short-circuit on the ais-only shape (the helper
// preserves it untouched).
func TestMessagePackage_RewriteMention_AisPassthrough(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["ais"])
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans)
}

// TestMessagePackage_RewriteMention_NilSafe — defensive: a future
// caller may invoke the shim with a nil payload. Must not panic.
func TestMessagePackage_RewriteMention_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RewriteMention shim panicked on nil: %v", r)
		}
	}()
	assert.Nil(t, RewriteMention(nil))
}
