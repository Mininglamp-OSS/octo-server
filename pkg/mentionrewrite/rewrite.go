// Package mentionrewrite owns the (now pass-through) inbound contract
// for the `mention.{all,humans,ais}` payload field
// (YUJ-202 / Mininglamp-OSS/octo-server#94 — three-state implementation
// of the PR#70 audit §5 recommendation).
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
// Pass-through behavior (Mininglamp-OSS/octo-server#142)
// ======================================================
// Earlier (Plan X / YUJ-1389) the helper rewrote inbound `mention.all=1`
// to also carry `mention.ais=1`, so legacy `@所有人` traffic auto-
// fanned-out to all AI bots without an SDK update on the sender side.
// Product intent has since been corrected: legacy `@所有人` MUST NOT
// trigger bots. The rewrite block has been removed; the helper is now
// a strict pass-through that leaves the inbound `mention` map untouched
// (modulo nil / non-map defenses). The function signature, the three
// ingress call sites, and the ais broadcast fan-out path
// (modules/robot/event.go, modules/robot/ais_broadcast.go) are
// intentionally unchanged — only the implicit `all → ais` inference is
// gone. New clients still set `mention.ais=1` explicitly to broadcast
// to bots, and `mention.humans=1` explicitly to notify humans.
//
// `mention.all`, `mention.humans`, `mention.ais`, `mention.uids`, and
// `mention.entities` are all left untouched.
//
// The helper is trivially idempotent: RewriteMention(RewriteMention(p))
// == RewriteMention(p) for every input. Idempotency lets callers invoke
// it at every chokepoint without worrying about re-entry from listeners
// / fan-out / future relay paths.
//
// Safe on nil / empty / non-map `mention` payloads (no panic, no
// mutation).
package mentionrewrite

import "encoding/json"

// MentionKey is the top-level payload key under which the three-state
// mention state lives. Exposed so callers and tests share one constant.
const MentionKey = "mention"

// AllKey is the legacy `@所有人` field. Historically (Plan X /
// YUJ-1389) inbound `all=1` was rewritten to also carry `ais=1` so
// legacy clients auto-triggered all AI bots; that inference was
// reverted (Mininglamp-OSS/octo-server#142) because the product intent
// is that legacy `@所有人` MUST NOT trigger bots. The field itself is
// still part of the wire contract (old read-side clients render the
// `@所有人` pill from it) and is passed through untouched by this
// helper.
const AllKey = "all"

// HumansKey signals a human-only broadcast (`@所有真人`). New read-side
// clients render the "@所有人" pill from this field and IGNORE `all`.
// This is the only signal that produces a channel-level human-visible
// reminder — bots respond via the message delivery path without
// needing a reminder row.
const HumansKey = "humans"

// AIsKey signals a bot broadcast (`@所有 AI`). Independent of `humans`
// — both can be set on the same message (`@所有人 + @所有 AI`). After
// Mininglamp-OSS/octo-server#142 this MUST be set explicitly by the
// client; it is no longer inferred from legacy `all=1`.
const AIsKey = "ais"

// RewriteMention is the (now pass-through) normalizer for the
// payload's `mention` sub-map. The signature is preserved so all three
// ingress chokepoints (modules/message/api.go, modules/bot_api/send.go,
// modules/robot/api.go) can keep the `payload = RewriteMention(payload)`
// assign-back pattern without code churn — see
// Mininglamp-OSS/octo-server#142 for why the implicit `all → ais`
// inference was removed.
//
// Behavior:
//   - payload == nil → returns nil (no allocation).
//   - payload has no `mention` key, or `mention` is not a
//     map[string]interface{} → returned untouched.
//   - otherwise the mention map and all its keys (all / humans / ais /
//     uids / entities / anything else) are returned untouched.
//
// Trivially idempotent: a second pass is a no-op.
func RewriteMention(payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		return nil
	}
	raw, ok := payload[MentionKey]
	if !ok || raw == nil {
		return payload
	}
	if _, ok := raw.(map[string]interface{}); !ok {
		// Defensive: malformed mention (string / int / array) — never
		// the shape a real client sends. Leave untouched so the read
		// side / validation tests can keep asserting on the original
		// payload.
		return payload
	}
	// Mininglamp-OSS/octo-server#142: the historical `all=1 → ais=1`
	// inference was removed. Pass through untouched.
	return payload
}

// isTruthyOne reports whether v is the numeric/boolean form of 1. The
// `mention.*` fields arrive from `json.Decoder.UseNumber()` so the
// hot path is json.Number, but client/test code may also send float64,
// int, int64, uint64, or bool — handle all of them defensively. The
// helper is no longer used by RewriteMention itself (the `all → ais`
// rewrite was reverted in Mininglamp-OSS/octo-server#142) but is kept
// for other call sites that need the same truthy-one semantics over
// the JSON-decoded numeric grab-bag.
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
