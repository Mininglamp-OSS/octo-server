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

// gjsonStrArray is a tiny helper that materialises a gjson.Result array
// into a []string so test assertions can compare against literal
// slices without per-test boilerplate.
func gjsonStrArray(arr []gjson.Result) []string {
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		out = append(out, v.String())
	}
	return out
}

// TestInjectBotUIDIntoMentionUIDs_AppendsToExistingUIDs — the primary
// rewrite case described in the YUJ-1784 issue body:
//
//	payload has mention.uids=[a,b] → after rewrite uids=[a,b,botX]
//
// This is what unblocks the legacy adapter (octo-server#137) for a
// payload that originally only carried ais=1 plus some explicit @uids.
func TestInjectBotUIDIntoMentionUIDs_AppendsToExistingUIDs(t *testing.T) {
	in := []byte(`{"type":1,"content":"hi","mention":{"uids":["bot_a","bot_b"]}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")

	got := gjsonStrArray(gjson.GetBytes(out, "mention.uids").Array())
	assert.Equal(t, []string{"bot_a", "bot_b", "bot_x"}, got)
}

// TestInjectBotUIDIntoMentionUIDs_DedupSkip — contract clause 3: if the
// bot's UID is already in mention.uids, the helper returns the original
// bytes UNCHANGED (same length, byte-identical). The dispatcher relies
// on this as defence-in-depth for the case where a bot appears both in
// explicit mention.uids AND in the ais broadcast member set.
func TestInjectBotUIDIntoMentionUIDs_DedupSkip(t *testing.T) {
	in := []byte(`{"type":1,"mention":{"uids":["bot_x","bot_b"]}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")
	assert.Equal(t, string(in), string(out), "already present, return unchanged bytes")
}

// TestInjectBotUIDIntoMentionUIDs_MentionWithoutUIDs — contract clause
// 4 (mention exists, uids missing): a fresh uids=[botX] is created and
// every other mention.* field is preserved. This is the most common
// real-world shape because the rewrite chokepoint
// (pkg/mentionrewrite/rewrite.go) emits mention.{all,ais}=1 without
// any uids when the source was a legacy `@所有人`.
func TestInjectBotUIDIntoMentionUIDs_MentionWithoutUIDs(t *testing.T) {
	in := []byte(`{"type":1,"mention":{"all":1,"ais":1}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")

	uids := gjsonStrArray(gjson.GetBytes(out, "mention.uids").Array())
	assert.Equal(t, []string{"bot_x"}, uids)
	assert.Equal(t, int64(1), gjson.GetBytes(out, "mention.all").Int(),
		"mention.all MUST survive the rewrite unchanged")
	assert.Equal(t, int64(1), gjson.GetBytes(out, "mention.ais").Int(),
		"mention.ais MUST survive the rewrite unchanged")
}

// TestInjectBotUIDIntoMentionUIDs_NoMention — contract clause 4 edge
// (no mention object at all): the helper creates mention.uids=[botX].
// Hard to imagine a real payload that reaches the ais branch without
// any mention object, but the helper is documented as safe to call on
// any parsable payload and the test pins the contract.
func TestInjectBotUIDIntoMentionUIDs_NoMention(t *testing.T) {
	in := []byte(`{"type":1,"content":"hi"}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")

	uids := gjsonStrArray(gjson.GetBytes(out, "mention.uids").Array())
	assert.Equal(t, []string{"bot_x"}, uids)
	// sibling keys preserved
	assert.Equal(t, int64(1), gjson.GetBytes(out, "type").Int())
	assert.Equal(t, "hi", gjson.GetBytes(out, "content").String())
}

// TestInjectBotUIDIntoMentionUIDs_DoesNotTouchMentionAllOrHumans —
// contract clause 2, the load-bearing invariant of the entire fix:
// ONLY mention.uids is mutated. mention.all participates in adapter-
// side `ignoreMentionAll` opt-outs (persona clones / OBO targets) and
// flipping it would break those opt-outs. mention.humans / mention.ais
// are likewise preserved verbatim.
func TestInjectBotUIDIntoMentionUIDs_DoesNotTouchMentionAllOrHumans(t *testing.T) {
	in := []byte(`{"type":1,"mention":{"all":0,"humans":1,"ais":1,"uids":["a"]}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")

	assert.Equal(t, int64(0), gjson.GetBytes(out, "mention.all").Int(),
		"mention.all=0 must NOT flip to 1 just because we appended a uid")
	assert.Equal(t, int64(1), gjson.GetBytes(out, "mention.humans").Int())
	assert.Equal(t, int64(1), gjson.GetBytes(out, "mention.ais").Int())
	assert.Equal(t, []string{"a", "bot_x"},
		gjsonStrArray(gjson.GetBytes(out, "mention.uids").Array()))
}

// TestInjectBotUIDIntoMentionUIDs_EmptyInputs — contract clause 1 fast
// rejects: empty payload and empty botUID are both no-ops. The
// dispatcher will not call the helper with these in practice, but
// keeping the guard explicit avoids a future caller producing a
// spurious `{"mention":{"uids":[""]}}` payload.
func TestInjectBotUIDIntoMentionUIDs_EmptyInputs(t *testing.T) {
	assert.Nil(t, injectBotUIDIntoMentionUIDs(nil, "bot_x"))
	assert.Empty(t, injectBotUIDIntoMentionUIDs([]byte{}, "bot_x"))

	in := []byte(`{"mention":{"uids":["a"]}}`)
	assert.Equal(t, string(in), string(injectBotUIDIntoMentionUIDs(in, "")))
}

// TestInjectBotUIDIntoMentionUIDs_MalformedPayload — contract clause 5:
// a non-JSON payload returns the original bytes unchanged. Dropping
// the message here would be strictly worse than the pre-fix state.
func TestInjectBotUIDIntoMentionUIDs_MalformedPayload(t *testing.T) {
	in := []byte(`{not json}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")
	assert.Equal(t, string(in), string(out))
}

// TestInjectBotUIDIntoMentionUIDs_MentionWrongType — contract clause 5
// branch: if mention exists but is not an object (e.g. it was set to a
// bare string by a buggy client), the helper returns the original
// bytes unchanged rather than panic-ing or overwriting a typed field.
func TestInjectBotUIDIntoMentionUIDs_MentionWrongType(t *testing.T) {
	in := []byte(`{"mention":"@all"}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")
	assert.Equal(t, string(in), string(out))
}

// TestInjectBotUIDIntoMentionUIDs_UIDsWrongType — sibling guard for
// contract clause 5: mention.uids exists but is e.g. an object, not
// an array. Best-effort skip.
func TestInjectBotUIDIntoMentionUIDs_UIDsWrongType(t *testing.T) {
	in := []byte(`{"mention":{"uids":{"a":1}}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")
	assert.Equal(t, string(in), string(out))
}

// TestInjectBotUIDIntoMentionUIDs_UIDsNull — `mention.uids: null` must
// be treated like a missing uids field: create uids=[botX]. This shape
// can be emitted by some JSON producers that always serialise every
// declared field.
func TestInjectBotUIDIntoMentionUIDs_UIDsNull(t *testing.T) {
	in := []byte(`{"mention":{"ais":1,"uids":null}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")
	uids := gjsonStrArray(gjson.GetBytes(out, "mention.uids").Array())
	assert.Equal(t, []string{"bot_x"}, uids)
}

// TestInjectBotUIDIntoMentionUIDs_PreservesMessageIDPrecision —
// contract clause 6: a near-MAX_INT64 message_id MUST survive the
// json.Unmarshal → json.Marshal round trip without float64 precision
// loss. UseNumber() on the decoder is what gives us this guarantee.
func TestInjectBotUIDIntoMentionUIDs_PreservesMessageIDPrecision(t *testing.T) {
	in := []byte(`{"message_id":9223372036854775806,"mention":{"ais":1}}`)
	out := injectBotUIDIntoMentionUIDs(in, "bot_x")
	assert.Equal(t, int64(9223372036854775806), gjson.GetBytes(out, "message_id").Int(),
		"int64 fields must NOT be coerced through float64 during the rewrite")
}

// TestInjectBotUIDIntoMentionUIDs_DoesNotMutateCaller — contract
// clause 1 hard guarantee: the input byte slice must NOT be mutated.
// We pass a slice and confirm its bytes are identical after the call.
// This protects callers that re-use payload bytes for other purposes
// (e.g. the dispatcher passes the original message.Payload to other
// non-ais bots in the same loop).
func TestInjectBotUIDIntoMentionUIDs_DoesNotMutateCaller(t *testing.T) {
	in := []byte(`{"mention":{"uids":["a"]}}`)
	snapshot := append([]byte(nil), in...)
	_ = injectBotUIDIntoMentionUIDs(in, "bot_x")
	assert.Equal(t, snapshot, in, "input byte slice MUST NOT be mutated")
}
