package bot_api

import (
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// BotSyncMessagesReq is the request for syncMessages.
type BotSyncMessagesReq struct {
	ChannelID       string `json:"channel_id"`
	ChannelType     uint8  `json:"channel_type"`
	StartMessageSeq uint32 `json:"start_message_seq"`
	EndMessageSeq   uint32 `json:"end_message_seq"`
	Limit           int    `json:"limit"`
	PullMode        int    `json:"pull_mode"`
}

// syncMessages handles POST /v1/bot/messages/sync.
func (ba *BotAPI) syncMessages(c *wkhttp.Context) {
	var req BotSyncMessagesReq
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		respondBotAPIRequestInvalid(c, "channel_id")
		return
	}
	if req.ChannelType == 0 {
		respondBotAPIRequestInvalid(c, "channel_type")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Limit > 200 {
		req.Limit = 200
	}

	robotID := getRobotIDFromContext(c)

	// Group: verify bot is a member
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		// App Bot is DM-only — deny group sync entirely
		botKind := getBotKindFromContext(c)
		if botKind == BotKindApp {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotDMOnly, nil, nil)
			return
		}
		var count int
		err := ba.db.session.SelectBySql(
			"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
			req.ChannelID, robotID,
		).LoadOne(&count)
		if err != nil {
			ba.Error("failed to query group members", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if count == 0 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypePerson.Uint8() {
		botKind := getBotKindFromContext(c)
		switch botKind {
		case BotKindApp:
			isFriend, err := ba.userService.IsFriend(robotID, req.ChannelID)
			if err != nil {
				ba.Error("failed to verify relationship", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
				return
			}
			if !isFriend {
				httperr.ResponseErrorL(c, errcode.ErrBotAPIConversationNotStarted, nil, nil)
				return
			}
		case BotKindUser:
			robot := getRobotFromContext(c)
			isCreator := robot != nil && robot.CreatorUID == req.ChannelID
			if !isCreator {
				// PR#82 R6 P0 — friend gate is OBO-aware. See
				// obo_friend_gate.go for rationale: managed-persona
				// clones need to read message history of channels they
				// have OBO authority over even when bot↔user is not a
				// friend pair.
				//
				// PR#82 R7 — messages/sync has no `on_behalf_of` field
				// and the response stream is delivered TO the bot, not
				// proxied through any grantor. A bot that holds an
				// unrelated grant covering some target must NOT be able
				// to pull that target's DM history without the user
				// opt-in friend gate. So hasOBOContext=false: pure
				// IsFriend, no bypass.
				allowed, err := ba.isFriendOrOBOBypass(robotID, req.ChannelID, req.ChannelType, false)
				if err != nil {
					ba.Error("failed to check friend relationship", zap.Error(err))
					httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
					return
				}
				if !allowed {
					httperr.ResponseErrorL(c, errcode.ErrBotAPINotFriend, nil, nil)
					return
				}
			}
		}
	} else if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		// Thread: App Bot denied (DM-only), User Bot must be member of parent group
		botKind := getBotKindFromContext(c)
		if botKind == BotKindApp {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotDMOnly, nil, nil)
			return
		}
		parts := strings.SplitN(req.ChannelID, threadChannelIDSeparator, 2)
		if len(parts) != 2 {
			respondBotAPIRequestInvalid(c, "channel_id")
			return
		}
		var count int
		err := ba.db.session.SelectBySql(
			"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
			parts[0], robotID,
		).LoadOne(&count)
		if err != nil {
			ba.Error("failed to query group members", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if count == 0 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
			return
		}
	}

	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)
	syncReq := config.SyncChannelMessageReq{
		LoginUID:        robotID,
		ChannelID:       channelID,
		ChannelType:     req.ChannelType,
		StartMessageSeq: req.StartMessageSeq,
		EndMessageSeq:   req.EndMessageSeq,
		Limit:           req.Limit,
		PullMode:        config.PullMode(req.PullMode),
	}
	resp, err := ba.ctx.IMSyncChannelMessage(syncReq)
	if err != nil {
		ba.Error("同步消息失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPISendFailed, nil, nil)
		return
	}

	// The legacy /v1/bot routes keep their previous unlabeled-history behavior.
	// CLI v2 supplies the authenticated Token principal so the shared filter
	// can attribute unlabeled messages only to that owner's default Space.
	if resp != nil && req.ChannelType == common.ChannelTypePerson.Uint8() {
		if spaceID := spacepkg.GetSpaceID(c); spaceID != "" {
			principal := cliv2.Principal{}
			if c.FullPath() == "/v2/cli/messages/_sync" {
				principal = cliv2.PrincipalFrom(c)
			}
			messages, filterErr := ba.filterCLIBotSyncMessagesBySpace(resp.Messages, req.ChannelID, spaceID, principal)
			if filterErr != nil {
				ba.Error("CLI v2 Bot default Space lookup failed", zap.Error(filterErr))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				return
			}
			resp.Messages = messages
		}
	}

	c.Response(resp)
}
