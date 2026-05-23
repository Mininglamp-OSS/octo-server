// Unit tests for CloneForExpansion — the wire-only payload clone
// introduced for the PR#145 follow-up (do not mutate the in-memory
// payload during the ExpandAisToBotUIDs chokepoint).
//
// Each test pins one clause from the helper's contract. The
// end-to-end "expansion runs on the clone, the original payload is
// unmutated" assertion is in TestExpand_DoesNotMutateOriginal — the
// regression the PR#145 review flagged.
package mentionrewrite

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCloneForExpansion_NilPayload — nil-in, nil-out (matches
// ExpandAisToBotUIDs' nil-payload behavior so the two helpers
// compose cleanly).
func TestCloneForExpansion_NilPayload(t *testing.T) {
	assert.Nil(t, CloneForExpansion(nil))
}

// TestCloneForExpansion_NoMention — shallow clone of the outer map
// when no `mention` key is present. Sibling values are shared by
// reference (correct: ExpandAisToBotUIDs cannot reach them).
func TestCloneForExpansion_NoMention(t *testing.T) {
	src := map[string]interface{}{"type": 1, "content": "hi"}
	out := CloneForExpansion(src)

	assert.NotSame(t, &src, &out, "must return a different outer map")
	assert.Equal(t, src, out, "values must match")

	// Mutating the clone's outer map must NOT affect the source.
	out["type"] = 99
	assert.Equal(t, 1, src["type"], "outer-map mutation must not leak")
}

// TestCloneForExpansion_MentionMapIsolated — the critical clause.
// Mutating `mention.uids` on the clone (the exact write
// ExpandAisToBotUIDs performs) must NOT alter the source payload.
func TestCloneForExpansion_MentionMapIsolated(t *testing.T) {
	src := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais":  json.Number("1"),
			"uids": []interface{}{"u_alice"},
		},
	}
	out := CloneForExpansion(src)

	// Replicate the ExpandAisToBotUIDs mutation against the clone.
	outMention := out["mention"].(map[string]interface{})
	outMention["uids"] = []interface{}{"u_alice", "bot_a", "bot_b"}

	srcMention := src["mention"].(map[string]interface{})
	srcUIDs, _ := srcMention["uids"].([]interface{})
	assert.Equal(t, []interface{}{"u_alice"}, srcUIDs,
		"source mention.uids must be untouched after clone mutation")
	// And the truthy `ais` flag is still preserved on both sides.
	assert.Equal(t, json.Number("1"), srcMention["ais"])
	assert.Equal(t, json.Number("1"), outMention["ais"])
}

// TestCloneForExpansion_UIDsSliceIsolated — append on the clone's
// uids slice must NOT perturb the source's view, even if the source
// slice's backing array still had capacity to grow.
func TestCloneForExpansion_UIDsSliceIsolated(t *testing.T) {
	// Allocate the source uids slice with extra capacity so a naive
	// (non-cloning) append on the clone would write through the
	// shared backing array.
	uids := make([]interface{}, 1, 8)
	uids[0] = "u_alice"
	src := map[string]interface{}{
		"mention": map[string]interface{}{"uids": uids},
	}
	out := CloneForExpansion(src)
	outMention := out["mention"].(map[string]interface{})
	cloneUIDs := outMention["uids"].([]interface{})
	cloneUIDs = append(cloneUIDs, "bot_a")
	outMention["uids"] = cloneUIDs

	srcMention := src["mention"].(map[string]interface{})
	srcUIDs := srcMention["uids"].([]interface{})
	assert.Equal(t, 1, len(srcUIDs), "source uids slice length must not grow")
	assert.Equal(t, "u_alice", srcUIDs[0], "source uids[0] must be unchanged")
}

// TestCloneForExpansion_MalformedMentionForwardedByReference — the
// non-map `mention` value is intentionally passed through by
// reference. The helper's job is to make ExpandAisToBotUIDs safe;
// the defensive non-map shape gate inside ExpandAisToBotUIDs handles
// the rest.
func TestCloneForExpansion_MalformedMentionForwardedByReference(t *testing.T) {
	src := map[string]interface{}{"mention": "not-a-map"}
	out := CloneForExpansion(src)
	assert.Equal(t, "not-a-map", out["mention"])
	// Outer map is still a fresh allocation.
	out["other"] = true
	_, hasOther := src["other"]
	assert.False(t, hasOther)
}

// TestCloneForExpansion_MalformedUIDsForwardedByReference — same
// reasoning as the mention-shape case; mention sub-map IS cloned,
// but the malformed inner uids value (string here) is forwarded.
func TestCloneForExpansion_MalformedUIDsForwardedByReference(t *testing.T) {
	src := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais":  json.Number("1"),
			"uids": "not-an-array",
		},
	}
	out := CloneForExpansion(src)
	outMention := out["mention"].(map[string]interface{})
	assert.Equal(t, "not-an-array", outMention["uids"])
	// Mutating the clone's mention map sibling key must not leak.
	outMention["new"] = "x"
	srcMention := src["mention"].(map[string]interface{})
	_, hasNew := srcMention["new"]
	assert.False(t, hasNew, "clone's mention sub-map must be a fresh allocation")
}

// TestExpand_DoesNotMutateOriginal is the regression the PR#145
// review surfaced: the chokepoint must keep the in-memory payload's
// `mention.uids` free of server-expanded bot UIDs so the reminder
// writer (modules/message/api_reminders.go) does NOT create one
// reminder row per bot member of the group. Explicit
// human-mentioned UIDs (`@alice`) MUST still flow into the
// in-memory payload.
//
// This test composes CloneForExpansion with ExpandAisToBotUIDs the
// way the three ingress chokepoints will compose them, and asserts
// the original payload is bit-for-bit identical to its input.
func TestExpand_DoesNotMutateOriginal(t *testing.T) {
	original := map[string]interface{}{
		"type":    1,
		"content": "@所有 AI plus @alice",
		"mention": map[string]interface{}{
			"ais":  json.Number("1"),
			"uids": []interface{}{"u_alice"},
		},
	}

	// Build the wire copy the exact way the ingress chokepoint will:
	// clone, then expand.
	wire := CloneForExpansion(original)
	wire = ExpandAisToBotUIDs(wire, channelTypeGroup, "ch_1", func(string) ([]string, error) {
		return []string{"bot_a", "bot_b"}, nil
	})

	// Wire payload carries the expansion.
	wireMention := wire["mention"].(map[string]interface{})
	wireUIDs, _ := wireMention["uids"].([]interface{})
	assert.ElementsMatch(t,
		[]interface{}{"u_alice", "bot_a", "bot_b"},
		wireUIDs,
		"wire bytes must include expanded bot UIDs for legacy adapter compat")

	// Original payload is unchanged: only the explicit human UID
	// remains in mention.uids. Reminder writer reading this map
	// will create exactly one row for `u_alice` and ZERO rows for
	// the expanded bot UIDs.
	origMention := original["mention"].(map[string]interface{})
	origUIDs, _ := origMention["uids"].([]interface{})
	assert.Equal(t,
		[]interface{}{"u_alice"},
		origUIDs,
		"in-memory payload's mention.uids must NOT be polluted by server-expanded bot UIDs")
	assert.Equal(t, json.Number("1"), origMention["ais"], "ais flag must be preserved on original")
}
