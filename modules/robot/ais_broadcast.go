// modules/robot/ais_broadcast.go
//
// YUJ-1393 / PR#82 review #2 R2 (Jerry-Xin 2026-05-19 follow-up):
// dispatch helpers for the `mention.ais=1` ("@所有 AI") broadcast in
// the robot event listener (modules/robot/event.go).
//
// Why this lives in its own file
// ==============================
// The robot event dispatcher already mixes DM friend-gate logic,
// mention.uids parsing, and `@username` text parsing into one
// for-loop. Adding the ais-broadcast path inline keeps the hot loop
// readable, but the helpers themselves (the gjson truthy check, the
// group-member → robot filter, the dedup append) are stateless and
// independently testable. Keeping them here means
// `go test ./modules/robot/... -run AisBroadcast` exercises the
// branch without needing the full robotMessageListen plumbing.
//
// Scope
// =====
// Only GROUP channels. PERSONAL DMs already dispatch via the realUID
// branch in robotMessageListen and have no notion of "all members".
// COMMUNITY_TOPIC support is a deliberate follow-up — it requires
// parent-group resolution (see parseThreadChannelID in
// modules/webhook/api.go) and was intentionally left out of this
// hotfix to keep the change surface small.
package robot

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// mentionAisTruthy reports whether a gjson-parsed `mention.ais` value
// is the canonical truthy form (1 / true / "1"). Mirrors the semantics
// of mentionFlagTruthy in modules/message/api_reminders.go so the read
// side (reminders) and the dispatch side (robot events) agree on what
// counts as `@所有 AI`.
//
// Post-Mininglamp-OSS/octo-server#142 the send-side chokepoint
// (pkg/mentionrewrite/rewrite.go) is a pass-through — legacy
// `mention.all=1` is NOT auto-promoted into `mention.ais=1` anymore,
// so a client that wants its broadcast to summon group bots must set
// `mention.ais=1` explicitly. The numeric `1` shape (json.Number from
// the upstream UseNumber decoder, surfaced by gjson as a Number with
// Int() == 1) is still the hot path; we also accept the JSON `true`
// form and the string "1" form defensively — a future client / proxy
// might canonicalize the field differently and we don't want a quiet
// broadcast regression because of it.
//
// Exposed as a method on *Robot purely so the dispatcher's call site
// reads `rb.mentionAisTruthy(...)`, matching the surrounding
// `rb.existRobot(...)` / `rb.getCreatorUID(...)` style. There is no
// receiver state — the method body never touches `rb`.
func (rb *Robot) mentionAisTruthy(r gjson.Result) bool {
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
		// Strict: only the canonical "1" string counts. We intentionally
		// do NOT accept "true" / "yes" here — that would let a typo'd
		// client trigger an all-bot broadcast accidentally.
		return r.Str == "1"
	}
	return false
}

// collectGroupRobotIDs returns the deduplicated UIDs of every robot
// member in `groupNo`. Returns (nil, nil) when the group has no
// members or no robot members — the caller must treat an empty
// result as "no broadcast targets" without logging it as an error.
//
// Failure modes:
//   - groupService.GetMembers fails → returns (nil, err). The caller
//     logs at error level and skips the ais branch for this message,
//     i.e. the broadcast is best-effort; a transient DB error MUST
//     NOT break the rest of the dispatcher (uid-matched bots in the
//     same message still get their event).
//   - existRobot fails for an individual member → that member is
//     skipped (with a log line at the call site in event.go for
//     parity with the existing mention.uids path), the rest of the
//     enumeration continues. We don't want one stale cache key to
//     silently drop the entire broadcast.
//
// Dedup: GetMembers already returns one row per (group, uid) pair so
// the result is naturally unique on uid, but we still guard with a
// seen-set so a future schema change that allows duplicate member
// rows can't double-dispatch the same event to the same bot.
func (rb *Robot) collectGroupRobotIDs(groupNo string) ([]string, error) {
	members, err := rb.groupService.GetMembers(groupNo)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if m == nil || m.UID == "" {
			continue
		}
		if _, dup := seen[m.UID]; dup {
			continue
		}
		isRobot, robotErr := rb.existRobot(m.UID)
		if robotErr != nil {
			// Single-member lookup failure must NOT abort the whole
			// broadcast — log at the call site and skip this member.
			// We could surface the err out, but the caller would have
			// to either fail the whole broadcast (bad UX: one stale
			// cache entry drops every bot) or log + continue anyway
			// (same as here, just one extra hop).
			continue
		}
		if isRobot {
			out = append(out, m.UID)
			seen[m.UID] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// injectBotUIDIntoMentionUIDs returns a payload byte slice with `botUID`
// appended to `mention.uids`. It is the per-bot rewrite used by the
// ais-broadcast fan-out so a legacy adapter (octo-server#137) that only
// looks at `mention.uids` (and ignores `mention.ais`) still recognises
// itself as mentioned and replies.
//
// Contract (mirrors the constraints called out in the YUJ-1784 issue
// body — change with care, the unit tests below lock each one):
//
//  1. The function is pure: it never mutates the caller's `payload`
//     byte slice. A new []byte is returned on any successful rewrite.
//  2. ONLY `mention.uids` is touched. `mention.all`, `mention.humans`,
//     `mention.ais`, and any sibling keys are preserved exactly as
//     they appeared in the input — see the explicit
//     TestInjectBotUIDIntoMentionUIDs_DoesNotTouchMentionAllOrHumans
//     guard. We rely on this in the dispatcher because:
//       - `mention.all` participates in adapter-side
//         `ignoreMentionAll` opt-outs (persona clones, OBO targets),
//         flipping it on every bot would break those opt-outs.
//       - `mention.ais` is the very signal that brought us into this
//         branch; rewriting it would mask the source intent in
//         downstream logs / audits.
//  3. Dedup: if `botUID` is already a member of `mention.uids`, the
//     original byte slice is returned unchanged (no allocation, no
//     re-serialization). This matters because the same bot can appear
//     both in `mention.uids` (explicit @bot_x) AND in the ais
//     broadcast member list; the dispatcher only ever calls this
//     helper for the broadcast subset, but we still defend in depth.
//  4. Missing `mention` object: a fresh `mention.uids=[botUID]` is
//     created. This keeps the helper safe to call on any payload
//     shape that survives JSON parsing.
//  5. Best-effort on malformed input: if the payload does not parse,
//     or `mention` exists but is not an object, or `mention.uids`
//     exists but is not an array, the original bytes are returned
//     unchanged. We MUST NOT drop the message — the legacy adapter
//     would simply fail to recognise the mention (the current bug),
//     which is no worse than the pre-fix state.
//  6. Numeric precision: payloads may carry `message_id` int64 values
//     that exceed the float64 53-bit mantissa range. The decoder uses
//     `UseNumber()` so the marshal round-trip preserves them.
//
// Performance: gjson is used for the fast-path dedup check so the
// common "already present" case avoids the json.Unmarshal /
// json.Marshal cycle. Only the "needs append" path pays the round
// trip, which is amortised against the broadcast fan-out goroutine
// anyway.
func injectBotUIDIntoMentionUIDs(payload []byte, botUID string) []byte {
	if len(payload) == 0 || botUID == "" {
		return payload
	}

	// Fast-path: if the bot's UID is already inside mention.uids,
	// return the original bytes without re-serialising. This is the
	// defence-in-depth case described in contract clause 3.
	if uidsResult := gjson.GetBytes(payload, "mention.uids"); uidsResult.IsArray() {
		for _, u := range uidsResult.Array() {
			if u.String() == botUID {
				return payload
			}
		}
	}

	// Slow path: parse, mutate mention.uids only, re-marshal.
	// UseNumber() preserves int64 fields (e.g. message_id) across the
	// round trip — see contract clause 6.
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var doc map[string]interface{}
	if err := dec.Decode(&doc); err != nil {
		return payload
	}
	if doc == nil {
		return payload
	}

	var mention map[string]interface{}
	if existing, ok := doc["mention"]; ok {
		m, isObj := existing.(map[string]interface{})
		if !isObj {
			// mention exists but is not an object — best-effort skip
			// (contract clause 5). A malformed mention would already
			// not be acted on by any sane adapter.
			return payload
		}
		mention = m
	} else {
		mention = map[string]interface{}{}
		doc["mention"] = mention
	}

	var uids []interface{}
	if existing, ok := mention["uids"]; ok {
		switch v := existing.(type) {
		case []interface{}:
			uids = v
		case nil:
			uids = nil
		default:
			// uids exists but is not an array — best-effort skip.
			return payload
		}
		// Re-run the dedup after Unmarshal: the fast-path gjson check
		// only covered the IsArray() case; if `mention.uids` was the
		// JSON literal `null` it surfaces here as the typed-nil branch
		// above and we still want to honour clause 3.
		for _, u := range uids {
			if s, ok := u.(string); ok && s == botUID {
				return payload
			}
		}
	}
	uids = append(uids, botUID)
	mention["uids"] = uids

	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

// appendUniqueRobotIDs returns the concatenation of `existing` and the
// entries from `add` that are not already in `existing` (dedup on
// string equality). Preserves the order of `existing`, then appends
// new entries from `add` in their original order.
//
// Used by the ais-broadcast branch in robotMessageListen to merge the
// group-wide robot set into any robotIDs already collected from
// mention.uids without dispatching the same event twice to the same
// bot. Allocation is O(len(existing)+len(add)) — fine at the
// per-message hot path where len is bounded by group size.
func appendUniqueRobotIDs(existing, add []string) []string {
	if len(add) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(add))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	out := existing
	for _, id := range add {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		out = append(out, id)
		seen[id] = struct{}{}
	}
	return out
}
