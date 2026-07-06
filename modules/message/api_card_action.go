package message

// card-message-interaction P2 D3/D4/D5（spec: .octospec/tasks/
// card-message-interaction/brief.md）：卡片动作上行通道。
//
// POST /v1/message/card/action —— 挂载在 /v1/message 组（AuthMiddleware +
// SharedUIDRateLimiter + SpaceMiddleware 已在组上，满足 D3 的挂载序要求）。
// 端点本身不改写任何卡片状态（状态权威在卡片内容，由 bot 经 botMessageEdit
// 重写）：只做 校验 → 幂等 → 给消息发送方 bot 的事件队列投递 card_action。
//
// 校验顺序（D3）：频道成员资格 → 目标消息存在且 type=17 → sender 是 bot 身份
// （信任模型 layer (c)；iwh_ webhook 发送者无事件消费端，D7 一并在此拒绝）→
// action_id 存在于「生效卡片」（content_edit 优先 —— 被重写移除的旧帧按钮
// fail-closed）。防枚举：除成员资格（403）外全部归并到单一 400 invalid，
// 具体原因只进日志。

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// cardActionIdemTTL D4：幂等窗口，与 card_action 事件可消费窗口共用一个常量
// （D8：actionable window == idempotency TTL）。
const cardActionIdemTTL = 24 * time.Hour

type cardActionReq struct {
	MessageID   string                 `json:"message_id"`
	ChannelID   string                 `json:"channel_id"`
	ChannelType uint8                  `json:"channel_type"`
	ActionID    string                 `json:"action_id"`
	Inputs      map[string]interface{} `json:"inputs"`
	ClientToken string                 `json:"client_token"`
}

// cardAction handles POST /v1/message/card/action.
func (m *Message) cardAction(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req cardActionReq
	if err := c.BindJSON(&req); err != nil {
		respondMessageRequestInvalid(c, "")
		return
	}
	for field, v := range map[string]string{
		"message_id": req.MessageID, "channel_id": req.ChannelID,
		"action_id": req.ActionID, "client_token": req.ClientToken,
	} {
		if strings.TrimSpace(v) == "" {
			respondMessageRequestInvalid(c, field)
			return
		}
	}
	if !cardmsg.Enabled() {
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D3 ①成员资格。person 频道由「按 (loginUID, peer) 的 fake channel 查询」
	// 内建证明（查得到即为会话双方）；群 / 子区做显式成员校验。
	fakeChannelID := req.ChannelID
	switch req.ChannelType {
	case common.ChannelTypePerson.Uint8():
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
	case common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
		groupNo := req.ChannelID
		if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			parent, err := m.resolveParentGroupNo(req.ChannelID)
			if err != nil {
				respondMessageRequestInvalid(c, "channel_id")
				return
			}
			groupNo = parent
		}
		// POC：经 GetMembers 全量扫描判成员；正式实现补 group.IService 的
		// IsMember 单点查询（避免大群 O(n) 拉取）。
		members, err := m.groupService.GetMembers(groupNo)
		if err != nil {
			m.Error("查询群成员失败", zap.Error(err), zap.String("groupNo", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		isMember := false
		for _, mem := range members {
			if mem != nil && mem.UID == loginUID {
				isMember = true
				break
			}
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrMessageCardActionDenied, nil, nil)
			return
		}
	default:
		respondMessageRequestInvalid(c, "channel_type")
		return
	}

	// D3 ②目标消息存在且为卡片。
	msgM, err := m.db.queryMessageByID(fakeChannelID, req.ChannelType, req.MessageID)
	if err != nil {
		m.Error("查询卡片动作目标消息失败", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if msgM == nil || len(msgM.Payload) == 0 || !cardmsg.IsCardRawPayload(msgM.Payload) {
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D3 ③sender 必须是 bot 身份（layer (c)）。iwh_ webhook 合成发送者不是
	// robot（D7：webhook 卡片无事件消费端）、被绕过渲染门禁的人类发送者卡片，
	// 都在此 fail-closed —— 伪造卡片点了也没有任何效果。
	senderIsBot, err := m.robotService.ExistRobot(msgM.FromUID)
	if err != nil {
		m.Error("查询发送者 bot 身份失败", zap.Error(err), zap.String("fromUID", msgM.FromUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if !senderIsBot {
		m.Warn("卡片动作目标消息 sender 非 bot,拒绝", zap.String("fromUID", msgM.FromUID), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D3 ④action_id 必须存在于「生效卡片」：content_edit（最新帧）优先于原始
	// payload —— 重写移除的按钮迟到点击在此 400，过期交互天然 fail-closed。
	effective := msgM.Payload
	if extra, err := m.messageExtraDB.queryWithMessageID(req.MessageID); err != nil {
		m.Error("查询消息扩展失败", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if extra != nil && extra.ContentEdit.Valid && extra.ContentEdit.String != "" {
		effective = []byte(extra.ContentEdit.String)
	}
	if _, ok := cardmsg.SubmitActionIDs(effective)[req.ActionID]; !ok {
		m.Warn("action_id 不在生效卡片中,拒绝", zap.String("actionID", req.ActionID), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D4 幂等：重复提交返回 replay ack，绝不产生第二个 bot 事件。
	// POC：octo-lib redis 包装器无 SETNX，用 GetString+SetAndExpire 近似
	// （存在竞态窗口）；正式实现换 SETNX / Lua 原子化。
	idemKey := fmt.Sprintf("cardaction:%s:%s:%s:%s", req.MessageID, req.ActionID, loginUID, req.ClientToken)
	if existing, err := m.ctx.GetRedisConn().GetString(idemKey); err != nil {
		m.Error("查询卡片动作幂等键失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if existing != "" {
		c.Response(map[string]interface{}{"accepted": true, "replay": true})
		return
	}
	if err := m.ctx.GetRedisConn().SetAndExpire(idemKey, "1", cardActionIdemTTL); err != nil {
		m.Error("写入卡片动作幂等键失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	// D5 投递 card_action（event_data 形状由 brief 冻结：只许增字段）。
	eventData := map[string]interface{}{
		"message_id":   req.MessageID,
		"channel_id":   req.ChannelID,
		"channel_type": req.ChannelType,
		"space_id":     spacepkg.GetSpaceID(c),
		"action_id":    req.ActionID,
		"inputs":       req.Inputs,
		"operator_uid": loginUID,
		"client_token": req.ClientToken,
		"acted_at":     time.Now().Unix(),
	}
	if err := m.robotService.EnqueueBotTypedEvent(msgM.FromUID, cardmsg.EventTypeCardAction, eventData); err != nil {
		m.Error("card_action 事件入队失败", zap.Error(err), zap.String("botUID", msgM.FromUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	c.Response(map[string]interface{}{"accepted": true, "replay": false})
}
