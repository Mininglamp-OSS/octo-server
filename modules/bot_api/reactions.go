package bot_api

import (
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// #111 Sprint B — bot reactions.
//
// POST   /v1/bot/messages/:message_id/reactions          body {channel_id, channel_type, emoji}
// DELETE /v1/bot/messages/:message_id/reactions/:emoji    ?channel_id&channel_type
//
// Semantics: a bot is a single-reaction actor per message (the reaction_users
// table is keyed (uid, message_id)). add(emoji) sets/replaces the bot's
// reaction; remove(emoji) clears it only when the current emoji matches. Both
// are idempotent — repeat/no-op cases return 200 without an extra broadcast.

type botReactionReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Emoji       string `json:"emoji"`
}

// addReaction handles POST /v1/bot/messages/:message_id/reactions
func (ba *BotAPI) addReaction(c *wkhttp.Context) {
	messageID := strings.TrimSpace(c.Param("message_id"))
	if messageID == "" {
		respondBotAPIRequestInvalid(c, "message_id")
		return
	}
	var req botReactionReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	emoji := strings.TrimSpace(req.Emoji)
	if strings.TrimSpace(req.ChannelID) == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if req.ChannelType == 0 {
		respondBotAPIRequestInvalid(c, "channel_type")
		return
	}
	if emoji == "" {
		respondBotAPIRequestInvalid(c, "emoji")
		return
	}
	ba.applyReaction(c, messageID, req.ChannelID, req.ChannelType, emoji, false)
}

// removeReaction handles DELETE /v1/bot/messages/:message_id/reactions/:emoji
func (ba *BotAPI) removeReaction(c *wkhttp.Context) {
	messageID := strings.TrimSpace(c.Param("message_id"))
	if messageID == "" {
		respondBotAPIRequestInvalid(c, "message_id")
		return
	}
	emoji := strings.TrimSpace(c.Param("emoji"))
	if emoji == "" {
		respondBotAPIRequestInvalid(c, "emoji")
		return
	}
	channelID := strings.TrimSpace(c.Query("channel_id"))
	channelTypeRaw, _ := strconv.Atoi(strings.TrimSpace(c.Query("channel_type")))
	channelType := uint8(channelTypeRaw)
	if channelID == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if channelType == 0 {
		respondBotAPIRequestInvalid(c, "channel_type")
		return
	}
	ba.applyReaction(c, messageID, channelID, channelType, emoji, true)
}

// applyReaction is the shared add/remove state machine. Permission and the
// channel-id store mapping mirror the user-facing reaction path so bot and
// user reactions are indistinguishable to clients.
func (ba *BotAPI) applyReaction(c *wkhttp.Context, messageID, channelID string, channelType uint8, emoji string, remove bool) {
	robotID := getRobotIDFromContext(c)
	botKind := getBotKindFromContext(c)

	// Permission: reuse the exact send-permission contract (handles group,
	// CommunityTopic/thread parent-membership, App Bot DM-only, DM friend).
	// Reactions carry no OBO semantics, so hasOBOContext is always false.
	if err := ba.checkSendPermission(c, botKind, robotID, channelID, channelType, false); err != nil {
		respondSendPermissionError(c, err)
		return
	}

	// Store channel id: groups use the raw channel id; DMs use the fake
	// channel id (peer, bot) — identical to modules/message addOrCancelReaction
	// so reaction_users rows land in the same per-conversation bucket and the
	// reaction seq stream is shared with the user path.
	storeChannelID := resolveReactionStoreChannelID(channelID, channelType, robotID)

	store := ba.newReactionStorer()
	existing, err := store.queryByActorAndMessage(robotID, messageID)
	if err != nil {
		ba.Error("查询bot消息回应失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	changed, err := reactionStateTransitionImpl(store, ba.resolveBotDisplayName, existing, reactionInput{
		messageID:      messageID,
		storeChannelID: storeChannelID,
		channelType:    channelType,
		robotID:        robotID,
		emoji:          emoji,
		remove:         remove,
	})
	if err != nil {
		ba.Error("写入bot消息回应失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}

	// Only broadcast a sync CMD when state actually changed. The CMD uses the
	// ORIGINAL channel id (not the fake store id) so it routes to the real
	// conversation, matching the user path.
	if changed {
		if err := ba.dispatchReactionCMD(config.MsgCMDReq{
			NoPersist:   true,
			ChannelID:   channelID,
			ChannelType: channelType,
			CMD:         common.CMDSyncMessageReaction,
			FromUID:     robotID,
		}); err != nil {
			ba.Error("发送回应同步命令失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageNotifyFailed, nil, nil)
			return
		}
	}
	c.ResponseOK()
}

// resolveReactionStoreChannelID maps a request channel id to the channel id
// used for reaction_users storage + seq generation. Groups/threads store under
// the raw channel id; DMs store under the fake channel id (peer, bot), matching
// the user-facing reaction path so bot and user reactions share one bucket.
func resolveReactionStoreChannelID(channelID string, channelType uint8, robotID string) string {
	if channelType == common.ChannelTypePerson.Uint8() {
		return common.GetFakeChannelIDWith(channelID, robotID)
	}
	return channelID
}

type reactionInput struct {
	messageID      string
	storeChannelID string
	channelType    uint8
	robotID        string
	emoji          string
	remove         bool
}

// newReactionStorer returns the reaction persistence surface. Overridable in
// tests (reactionStoreOverride) so handler-level tests can drive applyReaction
// without a live DB.
func (ba *BotAPI) newReactionStorer() reactionStorer {
	if ba.reactionStoreOverride != nil {
		return ba.reactionStoreOverride
	}
	return newReactionStore(ba.ctx)
}

// reactionStorer is the full persistence surface applyReaction needs.
type reactionStorer interface {
	reactionStateStore
	queryByActorAndMessage(uid, messageID string) (*reactionRow, error)
}

// reactionStateStore is the minimal persistence surface the state machine
// needs; factored out so unit tests can drive every branch with a fake.
type reactionStateStore interface {
	genSeq(storeChannelID string) (int64, error)
	insert(m *reactionRow) error
	update(m *reactionRow) error
}

// reactionStateTransitionImpl is the pure add/remove state machine over the
// single (uid, message_id) row. Returns whether a persisted change occurred
// (and thus whether a sync CMD should be broadcast). nameResolver supplies the
// bot display name lazily so the lookup only happens on an actual add.
//
// NOTE: bot reactions use SET semantics, deliberately different from the
// user-facing path (modules/message addOrCancelReaction), which TOGGLES
// (re-posting the same live emoji removes it). The bot has an explicit remove
// (DELETE), so re-adding the same emoji is a safe no-op rather than a surprise
// removal — important for agents that retry idempotently. Both write to the
// same reaction_users rows, so consumers must not assume toggle-for-all.
//
// add(emoji):
//   - no row            -> insert (emoji, is_deleted=0)            [changed]
//   - row deleted       -> revive (emoji, is_deleted=0)           [changed]
//   - row live, same    -> no-op                                   [unchanged]
//   - row live, diff    -> overwrite emoji (bot holds one)        [changed]
//
// remove(emoji):
//   - no row / deleted  -> no-op                                   [unchanged]
//   - row live, match   -> soft-delete (is_deleted=1)             [changed]
//   - row live, mismatch-> no-op (don't drop a different emoji)   [unchanged]
func reactionStateTransitionImpl(
	store reactionStateStore,
	nameResolver func(robotID string) string,
	existing *reactionRow,
	in reactionInput,
) (bool, error) {
	if in.remove {
		if existing == nil || existing.IsDeleted == 1 {
			return false, nil
		}
		if existing.Emoji != in.emoji {
			return false, nil
		}
		seq, err := store.genSeq(in.storeChannelID)
		if err != nil {
			return false, err
		}
		existing.Seq = seq
		existing.IsDeleted = 1
		if err := store.update(existing); err != nil {
			return false, err
		}
		return true, nil
	}

	// add
	if existing != nil && existing.IsDeleted == 0 && existing.Emoji == in.emoji {
		return false, nil // already reacting with this emoji
	}
	seq, err := store.genSeq(in.storeChannelID)
	if err != nil {
		return false, err
	}
	name := nameResolver(in.robotID)
	if existing == nil {
		return true, store.insert(&reactionRow{
			MessageID:   in.messageID,
			Seq:         seq,
			ChannelID:   in.storeChannelID,
			ChannelType: in.channelType,
			UID:         in.robotID,
			Name:        name,
			Emoji:       in.emoji,
			IsDeleted:   0,
		})
	}
	existing.Seq = seq
	existing.Emoji = in.emoji
	existing.IsDeleted = 0
	existing.Name = name
	return true, store.update(existing)
}

// dispatchReactionCMD sends the reaction-sync CMD, with a test seam mirroring
// dispatchTypingCMD so unit tests can capture the CMD without a live WuKongIM.
func (ba *BotAPI) dispatchReactionCMD(req config.MsgCMDReq) error {
	if ba.reactionCMDDispatch != nil {
		return ba.reactionCMDDispatch(req)
	}
	if ba.ctx == nil {
		return nil
	}
	return ba.ctx.SendCMD(req)
}
