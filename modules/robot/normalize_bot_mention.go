// modules/robot/normalize_bot_mention.go
//
// YUJ-2531 / Mininglamp-OSS/octo-server#208: server-side guard that
// strips a bare `mention.all=1` from payloads delivered to bots and
// injects `mention.humans=1` in its place.
//
// Why this exists
// ===============
// The three-state mention contract (octo-server#94 / #142) treats
// `mention.all=1` as the LEGACY `@所有人` signal. Read-side clients
// render the "@所有人" pill from it, but the new routing semantics are
// driven by `mention.humans=1` (notify humans) and `mention.ais=1`
// (notify bots). A legacy client that only emits `mention.all=1`
// produces a payload that a bot adapter cannot interpret cleanly — and
// the openclaw adapter (openclaw-channel-octo) lives on the user's own
// machine, so we cannot guarantee it is updated to map `all` → humans.
//
// To make bot ingress robust regardless of adapter version, every
// payload that reaches a bot's event queue is normalized here:
//   - `mention.all` is stripped (bots must never see the bare legacy
//     broadcast flag), and
//   - `mention.humans=1` is injected when it is not already present,
//     preserving the "notify humans" intent the legacy `all=1` carried.
//
// Human-client delivery is NOT affected: this helper is only invoked on
// the bot event-queue write paths (saveRobotMessage / enqueueBotEvent-
// Generic). The WuKongIM fan-out to human clients keeps `all=1` for
// rendering.
//
// Contract (locked by normalize_bot_mention_test.go):
//  1. Pure: never mutates the caller's `payload` byte slice; a new
//     []byte is returned only when a rewrite actually happens.
//  2. Only `mention.all` and `mention.humans` are touched. `mention.ais`,
//     `mention.uids`, `mention.entities`, and every sibling key are
//     preserved exactly.
//  3. No-op when there is no truthy `mention.all`: the original bytes are
//     returned unchanged (no allocation, no re-serialization).
//  4. Idempotent: a second pass over a normalized payload is a no-op
//     (the stripped `all` is already gone).
//  5. Best-effort on malformed input: if the payload does not parse, or
//     `mention` is not an object, the original bytes are returned. We
//     MUST NOT drop the message.
//  6. Numeric precision: int64 fields (e.g. message_id) survive the
//     round trip via json.Decoder.UseNumber().
package robot

import (
	"bytes"
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/tidwall/gjson"
)

// stripBareMentionAllForBot returns a payload byte slice with a truthy
// `mention.all` removed and `mention.humans=1` injected (when not
// already set). See the file header for the full contract.
func stripBareMentionAllForBot(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	// Fast-path: nothing to do unless `mention.all` is truthy.
	allResult := gjson.GetBytes(payload, "mention.all")
	if !mentionAllTruthy(allResult) {
		return payload
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var doc map[string]interface{}
	if err := dec.Decode(&doc); err != nil {
		return payload
	}
	if doc == nil {
		return payload
	}

	raw, ok := doc[mentionrewrite.MentionKey]
	if !ok || raw == nil {
		return payload
	}
	mention, isObj := raw.(map[string]interface{})
	if !isObj {
		// Malformed mention — best-effort skip (contract clause 5).
		return payload
	}

	// Strip the legacy `all` flag and preserve the "notify humans"
	// intent via `humans=1` unless the client already set it.
	delete(mention, mentionrewrite.AllKey)
	if !decodedTruthyOne(mention[mentionrewrite.HumansKey]) {
		mention[mentionrewrite.HumansKey] = 1
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

// decodedTruthyOne reports whether a json.Decoder(UseNumber)-decoded
// value is the numeric/boolean form of 1. Covers the shapes the bot
// payload decoder can produce for `mention.humans` (json.Number on the
// hot path, plus bool / float64 defensively).
func decodedTruthyOne(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case json.Number:
		n, err := x.Int64()
		return err == nil && n == 1
	case float64:
		return x == 1
	case int:
		return x == 1
	case int64:
		return x == 1
	case bool:
		return x
	default:
		return false
	}
}

// mentionAllTruthy reports whether a gjson-parsed `mention.all` value is
// the canonical truthy form (1 / true / "1"). Mirrors mentionAisTruthy
// (ais_broadcast.go) so the strip side and the dispatch side agree on
// what counts as a set flag.
func mentionAllTruthy(r gjson.Result) bool {
	if !r.Exists() {
		return false
	}
	switch r.Type {
	case gjson.True:
		return true
	case gjson.False, gjson.Null:
		return false
	case gjson.Number:
		return r.Int() == 1
	case gjson.String:
		return r.Str == "1"
	}
	return false
}
