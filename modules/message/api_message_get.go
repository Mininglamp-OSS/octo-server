package message

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// threadFeatureEnabled 与 modules/thread/1module.go 中 init() 的判定保持一致：
// DM_THREAD_ON=true 或 1 启用，其它（含未设置）禁用。
func threadFeatureEnabled() bool {
	v := strings.ToLower(os.Getenv("DM_THREAD_ON"))
	return v == "true" || v == "1"
}

// visiblesAllows 直接从原始 payload 字节解析 visibles 字段，判定 loginUID 是否允许查看。
//
// 解析行为：
//   - payload 不是 JSON 或没有 visibles → 允许（无白名单约束）
//   - visibles 是空数组 → 允许（与 from() 行为一致：仅在 len > 0 时才生效）
//   - visibles 非空 → 仅当 loginUID 命中其中一个字符串元素时允许
//
// 用 struct + RawMessage 而非 map[string]interface{}：encoding/json 仍线性扫描整个
// payload（成本 O(len(payload))），但只把 visibles 元素留下来，其它字段不建 map / 不
// 二次解码，对 1MB+ payload 的内存峰值显著低于 map 路径。
// 非字符串元素静默忽略（与 from() 的 if limitUID == loginUID 行为一致：
// 类型断言失败时不算命中）。
func visiblesAllows(rawPayload []byte, loginUID string) bool {
	var v struct {
		Visibles []json.RawMessage `json:"visibles"`
	}
	if err := json.Unmarshal(rawPayload, &v); err != nil {
		// payload 损坏或非对象时不约束（与 from() 走 placeholder 后等价：无白名单字段）
		return true
	}
	if len(v.Visibles) == 0 {
		return true
	}
	for _, raw := range v.Visibles {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		if s == loginUID {
			return true
		}
	}
	return false
}

func msgModelToMessageResp(m *messageModel) *config.MessageResp {
	resp := &config.MessageResp{
		MessageID:   m.MessageID,
		MessageSeq:  m.MessageSeq,
		ClientMsgNo: m.ClientMsgNo,
		Setting:     m.Setting,
		FromUID:     m.FromUID,
		ChannelID:   m.ChannelID,
		ChannelType: m.ChannelType,
		Timestamp:   int32(m.Timestamp),
		Payload:     m.Payload,
		Expire:      m.Expire,
	}
	// messageModel.Header 是 IM 写入时落库的 JSON 串（no_persist/red_dot/sync_once）。
	// 不解析的话单条响应这三个字段永远是 0，与 syncChannelMessage 行为不一致。
	// 解析失败保持零值即可，不阻断响应。
	if m.Header != "" {
		_ = json.Unmarshal([]byte(m.Header), &resp.Header)
	}
	return resp
}

// getGroupMessage GET /v1/groups/:group_no/messages/:message_id
func (m *Message) getGroupMessage(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	messageIDStr := c.Param("message_id")
	loginUID := c.GetLoginUID()

	if !thread.IsValidGroupNo(groupNo) {
		respondMessageRequestInvalid(c, "group_no")
		return
	}
	messageID, ok := parsePositiveMessageID(messageIDStr)
	if !ok {
		respondMessageRequestInvalid(c, "message_id")
		return
	}
	if !m.requireGroupMember(c, groupNo, loginUID) {
		return
	}
	m.respondSingleMessage(c, groupNo, common.ChannelTypeGroup.Uint8(), groupNo, messageID, loginUID)
}

// getThreadMessage GET /v1/groups/:group_no/threads/:short_id/messages/:message_id
func (m *Message) getThreadMessage(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	messageIDStr := c.Param("message_id")
	loginUID := c.GetLoginUID()

	if !thread.IsValidGroupNo(groupNo) {
		respondMessageRequestInvalid(c, "group_no")
		return
	}
	if !thread.IsValidShortID(shortID) {
		respondMessageRequestInvalid(c, "short_id")
		return
	}
	messageID, ok := parsePositiveMessageID(messageIDStr)
	if !ok {
		respondMessageRequestInvalid(c, "message_id")
		return
	}
	if !m.requireGroupMember(c, groupNo, loginUID) {
		return
	}
	t, err := m.threadDB.QueryByGroupNoAndShortID(groupNo, shortID)
	if err != nil {
		m.Error("查询子区失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	// 与 IM datasource (modules/thread/1module.go:87)、ExistByGroupNoAndShortID
	// (modules/thread/db.go:211)、service.GetThread (modules/thread/service.go:474)
	// 行为对齐：仅拒 ThreadStatusDeleted；归档子区允许访问历史消息（归档后仍可发
	// 消息，发完自动解档）。
	// body 用 "message not found"（与其它 404 一致）避免泄露 thread 是否存在的信号。
	if t == nil || t.Status == thread.ThreadStatusDeleted {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	channelID := thread.BuildChannelID(groupNo, shortID)
	m.respondSingleMessage(c, channelID, common.ChannelTypeCommunityTopic.Uint8(), groupNo, messageID, loginUID)
}

// parsePositiveMessageID 校验 path 参数 message_id 为正整数。
// 注：WuKongIM 雪花算法生成的 message_id 始终为正，0 视为非法。
func parsePositiveMessageID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// requireGroupMember 校验登录用户是该群的活跃成员（未删除、未被拉黑），
// 并且群本身仍处于活跃状态。失败时 handler 已写入响应；返回 false 表示终止。
//
// 群不存在、群已解散、非成员、被拉黑成员一律返回 404 而不是 403/400：
// 直查接口不能泄露"资源存在但你不可达"的信号，与撤回 / 删除 / visibles 等
// "对当前用户不可见"状态保持同一返回码。
//
// 关键差异说明：
//   - 群解散后 group_member 记录不会清理（见 modules/group/api.go disband 流程），
//     仅 group.status 置为 GroupStatusDisband，所以必须额外查群状态；
//   - 黑名单（group_member.status=GroupMemberStatusBlacklist）的 is_deleted 仍为 0，
//     普通 ExistMember 返回 true。本接口绕过 WuKongIM 直查本地分表，必须用
//     ExistMemberActive 显式排除黑名单，否则被拉黑用户仍能按 ID 把消息读出来
//     （IM 路径有 datasource blacklist 拦截，本路径必须自己拦）。
func (m *Message) requireGroupMember(c *wkhttp.Context, groupNo, loginUID string) bool {
	g, err := m.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		m.Error("查询群信息失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return false
	}
	// 白名单语义：只有 GroupStatusNormal 允许通过；Disband / Disabled / 未来新增的
	// 非正常状态一律 fail closed。与 ExistMemberActive 的 status=Normal 一致。
	if g == nil || g.Status != group.GroupStatusNormal {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return false
	}
	isActive, err := m.groupDB.ExistMemberActive(loginUID, groupNo)
	if err != nil {
		m.Error("检查群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return false
	}
	if !isActive {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return false
	}
	return true
}

// respondSingleMessage 共用：按 (channelID, channelType, messageID) 查本地消息正文，
// 拼装 message_extra / message_user_extra / reactions，并复用 syncChannelMessage 的
// channelOffset / 群消息 enrichment（thread_created 快照、外部成员标识），
// 行为与批量同步路径保持一致。
//
// 任何"对当前用户不可见"的状态（撤回 / 双删 / 用户级删 / channel_offset 截断 /
// expire / visibles 白名单未命中）一律返回 404，避免直查接口绕过批量路径的过滤。
//
// groupNoForEnrich 仅在群消息（channelType=Group）时用作 enrichExternalMarkers 的
// 父群定位参数；子区消息这一参数无效（enrich 函数本身按消息 channel_type 分派）。
func (m *Message) respondSingleMessage(c *wkhttp.Context, channelID string, channelType uint8, groupNoForEnrich string, messageID int64, loginUID string) {
	// VARCHAR(20) 列必须用字符串绑定才能命中索引；详见 queryMessageByID 注释。
	messageIDStr := strconv.FormatInt(messageID, 10)
	msgModel, err := m.db.queryMessageByID(channelID, channelType, messageIDStr)
	if err != nil {
		m.Error("查询消息失败", zap.Error(err), zap.String("channel_id", channelID), zap.Int64("message_id", messageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if msgModel == nil {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	// visibles 白名单服务端判定：直接基于原始 payload 解析，不依赖 from() 截断后的 map。
	// 必要原因：from() 走 TruncatedPayload，payload > hardParsePayloadLimit (1MB) 时
	// 会替换为 placeholder（不含 visibles 字段），导致 from() 内部的 visibles 检查
	// 拿不到字段、IsDeleted 不会被置 1，依赖"from() 后兜底 IsDeleted"的策略失效，
	// 非白名单成员能拿到该消息的元数据。原始 payload 解析在所有大小下都正确。
	if !visiblesAllows(msgModel.Payload, loginUID) {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	extra, userExtra, reactions, err := m.fetchMessageExtras(messageID, loginUID)
	if err != nil {
		m.Error("查询消息扩展失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if extra != nil && (extra.Revoke == 1 || extra.IsDeleted == 1) {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}
	if userExtra != nil && userExtra.MessageIsDeleted == 1 {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	// 用户级历史清理偏移：/v1/message/offset 写入 channel_offset(uid, channel_id,
	// channel_type, message_seq)。syncChannelMessage 会在 newSyncChannelMessageResp
	// 内按此跳过 message_seq <= offset 的消息；单条直查必须显式比对，否则用户清理
	// 后还能按 ID 把旧消息读回来。注意这与 lookupChannelOffsetSeq 查的频道级
	// channel_setting.offset_message_seq 是两张不同的表 / 两套语义，必须都查。
	if userOffset, err := m.channelOffsetDB.queryWithUIDAndChannel(loginUID, channelID, channelType); err != nil {
		m.Error("查询用户清理偏移失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if userOffset != nil && msgModel.MessageSeq <= userOffset.MessageSeq {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	channelOffsetSeq, err := m.lookupChannelOffsetSeq(channelID, channelType, loginUID)
	if err != nil {
		m.Error("查询频道设置失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	resp := &MsgSyncResp{}
	resp.from(msgModelToMessageResp(msgModel), loginUID, extra, userExtra, reactions, channelOffsetSeq)

	// 与 syncChannelMessage 群路径保持一致：补 ThreadCreated 实时快照和外部成员标识。
	// Person/CommunityTopic 不进入这两个 enrichment（函数内部各自判断 channel_type）。
	if channelType == common.ChannelTypeGroup.Uint8() {
		msgs := []*MsgSyncResp{resp}
		m.enrichThreadCreatedMessages(msgs)
		m.enrichExternalMarkers(groupNoForEnrich, msgs)
	}

	// from() 在 visibles 白名单未命中 / expire / channelOffset 截断时会把 IsDeleted
	// 置 1。批量同步把 IsDeleted=1 留给客户端过滤；单条直查不能依赖客户端，
	// 否则相当于把"白名单消息"在 200 响应里整段下发给非授权用户。
	if resp.IsDeleted == 1 {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}

	c.Response(resp)
}

// fetchMessageExtras 一次拉取 (message_extra, message_user_extra, reactions)。
// 任意一步 DB 错误时返回 wrap 后的错误，由调用方决定响应。
func (m *Message) fetchMessageExtras(messageID int64, loginUID string) (*messageExtraDetailModel, *messageUserExtraModel, []*reactionModel, error) {
	idList := []string{strconv.FormatInt(messageID, 10)}

	extras, err := m.messageExtraDB.queryWithMessageIDsAndUID(idList, loginUID)
	if err != nil {
		m.Error("查询消息扩展失败", zap.Error(err))
		return nil, nil, nil, errors.Wrap(err, "查询消息扩展失败")
	}
	var extra *messageExtraDetailModel
	if len(extras) > 0 {
		extra = extras[0]
	}

	userExtras, err := m.messageUserExtraDB.queryWithMessageIDsAndUID(idList, loginUID)
	if err != nil {
		m.Error("查询用户级扩展失败", zap.Error(err))
		return nil, nil, nil, errors.Wrap(err, "查询用户级扩展失败")
	}
	var userExtra *messageUserExtraModel
	if len(userExtras) > 0 {
		userExtra = userExtras[0]
	}

	reactions, err := m.messageReactionDB.queryWithMessageIDs(idList)
	if err != nil {
		m.Error("查询消息反应失败", zap.Error(err))
		return nil, nil, nil, errors.Wrap(err, "查询消息反应失败")
	}
	return extra, userExtra, reactions, nil
}

// lookupChannelOffsetSeq 复用 syncChannelMessage 的 channel offset 查询，私聊用 fakeChannelID。
func (m *Message) lookupChannelOffsetSeq(channelID string, channelType uint8, loginUID string) (uint32, error) {
	lookupID := channelID
	if channelType == common.ChannelTypePerson.Uint8() {
		lookupID = common.GetFakeChannelIDWith(channelID, loginUID)
	}
	settings, err := m.channelService.GetChannelSettings([]string{lookupID})
	if err != nil {
		return 0, err
	}
	if len(settings) > 0 && settings[0].OffsetMessageSeq > 0 {
		return settings[0].OffsetMessageSeq, nil
	}
	return 0, nil
}
