// Package obopayload owns the reserved-namespace contract for the
// On-Behalf-Of (OBO) persona-clone fan-out path
// (YUJ-1166 / Mininglamp-OSS/octo-server#81).
//
// The fan-out listener (modules/bot_api/obo_fanout.go) uses a payload key
// (`__obo_processed__`) to break the OBO dispatch → listener → fan-out
// loop. The marker MUST be server-only: if a client (bot OR user) could
// set it on an inbound message, that client would be able to suppress
// fan-out for any message and bypass the persona-clone delivery
// guarantee Jerry-Xin's PR#82 R8 review called out.
//
// PR#82 R8 (head 244fe9fa) gap: the bot API ingress
// (/v1/bot/sendMessage, see modules/bot_api/send.go) already rejects any
// `__obo_*` key on inbound payloads, but the user-message ingress
// (/v1/message/send, see modules/message/api.go) was passing user
// payloads through to ctx.SendMessage unchanged. A normal user could
// send `{"__obo_processed__": true, ...}` in a DM or group and the
// listener's gate-3 short-circuit would drop the message before any
// persona-clone copy reached the grantee bots.
//
// The remediation is to STRIP `__obo_*` top-level keys at every
// user-message ingress before persistence/dispatch. This package owns
// the prefix constant, the marker constant, and the shared strip/check
// helpers so every ingress (and every test) agrees on the same
// contract.
//
// Strip vs. reject: the bot API REJECTS (user-friendly 4xx — bots are
// expected to read the spec and not write reserved keys), the user
// message API STRIPS (silently removes the keys before dispatch — users
// are not expected to know which keys are reserved, and a normal client
// would never send them). Both behaviors share the same prefix
// definition here.
package obopayload

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ReservedKeyPrefix is the prefix that marks a payload key as part of
// the server-only OBO reserved namespace. Any top-level key starting
// with this prefix must be either rejected (bot API) or stripped (user
// API) before the payload is persisted or dispatched, so the fan-out
// listener's gate-3 marker (and any future server-only OBO field)
// cannot be forged by a client.
const ReservedKeyPrefix = "__obo_"

// ProcessedMarkerKey is the JSON payload key the OBO dispatch path sets
// on every authorized OBO send so the fan-out listener can short-circuit
// gate 3 without re-querying. The double-underscore prefix puts it in
// the ReservedKeyPrefix namespace — clients cannot set or suppress it
// because the ingress validators strip/reject anything under the
// prefix.
const ProcessedMarkerKey = "__obo_processed__"

// ExplicitReservedKeys lists payload keys that the server injects on
// the OBO send / fan-out path and that MUST be server-only: a client
// that could set them on inbound payloads could spoof the grantor
// identity, redirect fan-out, impersonate the system-hint pathway, or
// (for `actual_sender_uid`) forge the message's effective sender for
// any downstream consumer that trusts the field as the
// authenticated-by-server identity behind an OBO send.
//
// Two namespace shapes share this allowlist:
//
//   - `obo_*` (single-underscore) — the routing context injected by
//     modules/bot_api/obo_fanout.go's buildFanoutCopyReq (PR#121 R2 —
//     Jerry-Xin 2026-05-21 blocking review). They use the legacy
//     single-underscore `obo_` prefix (not the double-underscore
//     `__obo_` reserved-marker prefix) for compatibility with
//     downstream consumers that already read these names. We can't
//     retroactively switch to a `__obo_` prefix without breaking
//     those consumers, so the ingress filters guard the same set via
//     an explicit allowlist instead of by prefix.
//
//   - `actual_sender_uid` (no `obo_` prefix) — the server-injected
//     "real bot behind an OBO send" identity set by
//     modules/bot_api/send.go when fromUID != robotID (PR#121 R3 —
//     Jerry-Xin 2026-05-21 blocking review). It does NOT live under
//     any prefix because the field name predates the OBO reserved
//     namespace and downstream consumers (audit, persona-clone
//     attribution, fan-out copy provenance) read it by exact name. A
//     client that could set `actual_sender_uid` on an inbound payload
//     would be able to forge the bot identity that downstream paths
//     attribute the message to — the same impersonation risk the
//     `obo_grantor_uid` reservation closes for the user side.
//
// Notes
//   - `obo_processed` (single-underscore) is intentionally NOT in this
//     list: it is a legacy client-readable hint, not the server-only
//     gate-3 marker (that one is `__obo_processed__` under the
//     ReservedKeyPrefix).
//   - PR#121 R6 (Jerry-Xin + lml2468 2026-05-22 blocking): the
//     v2-canonical `obo_origin_message_id` and the resolved
//     `obo_grantor_name` are ALSO injected by buildFanoutCopyReq
//     alongside the legacy `obo_origin_message_idstr` /
//     `obo_grantor_uid`. They were missing from the allowlist in R5,
//     so a client could spoof either: faking `obo_origin_message_id`
//     would let a peer redirect a v2-aware adapter's reply to an
//     arbitrary message id, and faking `obo_grantor_name` would let a
//     peer rewrite the persona's user-visible display name (the
//     system-hint text is composed from it). Both are now reserved.
//   - When adding a new server-only OBO payload key in
//     buildFanoutCopyReq, send.go's OBO marker block, or any other
//     server injection site, add it here too. The shared
//     payload_test.go locks the contract.
var ExplicitReservedKeys = map[string]struct{}{
	"obo_respond_as":           {},
	"obo_grantor_uid":          {},
	"obo_fanout":               {},
	"obo_origin_channel_id":    {},
	"obo_origin_channel_type":  {},
	"obo_origin_from_uid":      {},
	"obo_origin_message_id":    {},
	"obo_origin_message_idstr": {},
	"obo_grantor_name":         {},
	"obo_system_hint":          {},
	// PR#121 R3 (Jerry-Xin 2026-05-21 blocking): server-set actual
	// sender identity behind an OBO send. Injected by
	// modules/bot_api/send.go when fromUID != robotID; no `obo_`
	// prefix because downstream readers use the exact field name.
	"actual_sender_uid": {},
}

// IsReservedKey reports whether a single top-level payload key name
// belongs to the server-only OBO reserved namespace. A key is reserved
// if EITHER:
//
//   - it starts with ReservedKeyPrefix (`__obo_`) — the original
//     double-underscore namespace, home of `__obo_processed__` (the
//     gate-3 marker) and any future server-only marker; OR
//   - it matches an entry in ExplicitReservedKeys — the
//     single-underscore `obo_*` set covering the buildFanoutCopyReq
//     routing fields (`obo_respond_as`, `obo_grantor_uid`,
//     `obo_fanout`, `obo_origin_*`, `obo_system_hint`) PLUS the
//     prefix-less `actual_sender_uid` field injected by send.go's
//     OBO marker block (PR#121 R3).
//
// Used by HasReservedKey (bot-API reject) and StripReservedKeys
// (user / robot ingress strip) so both ingresses + the listener's
// gate-3 check share one definition of "reserved".
func IsReservedKey(k string) bool {
	if strings.HasPrefix(k, ReservedKeyPrefix) {
		return true
	}
	_, ok := ExplicitReservedKeys[k]
	return ok
}

// HasReservedKey reports whether any top-level key in the decoded
// payload map is part of the OBO reserved namespace (see IsReservedKey
// for the full membership rule). Used by the bot API ingress
// (modules/bot_api/send.go) to fail fast with a 4xx when a bot client
// tries to forge server-only state.
func HasReservedKey(payload map[string]interface{}) bool {
	if len(payload) == 0 {
		return false
	}
	for k := range payload {
		if IsReservedKey(k) {
			return true
		}
	}
	return false
}

// StripReservedKeys removes every top-level key from `payload` that
// IsReservedKey reports as reserved (the `__obo_*` prefix namespace
// plus the explicit single-underscore `obo_*` allowlist). Returns the
// number of keys stripped so the caller can log/metric the rare
// events. Safe on a nil or empty map (no-op, returns 0).
//
// The map is mutated in place — callers that need to preserve the
// caller-supplied map should clone first. The user-message ingress in
// modules/message/api.go owns its decoded `req.Payload` by then, so
// in-place mutation is fine there.
//
// We do NOT recurse into nested objects/arrays. The OBO reserved
// namespace is defined at the TOP LEVEL of the dispatch payload (that
// is the level the fan-out listener and downstream routers inspect);
// nested fields under a user-controlled key (e.g.
// `extra.__obo_processed__` or `extra.obo_respond_as`) are not part of
// the contract and would not affect gate 3 or fan-out routing.
func StripReservedKeys(payload map[string]interface{}) int {
	if len(payload) == 0 {
		return 0
	}
	stripped := 0
	for k := range payload {
		if IsReservedKey(k) {
			delete(payload, k)
			stripped++
		}
	}
	return stripped
}

// HasProcessedMarker reports whether the raw JSON payload decodes as an
// object containing `ProcessedMarkerKey: true`. Non-JSON or
// non-boolean values are treated as absent so we err on the side of
// fanning out.
//
// PR#82 R8 (perf nit from Jerry-Xin): the cheap pre-check is
// bytes.Contains on the raw payload bytes — no `string()` conversion,
// no allocation. Most inbound messages do not carry the marker, so the
// JSON decode is short-circuited 99.9%+ of the time. Only the matching
// minority pays the unmarshal cost.
func HasProcessedMarker(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	// Quick reject before the unmarshal — payloads in the millions/sec
	// hot path shouldn't pay the JSON decode cost just to find no marker.
	// bytes.Contains avoids the string() alloc the prior strings.Contains
	// version forced on the entire payload (PR#82 R8 perf nit).
	if !bytes.Contains(payload, []byte(ProcessedMarkerKey)) {
		return false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	v, ok := m[ProcessedMarkerKey].(bool)
	return ok && v
}
