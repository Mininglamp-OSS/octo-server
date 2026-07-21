package bot_api

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// BotReactionReq is the request for POST /v1/bot/message/reaction.
//
// Semantics are EXPLICIT + IDEMPOTENT (not toggle): action="add" ensures the
// reaction is present, action="remove" ensures it is absent. This is the correct
// shape for an agent that may retry on timeout — unlike the user-facing
// /v1/reactions toggle, a duplicate add never silently cancels an existing
// reaction. `remove` on the OpenClaw side (ReactionParams.remove=true) maps to
// action="remove"; false maps to action="add".
type BotReactionReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	MessageID   string `json:"message_id"`
	Emoji       string `json:"emoji"`
	// Action is "add" (default when empty) or "remove".
	Action string `json:"action"`
}

// BotReactionResp echoes the final reaction state so the caller can reconcile.
// is_deleted=0 → reaction present, =1 → absent (mirrors the user-facing shape).
type BotReactionResp struct {
	MessageID   string `json:"message_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Emoji       string `json:"emoji"`
	Action      string `json:"action"`
	Seq         int64  `json:"seq"`
	IsDeleted   int    `json:"is_deleted"`
}

// botMessageReaction handles POST /v1/bot/message/reaction — add/remove an emoji
// reaction on a message the bot can see (as-bot; OBO is out of scope this round).
//
// Auth posture: authBot resolves the bot identity (robotID). The reaction is
// attributed to the bot itself. All ACL/visibility/text-only/thread/disband
// checks are delegated to message.ReactionService.WriteReaction — the SAME
// validation authority the user-facing /v1/reactions path uses (no divergence,
// preserves the #603 hardening). App Bots (app_ tokens) are DM-only, matching
// the group-operation gate in groups.go.
func (ba *BotAPI) botMessageReaction(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	var req BotReactionReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}

	// App Bot is DM-only — deny group/thread reactions (capability boundary,
	// enforced server-side; the CLI lets app_ bind but cannot do everything bf_
	// can — see channel.ts BOT_TOKEN_PREFIXES note).
	if getBotKindFromContext(c) == BotKindApp &&
		req.ChannelType != common.ChannelTypePerson.Uint8() {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return
	}

	action, ok := parseReactionAction(req.Action)
	if !ok {
		respondBotAPIRequestInvalid(c, "action")
		return
	}

	result, fail := ba.reactionService.WriteReaction(&message.ReactionWriteReq{
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		MessageID:   req.MessageID,
		UID:         robotID,
		Name:        ba.resolveBotDisplayName(robotID),
		Emoji:       req.Emoji,
		Action:      action,
		// PersonSpace nil: as-bot reactions live on the bot's own DM/group
		// channel; the target-message visibility check (viewer=robotID) already
		// gates cross-context access. User-facing DM per-Space isolation (which
		// needs SpaceMiddleware's request Space) does not apply to the bot path.
		PersonSpace: nil,
	})
	if fail != nil {
		respondBotReactionFailure(c, fail)
		return
	}

	c.Response(&BotReactionResp{
		MessageID:   result.MessageID,
		ChannelID:   result.ChannelID,
		ChannelType: result.ChannelType,
		Emoji:       result.Emoji,
		Action:      normalizeReactionActionLabel(req.Action),
		Seq:         result.Seq,
		IsDeleted:   result.IsDeleted,
	})
}

// parseReactionAction maps the wire `action` to a message.ReactionAction.
// Empty defaults to add (the common "react before reply" case). Bot reactions
// are always explicit add/remove — never toggle.
func parseReactionAction(raw string) (message.ReactionAction, bool) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "add":
		return message.ReactionAdd, true
	case "remove", "delete":
		return message.ReactionRemove, true
	default:
		return message.ReactionAdd, false
	}
}

// normalizeReactionActionLabel canonicalizes the echoed action label.
func normalizeReactionActionLabel(raw string) string {
	if a, ok := parseReactionAction(raw); ok && a == message.ReactionRemove {
		return "remove"
	}
	return "add"
}

// respondBotReactionFailure maps a message.ReactionService failure onto the
// localized envelope. The service already logged any internal cause; the codes
// are message-domain (ErrMessageChannelAccessDenied / ErrMessageNotFound /
// ErrMessageReactionUnsupportedType / …), reused as-is on the bot path.
func respondBotReactionFailure(c *wkhttp.Context, fail *message.ReactionFailure) {
	details := i18n.Details{}
	if fail.Field != "" {
		details["field"] = fail.Field
	}
	httperr.ResponseErrorL(c, fail.Code, nil, details)
}
