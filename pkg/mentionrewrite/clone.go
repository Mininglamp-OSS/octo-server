// Helper for Mininglamp-OSS/octo-server PR#145 follow-up
// (Jerry-Xin / lml2468 / yujiawei review): isolate the in-memory
// `payload` map from the wire-bytes representation so
// ExpandAisToBotUIDs can stamp expanded bot UIDs onto the bytes
// dispatched to WuKongIM WITHOUT polluting the in-memory payload
// the caller may still reference for persistence, listener fan-out,
// or downstream reminders.
//
// The bug this guards against
// ===========================
// Before this fix, every ingress chokepoint called
// ExpandAisToBotUIDs(payload, …) directly on the per-request
// `payload map[string]interface{}` and then serialized that same
// map for `MsgSendReq.Payload`. The expansion mutates the inner
// `mention` sub-map in place, so any subsequent code path that
// inspects the same `payload` (the reminder writer at
// modules/message/api_reminders.go iterates `mention.uids` to emit
// one ReminderTypeMentionMe row per UID) would see the
// server-injected bot UIDs and write a reminder per bot member.
// That's a contract violation — `ais=1` is supposed to mean "bots
// fan out via the delivery path; do NOT create human-visible
// `[有人@我]` red-dots" — and a flat-out DB pollution: one bot row
// per group member per broadcast.
//
// The fix is "Option B" from the PR#145 review: keep the helper
// itself unchanged, but call it on a clone of the payload that
// only feeds the wire bytes. This file owns that clone.
//
// Why a hand-rolled clone instead of json round-trip
// ==================================================
// (a) json.Marshal/json.Unmarshal would lose `json.Number` typing
//     (UseNumber is decoder-side, not present on a fresh decode of
//     re-marshalled bytes by default), and the rest of the
//     mentionrewrite contract is built around the `json.Number`
//     truthy gate (see isTruthyOne / RewriteMention). Round-tripping
//     would break that invariant.
// (b) ExpandAisToBotUIDs only ever mutates the `mention` sub-map's
//     `uids` key — it never touches sibling keys, never replaces
//     the outer map, never recurses. So we only need to clone what
//     might be mutated: the outer payload map (so a future helper
//     replacing payload[MentionKey] cannot leak), the `mention`
//     sub-map (so the in-place `mention[UIDsKey] = …` write doesn't
//     reach the original), and the `mention.uids` slice (defensive,
//     so an `existing = append(existing, …)` that doesn't grow the
//     backing array still cannot perturb the original slice header).
//     Everything else is shared by reference, which is fine because
//     ExpandAisToBotUIDs cannot reach it.
package mentionrewrite

// CloneForExpansion returns a wire-only copy of `payload` suitable
// for passing to ExpandAisToBotUIDs. The original `payload` and
// every key it transitively shares with the returned map remain
// untouched after the helper mutates the copy.
//
// Cloning is intentionally minimal — only the keys ExpandAisToBotUIDs
// can reach are duplicated:
//
//   - the outer `map[string]interface{}` (so a future helper that
//     replaces payload[MentionKey] cannot leak through the alias);
//   - the `mention` sub-map (so the in-place
//     `mention[UIDsKey] = newSlice` write in ExpandAisToBotUIDs
//     stays inside the wire copy);
//   - the `mention.uids` slice when present and well-formed (so an
//     append that fits the backing array's existing capacity still
//     cannot perturb the original slice's view of the data).
//
// All sibling keys (e.g. `type`, `content`, `space_id`,
// `__obo_processed`) are shared by reference — they are read-only
// from this package's perspective, and the caller is the only one
// that may mutate them via paths outside this helper.
//
// Contract:
//
//   - nil → nil (matches ExpandAisToBotUIDs' nil-payload behavior).
//   - payload with no `mention` key → shallow clone of the outer
//     map; sibling values shared by reference.
//   - payload[MentionKey] not a map → shallow clone of the outer
//     map; the malformed value is forwarded by reference (the
//     caller's defensive contract still applies).
//   - mention[UIDsKey] not a slice → shallow clone of the mention
//     sub-map; the malformed value is forwarded by reference.
//
// CloneForExpansion is safe to call repeatedly and is idempotent
// in the sense that cloning a clone produces an equivalent
// (separately-allocated) copy.
func CloneForExpansion(payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		return nil
	}
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	raw, ok := out[MentionKey]
	if !ok || raw == nil {
		return out
	}
	mention, ok := raw.(map[string]interface{})
	if !ok {
		return out
	}
	mentionCopy := make(map[string]interface{}, len(mention))
	for k, v := range mention {
		mentionCopy[k] = v
	}
	if rawUIDs, hasUIDs := mentionCopy[UIDsKey]; hasUIDs && rawUIDs != nil {
		if arr, isArr := rawUIDs.([]interface{}); isArr {
			cp := make([]interface{}, len(arr))
			copy(cp, arr)
			mentionCopy[UIDsKey] = cp
		}
	}
	out[MentionKey] = mentionCopy
	return out
}
