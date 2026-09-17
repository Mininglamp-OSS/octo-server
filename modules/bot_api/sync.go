package bot_api

import (
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
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

	// 撤回脱敏：bot 拉历史与人看到的状态必须一致（WS-168 / octo-server#777）。
	// IMSyncChannelMessage 直出的是 WuKongIM 未脱敏原始 payload，撤回是
	// message_extra.revoke=1 的软删除、原文仍在，直接下发会把撤回原文泄漏给 bot。
	// 与 /v1/message/channel/sync 同口径：撤回消息只返回占位 payload + revoke=1/revoker。
	out, err := ba.sanitizeRevokedSyncResp(resp)
	if err != nil {
		ba.Error("查询撤回状态失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}

	c.Response(out)
}

// botSyncMessage 是 bot 拉历史响应里的单条消息。内嵌 *config.MessageResp，使其原有
// 字段（payload / message_id / from_uid …）逐字节按原样序列化（纯增量、向后兼容既有
// bot adapter），并新增 revoke / revoker，让 bot 能识别「这里有过一条消息但被撤回了」
// 而读不到原文——即辉哥定的「agent 看 = 人看」一致原则。
type botSyncMessage struct {
	*config.MessageResp
	Revoke  int    `json:"revoke,omitempty"`
	Revoker string `json:"revoker,omitempty"`
}

// botSyncMessagesResp 保持与 config.SyncChannelMessageResp 相同的顶层形状，只是把每条
// 消息换成带 revoke 标志的 botSyncMessage。
type botSyncMessagesResp struct {
	StartMessageSeq uint32            `json:"start_message_seq"`
	EndMessageSeq   uint32            `json:"end_message_seq"`
	PullMode        config.PullMode   `json:"pull_mode"`
	Messages        []*botSyncMessage `json:"messages"`
}

// sanitizeRevokedSyncResp 查 message_extra 找出本页里的撤回消息，对其就地剥离正文
// （payload 替换为占位、清空 streams）并打上 revoke=1/revoker，其余消息原样透传。
//
// fail-closed：撤回状态查询失败时返回 error，由调用方回 500 —— 绝不能查询失败就当作
// 「没有撤回」把原文原样下发。
func (ba *BotAPI) sanitizeRevokedSyncResp(resp *config.SyncChannelMessageResp) (*botSyncMessagesResp, error) {
	if len(resp.Messages) == 0 {
		return buildBotSyncResp(resp, nil), nil
	}

	// message_extra.message_id 是 VARCHAR(20)，必须用字符串绑定才能命中索引。
	ids := make([]string, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		ids = append(ids, strconv.FormatInt(m.MessageID, 10))
	}
	var rows []struct {
		MessageID string `db:"message_id"`
		Revoker   string `db:"revoker"`
	}
	if _, err := ba.ctx.DB().
		Select("message_id", "revoker").
		From("message_extra").
		Where("message_id in ? and `revoke`=1", ids).
		Load(&rows); err != nil {
		return nil, err
	}
	revokedRevoker := make(map[string]string, len(rows))
	for _, r := range rows {
		revokedRevoker[r.MessageID] = r.Revoker
	}
	return buildBotSyncResp(resp, revokedRevoker), nil
}

// buildBotSyncResp 把 IM 原始 sync 响应转换成 bot 响应：revokedRevoker 命中（key 为
// message_id 字符串）的消息就地脱敏并打 revoke=1/revoker，其余原样透传。纯函数，不触
// DB，便于单测覆盖脱敏/不误伤两条路径。
func buildBotSyncResp(resp *config.SyncChannelMessageResp, revokedRevoker map[string]string) *botSyncMessagesResp {
	out := &botSyncMessagesResp{
		StartMessageSeq: resp.StartMessageSeq,
		EndMessageSeq:   resp.EndMessageSeq,
		PullMode:        resp.PullMode,
		Messages:        make([]*botSyncMessage, 0, len(resp.Messages)),
	}
	for _, m := range resp.Messages {
		wrapped := &botSyncMessage{MessageResp: m}
		if revoker, ok := revokedRevoker[strconv.FormatInt(m.MessageID, 10)]; ok {
			m.Payload = message.SanitizeRevokedPayloadBytes(m.Payload)
			m.Streams = nil
			wrapped.Revoke = 1
			wrapped.Revoker = revoker
		}
		out.Messages = append(out.Messages, wrapped)
	}
	return out
}
