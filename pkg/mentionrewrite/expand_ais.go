// Helper for Mininglamp-OSS/octo-server#144: expand `mention.ais=1`
// (the `@所有 AI` broadcast signal) into an explicit `mention.uids`
// list of every bot member of a GROUP channel, at the same three
// inbound chokepoints as RewriteMention.
//
// Why this lives next to RewriteMention
// =====================================
// RewriteMention (rewrite.go) is the existing inbound contract for the
// `mention` sub-map and is invoked at exactly the three message
// ingresses we need to extend (modules/message/api.go,
// modules/bot_api/send.go, modules/robot/api.go). Putting the new
// helper in the same package keeps the chokepoint-normalization
// surface in one file tree and means any future change to
// `mention.uids` semantics has a single home.
//
// pkg/mentionrewrite must remain a leaf package
// =============================================
// rewrite.go's package docstring spells out the historical
// `robot → message → robot` import cycle that motivated putting the
// inbound helper here in the first place. The new expansion needs
//
//   - group.IService.GetMembers(channelID) — list group members
//   - robot.IService.ExistRobot(uid)       — filter to bot members
//
// but importing either modules/group or modules/robot from pkg would
// re-introduce the cycle (modules/robot already imports modules/group,
// and modules/message imports modules/robot). So this helper accepts
// a pure-function callback `fetchBotUIDs(channelID) ([]string, error)`
// instead. Each call site composes the callback from the services it
// already holds. Keeping the helper free of module imports also lets
// the unit tests below pin every behavioral clause without the gocraft
// /dbr session and Redis machinery a real robot.IService would drag in.
package mentionrewrite

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
)

// UIDsKey is the per-user mention list inside the `mention` sub-map.
// Exposed alongside MentionKey / AllKey / HumansKey / AIsKey so the
// constant set used by the chokepoint helpers is visible at a glance.
const UIDsKey = "uids"

// ExpandAisToBotUIDs is the second-pass mention chokepoint helper.
// When the inbound payload signals an AI broadcast (`mention.ais=1`)
// in a GROUP channel, it appends every bot UID returned by
// `fetchBotUIDs` to `mention.uids` (deduplicated). This makes the
// WuKongIM-delivered payload itself carry the bot UIDs, so legacy
// adapter bots (Mininglamp-OSS/octo-server#137) that connect via the
// websocket and only inspect `mention.uids` still recognise the
// broadcast — PR #138's per-bot UID injection only reaches the bot
// event queue (/v1/bot/events) and never the websocket dispatch path.
//
// The helper intentionally mirrors RewriteMention's defensive posture:
// it is safe on nil / empty / malformed `mention` shapes and never
// drops the message. An error or empty result from `fetchBotUIDs`
// degrades to a no-op (passing the original payload through), which is
// no worse than the pre-#144 state.
//
// Contract (each clause is locked by a unit test in expand_ais_test.go):
//
//  1. No-op unless `mention.ais` is truthy AND channelType == GROUP.
//     PERSONAL DMs (ChannelTypePerson) and COMMUNITY_TOPIC are out of
//     scope per the issue body — bots in DMs have a different
//     dispatch path, and community-topic bots are out of v0.
//  2. ONLY `mention.uids` is touched. `mention.all`, `mention.humans`,
//     and `mention.ais` are forwarded untouched. The issue body
//     constraint "Do NOT modify mention.all, mention.humans, or
//     mention.ais — only mention.uids" is intentional: those fields
//     drive other read-side behaviors (legacy `@所有人` rendering,
//     human-broadcast reminders, downstream audits) and any
//     server-side rewrite of them would mask the original client
//     intent.
//  3. Idempotent: a UID already present in `mention.uids` is not
//     duplicated, and a second call is a no-op. This dedups in two
//     directions:
//       - within a single call (the same bot returned twice by
//         fetchBotUIDs, or already explicitly @-mentioned by the
//         user);
//       - across the broadcast pipeline — PR #138's
//         injectBotUIDIntoMentionUIDs (modules/robot/ais_broadcast.go)
//         also appends per-bot UIDs on the fan-out path; if a payload
//         passes through both helpers no UID will be duplicated.
//  4. Safe on nil payload (returns nil), missing `mention` key
//     (returns payload), non-map `mention` value (returns payload),
//     non-array `mention.uids` value (returns payload). Same
//     defensive shape as RewriteMention.
//  5. fetchBotUIDs error → original payload returned untouched. We
//     log nothing here (the call site already runs inside an HTTP
//     handler with its own logger); a failed lookup degrades to the
//     pre-#144 behaviour.
//  6. Empty channelID → no-op. The caller's request validation
//     already rejects empty channel_id, but defending here keeps the
//     helper safe to drop into future ingresses.
//
// fetchBotUIDs is expected to enumerate the active robot member UIDs
// of channelID (i.e. group.IService.GetMembers(channelID) filtered by
// robot.IService.ExistRobot). The shape is a callback rather than two
// service interfaces so this package stays a leaf — see the package
// docstring for the import-cycle background.
func ExpandAisToBotUIDs(
	payload map[string]interface{},
	channelType uint8,
	channelID string,
	fetchBotUIDs func(channelID string) ([]string, error),
) map[string]interface{} {
	// Clause 4: safe on nil payload (matches RewriteMention).
	if payload == nil {
		return nil
	}
	// Clause 1: GROUP-only — PERSONAL / COMMUNITY_TOPIC are out of
	// scope per the issue body.
	if channelType != common.ChannelTypeGroup.Uint8() {
		return payload
	}
	// Clause 6: defensive on empty channelID.
	if channelID == "" {
		return payload
	}
	// Clause 4: missing mention / non-map mention → no-op.
	raw, ok := payload[MentionKey]
	if !ok || raw == nil {
		return payload
	}
	mention, ok := raw.(map[string]interface{})
	if !ok {
		return payload
	}
	// Clause 1: ais must be truthy. isTruthyOne handles json.Number,
	// float64, int*, uint*, bool — the same JSON-decoded grab-bag
	// RewriteMention defends against.
	if !isTruthyOne(mention[AIsKey]) {
		return payload
	}
	// Clause 4: only mutate when uids is absent or already a
	// []interface{}. Any other shape (string, map, number) means a
	// malformed inbound payload and we forward untouched — same
	// defensive contract injectBotUIDIntoMentionUIDs follows on the
	// outbound side.
	//
	// P2 (PR#145 review, yujiawei 2026-05-23): this shape gate runs
	// BEFORE the fetchBotUIDs callback so a malformed `mention.uids`
	// value cannot trigger an unnecessary DB roundtrip
	// (group.GetMembers + per-member robot.ExistRobot). The previous
	// ordering called fetchBotUIDs first and then discarded the
	// result on a non-array uids — wasted IO on a payload we know we
	// will not mutate.
	var existing []interface{}
	if rawUIDs, hasUIDs := mention[UIDsKey]; hasUIDs && rawUIDs != nil {
		arr, isArr := rawUIDs.([]interface{})
		if !isArr {
			return payload
		}
		existing = arr
	}
	// Clause 5: nil callback or lookup error → no-op (best effort).
	if fetchBotUIDs == nil {
		return payload
	}
	botUIDs, err := fetchBotUIDs(channelID)
	if err != nil || len(botUIDs) == 0 {
		return payload
	}

	// Build a string-set for clause 3 dedup. Pre-allocate to the
	// maximum possible final size so the common all-new case avoids
	// any rehash.
	seen := make(map[string]struct{}, len(existing)+len(botUIDs))
	for _, v := range existing {
		if s, ok := v.(string); ok && s != "" {
			seen[s] = struct{}{}
		}
	}

	appended := false
	for _, uid := range botUIDs {
		if uid == "" {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		existing = append(existing, uid)
		appended = true
	}
	if !appended {
		// Clause 3: every bot UID was already in mention.uids — return
		// the payload untouched so a second call is observably a
		// no-op and downstream consumers that hash on payload bytes
		// are not perturbed.
		return payload
	}
	mention[UIDsKey] = existing
	return payload
}
