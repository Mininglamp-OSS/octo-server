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
// 校验顺序（D3，round-3 修订；P1-4 PR#548 review 调整幂等位次）：存储行定位 + 频道
// 绑定（anti-IDOR：消息按「请求声明的频道」定位 —— 分表按 channel_id 路由，WHERE
// 同时钉 channel_id+message_id，查得到 ⟺ 声明频道与存储行一致；此后所有授权判定一律
// 以存储行为准）→ 操作者对存储频道的成员资格 → 消息为 type=17 且 sender 是 bot 身份
// （信任模型 layer (c)；iwh_ webhook 发送者无事件消费端，D7 一并在此拒绝）→ 撤回/删除
// 门禁（已撤回 / 全局删除 / 操作者本地删除的卡片不可再触发动作，与单条读同口径）→
// D4 幂等 claim（已存在 claim 即 replay —— **先于生效帧校验**：已受理的动作在按钮被
// 重写移除后被重试时必须回 replay，而非撞 stale-frame 误判 400，P1-4）→ action_id
// 存在于「生效卡片」（content_edit 优先 —— 被重写移除按钮的**首次**迟到点击 fail-closed）
// → D11 inputs 校验 → 入队 + confirm。首次 claim 后任一校验失败均补偿释放 claim（纠正后
// 可重试）。防枚举：除成员资格（403 语义）外全部归并到单一 400 invalid，具体原因只进日志。

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
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

	// D3 ①②：anti-IDOR 频道绑定 + 存储频道成员资格（抽为共享门禁，与卡片修订查询
	// 复用同一口径，避免两端授权漂移）。
	msgM, handled := m.authorizeCardChannelMember(c, loginUID, req.MessageID, req.ChannelID, req.ChannelType,
		errcode.ErrMessageCardActionInvalid, errcode.ErrMessageCardActionDenied)
	if handled {
		return
	}

	// D3 ③sender 必须是当前有效 bot 身份（layer (c)）：robot.status=1 或
	// app_bot.status=1。这里每次首击都实时解析，不使用展示缓存，确保 App Bot
	// unpublish/revoke 立即阻止副作用。iwh_ webhook 没有事件消费端、人类发送者也
	// 没有权威 bot 行，均 fail-closed。
	if m.botIdentity == nil {
		m.Error("bot identity resolver 未初始化", zap.String("fromUID", msgM.FromUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	senderIdentity, err := m.botIdentity.Resolve(msgM.FromUID)
	if err != nil {
		m.Error("查询发送者 bot 身份失败", zap.Error(err), zap.String("fromUID", msgM.FromUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if senderIdentity == nil {
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

	// D3 ④ canonical 可见性对齐（PR#548 review P1）：card/action 必须与单条读
	// respondSingleMessage 同口径 —— 否则被 visibles 排除 / 已清理历史的成员，虽过了
	// 成员+sender+revoke/删除门禁，仍能枚举可读的 message_id 触发不可见卡片的 bot
	// 副作用。上面已覆盖 revoke/is_deleted + 操作者本地删除；这里补 visibles 白名单
	// + 消息过期 + 用户清理偏移 + 频道偏移（抽为 cardCanonicalVisibleToViewer，与
	// card/revisions 共用同一口径）。查询失败 fail-closed 回 500；不可见归并单一 invalid
	// （防枚举）。
	if visible, verr := m.cardCanonicalVisibleToViewer(msgM, loginUID); verr != nil {
		m.Error("查询卡片动作目标消息可见性失败", zap.Error(verr), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if !visible {
		m.Warn("卡片动作目标消息对操作者不可见,拒绝", zap.String("messageID", req.MessageID), zap.String("uid", loginUID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D4 ⑤幂等 claim —— **先于生效帧 / inputs 校验**（P1-4, PR#548 review）。业务
	// 身份键（不含 client_token —— 新 token 重试不得二次触发）。已存在 claim（pending
	// 或已 confirm）→ 直接回 replay,绝不产生第二个事件。这必须先于下方 stale-frame
	// 校验:否则「已受理的动作在 bot 重写移除该按钮后被重试」会撞 stale-frame 而误判
	// 400，违反 D4「任何已 claim 的请求 → replay」承诺（D8 兜底假设「超时后 re-tap」,
	// 但按钮已随帧消失、无可再点）。首次 claim 成功后继续跑校验,任一失败都补偿释放
	// claim（releaseCardClaim），使纠正后的重试仍可重新 claim。
	// 边角(属 D4「200 ack 不保证已入队」既有语义)：并发下请求 B 撞上 A 尚未释放的首次
	// claim 时会先回 replay:true,而 A 因校验失败 400、未入队 —— 无虚假事件,靠 D8 超时
	// re-tap 自愈。
	idemKey := cardActionClaimKey(req.MessageID, req.ActionID, loginUID)
	claimed, err := m.cardClaims.Claim(idemKey)
	if err != nil {
		m.Error("卡片动作幂等 claim 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	if !claimed {
		// 已 confirm → replay（事件确已入队）；仅 pending → 首请求尚在处理、未确认入队，
		// 回可重试 409 而非虚假成功（PR#548 review P2：避免 A 校验失败释放后 B 拿着 A 的
		// pending 得到成功 ack 却无事件入队而丢有效动作 —— 客户端按 D8 超时重试自愈）。
		// Redis 读失败 → 保守当 replay（fail-safe：绝不因读失败重复入队，退化回旧行为）。
		confirmed, cerr := m.cardClaims.Confirmed(idemKey)
		if cerr != nil {
			m.Warn("card_action claim 状态读取失败,按 replay 处理", zap.Error(cerr), zap.String("messageID", req.MessageID))
			c.Response(map[string]interface{}{"accepted": true, "replay": true})
			return
		}
		if confirmed {
			c.Response(map[string]interface{}{"accepted": true, "replay": true})
			return
		}
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInProgress, nil, nil)
		return
	}

	// D3 ⑥action_id 必须存在于「生效卡片」：content_edit（最新帧）优先于原始 payload
	// —— 重写移除按钮的**首次**迟到点击在此 400（过期交互 fail-closed）；已 claim 的
	// 重试已在上方回了 replay,不会到这里。同时取回匹配动作的静态 data（D11：服务端从
	// 生效帧提取，绝不取请求里的 data）。首次校验失败释放 claim（纠正后可重试）。
	effective := msgM.Payload
	if extra != nil && extra.ContentEdit.Valid && extra.ContentEdit.String != "" {
		effective = []byte(extra.ContentEdit.String)
	}
	actionData, found := cardmsg.SubmitAction(effective, req.ActionID)
	if !found {
		m.releaseCardClaim(idemKey)
		m.Warn("action_id 不在生效卡片中,拒绝", zap.String("actionID", req.ActionID), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
		return
	}

	// D11 ⑦inputs 信任边界（round-3 P1-3）：只放行生效帧声明过的 Input.* id，逐
	// 类型校验 + 尺寸上限 —— event_data.inputs 从此形状可信（内容仍是不可信用户
	// 文本，bot 侧照常转义）。首次校验失败释放 claim。
	if err := cardmsg.ValidateInputs(effective, req.Inputs); err != nil {
		m.releaseCardClaim(idemKey)
		m.Warn("卡片动作 inputs 校验失败,拒绝", zap.Error(err), zap.String("messageID", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageCardActionInvalid, nil, nil)
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
		"action_id":    req.ActionID,
		"inputs":       req.Inputs,
		"operator_uid": loginUID,
		"client_token": req.ClientToken,
		"acted_at":     time.Now().Unix(),
	}
	// P1-3（PR#548 review）：space_id 取卡片的**权威来源 Space**（存储行/群表），
	// 不取操作者请求上下文的 Space —— 后者仅经成员校验,可能与卡片来源 Space 不同
	// (操作者同属 A、B 两 Space 时可用 X-Space-ID: B 点 A 的卡)。与 send 出口
	// "服务端权威、无权威值即 strip"同口径:取不到则省略键(fail-closed),绝不回退到
	// 客户端可影响的上下文 Space。
	if cardSpaceID := m.resolveCardOriginSpaceID(msgM); cardSpaceID != "" {
		eventData["space_id"] = cardSpaceID
	}
	if actionData != nil {
		eventData["data"] = actionData
	}
	eventID, err := m.robotService.EnqueueBotTypedEvent(msgM.FromUID, cardmsg.EventTypeCardAction, eventData)
	if err != nil {
		m.releaseCardClaim(idemKey)
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

// releaseCardClaim 补偿释放首次校验失败 / 入队失败的幂等 claim（P1-4：首次 claim
// 后任一失败都要释放，否则纠正后的重试会撞残留 claim 误判 replay）。删不掉只是残留
// 至多 60s pending，不影响正确性，记日志便于发现系统性 Redis 故障。
func (m *Message) releaseCardClaim(idemKey string) {
	if relErr := m.cardClaims.Release(idemKey); relErr != nil {
		m.Error("释放卡片动作幂等 claim 失败(残留至多 60s pending)", zap.Error(relErr), zap.String("key", idemKey))
	}
}

// fakeChannelContainsUID 校验 person 频道存储行的 fake channel id（"a@b"）是否
// 包含给定 uid —— 成员资格以存储行为准（D3 anti-IDOR）。
func fakeChannelContainsUID(fakeChannelID, uid string) bool {
	parts := strings.SplitN(fakeChannelID, "@", 2)
	return len(parts) == 2 && (parts[0] == uid || parts[1] == uid)
}

// authorizeCardChannelMember 执行卡片消息端点共享的 D3 ①②：anti-IDOR 频道绑定
// （按请求声明的频道查存储消息 —— 查得到即证明「声明频道 == 存储行的频道」）+ 存储
// 频道成员资格（person：会话双方之一；group/topic：ExistMemberActive 白名单单点查）。
// 成功返回存储消息；失败已写好 i18n 错误响应（绑定/不存在/非卡片 → invalidCode，
// 非成员 → deniedCode，防枚举）并返回 (nil, true=已处理)。cardAction 与卡片修订查询
// 共用本门禁，保证两端授权口径不漂移（PR-C brief 要求）。
func (m *Message) authorizeCardChannelMember(c *wkhttp.Context, loginUID, messageID, channelID string, channelType uint8, invalidCode, deniedCode codes.Code) (*messageModel, bool) {
	lookupChannelID := channelID
	switch channelType {
	case common.ChannelTypePerson.Uint8():
		lookupChannelID = common.GetFakeChannelIDWith(loginUID, channelID)
	case common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
		// group / topic：消息就存于声明频道本身。
	default:
		respondMessageRequestInvalid(c, "channel_type")
		return nil, true
	}
	msgM, err := m.db.queryMessageByID(lookupChannelID, channelType, messageID)
	if err != nil {
		m.Error("查询卡片消息失败", zap.Error(err), zap.String("messageID", messageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return nil, true
	}
	if msgM == nil || len(msgM.Payload) == 0 || !cardmsg.IsCardRawPayload(msgM.Payload) {
		httperr.ResponseErrorL(c, invalidCode, nil, nil)
		return nil, true
	}
	switch msgM.ChannelType {
	case common.ChannelTypePerson.Uint8():
		if !fakeChannelContainsUID(msgM.ChannelID, loginUID) {
			httperr.ResponseErrorL(c, deniedCode, nil, nil)
			return nil, true
		}
	default:
		groupNo := msgM.ChannelID
		var threadShortID string
		if msgM.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			parent, shortID, perr := thread.ParseChannelID(msgM.ChannelID)
			if perr != nil {
				respondMessageRequestInvalid(c, "channel_id")
				return nil, true
			}
			groupNo = parent
			threadShortID = shortID
		}
		// D3 ②群状态门禁(PR#548 review H1/P1-a)：复刻单条读 requireGroupMember
		// (api_message_get.go:184) —— 仅 GroupStatusNormal + Disband 可见,Disabled
		// (管理员禁用)及未来非正常状态 fail closed。只查成员资格会漏群状态：禁用群置
		// group.Status=Disabled 但成员行仍 status=Normal、ExistMemberActive 照样为 true
		// —— 于是被禁用群里的成员读路径已 404,却仍能枚举 message_id 触发副作用。归并单一
		// invalid(防枚举)。Disband 与读路径同口径放行。
		statusVisible, serr := m.groupStatusVisibleForAction(groupNo)
		if serr != nil {
			m.Error("查询群状态失败", zap.Error(serr), zap.String("groupNo", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return nil, true
		}
		if !statusVisible {
			m.Warn("卡片目标群非可见状态,拒绝", zap.String("groupNo", groupNo))
			httperr.ResponseErrorL(c, invalidCode, nil, nil)
			return nil, true
		}
		// D3 ②子区状态门禁(PR#548 review P2-a)：复刻单条读 getThreadMessage
		// (api_message_get.go:139) —— 已删除子区(ThreadStatusDeleted)按不存在处理;
		// 归档子区允许(读历史,与读路径同口径)。
		if threadShortID != "" {
			t, terr := m.threadDB.QueryByGroupNoAndShortID(groupNo, threadShortID)
			if terr != nil {
				m.Error("查询子区失败", zap.Error(terr), zap.String("groupNo", groupNo), zap.String("shortID", threadShortID))
				httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
				return nil, true
			}
			if t == nil || t.Status == thread.ThreadStatusDeleted {
				m.Warn("卡片目标子区已删除,拒绝", zap.String("groupNo", groupNo), zap.String("shortID", threadShortID))
				httperr.ResponseErrorL(c, invalidCode, nil, nil)
				return nil, true
			}
		}
		isMember, merr := m.groupService.ExistMemberActive(groupNo, loginUID)
		if merr != nil {
			m.Error("查询群成员失败", zap.Error(merr), zap.String("groupNo", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return nil, true
		}
		if !isMember {
			httperr.ResponseErrorL(c, deniedCode, nil, nil)
			return nil, true
		}
	}
	return msgM, false
}

// resolveCardOriginSpaceID 解析卡片的**权威来源 Space**（P1-3, PR#548 review）。
// card_action 事件的 space_id 必须来自卡片自身的存储权威,而非操作者请求上下文的
// Space —— SpaceMiddleware 只证明操作者是所声明 Space 的成员,不证明卡片属于该
// Space(操作者同属 A、B 两 Space 时可用 X-Space-ID: B 点 A 群/DM 的卡)。与 send
// 出口 enrichPayloadWithSpaceID 同口径:
//   - GROUP           → 群表 SpaceID
//   - COMMUNITY_TOPIC → 父群 SpaceID
//   - PERSONAL        → 发送时服务端注入进 payload 顶层的权威 space_id（DM 无 Space
//     路由,payload 是收端唯一可信信号源;见 enrich*PayloadWithSpaceID）
//
// 任一步取不到 → 返回 ""(fail-closed:调用方省略 event_data.space_id,与 send 路径
// 无权威值时 strip 客户端 space 的行为对齐;绝不回退到客户端可影响的上下文 Space)。
func (m *Message) resolveCardOriginSpaceID(msgM *messageModel) string {
	switch msgM.ChannelType {
	case common.ChannelTypeGroup.Uint8():
		return m.groupSpaceIDOrEmpty(msgM.ChannelID)
	case common.ChannelTypeCommunityTopic.Uint8():
		parent, err := m.resolveParentGroupNo(msgM.ChannelID)
		if err != nil || parent == "" {
			return ""
		}
		return m.groupSpaceIDOrEmpty(parent)
	case common.ChannelTypePerson.Uint8():
		var env struct {
			SpaceID string `json:"space_id"`
		}
		if err := json.Unmarshal(msgM.Payload, &env); err != nil {
			return ""
		}
		return env.SpaceID
	default:
		return ""
	}
}

// groupSpaceIDOrEmpty 查群的权威 SpaceID;任何错误 / 空群 / 空 SpaceID → ""。
func (m *Message) groupSpaceIDOrEmpty(groupNo string) string {
	g, err := m.groupService.GetGroupWithGroupNo(groupNo)
	if err != nil || g == nil {
		return ""
	}
	return g.SpaceID
}

// cardCanonicalVisibleToViewer 复刻单条读 respondSingleMessage 的「内容可见性」层
// (api_message_get.go)——成员资格 + 生命周期(revoke/删除)之外的第四类门禁:
// visibles 白名单 / 消息过期(Expire 秒 TTL) / 用户清理偏移 / 频道偏移。card/action
// 与 card/revisions 共用本 helper,防止两端可见性口径漂移(与 authorizeCardChannelMember
// 同理:被 visibles 排除或历史已清理的成员,虽过成员+生命周期门禁,仍不得读到不可见卡片
// 的内容/触发其副作用)。person 频道跳过偏移(单条读不经 respondSingleMessage、偏移语义
// 未确立;2 方 DM visibles 恒过,已由成员+生命周期兜住;也避免对已是 fake id 的 person
// 频道二次 fake 化)。返回 (true,nil)=可见;(false,nil)=对该 viewer 不可见(调用方决定
// invalid / 空列表);(false,err)=偏移查询失败(调用方 fail-closed 回 500)。
func (m *Message) cardCanonicalVisibleToViewer(msgM *messageModel, loginUID string) (bool, error) {
	if !visiblesAllows(msgM.Payload, loginUID) {
		return false, nil
	}
	if msgM.Expire > 0 && time.Now().Unix()-int64(msgM.Expire) >= int64(msgM.Timestamp) {
		return false, nil
	}
	if msgM.ChannelType == common.ChannelTypePerson.Uint8() {
		return true, nil
	}
	userOffset, oerr := m.channelOffsetDB.queryWithUIDAndChannel(loginUID, msgM.ChannelID, msgM.ChannelType)
	if oerr != nil {
		return false, oerr
	}
	if userOffset != nil && msgM.MessageSeq <= userOffset.MessageSeq {
		return false, nil
	}
	channelOffsetSeq, cerr := m.lookupChannelOffsetSeq(msgM.ChannelID, msgM.ChannelType, loginUID)
	if cerr != nil {
		return false, cerr
	}
	if channelOffsetSeq != 0 && msgM.MessageSeq <= channelOffsetSeq {
		return false, nil
	}
	return true, nil
}

// groupStatusVisibleForAction 复刻单条读 requireGroupMember 的群状态门禁
// (api_message_get.go:184)：仅 GroupStatusNormal + GroupStatusDisband 可见,
// Disabled(管理员禁用)及未来非正常状态 fail closed。刻意用 groupDB.QueryWithGroupNo
// (与 requireGroupMember 同一读法)而非 groupService.GetGroupWithGroupNo：后者把「群不存在」
// 也返回成 error,会被误判成 ErrMessageQueryFailed;QueryWithGroupNo 群不存在返回 (nil,nil)
// → 判不可见(归并 invalid,与读路径 g==nil→NotFound 同口径),仅真实 DB 错误才 (false,err)
// → 调用方回 ErrMessageQueryFailed(fail-closed:安全门禁查询失败绝不放行)。
func (m *Message) groupStatusVisibleForAction(groupNo string) (bool, error) {
	g, err := m.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		return false, err
	}
	if g == nil || (g.Status != group.GroupStatusNormal && g.Status != group.GroupStatusDisband) {
		return false, nil
	}
	return true, nil
}

// isCardMessageWithdrawn 报告一张卡片是否已从可见消息面回收 —— 已撤回
// (message_extra.revoke) / 全局删除 (message_extra.is_deleted) / 操作者本地删除
// (message_user_extra.message_is_deleted)。与单条读 api_message_get.go:241 同口径。
//
// 动作端点(cardAction ④)内联了同一三项检查(那里顺带复用已查出的 extra 取生效帧,
// 避免二次查询);查询端点(getCardRevisions)调用本 helper 做可见性兜底 —— 两处必须
// 保持同步:动作与历史内容都不得在卡片被回收后仍可达(D10「revoked message leaves
// no queryable content history」;且不依赖 revoke 时 best-effort 删除是否成功)。
func (m *Message) isCardMessageWithdrawn(messageID, loginUID string) (bool, error) {
	extra, err := m.messageExtraDB.queryWithMessageID(messageID)
	if err != nil {
		return false, err
	}
	if extra != nil && (extra.Revoke == 1 || extra.IsDeleted == 1) {
		return true, nil
	}
	userExtras, err := m.messageUserExtraDB.queryWithMessageIDsAndUID([]string{messageID}, loginUID)
	if err != nil {
		return false, err
	}
	if len(userExtras) > 0 && userExtras[0].MessageIsDeleted == 1 {
		return true, nil
	}
	return false, nil
}
