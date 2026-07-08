package message

// card-message-interaction P2 D3/D4/D5/D11（spec: .octospec/tasks/
// card-message-interaction/brief.md，round-3 修订后形状）：卡片动作上行通道。
//
// POST /v1/message/card/action —— 挂在 /v1/message 组（AuthMiddleware +
// SharedUIDRateLimiter + SpaceMiddleware 已在组上，满足 D3 的挂载序要求），本
// 路由额外挂 64KiB pre-decode 上限（round-3 P1-3：带用户输入 map 的路由与 P1
// 发送路由同享 body-cap 纪律）。端点本身不改写任何卡片状态（状态权威在卡片
// 内容，由 bot 经 botMessageEdit 重写）：只做 校验 → 幂等 claim → 给消息发送方
// bot 的事件队列投递 card_action → confirm。
//
// 校验顺序（D3，round-3 修订）：存储行定位 + 频道绑定（anti-IDOR：消息按「请求
// 声明的频道」定位 —— 分表按 channel_id 路由，WHERE 同时钉 channel_id+message_id，
// 查得到 ⟺ 声明频道与存储行一致；此后所有授权判定一律以存储行为准）→ 操作者对
// 存储频道的成员资格 → 消息为 type=17 且 sender 是 bot 身份（信任模型 layer (c)；
// iwh_ webhook 发送者无事件消费端，D7 一并在此拒绝）→ 撤回/删除门禁（已撤回 /
// 全局删除 / 操作者本地删除的卡片不可再触发动作，与单条读同口径）→ action_id
// 存在于「生效卡片」（content_edit 优先 —— 被重写移除的旧帧按钮 fail-closed）→
// D11 inputs 校验 → D4 幂等入队。防枚举：除成员资格（403 语义）外全部归并到单一
// 400 invalid，具体原因只进日志。

import (
	"net/http"
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

// cardActionMaxBodyBytes D3（round-3 P1-3）：pre-decode body 上限。请求体只有
// 定位字段 + inputs（D11 序列化上限 16KiB），64KiB 余量充足。
const cardActionMaxBodyBytes = 64 << 10

// cardActionReq 刻意不含 data 字段：Action.Submit.data 是作者静态上下文，服务端
// 从生效帧提取（D11 anti-forgery），请求携带的任何 data 一律被忽略（不绑定）。
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cardActionMaxBodyBytes)
	var req cardActionReq
	if err := c.BindJSON(&req); err != nil {
		respondMessageRequestInvalid(c, "")
		return
	}
	// 必填字段按固定顺序校验（有序 slice 而非 map —— map 迭代随机会让上报/日志的
	// 字段名非确定，妨碍排障）。缺任一均归并到同一 invalid（防枚举）。
	for _, f := range []struct{ name, val string }{
		{"message_id", req.MessageID}, {"channel_id", req.ChannelID},
		{"action_id", req.ActionID}, {"client_token", req.ClientToken},
	} {
		if strings.TrimSpace(f.val) == "" {
			respondMessageRequestInvalid(c, f.name)
			return
		}
	}
	if !cardmsg.Enabled() {
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D3 ①存储行定位 + 频道绑定（round-3 P1-4 anti-IDOR）。消息表按 channel_id
	// 分表路由，查询 WHERE 同时钉 (channel_id, channel_type, message_id) —— 查得到
	// 即证明「请求声明的频道 == 存储行的频道」；不一致/不存在统一 400（防枚举，
	// 两者不可区分）。person 频道的 fake id 由 (loginUID, 对端) 生成，指着别人
	// 会话的 message_id 天然查不到。
	lookupChannelID := req.ChannelID
	switch req.ChannelType {
	case common.ChannelTypePerson.Uint8():
		lookupChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
	case common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
		// group / topic：消息就存于声明频道本身。
	default:
		respondMessageRequestInvalid(c, "channel_type")
		return
	}
	msgM, err := m.db.queryMessageByID(lookupChannelID, req.ChannelType, req.MessageID)
	if err != nil {
		m.Error("查询卡片动作目标消息失败", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if msgM == nil || len(msgM.Payload) == 0 || !cardmsg.IsCardRawPayload(msgM.Payload) {
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D3 ②成员资格 —— 判定对象取自存储行（assert, don't assume：即使 WHERE 已
	// 证明一致，这里也只读 msgM.* 而非 req.*，让「存储行是授权主体」在代码形状上
	// 成立）。person：操作者必须是存储 fake 频道的会话双方之一；group/topic：显式
	// 成员校验（ExistMemberActive 单点查询 —— 白名单变体，排除被拉黑成员）。
	switch msgM.ChannelType {
	case common.ChannelTypePerson.Uint8():
		if !fakeChannelContainsUID(msgM.ChannelID, loginUID) {
			httperr.ResponseErrorL(c, errcode.ErrMessageCardActionDenied, nil, nil)
			return
		}
	default:
		groupNo := msgM.ChannelID
		if msgM.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			parent, err := m.resolveParentGroupNo(msgM.ChannelID)
			if err != nil {
				respondMessageRequestInvalid(c, "channel_id")
				return
			}
			groupNo = parent
		}
		isMember, err := m.groupService.ExistMemberActive(groupNo, loginUID)
		if err != nil {
			m.Error("查询群成员失败", zap.Error(err), zap.String("groupNo", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrMessageCardActionDenied, nil, nil)
			return
		}
	}

	// D3 ③sender 必须是 bot 身份（layer (c)）。iwh_ webhook 合成发送者不是 robot
	// （D7：webhook 卡片无事件消费端）、被绕过渲染门禁的人类发送者卡片，都在此
	// fail-closed —— 伪造卡片点了也没有任何效果。
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
	// payload —— 重写移除的按钮迟到点击在此 400，过期交互天然 fail-closed。同时
	// 取回匹配动作的静态 data（D11：服务端从生效帧提取，绝不取请求里的 data）。
	// D3 ④撤回/删除门禁 + 生效帧。已撤回(revoke)或全局删除(is_deleted)的卡片不可
	// 再触发动作 —— 与单条读 api_message_get.go 同口径（extra.Revoke/IsDeleted、
	// userExtra.MessageIsDeleted 均按「不存在」处理），防止 stale client 点击已从
	// 可见消息面回收的卡片、触发 bot 副作用。归并到单一 invalid（防枚举）。
	extra, err := m.messageExtraDB.queryWithMessageID(req.MessageID)
	if err != nil {
		m.Error("查询消息扩展失败", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if extra != nil && (extra.Revoke == 1 || extra.IsDeleted == 1) {
		m.Warn("卡片动作目标消息已撤回/删除,拒绝", zap.String("messageID", req.MessageID),
			zap.Int("revoke", extra.Revoke), zap.Int("isDeleted", extra.IsDeleted))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}
	// 操作者本地删除（单条可见性对齐）：该用户已把这张卡从自己视图删除 → 不可再操作。
	if userExtras, uerr := m.messageUserExtraDB.queryWithMessageIDsAndUID([]string{req.MessageID}, loginUID); uerr != nil {
		m.Error("查询消息用户扩展失败", zap.Error(uerr), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if len(userExtras) > 0 && userExtras[0].MessageIsDeleted == 1 {
		m.Warn("卡片动作目标消息已被操作者本地删除,拒绝", zap.String("messageID", req.MessageID), zap.String("uid", loginUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// 生效帧：content_edit（最新帧）优先于原始 payload —— 重写移除的按钮迟到点击
	// 在此 400，过期交互天然 fail-closed。同时取回匹配动作的静态 data（D11：服务端
	// 从生效帧提取，绝不取请求里的 data）。
	effective := msgM.Payload
	if extra != nil && extra.ContentEdit.Valid && extra.ContentEdit.String != "" {
		effective = []byte(extra.ContentEdit.String)
	}
	actionData, found := cardmsg.SubmitAction(effective, req.ActionID)
	if !found {
		m.Warn("action_id 不在生效卡片中,拒绝", zap.String("actionID", req.ActionID), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D11 ⑤inputs 信任边界（round-3 P1-3）：只放行生效帧声明过的 Input.* id，逐
	// 类型校验 + 尺寸上限 —— event_data.inputs 从此形状可信（内容仍是不可信用户
	// 文本，bot 侧照常转义）。
	if err := cardmsg.ValidateInputs(effective, req.Inputs); err != nil {
		m.Warn("卡片动作 inputs 校验失败,拒绝", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D4 ⑥幂等（round-3 P1-1）：业务身份键（不含 client_token —— 新 token 重试
	// 不得二次触发），claim → 入队 → confirm；入队失败补偿释放，客户端可重试。
	idemKey := cardActionClaimKey(req.MessageID, req.ActionID, loginUID)
	claimed, err := m.cardClaims.Claim(idemKey)
	if err != nil {
		m.Error("卡片动作幂等 claim 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	if !claimed {
		// pending 或已 confirm 都是同一答案：这个人对这个动作已经提交过。
		c.Response(map[string]interface{}{"accepted": true, "replay": true})
		return
	}

	// D5 投递 card_action（event_data 形状由 brief 冻结：只许增字段）。
	//   - data：匹配 Action.Submit 的作者静态对象，从生效帧提取（D11，仅当声明了
	//     data 才置键）—— trusted-as-authored，不可伪造。
	//   - inputs：D11 已 shape-checked 的用户输入（内容仍不可信）。
	//   - client_token：D4 关联 ID —— 消费方不得当作幂等身份，bot 侧幂等按 event_id。
	//   - channel 字段回显请求值 —— D3 ①已证明与存储行一致，且这是 API 层频道
	//     标识（person 频道 = 对端 uid），不泄漏内部 fake id 编码。
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
	if actionData != nil {
		eventData["data"] = actionData
	}
	eventID, err := m.robotService.EnqueueBotTypedEvent(msgM.FromUID, cardmsg.EventTypeCardAction, eventData)
	if err != nil {
		if relErr := m.cardClaims.Release(idemKey); relErr != nil {
			m.Error("card_action 入队失败后释放幂等 claim 也失败(残留至多 60s pending)", zap.Error(relErr), zap.String("key", idemKey))
		}
		m.Error("card_action 事件入队失败,已释放幂等 claim", zap.Error(err), zap.String("botUID", msgM.FromUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	if ok, err := m.cardClaims.Confirm(idemKey, eventID); err != nil || !ok {
		// 事件已入队（at-least-once，bot 按 event_id 幂等）；confirm 失败只影响
		// replay 窗口长度，记日志即可，不回滚。
		m.Warn("卡片动作幂等 confirm 未生效", zap.Bool("ok", ok), zap.Error(err), zap.String("key", idemKey))
	}
	c.Response(map[string]interface{}{"accepted": true, "replay": false})
}

// fakeChannelContainsUID 校验 person 频道存储行的 fake channel id（"a@b"）是否
// 包含给定 uid —— 成员资格以存储行为准（D3 anti-IDOR）。
func fakeChannelContainsUID(fakeChannelID, uid string) bool {
	parts := strings.SplitN(fakeChannelID, "@", 2)
	return len(parts) == 2 && (parts[0] == uid || parts[1] == uid)
}
