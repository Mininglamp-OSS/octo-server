// Package bot_api · YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone
// fan-out hook.
//
// Hook design (RFC §5.3): we register a MessagesListener on the shared
// context — same pattern the robot, botfather, thread, and message modules
// already use — so the fan-out happens AFTER WuKongIM has persisted the
// inbound message but BEFORE we deliver the copy. This matches "candidate 1"
// in the RFC and keeps the listener side-effect free with respect to the
// original message.
//
// The listener pulls grants by (channel_id, channel_type) — a single index
// hit per inbound message — then applies the three loop-protection gates
// from RFC §5.3:
//
//	Gate 1: bot self-sent → never replay to that same bot
//	Gate 2: grantor's own outbound → don't fan it to the grantor's bot
//	        (covers the "I typed on my phone" case — bot should not echo)
//	Gate 3: already-OBO-processed → message_extra has __obo_processed__=true
//	        (the bot's outbound, marked by sendMessage, must not bounce)
//
// PR#82 review #2 P1-2: gate 3's marker key is `__obo_processed__` (double-
// underscore reserved prefix), NOT the v0-shipped `obo_processed`. The
// v0 key was a plain JSON field that any bot could set on its own
// /v1/bot/sendMessage payload — letting a bot suppress its own fan-out by
// crafting `{"content":"…", "obo_processed":true}`. The new key sits in
// a reserved namespace (`__obo_*`) that sendMessage strips off inbound
// payloads (see send.go) before processing, so the marker is now
// server-only state. Compatibility note: messages persisted under the
// legacy key during the v0 testing window are NOT honored — gate 3 is
// strict on the new name. Any in-flight v0 messages would only suppress
// their own fan-out (a bounded edge case) and the test suite is the only
// caller that ever wrote the legacy key in this branch.
//
// For each surviving (message, grant) pair we build a MsgSendReq addressed
// to the grantee bot's own PERSONAL mailbox (ChannelID=grantee_bot_uid,
// ChannelType=Person, Subscribers OMITTED). The original delivery to real
// users is untouched.
//
// PR#82 review #5 P0 — WuKongIM /message/send contract: `channel_id` and
// `subscribers` are MUTUALLY EXCLUSIVE on a single MsgSendReq. The v0
// implementation set BOTH (ChannelID = origin conversation, Subscribers =
// [granteeBot]) and WuKongIM rejected every dispatch with:
//
//	【message】channelId和subscribers不能同时存在！
//
// The "OBO fan-out dispatch failed" line in im-test prod showed every
// inbound message tripping this. Fix: address the fan-out copy at the
// bot's personal mailbox and drop Subscribers. The original conversation
// context is preserved in the payload's `obo_origin_*` fields so the bot
// (and any downstream consumer) can still reason about where the message
// originated. We go through octo-lib's `NewPersonalMsgSendReq` builder so
// the PERSONAL DM authoritative-payload contract (Mininglamp-OSS#37) is
// preserved and the `tools/lint-personal-msgsendreq` invariant holds.
//
// What we do NOT do here:
//   - We do NOT call SendMessageWithResult (which would create a new
//     persisted message everyone sees). The Person-channel route +
//     NoPersist=1 gives the bot a one-shot copy via its existing
//     subscriber pipeline (the bot is the sole subscriber of its own
//     mailbox channel).
//   - We do NOT recompute permissions; checkOBO already ran when the bot
//     authored the message that's now bouncing, and inbound messages from
//     real users are by definition allowed in the channel they arrived in.
//
// PR#82 R6 P0 — The fan-out copy's FromUID is the GRANTOR uid (not the
// original sender). The v0 implementation used `FromUID=m.FromUID`
// (= the peer who sent the inbound, e.g. u_bob), so for DMs WuKongIM
// observed a (FromUID=u_bob, ChannelID=granteeBotUID) PERSONAL message
// and synced the conversation pair `u_bob ↔ granteeBot` to **u_bob's**
// client — leaking the persona-clone bot into bob's conversation list
// even though bob only ever spoke to admin. The whole point of "managed
// persona" is that bob sees ONLY admin as the counterparty; the bot is
// strictly behind admin's identity.
//
// The fix routes the fan-out copy as "admin (grantor) forwarding to the
// bot's own mailbox". WuKongIM then syncs the pair `admin ↔ granteeBot`
// only — which is semantically correct because admin owns the bot
// (admin is the grantor in the OBO grant row) and the bot is admin's
// own managed persona. Bob is no longer in either UID of the fan-out
// copy and therefore cannot see the bot at all. The bot still learns
// who actually spoke via `obo_origin_from_uid` in the payload.
package bot_api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/obopayload"
	"go.uber.org/zap"
)

// oboMessagesListen is the registered MessagesListener. Hot path: must be
// O(1) for messages in channels with no active grants. The early-out on
// the channel scope lookup achieves that — the JOIN returns 0 rows when
// neither obo_grants nor obo_scopes has matching data.
//
// Wired in BotAPI.Route via ba.ctx.AddMessagesListener. Test surface is
// the lower-level fanoutForMessage method.
//
// CONTENT-TYPE CONTRACT (YUJ-1356 / Mininglamp-OSS/octo-server#96 audit,
// 2026-05-19): this listener is intentionally CONTENT-TYPE-AGNOSTIC. We
// dispatch a fan-out copy for every inbound message regardless of
// payload `type`, because the persona-clone bot needs to observe the
// FULL conversation (text, image, voice, video, file, stickers, etc.)
// to act as a faithful replica. Any future "skip CMD / system messages"
// optimization MUST live at the upstream layer (webhook handleMessageNotify
// already gates Header.SyncOnce/NoPersist for that purpose) and MUST
// preserve fan-out for all real user content types. The
// TestFanout_ContentTypeAgnostic + TestFanout_OBOMessagesListen_BatchAllContentTypes
// pair in obo_fanout_content_type_test.go locks this contract in so a
// regression that quietly adds a type filter here surfaces at unit-test
// time instead of slipping into E2E (where it was first reported).
//
// If a deployment observes some content types triggering fan-out and
// others not (e.g. file messages silently dropped while text/image work),
// the audit checklist is:
//
//  1. WuKongIM webhook config (octo-deployment) — confirm the
//     `event.webhook.on=msg.notify` subscription delivers EVERY content
//     type. If WuKongIM is filtering at the source, no listener — fan-out
//     or otherwise — will ever see those payloads.
//  2. Header.SyncOnce / Header.NoPersist on the inbound — clients
//     should not set these on real chat content. handleMessageNotify
//     gates listener notification on both flags (cmd / ephemeral messages
//     do not reach listeners by design).
//  3. payload.__obo_processed__ marker — gate 3 in fanoutForMessage
//     short-circuits on this. Real user messages cannot set the marker
//     (it's stripped at /v1/message/send and rejected at /v1/bot/sendMessage).
func (ba *BotAPI) oboMessagesListen(messages []*config.MessageResp) {
	for _, m := range messages {
		ba.fanoutForMessage(m)
	}
}

// fanoutLookupChannelID normalizes the channel id we use to look up scope
// rows for an inbound listener message. The OBO scope contract stores
// `channel_id` in the GRANTOR's "what channel did I subscribe to" frame
// of reference (see grantorCanReadChannel / oboCreateScope), and for DMs
// that frame is the PEER uid — not the receiver's own uid.
//
// Listener messages, however, carry the WuKongIM-native view. For DMs,
// `m.ChannelID` is the receiver of the message (= grantor when fan-out is
// meant to trigger) and `m.FromUID` is the sender (= peer). Looking up
// scopes by `m.ChannelID` for DMs therefore searches for a row whose
// `channel_id = grantor`, which can never match the scope rows the
// grantor actually installed (those have `channel_id = peer`).
//
// PR#82 round-2 P1-B fix: for ChannelTypePerson we look up by
// `m.FromUID` (the peer in the "peer → grantor" direction the fan-out is
// designed to relay). For groups / community topics the channel id is
// already the grantor's frame of reference, so we pass it through.
//
// The "grantor → peer" direction (Alice typing on her own device) is
// caught two layers down — the lookup against `m.FromUID = grantor`
// finds no scope rows (the grantor's scopes have `channel_id = peer`),
// so fan-out is a no-op without even needing gate 2. Gate 2 still acts
// as defense-in-depth for any future code path that uses the original
// `m.ChannelID` lookup.
func fanoutLookupChannelID(m *config.MessageResp) string {
	if m.ChannelType == common.ChannelTypePerson.Uint8() {
		return m.FromUID
	}
	return m.ChannelID
}

// mergeGrantsDedup appends `extras` to `base`, dropping any grant whose ID
// already appears in `base`. Used by the DM fan-out path
// (Mininglamp-OSS/octo-server#161 / YUJ-1977) to merge the explicit-feeder
// result (`findActiveGrantsForChannel`) with the implicit-feeder result
// (`findGlobalGrantsForDM`). The two production queries are disjoint by
// construction — one INNER JOINs on `obo_scopes.enabled=1`, the other
// `NOT EXISTS`-filters on the same table — but the helper guards against a
// future store impl regression and against the test-fake aggregating both
// branches in a single method.
//
// Order is preserved: explicit-feeder grants come first (they are the
// authoritative dispatch shape for DMs the grantor explicitly opted into),
// implicit grants append after. This keeps the dispatch loop's per-message
// `grantorAccess` cache and the fan-out copy ordering deterministic across
// the two paths.
func mergeGrantsDedup(base, extras []*oboGrantModel) []*oboGrantModel {
	if len(extras) == 0 {
		return base
	}
	seen := make(map[int64]struct{}, len(base))
	for _, g := range base {
		if g != nil {
			seen[g.ID] = struct{}{}
		}
	}
	for _, g := range extras {
		if g == nil {
			continue
		}
		if _, dup := seen[g.ID]; dup {
			continue
		}
		seen[g.ID] = struct{}{}
		base = append(base, g)
	}
	return base
}

// fanoutForMessage is the single-message entry point used by tests AND by
// oboMessagesListen. Returns the number of copies dispatched so tests can
// assert without poking the dispatcher hook.
func (ba *BotAPI) fanoutForMessage(m *config.MessageResp) int {
	if m == nil || strings.TrimSpace(m.ChannelID) == "" {
		return 0
	}
	ba.Info("OBO fan-out: processing message",
		zap.String("from", m.FromUID),
		zap.String("channel_id", m.ChannelID),
		zap.Uint8("channel_type", m.ChannelType))

	// Gate 3 (cheapest, no DB): drop messages already minted by the OBO
	// dispatch path. Marker lives in payload (= message_extra). We don't
	// require all bot outbound to be JSON — if the payload isn't a JSON
	// object the marker can't be present, so we leave it as a no-op.
	if hasOBOProcessedMarker(m.Payload) {
		return 0
	}

	// PR#82 round-2 P1-B — normalize to the GRANTOR's frame of reference
	// before consulting scope rows. For DMs this means looking up by the
	// peer uid (= m.FromUID), not by m.ChannelID (which is the receiver /
	// grantor). For groups / topics the two are the same.
	lookupChannelID := fanoutLookupChannelID(m)
	// Defensive: a DM with an empty FromUID would translate to a blank
	// lookup key that could spuriously match scope rows for "channel_id =
	// ''" (none should exist in prod but the API allows the row). Treat
	// as no-op rather than risk a stray match.
	if lookupChannelID == "" {
		return 0
	}

	store := ba.oboStoreOrDefault()

	// YUJ-1465 / Mininglamp-OSS/octo-server#108 — OBO v2 fan-out
	// narrowing. v1 fanned out EVERY message in a scoped channel
	// (modulo the loop-protection gates) so the persona could observe
	// the full conversation. v2 narrows the trigger: a fan-out copy is
	// only minted when the inbound message explicitly summons the
	// grantor via `payload.mention.uids`, OR sets `mention.ais=1`
	// (`@所有 AI` — Plan X broadcast that summons every AI/bot), OR
	// sets `mention.humans=1` (`@所有人` — Plan X web client human
	// broadcast that fans out to every persona clone of in-channel
	// grantors; see YUJ-1709 / #125).
	//
	// IMPORTANT (Mininglamp-OSS/octo-server#142 review follow-up):
	// legacy `mention.all=1` (old WuKongIM `@所有人` shape) MUST NOT
	// trigger OBO bot / persona dispatch. The historical `all → ais`
	// rewrite chokepoint was removed by #143, but this fan-out gate
	// still treated `mention.all=1` as equivalent to the persona-
	// summon broadcast. With the rewrite gone, accepting `all=1` here
	// would re-create the implicit inference one layer deeper and
	// silently fan-out legacy `@所有人` traffic to every persona
	// clone. The gate therefore now requires an EXPLICIT `mention.ais=1`
	// or `mention.humans=1` signal; bare `mention.all=1` (with no ais /
	// humans / uids) is treated as plain traffic and short-circuits.
	//
	// PR#114 R3 (Jerry-Xin perf blocker) — the per-message mention gate
	// runs BEFORE the grant DB lookup for group-like channels, so plain
	// / @AI-only / @bot-only group traffic short-circuits without
	// touching MySQL.
	//
	// DM (Person) traffic does NOT run through the mention gate (DM
	// payloads carry no mention object) and still uses the unfiltered
	// scope-joined query.
	var (
		mentionedUIDs []string
		summonAll     bool
		grants        []*oboGrantModel
		err           error
	)
	if m.ChannelType == common.ChannelTypePerson.Uint8() {
		// DM path — unchanged, uses scope-joined query.
		grants, err = store.findActiveGrantsForChannel(lookupChannelID, m.ChannelType)
		// Mininglamp-OSS/octo-server#161 (YUJ-1977) — DM implicit-scope
		// fallback. `findActiveGrantsForChannel` INNER JOINs on
		// `obo_scopes` with `enabled=1`, so a grant with
		// `global_enabled=1` but ZERO scope rows produces an empty
		// result and the DM never fans out. PR#121 introduced the
		// equivalent path for group-shaped channels via
		// `findGlobalGrantsWithoutScope`; this branch closes the
		// symmetrical gap for Person channels.
		//
		// `m.ChannelID` under the listener's WuKongIM-native view is
		// the receiver of the inbound DM, which IS the grantor for
		// fan-out (the multi-grantor recipient filter further down
		// already pivots on this same identity). `lookupChannelID` is
		// the peer (= `m.FromUID`, after the P1-B normalization) and
		// is what an explicit scope row would have keyed under.
		//
		// The fallback is best-effort: a DB error on the implicit
		// feeder MUST NOT mask whatever the explicit feeder
		// successfully returned, so we log + carry on rather than
		// failing the whole lookup. Merging is dedup-safe even though
		// the two queries are disjoint by construction (one requires
		// an enabled scope row, the other requires no scope row at
		// all) — `mergeGrantsDedup` is belt-and-braces against future
		// store impls that relax that invariant.
		if err == nil && m.ChannelID != "" {
			globalGrants, gErr := store.findGlobalGrantsForDM(m.ChannelID, lookupChannelID)
			if gErr != nil {
				ba.Warn("OBO DM implicit-scope lookup failed",
					zap.String("grantor", m.ChannelID),
					zap.String("peer", lookupChannelID),
					zap.Error(gErr))
			} else if len(globalGrants) > 0 {
				grants = mergeGrantsDedup(grants, globalGrants)
			}
		}
	} else {
		// Group-like channels: gate on mention first. `mention.all` is
		// parsed (decoder still surfaces it for telemetry / future use)
		// but intentionally NOT consulted at the fan-out branch — see
		// the rationale block above and Mininglamp-OSS/octo-server#142.
		var (
			mentionAis    bool
			mentionHumans bool
		)
		mentionedUIDs, _, mentionAis, mentionHumans = decodeMentionGate(m.Payload)
		summonAll = mentionAis || mentionHumans
		if summonAll {
			// @所有 AI (mention.ais=1) or @所有人 (mention.humans=1) —
			// summon every grantor's persona in the channel. Use the
			// unfiltered channel-wide query (the only way to enumerate
			// "every grant in this channel" without knowing membership).
			grants, err = store.findActiveGrantsForChannel(lookupChannelID, m.ChannelType)
		} else if len(mentionedUIDs) > 0 {
			// Explicit @grantor(s) — use the UID-filtered query (PR#114
			// R3). The query restricts the system-wide scan to just the
			// mentioned UIDs and bypasses the channel-wide cache (PR#114
			// R4) so the filtered miss cannot poison the unfiltered hit.
			grants, err = store.findActiveGrantsForChannelByGrantors(lookupChannelID, m.ChannelType, mentionedUIDs)
		} else {
			// Plain / @AI-text-only / @bot-only / bare mention.all=1
			// (legacy `@所有人`, post-#142 + this PR no longer a fan-out
			// trigger) — no fan-out trigger for v2.
			return 0
		}
	}
	ba.Info("OBO fan-out: scope lookup result",
		zap.String("lookup_channel_id", lookupChannelID),
		zap.Int("scope_grants", len(grants)),
		zap.Bool("err", err != nil))
	if err != nil {
		ba.Error("OBO fan-out lookup failed",
			zap.String("lookup_channel_id", lookupChannelID),
			zap.String("channel_id", m.ChannelID),
			zap.Uint8("channel_type", m.ChannelType),
			zap.Error(err))
		return 0
	}

	// Implicit scope: for group-like channels (GROUP and CommunityTopic),
	// also find global_enabled grants whose grantor is a member of the
	// (parent) group AND whose bot is NOT a member, with no explicit scope
	// row for the channel. The store call collapses all three predicates
	// into a single JOIN — see findGlobalGrantsWithoutScope in obo_db.go
	// for the SQL. Pre-PR#121 this loop did one `grantorCanReadChannel`
	// query per global grant here AND one `userIsGroupMember(bot)` (Gate 4)
	// query per grant in the dispatch loop below; for a system with N
	// global grants every inbound group message paid 2*N per-message
	// round-trips. After PR#121 the implicit-scope feeder is a single SQL
	// statement and the dispatch loop skips both per-grant checks for
	// these rows (tracked via `implicitGrantIDs`). Explicit-scope grants
	// returned by findActiveGrantsForChannel still pay the per-grant
	// Gate 4 + TOCTOU re-check below — that set is bounded by the number
	// of scope rows installed for this specific channel and is never
	// large.
	//
	// PR#121 R9 (YUJ-1676 / Jerry-Xin + lml2468 blocking) — extended
	// from GROUP-only to also cover CommunityTopic. Without the topic
	// branch, a `mention.ais=1` / `mention.humans=1` topic message
	// from a non-grantor whose parent group has a global_enabled grant
	// on file (and bot NOT in the parent group) produced ZERO fan-out
	// copies — even though checkOBO already authorized the symmetric
	// reply path for topics (obo_check.go:105) and Gate 4 / send-
	// permission bypass / explicit-mention feeder all already supported
	// topics. The asymmetry meant the bot could authorize a reply but
	// never receive the message in the first place. For topics we
	// extract the parent group id (split on threadChannelIDSeparator,
	// mirroring grantorCanReadChannel) and pass it to the store as
	// `membershipGroupID`; the scope anti-join
	// keeps using the topic's own (channel_id, channel_type) so an
	// `enabled=0` row on the topic still wins.
	//
	// "admin is in any group → persona clone auto-covers it" UX without
	// requiring manual scope management per channel.
	implicitGrantIDs := map[int64]bool{}
	if isGroupLikeChannelType(m.ChannelType) {
		var membershipGroupID string
		switch m.ChannelType {
		case common.ChannelTypeGroup.Uint8():
			membershipGroupID = lookupChannelID
		case common.ChannelTypeCommunityTopic.Uint8():
			parts := strings.SplitN(lookupChannelID, threadChannelIDSeparator, 2)
			if len(parts) == 2 && parts[0] != "" {
				membershipGroupID = parts[0]
			}
			// Malformed thread id → leave empty; the feeder fail-closes
			// on an empty membership group id and returns no grants.
		}
		if membershipGroupID != "" {
			globalGrants, gErr := store.findGlobalGrantsWithoutScope(membershipGroupID, lookupChannelID, m.ChannelType)
			if gErr != nil {
				ba.Warn("OBO global-grant lookup failed",
					zap.String("channel_id", lookupChannelID),
					zap.String("membership_group", membershipGroupID),
					zap.Error(gErr))
			} else {
				// PR#121 R6 / B2 (Jerry-Xin + lml2468 2026-05-22 blocking)
				// — when only specific grantors were @-mentioned (i.e.
				// `mention.uids` is set and neither `mention.ais` nor
				// `mention.humans` are), the implicit-scope feeder MUST
				// be filtered to those uids before being merged with the
				// explicit-scope set above.
				//
				// Without this filter, a message that mentions only Alice
				// (`mention.uids = [alice_uid]`) would silently pull in
				// Bob's persona too — `findGlobalGrantsWithoutScope`
				// returns every global-enabled grant whose grantor is in
				// the group, with no awareness of the mention gate. That
				// directly violates the documented v2 mention contract:
				// `mention.uids` summons ONLY the mentioned grantor(s);
				// only `mention.ais=1` / `mention.humans=1` summon
				// everyone (mention.all=1 alone no longer summons —
				// see Mininglamp-OSS/octo-server#142 / #143 follow-up).
				//
				// The summonAll branch passes through unfiltered (matches
				// the explicit-path behavior — `@所有 AI` / `@所有人`
				// summons every grantor in the channel). The DM branch
				// never reaches this block (gated above on
				// isGroupLikeChannelType) and therefore needs no
				// symmetrical filter.
				var mentionFilter map[string]struct{}
				if !summonAll && len(mentionedUIDs) > 0 {
					mentionFilter = make(map[string]struct{}, len(mentionedUIDs))
					for _, u := range mentionedUIDs {
						mentionFilter[u] = struct{}{}
					}
				}
				// PR#114 R3 dedup — when the mention-filtered query above
				// already returned a grant that ALSO satisfies the implicit-
				// scope predicates, mark it as implicit (skip the per-grant
				// access re-check) rather than appending a duplicate row.
				// The same SQL JOIN that proved implicit-scope ran inside
				// findGlobalGrantsWithoutScope, so the access re-check is
				// redundant for these grants and the test surface pins that
				// invariant explicitly.
				alreadyHave := map[int64]bool{}
				for _, g := range grants {
					alreadyHave[g.ID] = true
				}
				for _, g := range globalGrants {
					if mentionFilter != nil {
						if _, ok := mentionFilter[g.GrantorUID]; !ok {
							// Grantor not mentioned — implicit-scope
							// fan-out must NOT summon them. (B2.)
							continue
						}
					}
					ba.Info("OBO implicit-scope: SQL-validated grant, auto-including",
						zap.String("grantor", g.GrantorUID),
						zap.String("bot", g.GranteeBotUID),
						zap.String("channel_id", lookupChannelID),
						zap.String("membership_group", membershipGroupID))
					implicitGrantIDs[g.ID] = true
					if alreadyHave[g.ID] {
						continue
					}
					grants = append(grants, g)
				}
			}
		}
	}

	if len(grants) == 0 {
		return 0
	}

	// PR#82 round-2 P1-A — per-call cache for the grantor channel-access
	// re-check. Multiple active grants for the same (channel, grantor)
	// pair are rare in v0 (uk_grantor_grantee makes it (grantor, bot)),
	// but for any given inbound message we batch the check so we don't
	// hit the DB twice for the same grantor in one listener invocation.
	// The boolean is the "can read" answer; presence in the map means
	// "answer is final, do not re-query for this message".
	grantorAccess := map[string]bool{}

	dispatched := 0
	for _, g := range grants {
		// Gate 1: bot self-sent → don't replay back to the same bot.
		// (The bot is allowed to send messages to itself in principle, but
		// the OBO copy of a bot's own send would be a strict loop.)
		if g.GranteeBotUID == m.FromUID {
			continue
		}
		// Gate 2: grantor sent this message from their real device →
		// don't fan to the grantor's bot. Without this gate the bot
		// would see every word the grantor types and potentially reply.
		if g.GrantorUID == m.FromUID {
			continue
		}
		// Gate 4: bot is already a member of this group → it receives
		// messages directly via the WuKongIM subscriber pipeline, so a
		// fan-out copy would cause duplicate processing (double typing,
		// double reply, identity confusion on @AI/@bot). The adapter-
		// side mention logic handles identity switching (“@所有人 →
		// respond as grantor”) for the direct-receipt path.
		// Applies to GROUP and CommunityTopic channels; DM fan-out is
		// the sole delivery path (bot is never a “member” of the peer’s
		// DM). For CommunityTopic the membership we care about is the
		// PARENT group: a topic message is delivered to every parent-
		// group member, so a bot that is already in the parent group
		// gets the message normally and the OBO copy would be a strict
		// duplicate (same bug Gate 4 prevents for ChannelTypeGroup).
		// Parent-group extraction mirrors grantorCanReadChannel /
		// send.go — split on threadChannelIDSeparator.
		//
		// PR#121 perf: implicit-scope grants (sourced from
		// findGlobalGrantsWithoutScope) have ALREADY been filtered by the
		// SQL JOIN to guarantee `gm_bot.uid IS NULL`, so the per-grant
		// `userIsGroupMember(bot)` query here is redundant for them. We
		// keep it for explicit-scope grants (sourced from
		// findActiveGrantsForChannel) where the bot-membership status is
		// not part of the feeder's filter. PR#121 R9 (YUJ-1676) extended
		// the implicit-scope feeder to CommunityTopic as well, so the
		// implicit-grant skip below now correctly fires for both Group
		// and CommunityTopic when the SQL JOIN already proved the bot
		// is NOT a (parent-)group member.
		if !implicitGrantIDs[g.ID] {
			var membershipGroup string
			switch m.ChannelType {
			case common.ChannelTypeGroup.Uint8():
				membershipGroup = lookupChannelID
			case common.ChannelTypeCommunityTopic.Uint8():
				parts := strings.SplitN(lookupChannelID, threadChannelIDSeparator, 2)
				if len(parts) == 2 && parts[0] != "" {
					membershipGroup = parts[0]
				}
				// Malformed thread id → leave membershipGroup empty
				// and skip the check; downstream gates (TOCTOU
				// re-check via grantorCanReadChannel) still fail-
				// closed on the same malformed id.
			}
			if membershipGroup != "" {
				isBotInGroup, gErr := ba.userIsGroupMember(g.GranteeBotUID, membershipGroup)
				if gErr != nil {
					// PR#121 R9 (YUJ-1676) fail-closed. Pre-R9 a
					// userIsGroupMember error fell through to the
					// dispatch path, which caused a duplicate fan-out
					// whenever the bot ACTUALLY was a parent-group
					// member but the membership query transiently
					// errored (DB blip, deadlock, …). The bot would
					// then receive the message BOTH directly via the
					// WuKongIM subscriber pipeline AND as an OBO
					// fan-out copy — the exact double-processing bug
					// Gate 4 is meant to prevent. Log and skip the
					// grant: a missed fan-out copy on a transient
					// error is strictly safer than a guaranteed
					// duplicate, and the next inbound message in the
					// channel will retry naturally.
					ba.Warn("OBO fan-out gate 4: bot membership check errored, skipping grant (fail-closed)",
						zap.String("bot", g.GranteeBotUID),
						zap.String("channel_id", lookupChannelID),
						zap.String("membership_group", membershipGroup),
						zap.Uint8("channel_type", m.ChannelType),
						zap.Error(gErr))
					continue
				}
				if isBotInGroup {
					ba.Info("OBO fan-out gate 4: bot is parent-group member, skipping fan-out (adapter handles directly)",
						zap.String("bot", g.GranteeBotUID),
						zap.String("channel_id", lookupChannelID),
						zap.String("membership_group", membershipGroup),
						zap.Uint8("channel_type", m.ChannelType))
					continue
				}
			}
		}
		// PR#82 round-3 P1 — Multi-grantor DM recipient filter. For
		// DMs, findActiveGrantsForChannel is keyed by the peer uid
		// (= m.FromUID after the P1-B lookup normalization), so it
		// returns EVERY grantor who installed a `(peer=this peer)`
		// scope — not just the grantor who is the actual recipient
		// of this specific message. Without this filter, a Bob →
		// Alice DM would also fan out to Carol's clone bot if Carol
		// also scoped Bob: findActiveGrantsForChannel(Bob, Person)
		// returns both Alice's grant and Carol's grant, and the
		// per-grant access re-check below confirms Carol can read
		// DMs with Bob (they're friends) — so without this gate the
		// message silently leaks across users.
		//
		// The actual DM recipient is m.ChannelID under the listener's
		// WuKongIM-native view (DM ChannelID = receiver, FromUID =
		// sender). Drop any grant whose grantor is NOT that receiver.
		// For groups / community topics the lookup is already 1:1
		// with the conversation, so this filter is a DM-only concern.
		if m.ChannelType == common.ChannelTypePerson.Uint8() && m.ChannelID != g.GrantorUID {
			continue
		}
		// PR#82 round-2 P1-A — TOCTOU close-out on the fan-out hot path.
		// Even though the scope row exists, the grantor may have lost
		// access to the channel since (kicked from group, un-friended
		// peer, left parent group of a thread). Skipping the dispatch
		// keeps the bot from continuing to harvest channels the grantor
		// no longer has eyes on. DB error → fail-closed (skip this
		// grant, log, continue with the remaining ones; we never want a
		// transient DB blip to leak otherwise-denied traffic).
		//
		// PR#121 perf: implicit-scope grants are skipped here. Their SQL
		// feeder INNER JOIN'd `group_member` on the grantor exactly
		// 1ms ago, so a redundant Go-level recheck would just pay for
		// the same index lookup twice without changing the answer. The
		// TOCTOU window between the SQL JOIN and the dispatch is
		// negligible compared to the per-message cost we are removing.
		if !implicitGrantIDs[g.ID] {
			canRead, cached := grantorAccess[g.GrantorUID]
			if !cached {
				ok, err := ba.grantorCanReadChannel(g.GrantorUID, lookupChannelID, m.ChannelType)
				if err != nil {
					ba.Error("OBO fan-out grantor channel-access re-check failed",
						zap.String("grantor", g.GrantorUID),
						zap.String("lookup_channel_id", lookupChannelID),
						zap.Uint8("channel_type", m.ChannelType),
						zap.Error(err))
					grantorAccess[g.GrantorUID] = false
					continue
				}
				canRead = ok
				grantorAccess[g.GrantorUID] = canRead
			}
			if !canRead {
				ba.Warn("OBO fan-out skipped: grantor no longer has read access",
					zap.String("grantor", g.GrantorUID),
					zap.String("grantee_bot", g.GranteeBotUID),
					zap.String("lookup_channel_id", lookupChannelID),
					zap.Uint8("channel_type", m.ChannelType))
				continue
			}
		}
		// Build a fan-out copy addressed to the bot's own Person mailbox.
		// NoPersist=1 + SyncOnce=1 keep delivery silent and the bot is the
		// only subscriber of its own channel, so no real user sees the
		// copy — even though Subscribers is now omitted (see PR#82 R5 P0
		// in buildFanoutCopyReq for why both can't be set).
		//
		// PR#82 R6 P0 — FromUID is the GRANTOR (not the original sender)
		// so WuKongIM does NOT surface a `<peer> ↔ <granteeBot>`
		// conversation entry on the original sender's client. See the
		// package-level comment for the full rationale.
		//
		// YUJ-1465 — also pass the grant + resolved display names so the
		// v2 payload can carry `obo_grantor_uid` / `obo_grantor_name` /
		// `obo_respond_as` + a natural-language `obo_system_hint`
		// composed from persona_prompt and the channel/sender context.
		grantorName := ba.oboResolveDisplayName(g.GrantorUID)
		senderName := ba.oboResolveDisplayName(m.FromUID)
		var groupName string
		if m.ChannelType != common.ChannelTypePerson.Uint8() {
			groupName = ba.oboResolveGroupName(m.ChannelID, m.ChannelType)
		}
		copyReq := buildFanoutCopyReq(m, g, grantorName, senderName, groupName)
		if err := ba.dispatchFanout(copyReq); err != nil {
			ba.Error("OBO fan-out dispatch failed",
				zap.String("grantee_bot", g.GranteeBotUID),
				zap.String("channel_id", m.ChannelID),
				zap.Error(err))
			continue
		}
		// YUJ-1424 — also enqueue the synthetic event into the grantee
		// bot's event queue so /v1/bot/events serves it. WuKongIM's
		// webhook.handleMessageNotify drops messages flagged
		// NoPersist=1 / SyncOnce=1 (the very flags we set on the
		// fan-out copy), so the bot would otherwise never observe the
		// message even though dispatch succeeded. Best-effort: enqueue
		// failure does NOT roll back the dispatch (the copy is already
		// in WuKongIM).
		enqueueMsg := fanoutCopyToMessageResp(copyReq)
		if err := ba.enqueueFanoutBotEvent(g.GranteeBotUID, enqueueMsg); err != nil {
			ba.Warn("OBO fan-out bot-event enqueue failed",
				zap.String("grantee_bot", g.GranteeBotUID),
				zap.Error(err))
		}
		dispatched++
	}
	return dispatched
}

// buildFanoutCopyReq turns an inbound MessageResp into a one-shot copy
// addressed to `granteeBotUID`'s PERSONAL mailbox. The payload is augmented
// with `obo_fanout=true` plus `obo_origin_*` fields that pin down the
// original conversation (the marker is informational; loop protection
// uses `__obo_processed__` set by the bot's own outbound).
//
// Contract enforcement (PR#82 R5 P0): the returned MsgSendReq sets exactly
// ONE of `ChannelID` / `Subscribers` (channel_id mode), never both —
// WuKongIM `/message/send` rejects requests carrying both with
// `channelId和subscribers不能同时存在`. We route via the bot's own Person
// channel so:
//
//   - ChannelID    = granteeBotUID (bot's own mailbox)
//   - ChannelType  = Person (set by NewPersonalMsgSendReq)
//   - FromUID      = grantorUID (NOT the original sender — see below)
//   - Subscribers  = nil (omitted)
//
// FromUID rationale (PR#82 R6 P0): using `m.FromUID` (the original sender,
// e.g. u_bob in a DM to admin) caused WuKongIM to sync a `<sender> ↔
// <granteeBot>` conversation entry to the original sender's client,
// leaking the persona-clone bot into bob's conversation list. Setting
// FromUID to the GRANTOR fixes that — the only conversation entry now
// shows admin ↔ granteeBot, which is fine because admin already owns
// the bot (granted the OBO row that birthed the fan-out). The bot still
// learns the real speaker via `obo_origin_from_uid` in the payload, so
// the adapter can address its reply to the right user.
//
// NoPersist=1 + SyncOnce=1 keep the copy ephemeral so we don't bump red
// dots or update conversation positions for any real user.
//
// senderSpaceID is intentionally "" — the fan-out is an internal control
// channel, not a user-authored DM. The builder will strip any
// payload-supplied `space_id` (fail-closed per Mininglamp-OSS/octo-server
// PR#35 R3). Downstream consumers must read `obo_origin_*` for routing
// context, not `space_id`.
func buildFanoutCopyReq(m *config.MessageResp, g *oboGrantModel, grantorName, senderName, groupName string) *config.MsgSendReq {
	payload := map[string]interface{}{}
	if len(m.Payload) > 0 {
		// Best-effort decode. If the original is a non-JSON payload we
		// fall back to wrapping the bytes so the bot still sees the
		// original content under a known key.
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			payload = map[string]interface{}{
				"raw":  string(m.Payload),
				"type": 0,
			}
		}
	}
	payload["obo_fanout"] = true
	payload["obo_origin_channel_id"] = m.ChannelID
	payload["obo_origin_channel_type"] = m.ChannelType
	payload["obo_origin_from_uid"] = m.FromUID
	if m.MessageIDStr != "" {
		// YUJ-1465 — v2 canonical key per Mininglamp-OSS/octo-server#108.
		// The legacy `obo_origin_message_idstr` key is preserved for
		// backward compatibility with adapter builds shipped before
		// v2 landed; v2-aware adapters should read `obo_origin_message_id`.
		payload["obo_origin_message_id"] = m.MessageIDStr
		payload["obo_origin_message_idstr"] = m.MessageIDStr
	}

	// YUJ-1465 — v2 OBO fields. The adapter routes the bot's reply back
	// to `obo_origin_channel_id` with `fromUID = obo_grantor_uid`; the
	// `obo_respond_as` field is a redundant, explicit signal so the
	// adapter never has to infer "which identity should sign this
	// reply" from the multiple `*_uid` fields above.
	resolvedGrantorName := grantorName
	if resolvedGrantorName == "" {
		// Fall back to the bare uid so the hint string never reads
		// "你正在以「」的分身身份运作" — that would be a worse UX than the
		// raw uid (which at least uniquely identifies the persona).
		resolvedGrantorName = g.GrantorUID
	}
	payload["obo_grantor_uid"] = g.GrantorUID
	payload["obo_grantor_name"] = resolvedGrantorName
	payload["obo_respond_as"] = g.GrantorUID

	// Natural-language system hint. Composed from the resolved names
	// (with safe fallbacks to raw uids / channel ids) and optionally
	// extended with the grant's persona_prompt. Per the
	// octo-server#108 spec the hint is Chinese; the prompt is
	// appended verbatim so grantors can author in any language.
	resolvedSenderName := senderName
	if resolvedSenderName == "" {
		resolvedSenderName = m.FromUID
	}
	var hint string
	if m.ChannelType == common.ChannelTypePerson.Uint8() {
		// DM origin — no group name, peer is the sender. Mirrors the
		// group hint shape so adapters don't need a branch.
		hint = fmt.Sprintf(
			"你正在以「%s」的分身身份运作。这条消息来自与「%s」的私聊。请以 %s 的身份回复。",
			resolvedGrantorName, resolvedSenderName, resolvedGrantorName,
		)
	} else {
		resolvedGroupName := groupName
		if resolvedGroupName == "" {
			resolvedGroupName = m.ChannelID
		}
		hint = fmt.Sprintf(
			"你正在以「%s」的分身身份运作。这条消息来自群「%s」，发送者是 %s。请以 %s 的身份回复。",
			resolvedGrantorName, resolvedGroupName, resolvedSenderName, resolvedGrantorName,
		)
	}
	if prompt := strings.TrimSpace(g.PersonaPrompt); prompt != "" {
		// Two-newline separator so an adapter that surfaces the hint as
		// a system message keeps the auto and grantor-authored
		// sections visually distinct.
		hint = hint + "\n\n" + prompt
	}
	payload["obo_system_hint"] = hint

	// PERSONAL DM dispatch — must go through the octo-lib builder so
	// payload.space_id authoritative semantics + the channel_id/subscribers
	// mutex are uniformly applied. Subscribers omitted intentionally; see
	// the contract block in the function doc. FromUID is the grantor (NOT
	// m.FromUID) — see PR#82 R6 P0 rationale above.
	return config.NewPersonalMsgSendReq(
		g.GranteeBotUID,
		g.GrantorUID,
		payload,
		"", // no authoritative sender Space for an internal control copy
		config.PersonalMsgOptions{
			Header: config.MsgHeader{
				NoPersist: 1, // silent copy — doesn't enter normal storage
				RedDot:    0,
				SyncOnce:  1,
			},
			// Subscribers intentionally OMITTED — WuKongIM rejects when
			// channel_id AND subscribers are both set.
		},
	)
}

// decodeMentionGate — YUJ-1465 / YUJ-1538 / YUJ-1709 / #142 follow-up.
// Pulls `mention.uids`, `mention.all`, `mention.ais`, and
// `mention.humans` off the raw payload. Returns:
//   - mentionedUIDs: slice of distinct UIDs (sorted ascending for
//     stable IN(...) bind ordering); empty when payload has no
//     mention.uids or it is empty / malformed.
//   - mentionAll: true iff `mention.all` is the integer 1 (numeric
//     form) OR boolean true. The legacy WuKongIM clients emit
//     `mention.all=1` for `@所有人` (PR#114 R3 + YUJ-1538). After
//     Mininglamp-OSS/octo-server#142 / #143 this flag is parsed for
//     telemetry / future use but is NOT consumed by the OBO fan-out
//     gate — see fanoutForMessage above for the rationale: the
//     `all → ais` rewrite chokepoint was removed by #142, and
//     accepting `all=1` here would re-create the implicit inference
//     one layer deeper. Callers MUST gate fan-out on
//     `mentionAis || mentionHumans`.
//   - mentionAis: true iff `mention.ais` is truthy (numeric 1 or
//     boolean true). `@所有 AI` (Plan X / YUJ-1389) — summons every
//     bot/persona clone in the channel.
//   - mentionHumans: true iff `mention.humans` is truthy. The Plan X
//     web client emits `mention.humans=1` for `@所有人` (YUJ-1709 /
//     #125) and never sets `mention.all`. The OBO fan-out gate
//     treats this as a persona-clone summon.
//
// Returns (nil, false, false, false) on any decode error or absent
// `mention` field. The fan-out narrowing gate treats (no uids, no
// ais, no humans) as "no summon".
func decodeMentionGate(payload []byte) ([]string, bool, bool, bool) {
	if len(payload) == 0 {
		return nil, false, false, false
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, false, false, false
	}
	raw, ok := decoded["mention"]
	if !ok || raw == nil {
		return nil, false, false, false
	}
	mentionMap, ok := raw.(map[string]interface{})
	if !ok {
		return nil, false, false, false
	}
	// truthy1 — accept both numeric 1 and boolean true. Mirrors the
	// two shapes real WuKongIM / Plan X clients emit. Strings, null,
	// missing, and any other type all decode to false (consistent with
	// the YUJ-1538 contract pinned by TestDecodeMentionGate_*).
	truthy1 := func(v interface{}) bool {
		switch t := v.(type) {
		case float64:
			return t == 1
		case int:
			return t == 1
		case bool:
			return t
		}
		return false
	}
	// mention.all (legacy WuKongIM clients) — YUJ-1538. Parsed but
	// NOT consumed by the OBO fan-out gate (see #142 / #143).
	var all bool
	if v, ok := mentionMap["all"]; ok && v != nil {
		all = truthy1(v)
	}
	// mention.ais (Plan X / YUJ-1389) — `@所有 AI`. Triggers the
	// unfiltered fan-out branch in fanoutForMessage.
	var ais bool
	if v, ok := mentionMap["ais"]; ok && v != nil {
		ais = truthy1(v)
	}
	// mention.humans (Plan X web client `@所有人`) — YUJ-1709 / #125.
	// Plan X never co-emits `mention.all`, so without this branch the
	// fan-out gate early-returns and any grantee bot for a grantor in
	// the channel silently misses the OBO DM. Treated as a persona-
	// clone summon at the gate.
	var humans bool
	if v, ok := mentionMap["humans"]; ok && v != nil {
		humans = truthy1(v)
	}
	// mention.uids — distinct, trim whitespace, drop empties.
	var uids []string
	if uidsRaw, ok := mentionMap["uids"]; ok && uidsRaw != nil {
		if uidsSlice, ok := uidsRaw.([]interface{}); ok {
			seen := map[string]struct{}{}
			for _, v := range uidsSlice {
				if s, ok := v.(string); ok {
					s = strings.TrimSpace(s)
					if s == "" {
						continue
					}
					if _, dup := seen[s]; dup {
						continue
					}
					seen[s] = struct{}{}
					uids = append(uids, s)
				}
			}
		}
	}
	// Sort for stable IN(...) bind ordering (lets tests pin a
	// deterministic call shape).
	if len(uids) > 1 {
		// Use a tiny manual sort to avoid pulling in sort just for this.
		for i := 1; i < len(uids); i++ {
			for j := i; j > 0 && uids[j-1] > uids[j]; j-- {
				uids[j-1], uids[j] = uids[j], uids[j-1]
			}
		}
	}
	return uids, all, ais, humans
}

// oboResolveDisplayName — YUJ-1465. Resolves a uid to a human display
// name for the `obo_system_hint` composition. Returns "" when the uid
// is unknown so the caller can fall back to the bare uid. Production
// path runs a covering-index query on `user.name`; the
// `oboDisplayNameLookup` test seam lets unit tests inject a
// deterministic map without standing up MySQL.
func (ba *BotAPI) oboResolveDisplayName(uid string) string {
	if uid == "" {
		return ""
	}
	if ba.oboDisplayNameLookup != nil {
		return ba.oboDisplayNameLookup(uid)
	}
	if ba.db == nil || ba.db.session == nil {
		return ""
	}
	var name string
	err := ba.db.session.SelectBySql(
		"SELECT COALESCE(name,'') FROM `user` WHERE uid=? LIMIT 1", uid,
	).LoadOne(&name)
	if err != nil {
		// Best-effort: the hint falls back to the raw uid on any DB
		// error. We deliberately do not log at error level here — name
		// resolution failures are common (e.g. for synthetic system
		// uids) and would otherwise spam the listener log per inbound
		// message in a busy channel.
		return ""
	}
	return name
}

// oboResolveGroupName — YUJ-1465. Resolves a group / community-topic
// channel id to its human group name for `obo_system_hint`. Returns ""
// on any failure / unknown channel; the caller falls back to the bare
// channel id. Community topic channel ids decompose into
// `<parent_group_no>____<short_id>` — we resolve the parent group's
// name in that case so the hint reads sensibly ("群「<parent>」").
func (ba *BotAPI) oboResolveGroupName(channelID string, channelType uint8) string {
	if channelID == "" {
		return ""
	}
	if ba.oboGroupNameLookup != nil {
		return ba.oboGroupNameLookup(channelID, channelType)
	}
	if ba.db == nil || ba.db.session == nil {
		return ""
	}
	lookupGroupNo := channelID
	if channelType == common.ChannelTypeCommunityTopic.Uint8() {
		parts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
		if len(parts) != 2 || parts[0] == "" {
			return ""
		}
		lookupGroupNo = parts[0]
	}
	var name string
	err := ba.db.session.SelectBySql(
		"SELECT COALESCE(name,'') FROM `group` WHERE group_no=? LIMIT 1", lookupGroupNo,
	).LoadOne(&name)
	if err != nil {
		return ""
	}
	return name
}

// dispatchFanout sends the fan-out copy. Test override is consulted first
// so unit tests can capture the request without needing a live WuKongIM.
// Production path goes through ctx.SendMessage (NOT SendMessageWithResult
// — we don't need the result and the simpler call avoids a wait).
func (ba *BotAPI) dispatchFanout(req *config.MsgSendReq) error {
	if ba.oboFanoutDispatch != nil {
		return ba.oboFanoutDispatch(req)
	}
	if ba.ctx == nil {
		// Defensive: shouldn't happen in prod (Route is called with a real
		// ctx) but guards against unit tests that wire BotAPI piecemeal.
		return nil
	}
	return ba.ctx.SendMessage(req)
}

// fanoutCopyToMessageResp converts a dispatched MsgSendReq into the
// MessageResp shape /v1/bot/events serves. Used by enqueueFanoutBotEvent
// so the bot observes a synthetic event identical to what the listener
// path would have produced for a normally-persisted message.
func fanoutCopyToMessageResp(req *config.MsgSendReq) *config.MessageResp {
	if req == nil {
		return nil
	}
	return &config.MessageResp{
		FromUID:     req.FromUID,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		Payload:     req.Payload,
		Header:      req.Header,
	}
}

// enqueueFanoutBotEvent — YUJ-1424 — pushes the synthetic event into
// the grantee bot's event queue. WuKongIM's webhook.handleMessageNotify
// drops NoPersist=1 / SyncOnce=1 messages, so without this direct
// enqueue the fan-out copy would dispatch successfully but never reach
// /v1/bot/events. Test seam (oboFanoutBotEnqueue) consulted first;
// production path goes through ba.robotService.EnqueueBotEvent.
func (ba *BotAPI) enqueueFanoutBotEvent(robotID string, message *config.MessageResp) error {
	if ba.oboFanoutBotEnqueue != nil {
		return ba.oboFanoutBotEnqueue(robotID, message)
	}
	if ba.robotService == nil {
		// Defensive: tests that don't wire robotService nor the
		// enqueue seam see a no-op, which mirrors the
		// "production with Redis disabled" degraded mode.
		return nil
	}
	return ba.robotService.EnqueueBotEvent(robotID, message)
}

// oboProcessedMarkerKey is the JSON payload key set by sendMessage on
// every OBO-authorized send so the fan-out listener can short-circuit
// gate 3 without re-querying. The double-underscore prefix marks it as
// part of the reserved `__obo_*` namespace that every user-message and
// /v1/bot/sendMessage ingress strips/rejects on client payloads —
// making the marker server-only state that bots OR users cannot forge
// or suppress through the public APIs.
//
// Source of truth for both the prefix and the marker key lives in
// pkg/obopayload so the bot API (reject), the user message API
// (strip), and the fan-out listener (gate-3 check) cannot drift.
// (PR#82 review #2 P1-2 + R8 user-ingress hardening.)
const oboProcessedMarkerKey = obopayload.ProcessedMarkerKey

// oboReservedKeyPrefix is the reserved-namespace prefix for server-only
// OBO payload fields. Inbound payloads containing keys with this prefix
// are rejected (bot API) or stripped (user message API) so the gate-3
// marker — and any future server-only OBO field — cannot be
// impersonated by a client.
const oboReservedKeyPrefix = obopayload.ReservedKeyPrefix

// hasOBOProcessedMarker — Gate 3. Returns true iff the payload decodes as
// a JSON object containing `oboProcessedMarkerKey: true`. Non-JSON /
// non-bool values are treated as absent so we err on the side of fanning
// out.
//
// PR#82 R8 perf nit (Jerry-Xin): the cheap pre-check uses bytes.Contains
// on the raw payload instead of the previous strings.Contains(string(...))
// which forced an extra allocation on every inbound message (the vast
// majority of which do not carry the marker at all). Both the pre-check
// and the full decode live in pkg/obopayload now so the user-ingress
// strip and the listener's gate-3 cannot disagree about what "marker
// present" means.
func hasOBOProcessedMarker(payload []byte) bool {
	return obopayload.HasProcessedMarker(payload)
}

// payloadHasReservedOBOKey reports whether any top-level key in the
// JSON-decoded `payload` map starts with the reserved `__obo_` prefix.
// Used by /v1/bot/sendMessage to reject inbound client payloads that
// would attempt to spoof a server-only OBO marker (gate-3 bypass).
func payloadHasReservedOBOKey(payload map[string]interface{}) bool {
	return obopayload.HasReservedKey(payload)
}
