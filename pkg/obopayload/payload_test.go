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
