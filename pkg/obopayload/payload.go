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

// HasReservedKey reports whether any top-level key in the decoded
// payload map starts with ReservedKeyPrefix. Used by the bot API
// ingress (modules/bot_api/send.go) to fail fast with a 4xx when a bot
// client tries to forge server-only state.
func HasReservedKey(payload map[string]interface{}) bool {
	if len(payload) == 0 {
		return false
	}
	for k := range payload {
		if strings.HasPrefix(k, ReservedKeyPrefix) {
			return true
		}
	}
	return false
}

// StripReservedKeys removes every top-level key from `payload` whose
// name starts with ReservedKeyPrefix. Returns the number of keys
// stripped so the caller can log/metric the rare events. Safe on a nil
// or empty map (no-op, returns 0).
//
// The map is mutated in place — callers that need to preserve the
// caller-supplied map should clone first. The user-message ingress in
// modules/message/api.go owns its decoded `req.Payload` by then, so
// in-place mutation is fine there.
//
// We do NOT recurse into nested objects/arrays. The OBO reserved
// namespace is defined at the TOP LEVEL of the dispatch payload (that
// is the level the fan-out listener inspects); nested fields under a
// user-controlled key (e.g. `extra.__obo_processed__`) are not part of
// the contract and would not affect gate 3.
func StripReservedKeys(payload map[string]interface{}) int {
	if len(payload) == 0 {
		return 0
	}
	stripped := 0
	for k := range payload {
		if strings.HasPrefix(k, ReservedKeyPrefix) {
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
