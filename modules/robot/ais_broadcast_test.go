// Unit tests for the YUJ-1393 / PR#82 review #2 R2 ais-broadcast
// dispatch helpers (modules/robot/ais_broadcast.go).
//
// These exercise the three stateless pieces of the broadcast path:
//
//   - mentionAisTruthy: the gjson-side `mention.ais` truthy predicate.
//   - appendUniqueRobotIDs: the dedup-merge used to fold the group-
//     wide robot set into any uid-matched robots already collected
//     from mention.uids.
//   - (collectGroupRobotIDs is exercised in the api integration tests
//     because it needs the groupService; here we lock the pure pieces.)
package robot

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

// helper to parse a fragment and return the mention.ais gjson result.
func aisResult(t *testing.T, payload string) gjson.Result {
	t.Helper()
	return gjson.Parse(payload).Get("mention.ais")
}

// TestMentionAisTruthy_CanonicalNumberOne — the rewrite chokepoint
// (pkg/mentionrewrite/rewrite.go) writes `json.Number("1")`; the
// dispatcher MUST treat that as truthy, otherwise legacy `mention.
// all=1` traffic (rewritten to also carry ais=1) silently fails to
// reach group bots.
func TestMentionAisTruthy_CanonicalNumberOne(t *testing.T) {
	rb := &Robot{}
	r := aisResult(t, `{"mention":{"ais":1}}`)
	assert.True(t, rb.mentionAisTruthy(r),
		"the rewrite hot path (json.Number(\"1\")) MUST be truthy")
}

// TestMentionAisTruthy_BooleanTrue — defensive: a client / proxy may
// canonicalize ais as JSON `true`. We accept it so a quiet broadcast
// regression cannot be introduced by an upstream serialization change.
func TestMentionAisTruthy_BooleanTrue(t *testing.T) {
	rb := &Robot{}
	r := aisResult(t, `{"mention":{"ais":true}}`)
	assert.True(t, rb.mentionAisTruthy(r))
}

// TestMentionAisTruthy_StringOne — defensive: same rationale as the
// boolean form. Only the literal string "1" is accepted; "true" /
// "yes" are NOT, to avoid a typo'd client accidentally fan-out-ing
// to every bot.
func TestMentionAisTruthy_StringOne(t *testing.T) {
	rb := &Robot{}
	assert.True(t, rb.mentionAisTruthy(aisResult(t, `{"mention":{"ais":"1"}}`)))
	assert.False(t, rb.mentionAisTruthy(aisResult(t, `{"mention":{"ais":"true"}}`)),
		"only the canonical \"1\" string is accepted; \"true\" is not")
	assert.False(t, rb.mentionAisTruthy(aisResult(t, `{"mention":{"ais":"yes"}}`)))
}

// TestMentionAisTruthy_Falsy — every other shape MUST be falsy. This
// is the symmetric guard for the truthy cases above: a `0`, `false`,
// missing field, null, or non-1 number must NOT trigger the broadcast.
func TestMentionAisTruthy_Falsy(t *testing.T) {
	rb := &Robot{}
	cases := []struct {
		name string
		raw  string
	}{
		{"missing field", `{"mention":{}}`},
		{"explicit zero", `{"mention":{"ais":0}}`},
		{"explicit false", `{"mention":{"ais":false}}`},
		{"explicit null", `{"mention":{"ais":null}}`},
		{"number two", `{"mention":{"ais":2}}`},
		{"negative one", `{"mention":{"ais":-1}}`},
		{"empty string", `{"mention":{"ais":""}}`},
		{"non-truthy string", `{"mention":{"ais":"x"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, rb.mentionAisTruthy(aisResult(t, tc.raw)),
				"shape %s must NOT trigger ais broadcast", tc.name)
		})
	}
}

// TestMentionAisTruthy_NonExistent — calling on a gjson.Result that
// does not Exist() must be a clean `false` (no panic, no allocation).
// Common path: payload has no `mention` key at all.
func TestMentionAisTruthy_NonExistent(t *testing.T) {
	rb := &Robot{}
	// A literal "no such field" result.
	r := gjson.Parse(`{"type":1}`).Get("mention.ais")
	assert.False(t, r.Exists())
	assert.False(t, rb.mentionAisTruthy(r))
}

// TestAppendUniqueRobotIDs_MergesAndDedups — the dedup append MUST
// preserve order, drop duplicates from `add` that are already in
// `existing`, and drop empty strings. Order matters: the goroutine
// fan-out in event.go iterates in slice order and the test makes
// the order contract explicit so a future refactor can't silently
// reshuffle dispatch order.
func TestAppendUniqueRobotIDs_MergesAndDedups(t *testing.T) {
	existing := []string{"bot_a", "bot_b"}
	add := []string{"bot_b", "bot_c", "", "bot_a", "bot_d"}

	out := appendUniqueRobotIDs(existing, add)

	assert.Equal(t, []string{"bot_a", "bot_b", "bot_c", "bot_d"}, out,
		"order: existing first, then new entries from add in original order, no dups, no empty")
}

// TestAppendUniqueRobotIDs_EmptyAddIsNoOp — appending an empty slice
// MUST return the existing slice unchanged (and without allocating a
// new backing array). This is the hot path when ais=1 is set but the
// group has no bot members.
func TestAppendUniqueRobotIDs_EmptyAddIsNoOp(t *testing.T) {
	existing := []string{"bot_a"}
	out := appendUniqueRobotIDs(existing, nil)
	assert.Equal(t, existing, out)

	out2 := appendUniqueRobotIDs(existing, []string{})
	assert.Equal(t, existing, out2)
}

// TestAppendUniqueRobotIDs_EmptyExistingPreservesAdd — when no uid-
// matched bots were collected before the ais branch (the common case
// for a pure `@所有 AI` message), the group-wide robot list becomes
// the entire dispatch target as-is.
func TestAppendUniqueRobotIDs_EmptyExistingPreservesAdd(t *testing.T) {
	out := appendUniqueRobotIDs(nil, []string{"bot_a", "bot_b"})
	assert.Equal(t, []string{"bot_a", "bot_b"}, out)
}

// TestAppendUniqueRobotIDs_DedupesWithinAdd — `add` itself may carry
// duplicates (defensive against a future collectGroupRobotIDs change
// that loosens its own dedup). The merge must still produce a unique
// result.
func TestAppendUniqueRobotIDs_DedupesWithinAdd(t *testing.T) {
	out := appendUniqueRobotIDs(nil, []string{"bot_a", "bot_a", "bot_b"})
	assert.Equal(t, []string{"bot_a", "bot_b"}, out)
}

// TestMentionAisTruthy_AfterRewrite — round-trip with the canonical
// rewrite shape: encode a payload through encoding/json, parse it
// with gjson, and confirm the dispatcher treats the rewritten ais
// field as truthy. This locks the wire-format contract between
// pkg/mentionrewrite.RewriteMention (which writes json.Number("1"))
// and modules/robot.mentionAisTruthy.
func TestMentionAisTruthy_AfterRewrite(t *testing.T) {
	rb := &Robot{}
	payload := map[string]interface{}{
		"type":    1,
		"content": "@所有人 hi",
		"mention": map[string]interface{}{
			"all": json.Number("1"),
			"ais": json.Number("1"), // what RewriteMention writes
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assert.True(t, rb.mentionAisTruthy(gjson.ParseBytes(b).Get("mention.ais")),
		"json.Number(\"1\") MUST survive json.Marshal → gjson round-trip as truthy")
}
