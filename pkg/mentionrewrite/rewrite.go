// Package mentionrewrite owns the inbound rewrite + outbound double-write
// contract for the `mention.{all,humans,ais}` payload field
// (YUJ-202 / Mininglamp-OSS/octo-server#94 — 方案 X "dead-field" strategy
// for `@所有人`, three-state implementation of the PR#70 audit §5
// recommendation).
//
// Why a separate pkg/
// ===================
// The helper has to be invoked from THREE client-controlled message
// ingresses (modules/message/api.go, modules/bot_api/send.go,
// modules/robot/api.go). `modules/message` already imports
// `modules/robot` (revoke flow), so placing the helper in
// `modules/message` would create a `robot → message → robot` import
// cycle. The cleanest fix is to keep the helper in a leaf package neither
// transport-layer module depends on transitively, mirroring how
// pkg/obopayload solved the same shape of cross-module sharing for the
// OBO `__obo_*` reserved key strip/reject contract.
//
// A thin re-export lives at `modules/message/mention_rewrite.go` so the
// message-module callers can keep using the unqualified `RewriteMention`
// symbol and so the issue spec's "modules/message/mention_rewrite.go is
// the helper file" expectation is preserved.
//
// Inbound rewrite (CALL from message ingress chokepoints)
// =======================================================
// Legacy clients still emit `mention.all=1` for `@所有人`. Plan X
// (YUJ-1389) assigns `all=1` the **ais broadcast** semantics: legacy
// `@所有人` traffic automatically fans out to all AI bots without
// requiring an SDK update on the sender side. So inbound
// `mention.all=1` is rewritten to also carry `mention.ais=1`. A NEW
// field `mention.humans=1` is the explicit human-notification signal
// — it is the only way a client can request a channel-level reminder
// for the human members of a channel. The legacy `mention.all=1` field
// is INTENTIONALLY preserved on the outbound payload (double-write) so
// old read-side clients that only understand `all` keep rendering the
// "@所有人" pill until their roll-out catches up. New read-side clients
// prefer `humans` / `ais` and IGNORE `all` when either of the new
// fields is set — see the read-side change in Message.getMention
// (modules/message/api_reminders.go).
//
// `mention.humans=1` is left untouched. `mention.ais=1` is left
// untouched. `mention.uids` / `mention.entities` are left untouched.
//
// The helper is idempotent: RewriteMention(RewriteMention(p)) ==
// RewriteMention(p) for every input. Idempotency lets callers invoke it
// at every chokepoint without worrying about re-entry from listeners /
// fan-out / future relay paths.
//
// Safe on nil / empty / non-map `mention` payloads (no panic, no
// mutation beyond the strict double-write).
package mentionrewrite

import "encoding/json"

// MentionKey is the top-level payload key under which the three-state
// mention state lives. Exposed so callers and tests share one constant.
const MentionKey = "mention"

// AllKey is the legacy `@所有人` field. Inbound `all=1` is rewritten
// (Plan X / YUJ-1389) to also carry `ais=1` so legacy clients
// automatically trigger all AI bots without an SDK update; the `all`
// field itself is preserved on the dispatched payload (outbound
// double-write) for backward compat with old read-side clients that
// only understand `all`.
const AllKey = "all"

// HumansKey signals a human-only broadcast (`@所有真人`). New read-side
// clients render the "@所有人" pill from this field and IGNORE `all`.
// Plan X: this is the ONLY signal that produces a channel-level
// human-visible reminder — bots respond via the message delivery path
// without needing a reminder row.
const HumansKey = "humans"

// AIsKey signals a bot broadcast (`@所有 AI`). Independent of `humans`
// — both can be set on the same message (`@所有人 + @所有 AI`). Plan X:
// inbound legacy `all=1` is rewritten to carry `ais=1` so all bots
// fan out by default for the legacy `@所有人` shape.
const AIsKey = "ais"

// RewriteMention normalizes the payload's `mention` sub-map per the
// three-state contract. Mutates the inner map in place when the inbound
// shape calls for a rewrite, and returns the (possibly same) outer map
// so callers can keep the `payload = RewriteMention(payload)` assign-
// back pattern used elsewhere in the message dispatch stack (mirrors
// `enrichPayloadWithSpaceID`).
//
// Behavior (Plan X / YUJ-1389):
//   - payload == nil → returns nil (no allocation).
//   - payload has no `mention` key, or `mention` is not a
//     map[string]interface{} → returned untouched.
//   - mention.all is truthy (==1 in either json.Number, float64, int,
//     int64, uint64, or bool form) → mention.ais is set to the
//     canonical json.Number("1"). mention.all is preserved.
//   - mention.ais is already truthy → no-op on ais (preserves
//     whatever numeric/bool form the caller sent).
//   - mention.humans is left untouched in every branch — humans is the
//     explicit human-notification signal and must NEVER be inferred
//     from a legacy `all=1`.
//   - mention.uids / mention.entities / any other key inside mention is
//     left untouched.
//
// Idempotent by construction: a second pass over an already-rewritten
// payload sees ais truthy and does nothing.
func RewriteMention(payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		return nil
	}
	raw, ok := payload[MentionKey]
	if !ok || raw == nil {
		return payload
	}
	mention, ok := raw.(map[string]interface{})
	if !ok {
		// Defensive: malformed mention (string / int / array) — never
		// the shape a real client sends. Leave untouched so the read
		// side / validation tests can keep asserting on the original
		// payload.
		return payload
	}
	if !isTruthyOne(mention[AllKey]) {
		return payload
	}
	// Plan X / YUJ-1389: inbound `all=1` → make sure ais=1 is also
	// present so legacy `@所有人` automatically fans out to all AI bots
	// without requiring an SDK update on the sender side. Don't
	// overwrite an ais value the client already supplied (might be a
	// new client that explicitly set ais in addition to legacy all for
	// forward+backward compat).
	if !isTruthyOne(mention[AIsKey]) {
		mention[AIsKey] = json.Number("1")
	}
	// `all` is INTENTIONALLY preserved — see package godoc on the
	// outbound double-write rationale.
	return payload
}

// isTruthyOne reports whether v is the numeric/boolean form of 1. The
// `mention.*` fields arrive from `json.Decoder.UseNumber()` so the
// hot path is json.Number, but client/test code may also send float64,
// int, int64, uint64, or bool — handle all of them defensively so a
// caller does not have to pre-normalize before calling RewriteMention.
func isTruthyOne(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case json.Number:
		n, err := x.Int64()
		return err == nil && n == 1
	case float64:
		return x == 1
	case float32:
		return x == 1
	case int:
		return x == 1
	case int8:
		return x == 1
	case int16:
		return x == 1
	case int32:
		return x == 1
	case int64:
		return x == 1
	case uint:
		return x == 1
	case uint8:
		return x == 1
	case uint16:
		return x == 1
	case uint32:
		return x == 1
	case uint64:
		return x == 1
	case bool:
		return x
	default:
		return false
	}
}
