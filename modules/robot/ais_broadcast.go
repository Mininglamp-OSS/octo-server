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
	"github.com/tidwall/gjson"
)

// mentionAisTruthy reports whether a gjson-parsed `mention.ais` value
// is the canonical truthy form (1 / true / "1"). Mirrors the semantics
// of mentionFlagTruthy in modules/message/api_reminders.go so the read
// side (reminders) and the dispatch side (robot events) agree on what
// counts as `@所有 AI`.
//
// The send-side rewrite chokepoint (pkg/mentionrewrite/rewrite.go)
// writes json.Number("1") which gjson surfaces as a Number with
// Int() == 1, so that's the hot path. We also accept the JSON `true`
// form and the string "1" form defensively — a future client / proxy
// rewrite might canonicalize the field differently and we don't want
// a quiet broadcast regression because of it.
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
