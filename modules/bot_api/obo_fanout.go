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
//   Gate 1: bot self-sent → never replay to that same bot
//   Gate 2: grantor's own outbound → don't fan it to the grantor's bot
//           (covers the "I typed on my phone" case — bot should not echo)
//   Gate 3: already-OBO-processed → message_extra has __obo_processed__=true
//           (the bot's outbound, marked by sendMessage, must not bounce)
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
// For each surviving (message, grant) pair we build a CMD-style copy via
// MsgSendReq with Subscribers=[grantee_bot_uid] so only the bot receives
// the fan-out. The original delivery to real users is untouched.
//
// What we do NOT do here:
//   - We do NOT call SendMessageWithResult (which would create a new
//     persisted message everyone sees). Subscribers + NoPersist gives the
//     bot a one-shot copy via its existing subscriber pipeline.
//   - We do NOT recompute permissions; checkOBO already ran when the bot
//     authored the message that's now bouncing, and inbound messages from
//     real users are by definition allowed in the channel they arrived in.
package bot_api

import (
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

// oboMessagesListen is the registered MessagesListener. Hot path: must be
// O(1) for messages in channels with no active grants. The early-out on
// the channel scope lookup achieves that — the JOIN returns 0 rows when
// neither obo_grants nor obo_scopes has matching data.
//
// Wired in BotAPI.Route via ba.ctx.AddMessagesListener. Test surface is
// the lower-level fanoutForMessage method.
func (ba *BotAPI) oboMessagesListen(messages []*config.MessageResp) {
	for _, m := range messages {
		ba.fanoutForMessage(m)
	}
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

	store := ba.oboStoreOrDefault()
	grants, err := store.findActiveGrantsForChannel(m.ChannelID, m.ChannelType)
	if err != nil {
		ba.Error("OBO fan-out lookup failed",
			zap.String("channel_id", m.ChannelID),
			zap.Uint8("channel_type", m.ChannelType),
			zap.Error(err))
		return 0
	}
	if len(grants) == 0 {
		return 0
	}

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
		// Build a fan-out copy. NoPersist=1 + SyncOnce=1 + Subscribers
		// limits delivery to the bot only and avoids re-incrementing
		// red dots / conversation positions for any real user.
		copyReq := buildFanoutCopyReq(m, g.GranteeBotUID)
		if err := ba.dispatchFanout(copyReq); err != nil {
			ba.Error("OBO fan-out dispatch failed",
				zap.String("grantee_bot", g.GranteeBotUID),
				zap.String("channel_id", m.ChannelID),
				zap.Error(err))
			continue
		}
		dispatched++
	}
	return dispatched
}

// buildFanoutCopyReq turns an inbound MessageResp into a CMD-style copy
// addressed only to `granteeBotUID`. The payload is augmented with an
// `obo_fanout=true` marker so downstream consumers can distinguish the
// copy from the original (the marker is informational; loop protection
// uses `obo_processed=true` set by the bot's own outbound).
func buildFanoutCopyReq(m *config.MessageResp, granteeBotUID string) *config.MsgSendReq {
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

	return &config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 1, // silent copy — doesn't enter normal storage
			RedDot:    0,
			SyncOnce:  1,
		},
		FromUID:     m.FromUID,
		ChannelID:   m.ChannelID,
		ChannelType: m.ChannelType,
		Subscribers: []string{granteeBotUID},
		Payload:     []byte(util.ToJson(payload)),
	}
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
// part of the reserved `__obo_*` namespace that the inbound
// /v1/bot/sendMessage handler strips off client payloads — making the
// marker server-only state that bots cannot forge or suppress through
// the public REST API. (PR#82 review #2 P1-2.)
const oboProcessedMarkerKey = "__obo_processed__"

// oboReservedKeyPrefix is the reserved-namespace prefix for server-only
// OBO payload fields. Inbound /v1/bot/sendMessage payloads containing
// keys with this prefix are rejected (see send.go) so the gate-3 marker
// — and any future server-only OBO field — cannot be impersonated by a
// bot client.
const oboReservedKeyPrefix = "__obo_"

// hasOBOProcessedMarker — Gate 3. Returns true iff the payload decodes as
// a JSON object containing `oboProcessedMarkerKey: true`. Non-JSON /
// non-bool values are treated as absent so we err on the side of fanning
// out.
func hasOBOProcessedMarker(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	// Quick reject before the unmarshal — payloads in the millions/sec
	// hot path shouldn't pay the JSON decode cost just to find no marker.
	if !strings.Contains(string(payload), oboProcessedMarkerKey) {
		return false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	v, ok := m[oboProcessedMarkerKey].(bool)
	return ok && v
}

// payloadHasReservedOBOKey reports whether any top-level key in the
// JSON-decoded `payload` map starts with the reserved `__obo_` prefix.
// Used by /v1/bot/sendMessage to reject inbound client payloads that
// would attempt to spoof a server-only OBO marker (gate-3 bypass).
func payloadHasReservedOBOKey(payload map[string]interface{}) bool {
	if len(payload) == 0 {
		return false
	}
	for k := range payload {
		if strings.HasPrefix(k, oboReservedKeyPrefix) {
			return true
		}
	}
	return false
}
