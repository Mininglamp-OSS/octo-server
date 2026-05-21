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
	"sort"
	"strings"
	"time"

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

// fanoutForMessage is the single-message entry point used by tests AND by
// oboMessagesListen. Returns the number of copies dispatched so tests can
// assert without poking the dispatcher hook.
func (ba *BotAPI) fanoutForMessage(m *config.MessageResp) int {
	if m == nil || strings.TrimSpace(m.ChannelID) == "" {
		return 0
	}

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
	isGroupLike := m.ChannelType != common.ChannelTypePerson.Uint8()

	// PR#114 R3 (Jerry-Xin perf blocker, 2026-05-21) — mention gate runs
	// BEFORE the grant DB lookup for group-like channels. The previous
	// shape called `findActiveGrantsForChannel` first, which for groups
	// loads EVERY active+global_enabled grant system-wide with no
	// channel filter — so every ordinary group message (plain text,
	// @AI only, @bot, etc.) paid a full obo_grants scan even though
	// the per-message v2 narrowing gate (decoded a few lines below)
	// was going to reject them anyway.
	//
	// New shape for group-like channels:
	//
	//  1. Decode mentions ONCE up-front (cheap, in-memory JSON parse).
	//  2. If neither `mention.all` nor any `mention.uids` is set →
	//     EARLY RETURN. No DB query, no Redis hit beyond the cache
	//     short-circuit that channelCacheSaysNone already provides.
	//  3. For `mention.uids` (explicit @grantor): filter the grant
	//     query at the DB layer via `findActiveGrantsForChannelByGrantors`
	//     so we never load grants for OTHER grantors who weren't
	//     mentioned. uk_grantor_grantee guarantees one row per grantor,
	//     so the query returns at most `len(mention.uids)` rows.
	//  4. For `mention.all` (@所有人): the full grant scan is
	//     UNAVOIDABLE because every grantor in the group is implicitly
	//     mentioned and we don't know the membership at this layer.
	//     This is acceptable because `@所有人` is rare — operators
	//     restrict its use to admins / announcements — so the
	//     occasional full scan is bounded. The per-grant
	//     `grantorCanReadChannel` re-check below still drops grantors
	//     who aren't actually in the group.
	//
	// DM (Person) path stays unchanged: DM payloads carry no mention
	// metadata, so we still call `findActiveGrantsForChannel` first
	// and rely on the JOIN against the (per-peer) scope row to narrow.
	mentioned, mentionAll := decodeMentionGate(m.Payload)

	var grants []*oboGrantModel
	var err error
	if isGroupLike {
		// Mention gate first — refuse to touch MySQL for plain / @AI /
		// @bot traffic. This is THE perf fix Jerry-Xin flagged on the
		// PR#114 review: without it every group message went through a
		// full `obo_grants` scan.
		if !mentionAll && len(mentioned) == 0 {
			return 0
		}
		if mentionAll {
			// @所有人 broadcast — every grantor is implicitly mentioned.
			// The unfiltered scan is unavoidable here (we don't know
			// who is in the group from this layer) but @所有人 is rare
			// in practice and the alternative — fetching group
			// membership just to filter the IN list — is more work
			// than the scan saves.
			grants, err = store.findActiveGrantsForChannel(lookupChannelID, m.ChannelType)
		} else {
			// Explicit @grantor(s) — filter at the DB layer so we
			// never load grants for un-mentioned grantors. Collect
			// the set into a slice in deterministic order so the
			// IN(...) placeholder set is stable across calls (helps
			// query-plan caching at the MySQL layer too).
			grantorUIDs := make([]string, 0, len(mentioned))
			for uid := range mentioned {
				grantorUIDs = append(grantorUIDs, uid)
			}
			// Stable ordering — `range` on a map is unordered. Use a
			// simple sort so identical mention sets produce identical
			// query bind shapes.
			sort.Strings(grantorUIDs)
			grants, err = store.findActiveGrantsForChannelByGrantors(
				lookupChannelID, m.ChannelType, grantorUIDs,
			)
		}
	} else {
		grants, err = store.findActiveGrantsForChannel(lookupChannelID, m.ChannelType)
	}
	if err != nil {
		ba.Error("OBO fan-out lookup failed",
			zap.String("lookup_channel_id", lookupChannelID),
			zap.String("channel_id", m.ChannelID),
			zap.Uint8("channel_type", m.ChannelType),
			zap.Error(err))
		return 0
	}
	if len(grants) == 0 {
		return 0
	}

	// YUJ-1465 / Mininglamp-OSS/octo-server#108 — OBO v2 fan-out
	// narrowing. v1 fanned out EVERY message in a scoped channel
	// (modulo the loop-protection gates) so the persona could observe
	// the full conversation. v2 narrows the trigger: a fan-out copy is
	// only minted when the inbound message explicitly summons the
	// grantor via `payload.mention.uids` — OR (YUJ-1538) when the
	// message is a `@所有人` broadcast (`payload.mention.all=1`), which
	// by spec is the strongest possible "you specifically were
	// addressed" signal. @AI-only / @bot / plain (no mention) traffic
	// still does NOT trigger the persona.
	//
	// PR#114 R3 — for group-like channels the mention gate has already
	// run UP-FRONT (above) and either early-returned or narrowed the
	// grant query by the mentioned UIDs. The per-grant check inside
	// the loop below remains as a belt-and-suspenders verification —
	// the DB filter could in principle drop rows but mentionAll uses
	// the unfiltered scan, and we still want to verify each surviving
	// grantor was actually summoned. For DMs the mention set is
	// always empty (DM payloads carry no mention), but the DM-only
	// "implicitly mentioned" branch below preserves the v2 contract.

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
		// YUJ-1465 / Mininglamp-OSS/octo-server#108 — OBO v2 fan-out
		// narrowing gate. The grantor MUST be explicitly mentioned in
		// `payload.mention.uids` for this message; @AI / @bot / plain
		// traffic does NOT summon the persona. Mention set was decoded
		// once at the top of fanoutForMessage; map lookup is O(1).
		//
		// YUJ-1538 — `@所有人` (`mention.all=1`) ALSO counts as
		// "grantor was summoned". Real WuKongIM `@所有人` payloads
		// commonly carry `mention.all=1` without re-listing every
		// group member in `mention.uids`, so without this branch a
		// broadcast in a group with an active persona grant would
		// silently never fan out.
		//
		// DM-only special case: a DM is a 1:1 conversation in which the
		// grantor is the implicit recipient (m.ChannelID == grantor for
		// DMs after the round-3 P1 filter above). DM payloads in
		// practice carry no mention.uids array, so requiring an
		// explicit @grantor on every DM would silently break the
		// managed-persona DM path. We treat the DM recipient as
		// implicitly mentioned for v2 — the narrowing gate's
		// `@AI / @bot / plain` rejection is targeted at GROUP /
		// COMMUNITY_TOPIC traffic where the persona summon is
		// disambiguating.
		if m.ChannelType != common.ChannelTypePerson.Uint8() {
			if !mentionAll {
				if _, ok := mentioned[g.GrantorUID]; !ok {
					continue
				}
			}
		}
		// PR#82 round-2 P1-A — TOCTOU close-out on the fan-out hot path.
		// Even though the scope row exists, the grantor may have lost
		// access to the channel since (kicked from group, un-friended
		// peer, left parent group of a thread). Skipping the dispatch
		// keeps the bot from continuing to harvest channels the grantor
		// no longer has eyes on. DB error → fail-closed (skip this
		// grant, log, continue with the remaining ones; we never want a
		// transient DB blip to leak otherwise-denied traffic).
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
		// YUJ-1465 — also pass through the grant so the v2 payload can
		// carry `obo_grantor_uid` / `obo_grantor_name` / `obo_respond_as`
		// + a natural-language `obo_system_hint` composed from the
		// grant's persona_prompt and the resolved display names.
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
		// YUJ-1424 / PR#82 Jerry-Xin review blocker (2026-05-20) —
		// directly enqueue the fan-out copy into the grantee bot's
		// /v1/bot/events queue. WuKongIM has now accepted the message
		// (above) and will deliver it to the bot's mailbox, but the
		// webhook → NotifyMessagesListeners path drops NoPersist=1
		// messages before listeners fire (modules/webhook/api.go,
		// handleMessageNotify, by design). Without this enqueue the
		// grantee bot never observes the fan-out — the bug Jerry-Xin
		// flagged. The synthetic event mirrors what the listener path
		// would have produced, so /v1/bot/events serves it
		// transparently. Best-effort: a Redis failure here logs but
		// does NOT roll back the dispatch (the message is already in
		// WuKongIM's store and a later replay path is preferable to a
		// no-op).
		copyResp := buildFanoutCopyMessageResp(copyReq, m)
		if err := ba.enqueueFanoutBotEvent(g.GranteeBotUID, copyResp); err != nil {
			ba.Error("OBO fan-out bot-event enqueue failed",
				zap.String("grantee_bot", g.GranteeBotUID),
				zap.String("channel_id", m.ChannelID),
				zap.Error(err))
		}
		dispatched++
	}
	return dispatched
}

// buildFanoutCopyReq turns an inbound MessageResp into a one-shot copy
// addressed to `g.GranteeBotUID`'s PERSONAL mailbox. The payload is augmented
// with `obo_fanout=true` plus `obo_origin_*` fields that pin down the
// original conversation, plus the v2 (YUJ-1465 / octo-server#108) fields
// the persona-clone adapter consumes:
//
//	obo_grantor_uid     — the OBO grantor (also the FromUID of the copy)
//	obo_grantor_name    — display name of the grantor (best-effort)
//	obo_respond_as      — the uid the adapter should sign the reply with
//	                      (always the grantor in v2 — the bot replies AS
//	                      the persona, not as itself)
//	obo_system_hint     — natural-language Chinese prompt summarising
//	                      "you are running as <grantor>'s persona; this
//	                      came from group <X>; sender is <Y>". The
//	                      persona_prompt (if non-empty) is appended after
//	                      the auto hint so the grantor's behavioral
//	                      prompt overrides / extends the base context.
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

// decodeMentionGate — YUJ-1465 / Mininglamp-OSS/octo-server#108 (OBO v2)
// + YUJ-1538 (`@所有人` broadcast support).
//
// Pulls the v2 narrowing-gate inputs off the raw inbound payload:
//
//   - `uids`: the explicit `mention.uids` array, returned as a
//     set keyed by uid (O(1) per-grant membership tests in
//     fanoutForMessage's loop).
//   - `all`:  whether `mention.all` is truthy (1 / true). `@所有人`
//     traffic in WuKongIM commonly carries `mention.all=1` without
//     re-listing every group member in `mention.uids`, so the gate
//     treats it as "every grantor was implicitly mentioned" for
//     group/topic channels.
//
// Returns an empty (non-nil) set + `all=false` when:
//
//   - the payload is empty or not JSON-decodable;
//   - the payload has no `mention` object;
//   - `mention.uids` is missing, not an array, or empty;
//   - `mention.all` is missing / falsey;
//   - individual `uids` entries are not strings.
//
// Empty set + `all=false` = "no one was mentioned"; combined with the
// v2 narrowing gate it means "do not fan out" for the GROUP /
// COMMUNITY_TOPIC path. We intentionally do NOT honour `mention.ais`
// here — per the v2 spec, @AI / @bot traffic by itself MUST NOT summon
// the persona; the grantor must be a target of `mention.uids` OR the
// message must be a `@所有人` broadcast.
func decodeMentionGate(payload []byte) (uids map[string]struct{}, all bool) {
	uids = map[string]struct{}{}
	if len(payload) == 0 {
		return uids, false
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return uids, false
	}
	raw, ok := decoded["mention"]
	if !ok || raw == nil {
		return uids, false
	}
	mentionMap, ok := raw.(map[string]interface{})
	if !ok {
		return uids, false
	}
	if uidsRaw, ok := mentionMap["uids"]; ok && uidsRaw != nil {
		if uidsSlice, ok := uidsRaw.([]interface{}); ok {
			for _, v := range uidsSlice {
				if s, ok := v.(string); ok {
					s = strings.TrimSpace(s)
					if s != "" {
						uids[s] = struct{}{}
					}
				}
			}
		}
	}
	all = mentionFlagTruthy(mentionMap["all"])
	return uids, all
}

// mentionFlagTruthy reports whether a parsed `mention.*` flag is the
// numeric/boolean form of 1. Mirrors `pkg/mentionrewrite.isTruthyOne`
// (unexported there) and the read-side helper in
// `modules/message/api_reminders.go` so the OBO fan-out gate cannot
// disagree with the message-write/read-reminders code about what
// counts as "set". Kept local to avoid widening pkg/mentionrewrite's
// public surface for a helper that's mostly used at write-time.
//
// Real WuKongIM payloads decode into a mix of float64 (the default
// json.Unmarshal numeric type — what `decodeMentionGate` produces) and
// json.Number (used by other read paths that opt into
// `json.Decoder.UseNumber()`). We accept both plus bool / int* / uint*
// so a caller sending the legacy `"all": true` shape continues to work.
func mentionFlagTruthy(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x == 1
	case float32:
		return x == 1
	case json.Number:
		n, err := x.Int64()
		return err == nil && n == 1
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
	}
	return false
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

// buildFanoutCopyMessageResp synthesizes a *config.MessageResp that
// mirrors what the WuKongIM → webhook → listener path WOULD have
// produced for the just-dispatched fan-out copy, but doesn't (the
// webhook drops NoPersist=1 — see fanoutForMessage and the package
// comment).
//
// The fields are kept tight to what /v1/bot/events consumers actually
// read: ChannelID / ChannelType / FromUID / Payload identify the
// conversation, Header records the NoPersist+SyncOnce semantics the
// listener path would have surfaced, and Timestamp lets the bot order
// events. MessageID / MessageSeq / ClientMsgNo are left zero / empty
// because we did not round-trip through WuKongIM's SendMessageWithResult
// (the existing dispatchFanout uses SendMessage, which intentionally
// discards the response per its docstring — "we don't need the result
// and the simpler call avoids a wait"). A future change can upgrade
// dispatchFanout to capture the response and populate these fields if
// a bot adapter starts requiring them; the current persona-clone
// adapter reads obo_origin_* from the payload, not the wire IDs.
//
// `origin` is the inbound message that triggered the fan-out — used
// only to propagate the original Timestamp when present, falling back
// to time.Now() when the inbound carried no timestamp.
func buildFanoutCopyMessageResp(req *config.MsgSendReq, origin *config.MessageResp) *config.MessageResp {
	if req == nil {
		return nil
	}
	ts := time.Now().Unix()
	if origin != nil && origin.Timestamp > 0 {
		ts = int64(origin.Timestamp)
	}
	return &config.MessageResp{
		Header:      req.Header,
		FromUID:     req.FromUID,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		Payload:     req.Payload,
		Timestamp:   int32(ts),
	}
}

// enqueueFanoutBotEvent appends the synthetic fan-out event to the
// grantee bot's /v1/bot/events queue. Honors the oboFanoutBotEnqueue
// test seam first so unit tests can assert enqueue behavior without
// standing up Redis; production path goes through
// ba.robotService.EnqueueBotEvent. A nil robotService (defensive — the
// constructor wires one, but BotAPI is sometimes assembled piecemeal
// in older tests) is treated as a silent no-op so the dispatch loop
// stays robust.
func (ba *BotAPI) enqueueFanoutBotEvent(robotID string, message *config.MessageResp) error {
	if ba.oboFanoutBotEnqueue != nil {
		return ba.oboFanoutBotEnqueue(robotID, message)
	}
	if ba.robotService == nil {
		return nil
	}
	return ba.robotService.EnqueueBotEvent(robotID, message)
}
