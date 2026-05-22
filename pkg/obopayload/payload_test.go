// Package obopayload tests — locks down the reserved-namespace contract
// the bot API (reject), the user message API (strip), and the fan-out
// listener (gate-3 check) all share. A regression here would let one
// ingress drift from the others and silently break the persona-clone
// fan-out guarantee.
package obopayload

import (
	"bytes"
	"testing"
)

// TestIsReservedKey locks down the reserved-namespace membership rule
// per-key (rather than per-payload, which HasReservedKey covers). The
// rule has two halves — `__obo_` prefix OR explicit allowlist — and a
// regression in either half is a security bug, so the test asserts
// both halves independently.
func TestIsReservedKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// Double-underscore prefix half.
		{"__obo_processed__", true},
		{"__obo_anything", true},
		{"__obo_", true},
		// Explicit single-underscore allowlist (PR#121 R2).
		{"obo_respond_as", true},
		{"obo_grantor_uid", true},
		{"obo_fanout", true},
		{"obo_origin_channel_id", true},
		{"obo_origin_channel_type", true},
		{"obo_origin_from_uid", true},
		{"obo_origin_message_id", true},
		{"obo_origin_message_idstr", true},
		{"obo_grantor_name", true},
		{"obo_system_hint", true},
		// Prefix-less server-injected sender identity (PR#121 R3).
		// Set by modules/bot_api/send.go when fromUID != robotID;
		// a client that could set it would forge the "real bot
		// behind an OBO send" identity downstream audit /
		// attribution paths trust.
		{"actual_sender_uid", true},
		// Anti-overreach — plain keys, legacy keys, and unknown
		// `obo_*` user-payload keys must remain non-reserved.
		{"type", false},
		{"content", false},
		{"_obo_internal", false},
		{"obo_processed", false},
		{"obo_random_user_field", false},
		{"obo", false},
		// Anti-overreach for `actual_sender_uid` (PR#121 R3): only
		// the exact lowercase field is reserved. Common adjacent
		// names that downstream code DOES NOT trust must stay
		// pass-through so we don't break unrelated client schemas.
		{"sender_uid", false},
		{"actual_sender", false},
		{"ACTUAL_SENDER_UID", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsReservedKey(tc.key); got != tc.want {
			t.Errorf("IsReservedKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestHasReservedKey(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    bool
	}{
		{"nil", nil, false},
		{"empty", map[string]interface{}{}, false},
		{"plain", map[string]interface{}{"type": 1, "content": "hi"}, false},
		{"single underscore not reserved", map[string]interface{}{"_obo_internal": true}, false},
		{"legacy obo_processed not reserved", map[string]interface{}{"obo_processed": true}, false},
		{"the marker itself", map[string]interface{}{"__obo_processed__": true}, true},
		{"any double-underscore obo key", map[string]interface{}{"__obo_anything__": "x"}, true},
		{"mixed in", map[string]interface{}{"type": 1, "__obo_marker": false}, true},

		// PR#121 R2: single-underscore obo_* fan-out keys must also be
		// recognized as server-only so the bot-API ingress rejects them
		// and the user / robot ingress strips them. These keys are
		// injected by modules/bot_api/obo_fanout.go's
		// buildFanoutCopyReq — a client setting them on inbound would
		// spoof the OBO grantor identity or fan-out routing.
		{"obo_respond_as reserved", map[string]interface{}{"obo_respond_as": "u_admin"}, true},
		{"obo_grantor_uid reserved", map[string]interface{}{"obo_grantor_uid": "u_admin"}, true},
		{"obo_fanout reserved", map[string]interface{}{"obo_fanout": true}, true},
		{"obo_origin_channel_id reserved", map[string]interface{}{"obo_origin_channel_id": "ch"}, true},
		{"obo_origin_channel_type reserved", map[string]interface{}{"obo_origin_channel_type": 1}, true},
		{"obo_origin_from_uid reserved", map[string]interface{}{"obo_origin_from_uid": "u"}, true},
		{"obo_origin_message_id reserved", map[string]interface{}{"obo_origin_message_id": "m1"}, true},
		{"obo_origin_message_idstr reserved", map[string]interface{}{"obo_origin_message_idstr": "m1"}, true},
		{"obo_grantor_name reserved", map[string]interface{}{"obo_grantor_name": "Admin"}, true},
		{"obo_system_hint reserved", map[string]interface{}{"obo_system_hint": "noop"}, true},

		// PR#121 R3: actual_sender_uid is server-only (no `obo_`
		// prefix) — modules/bot_api/send.go injects it on every OBO
		// send when fromUID != robotID so downstream consumers can
		// attribute the message to the real bot behind the
		// persona-clone dispatch. A client (bot OR user) that could
		// set it on inbound would spoof that attribution.
		{"actual_sender_uid reserved", map[string]interface{}{"actual_sender_uid": "u_admin"}, true},
		{"actual_sender_uid mixed in", map[string]interface{}{"type": 1, "content": "hi", "actual_sender_uid": "bot_x"}, true},

		// Anti-overreach: only the explicit set is reserved at the
		// single-underscore level. A user payload key that merely
		// starts with `obo_` but is NOT one of the known fan-out
		// fields must still be passed through (else we'd break legacy
		// callers who happened to choose `obo_*` for unrelated data).
		{"unknown single-underscore obo_ not reserved", map[string]interface{}{"obo_random_user_field": "x"}, false},

		// PR#121 R3 anti-overreach: only the exact lowercase
		// `actual_sender_uid` is reserved. Adjacent client field
		// names that downstream code does NOT trust must pass
		// through so we don't break unrelated schemas.
		{"adjacent sender_uid not reserved", map[string]interface{}{"sender_uid": "u"}, false},
		{"adjacent actual_sender not reserved", map[string]interface{}{"actual_sender": "u"}, false},
		{"upper-case ACTUAL_SENDER_UID not reserved", map[string]interface{}{"ACTUAL_SENDER_UID": "u"}, false},
	}
	for _, tc := range cases {
		got := HasReservedKey(tc.payload)
		if got != tc.want {
			t.Errorf("%s: HasReservedKey(%v) = %v, want %v", tc.name, tc.payload, got, tc.want)
		}
	}
}

func TestStripReservedKeys(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]interface{}
		wantN    int
		wantLeft map[string]interface{}
	}{
		{"nil no-op", nil, 0, nil},
		{"empty no-op", map[string]interface{}{}, 0, map[string]interface{}{}},
		{
			"no reserved keys untouched",
			map[string]interface{}{"type": 1, "content": "hi"},
			0,
			map[string]interface{}{"type": 1, "content": "hi"},
		},
		{
			"single marker stripped",
			map[string]interface{}{"type": 1, "__obo_processed__": true},
			1,
			map[string]interface{}{"type": 1},
		},
		{
			"multiple reserved stripped",
			map[string]interface{}{
				"type":              1,
				"content":           "hi",
				"__obo_processed__": true,
				"__obo_marker":      "x",
				"__obo_anything__":  42,
			},
			3,
			map[string]interface{}{"type": 1, "content": "hi"},
		},
		{
			"single underscore preserved",
			map[string]interface{}{"_obo_internal": "keep", "__obo_processed__": true},
			1,
			map[string]interface{}{"_obo_internal": "keep"},
		},
		{
			"legacy obo_processed preserved",
			map[string]interface{}{"obo_processed": true},
			0,
			map[string]interface{}{"obo_processed": true},
		},

		// PR#121 R2: single-underscore obo_* fan-out keys must be
		// stripped from user / robot ingress payloads so a malicious
		// client cannot inject buildFanoutCopyReq-only routing fields.
		{
			"all explicit reserved keys stripped",
			map[string]interface{}{
				"type":                     1,
				"content":                  "hi",
				"obo_respond_as":           "u_admin",
				"obo_grantor_uid":          "u_admin",
				"obo_fanout":               true,
				"obo_origin_channel_id":    "ch",
				"obo_origin_channel_type":  1,
				"obo_origin_from_uid":      "u",
				"obo_origin_message_id":    "m1",
				"obo_origin_message_idstr": "m1",
				"obo_grantor_name":         "Admin",
				"obo_system_hint":          "noop",
				// PR#121 R3 — actual_sender_uid is in the same
				// allowlist; co-strip with the obo_* set.
				"actual_sender_uid": "bot_admin",
			},
			11,
			map[string]interface{}{"type": 1, "content": "hi"},
		},
		{
			"explicit + double-underscore stripped together",
			map[string]interface{}{
				"type":              1,
				"__obo_processed__": true,
				"obo_respond_as":    "u_admin",
				"obo_fanout":        true,
				"actual_sender_uid": "bot_x",
			},
			4,
			map[string]interface{}{"type": 1},
		},
		{
			"unknown obo_ key preserved",
			map[string]interface{}{"obo_my_random_field": "x", "obo_respond_as": "u"},
			1,
			map[string]interface{}{"obo_my_random_field": "x"},
		},

		// PR#121 R3 dedicated cases: actual_sender_uid alone must
		// strip; adjacent client field names must be preserved.
		{
			"actual_sender_uid alone stripped",
			map[string]interface{}{"type": 1, "content": "hi", "actual_sender_uid": "bot_x"},
			1,
			map[string]interface{}{"type": 1, "content": "hi"},
		},
		{
			"adjacent sender_uid / actual_sender preserved",
			map[string]interface{}{
				"sender_uid":        "u1",
				"actual_sender":     "u2",
				"actual_sender_uid": "bot_x",
			},
			1,
			map[string]interface{}{"sender_uid": "u1", "actual_sender": "u2"},
		},
	}
	for _, tc := range cases {
		n := StripReservedKeys(tc.payload)
		if n != tc.wantN {
			t.Errorf("%s: StripReservedKeys returned %d, want %d", tc.name, n, tc.wantN)
		}
		if !mapsEqual(tc.payload, tc.wantLeft) {
			t.Errorf("%s: after strip payload = %v, want %v", tc.name, tc.payload, tc.wantLeft)
		}
	}
}

func mapsEqual(a, b map[string]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || bv != v {
			return false
		}
	}
	return true
}

func TestHasProcessedMarker_Variants(t *testing.T) {
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
		got := HasProcessedMarker([]byte(tc.payload))
		if got != tc.want {
			t.Errorf("%s: HasProcessedMarker(%q) = %v, want %v", tc.name, tc.payload, got, tc.want)
		}
	}
}

// TestFanout_HasOBOProcessedMarker_UsesContains — PR#82 R8 perf nit
// regression guard. Jerry-Xin flagged the prior implementation calling
// strings.Contains(string(payload), key), which allocated a full copy
// of every inbound payload (including bot media, sticker JSON, etc).
// The fix uses bytes.Contains directly on the raw payload bytes.
//
// We can't observe "alloc skipped" portably in a unit test, so we
// instead lock in the two contracts that prove the bytes path is live:
//
//  1. The fast-path reject works on payloads that have no JSON
//     structure at all (random bytes) without panicking — the bytes
//     pre-check must answer false BEFORE json.Unmarshal sees the
//     garbage. If anyone reverts to strings.Contains(string(payload), …)
//     the function still passes this case but the next one catches a
//     stricter property: the implementation must accept payloads with
//     embedded NUL bytes (a string([]byte{0,…}) cast is legal in Go,
//     but a regression that switches to a strings.Index loop over
//     `string(payload)` would still observe the NULs and we'd at
//     minimum prove the function is byte-safe).
//
//  2. The implementation MUST short-circuit on a payload missing the
//     marker substring — i.e. the byte scan happens, and a payload
//     that is valid JSON but does not contain the marker substring
//     never reaches the decoder. We enforce this by feeding a
//     deliberately malformed JSON tail AFTER a leading object that
//     would otherwise unmarshal; a strings.Contains pre-check or a
//     bytes.Contains pre-check both short-circuit identically here,
//     so the assertion is that the function returns false WITHOUT
//     surfacing an unmarshal error to the caller (i.e. no panic, no
//     side channel).
func TestFanout_HasOBOProcessedMarker_UsesContains(t *testing.T) {
	// Case 1: NUL bytes are tolerated. bytes.Contains handles this
	// natively; a strings.Contains(string(payload), …) regression would
	// also pass this case, but the assertion proves the function does
	// not blow up on non-UTF-8 / raw binary inputs (Jerry-Xin's perf
	// argument was about not paying string() conversion cost; in
	// practice clients are JSON, but bot media frames can carry
	// arbitrary bytes inside string fields).
	nul := []byte{'{', '"', 'x', '"', ':', '"', 0, 0, 0, '"', '}'}
	if HasProcessedMarker(nul) {
		t.Errorf("NUL-tolerant pre-check should return false, got true")
	}

	// Case 2: payload that contains the marker SUBSTRING in a string
	// value but NOT as a top-level key. The cheap pre-check passes
	// (substring present) so json.Unmarshal runs; the decoded map
	// shows the marker is NOT a top-level key, so the function returns
	// false. This proves both halves of the contract: (a) the
	// pre-check is just a substring test, not a JSON parse, so it
	// can't reject false positives on its own; (b) the post-check
	// requires the marker to be a real top-level key set to true.
	embedded := []byte(`{"content":"talking about __obo_processed__ literally","type":1}`)
	if HasProcessedMarker(embedded) {
		t.Errorf("marker substring inside a string value must NOT trigger gate 3, got true")
	}

	// Case 3: real marker — sanity that the function still returns
	// true on the canonical input.
	real := []byte(`{"__obo_processed__":true}`)
	if !HasProcessedMarker(real) {
		t.Errorf("canonical marker payload must trigger gate 3, got false")
	}

	// Case 4: the marker key MUST be findable byte-wise in the raw
	// payload (this is the bytes.Contains contract). If a future
	// refactor introduces a partial match (e.g. searching for
	// "__obo_" only) the pre-check would accept payloads that don't
	// carry the actual marker. Guard against that by asserting a
	// payload with only the prefix (not the full marker key) does
	// NOT match.
	prefixOnly := []byte(`{"__obo_other_key":true}`)
	if HasProcessedMarker(prefixOnly) {
		t.Errorf("prefix-only payload must NOT trigger gate 3, got true")
	}
	// Defensive sanity: the bytes pre-check we rely on really does
	// see the marker substring on the canonical payload.
	if !bytes.Contains(real, []byte(ProcessedMarkerKey)) {
		t.Fatalf("bytes pre-check failed to locate marker in canonical payload — test setup bug")
	}
}

// BenchmarkHasProcessedMarker_NoMarker — micro-benchmark covering the
// hot path Jerry-Xin called out: most inbound payloads don't carry the
// marker, so the pre-check must be allocation-free. Run with `go test
// -bench=HasProcessedMarker -benchmem ./pkg/obopayload/` to confirm
// 0 B/op. A regression to `strings.Contains(string(payload), …)` would
// show 1 alloc/op proportional to payload length.
func BenchmarkHasProcessedMarker_NoMarker(b *testing.B) {
	// 1 KiB JSON payload typical of a chat message — wide enough that
	// a string() conversion would be measurable.
	payload := []byte(`{"type":1,"content":"` +
		string(make([]byte, 0, 1024)) +
		`hello world hello world hello world hello world hello world hello ` +
		`world hello world hello world hello world hello world hello world ` +
		`hello world hello world hello world hello world hello world hello"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if HasProcessedMarker(payload) {
			b.Fatalf("payload should not match marker")
		}
	}
}
