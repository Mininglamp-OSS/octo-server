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
	grants, err := store.findActiveGrantsForChannel(lookupChannelID, m.ChannelType)
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
		copyReq := buildFanoutCopyReq(m, g.GrantorUID, g.GranteeBotUID)
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
func buildFanoutCopyReq(m *config.MessageResp, grantorUID, granteeBotUID string) *config.MsgSendReq {
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
		payload["obo_origin_message_idstr"] = m.MessageIDStr
	}

	// PERSONAL DM dispatch — must go through the octo-lib builder so
	// payload.space_id authoritative semantics + the channel_id/subscribers
	// mutex are uniformly applied. Subscribers omitted intentionally; see
	// the contract block in the function doc. FromUID is the grantor (NOT
	// m.FromUID) — see PR#82 R6 P0 rationale above.
	return config.NewPersonalMsgSendReq(
		granteeBotUID,
		grantorUID,
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
