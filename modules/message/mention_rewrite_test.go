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

// TestMessagePackage_RewriteMention_AllRewrittenToHumansDoubleWrite is
// the canonical regression guard for the message-package shim. If
// someone deletes the import or breaks the wiring this test fails.
func TestMessagePackage_RewriteMention_AllRewrittenToHumansDoubleWrite(t *testing.T) {
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
	assert.Equal(t, json.Number("1"), mention["humans"],
		"all=1 inbound must rewrite to add humans=1")
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs,
		"Yu D1: legacy @所有人 must NOT auto-trigger ais=1 (the bot-fan-out pain point)")
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
