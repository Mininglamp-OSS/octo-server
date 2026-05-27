package message

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/channel"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"github.com/pkg/errors"
	"github.com/sendgrid/rest"
	"go.uber.org/zap"
)

// LargePayloadThreshold caller 用此字节阈值决定是否调用 TruncatedPayload 走类型
// 感知的截断流程；不是真正的 payload 上限。历史背景：issue #1097 中 Bot 把嵌套
// JSON 对象塞进 type=Text 的 content，前端按 string 解析时递归 JSON.parse 爆栈。
// CoerceTextPayloadContent 已防御性把 Text content 规约为 string，根因消除后
// 只对 Text 按 rune 截（issue #1310），其它类型 content 携带结构化数据，原样
// 下发不截断。
// hardParsePayloadLimit 更高一级的硬上限：超过则不再尝试 JSON 解析，直接占位。
const (
	LargePayloadThreshold  = 10 * 1024
	hardParsePayloadLimit  = 1 * 1024 * 1024
	TextContentMaxRunes    = 4000
	truncatedContentSuffix = "...[消息过大]"
)

// truncatedFallback 极端场景下（解析失败 / 无 content 字段 / 超过硬上限）的占位。
func truncatedFallback(m map[string]interface{}) map[string]interface{} {
	safe := map[string]interface{}{
		"content": truncatedContentSuffix,
	}
	if t, ok := m["type"]; ok {
		safe["type"] = t
	} else {
		safe["type"] = common.ContentError.Int()
	}
	if v, ok := m["visibles"]; ok {
		safe["visibles"] = v
	}
	return safe
}

// TruncatedPayload 仅对 type=Text (=1) 按 rune 数截 content（issue #1310）；
// 其它类型（媒体 Image/Voice/Video/File、富文本 RichText、群通知/客服等系统消息）
// content 携带结构化关键信息，按字节切片会破坏前端解析，全部原样下发。
//
// 仅在以下场景产生占位：
//   - 超过 1MB 硬上限（hardParsePayloadLimit）
//   - JSON 解析失败或得到空 map
//
// Text 类型 content 已被 CoerceTextPayloadContent 规约为 string，无递归解码风险。
//
// 内部对 raw 反序列化产生的 map 进行就地修改，调用方拿到的返回值即同一个 map；
// 由于 raw []byte 来自 caller，map 是 TruncatedPayload 自己 Unmarshal 出来的，
// 不存在外部别名引用，因此就地修改是安全的。
//
// 导出供 search 等其他路径复用。
func TruncatedPayload(raw []byte) map[string]interface{} {
	if len(raw) > hardParsePayloadLimit {
		return placeholderPayload()
	}
	var m map[string]interface{}
	if err := util.ReadJsonByByte(raw, &m); err != nil || len(m) == 0 {
		return placeholderPayload()
	}
	CoerceTextPayloadContent(m)
	if isTextType(m) {
		return truncateTextPayload(m)
	}
	return m
}

func placeholderPayload() map[string]interface{} {
	return map[string]interface{}{
		"type":    common.ContentError.Int(),
		"content": truncatedContentSuffix,
	}
}

// isTextType 判断 payload type 是否为 common.Text（=1）。兼容 json.Number / float64 / int
// 几种反序列化结果；string 类型的 "1" 不识别为 Text，避免误命中。
func isTextType(m map[string]interface{}) bool {
	switch v := m["type"].(type) {
	case float64:
		return int(v) == common.Text.Int()
	case int:
		return v == common.Text.Int()
	case json.Number:
		i, err := v.Int64()
		return err == nil && int(i) == common.Text.Int()
	}
	return false
}

// truncateTextPayload 仅对 content 按 rune 数截断，其它字段原样保留。
// 前置约束：CoerceTextPayloadContent 已保证 content 为 string。
func truncateTextPayload(m map[string]interface{}) map[string]interface{} {
	s, ok := m["content"].(string)
	if !ok {
		// CoerceTextPayloadContent 已确保 string；防御性兜底为占位。
		return truncatedFallback(m)
	}
	if utf8.RuneCountInString(s) <= TextContentMaxRunes {
		return m
	}
	m["content"] = truncateRunes(s, TextContentMaxRunes) + truncatedContentSuffix
	return m
}

// CoerceTextPayloadContent 对 type=Text 的消息把 content 字段强制规约为字符串。
// 正常客户端 content 本就是 string；兼容 bot 等误把嵌套 object 塞进 content
// 的场景（见 issue #1097），避免前端按 string 解析时崩溃。
func CoerceTextPayloadContent(m map[string]interface{}) {
	if len(m) == 0 {
		return
	}
	// 初始化为 -1 表示未识别类型，避免 common.Text 若为 0 时的隐式误匹配。
	t := -1
	switch v := m["type"].(type) {
	case float64:
		t = int(v)
	case int:
		t = v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			t = int(i)
		}
	}
	if t != common.Text.Int() {
		return
	}
	c, exists := m["content"]
	if !exists {
		return
	}
	if _, ok := c.(string); ok {
		return
	}
	m["content"] = contentToString(c)
}

func contentToString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		if b, err := json.Marshal(x); err == nil {
			return string(b)
		}
		return ""
	}
}

// truncateRunes 按 rune（字符）数上限截断，确保中英文等长度感知一致。
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i]
		}
		count++
	}
	return s
}

// Message 消息相关API
type Message struct {
	ctx *config.Context
	log.Log
	db                  *DB
	messageReactionDB   *messageReactionDB
	userDB              *user.DB
	messageExtraDB      *messageExtraDB
	memberReadedDB      *memberReadedDB
	channelOffsetDB     *channelOffsetDB
	deviceOffsetDB      *deviceOffsetDB
	conversationExtradb *conversationExtraDB
	messageUserExtraDB  *messageUserExtraDB
	remindersDB         *remindersDB
	pinnedDB            *pinnedDB
	userService         user.IService
	groupService        group.IService
	// robotService 仅用于 GetCreatorUID (YUJ-60 允许 bot 创建者撤回自己 bot 发的消息)。
	robotService   robot.IService
	commonService  commonapi.IService
	fileService    file.IService
	channelService chservice.IService
	threadDB       *thread.DB
	// groupDB: 直查 group 表，区分"群不存在"和"群已解散"两种 404 情况，
	// groupService.GetGroupWithGroupNo 把 nil 也包成 error 不便分辨。
	groupDB  *group.DB
	mutex    sync.Mutex
	stopChan chan struct{}
	// reminderSeqOverride lets unit tests stub the version generator
	// used by getReminders so the matrix helpers can run without
	// standing up the seq table / MySQL. Production path: nil →
	// ctx.GenSeq(common.RemindersKey) runs. Tests inject a
	// deterministic counter so the matrix tests in
	// api_reminders_test.go don't need a live DB. See nextReminderSeq.
	//
	// Scope: getReminders + reminderDone only (everything wired through
	// nextReminderSeq). cancelMentionReminderIfNeed and other in-tree
	// callers still call ctx.GenSeq directly — those paths are not
	// exercised by the matrix suite, so widening the seam there is
	// deliberately deferred to keep the diff minimal.
	reminderSeqOverride func() (int64, error)
}

// New New
func New(ctx *config.Context) *Message {

	m := &Message{

		ctx:                 ctx,
		Log:                 log.NewTLog("Message"),
		db:                  NewDB(ctx),
		userDB:              user.NewDB(ctx),
		messageExtraDB:      newMessageExtraDB(ctx),
		groupService:        group.NewService(ctx),
		memberReadedDB:      newMemberReadedDB(ctx),
		conversationExtradb: newConversationExtraDB(ctx),
		messageReactionDB:   newMessageReactionDB(ctx),
		messageUserExtraDB:  newMessageUserExtraDB(ctx),
		channelOffsetDB:     newChannelOffsetDB(ctx),
		deviceOffsetDB:      newDeviceOffsetDB(ctx.DB()),
		remindersDB:         newRemindersDB(ctx),
		pinnedDB:            newPinnedDB(ctx),
		userService:         user.NewService(ctx),
		// robotService: 只读 robot 服务，用于 hasRevokePermission 判断 bot 所有者。
		robotService:   robot.NewService(ctx),
		commonService:  commonapi.NewService(ctx),
		fileService:    file.NewService(ctx),
		channelService: channel.NewService(ctx),
		threadDB:       thread.NewDB(ctx),
		groupDB:        group.NewDB(ctx),
		stopChan:       make(chan struct{}),
	}
	m.ctx.AddEventListener(event.GroupMemberAdd, m.handleGroupMemberAddEvent)
	m.ctx.AddEventListener(event.GroupMemberScanJoin, m.handleGroupMemberScanJoinEvent)
	return m
}

// Route 路由配置
func (m *Message) Route(r *wkhttp.WKHttp) {
	// UID 限流：所有认证路由组共享同一桶（详见 SharedUIDRateLimiter 注释）
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, m.ctx)
	// SpaceMiddleware 对齐 /v1/conversation：opt-in，未声明 X-Space-ID / space_id
	// query 时直接放行；一旦声明就做成员校验并把 validated spaceID 写入 gin
	// context。syncChannelMessage 的 Person 过滤（YUJ-219-A §4.1）因此读取 context
	// 值而不是 raw header，防止任何 authenticated client 指定别人 Space 的
	// X-Space-ID 来撞 SystemBot 历史消息（YUJ-226 / PR#1284 lml P1-1）。
	message := r.Group("/v1/message", m.ctx.AuthMiddleware(r), uidLimit, spacepkg.SpaceMiddleware(m.ctx))
	{

		message.POST("/sync", m.sync)                             // 同步消息 (写模式才用到 TODO：此方法未来将弃用)
		message.POST("/syncack/:last_message_seq", m.syncack)     // 同步消息回执 （写模式才用到 TODO：此方法未来将弃用）
		message.DELETE("", m.delete)                              // 删除消息
		message.DELETE("/mutual", m.mutualDelete)                 // 双向删除消息
		message.POST("/revoke", m.revoke)                         // 撤回消息
		message.POST("/offset", m.offset)                         // 清除某频道消息
		message.PUT("/voicereaded", m.voiceReaded)                // 语音消息设置为已读
		message.POST("/search", m.search)                         // 消息搜索
		message.POST("/typing", m.typing)                         // 发送typing消息
		message.POST("/channel/sync", m.syncChannelMessage)       // 同步频道消息
		message.POST("/extra/sync", m.syncMessageExtra)           // 同步消息扩展
		message.POST("/readed", m.messageReaded)                  // 消息已读
		message.GET("/sync/sensitivewords", m.syncSensitiveWords) // 同步敏感词
		message.POST("/edit", m.messageEdit)                      // 消息编辑
		message.POST("/reminder/sync", m.reminderSync)            // 同步提醒
		message.POST("/reminder/done", m.reminderDone)            // 提醒已处理完成
		message.GET("/prohibit_words/sync", m.syncProhibitWords)  // 同步违禁词
		message.POST("/pinned", m.pinnedMessage)                  // 置顶消息
		message.POST("/pinned/sync", m.syncPinnedMessage)         // 同步置顶消息
		message.POST("/pinned/clear", m.clearPinnedMessage)       // 删除所有置顶消息
		message.POST("/channel/files", m.channelFiles)            // 频道文件聚合
	}
	messages := r.Group("/v1/messages", m.ctx.AuthMiddleware(r), uidLimit)
	{
		// messages.PUT("/:message_id/voicereaded", m.voiceReaded)
		messages.GET("/:message_id/receipt", m.messageReceiptList) // 消息回执列表
	}
	// 回应
	reactions := r.Group("/v1/reactions", m.ctx.AuthMiddleware(r), uidLimit)
	{
		reactions.POST("", m.addOrCancelReaction) // 添加或取消回应
	}
	reaction := r.Group("/v1/reaction", m.ctx.AuthMiddleware(r), uidLimit)
	{
		reaction.POST("/sync", m.syncReaction)
	}
	msg := r.Group("/v1/message", m.ctx.AuthMiddleware(r), uidLimit, spacepkg.SpaceMiddleware(m.ctx))
	{
		msg.POST("/send", m.sendMsg) // 代发消息
	}
	// 单条消息查询（Discord-style）
	groups := r.Group("/v1/groups", m.ctx.AuthMiddleware(r), uidLimit)
	{
		groups.GET("/:group_no/messages/:message_id", m.getGroupMessage)
		// thread 路由与 modules/thread/1module.go 同一 feature flag 对齐：
		// DM_THREAD_ON 关闭时 thread 模块不注册、thread 表迁移不跑，
		// 此时若仍注册 GET 路由会让请求落到不存在的 thread 表上。
		if threadFeatureEnabled() {
			groups.GET("/:group_no/threads/:short_id/messages/:message_id", m.getThreadMessage)
		}
	}
	m.ctx.AddMessagesListener(m.listenerMessages) // 监听消息
	m.syncMessageReadedCount()
}

func (m *Message) sendMsg(c *wkhttp.Context) {
	if !m.ctx.GetConfig().Message.SendMessageOn {
		c.ResponseError(errors.New("不支持代发消息"))
		return
	}
	var req struct {
		Token              string                 `json:"token"`                // 发送者
		ReceiveChannelID   string                 `json:"receive_channel_id"`   // 接受者id
		ReceiveChannelType uint8                  `json:"receive_channel_type"` // 接受类型
		Payload            map[string]interface{} `json:"payload"`              // 消息体
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("数据格式有误！", err)
		return
	}
	if req.Token == "" {
		c.ResponseError(errors.New("发送者token不能为空"))
		return
	}
	if req.ReceiveChannelID == "" {
		c.ResponseError(errors.New("接受channelID不能为空"))
		return
	}
	if req.Payload == nil {
		c.ResponseError(errors.New("消息不能为空"))
		return
	}
	raw, err := m.ctx.Cache().Get(m.ctx.GetConfig().Cache.TokenCachePrefix + req.Token)
	if err != nil {
		m.Error("解析token错误", zap.Error(err))
		c.ResponseError(errors.New("解析token错误"))
		return
	}
	if strings.TrimSpace(raw) == "" {
		c.ResponseError(errors.New("请先登录"))
		return
	}
	info, decodeErr := auth.Decode(raw)
	if decodeErr != nil {
		c.ResponseError(errors.New("token错误"))
		return
	}
	uid := info.UID
	if uid == "" {
		c.ResponseError(errors.New("发送者不能为空"))
		return
	}

	if req.ReceiveChannelType == common.ChannelTypePerson.Uint8() {
		spaceID, peerID := spacepkg.ParseChannelID(req.ReceiveChannelID)
		if spaceID != "" {
			// Space 模式：校验双方都是 Space 成员
			bothOk, err := spacepkg.CheckBothMembers(m.ctx.DB(), spaceID, uid, peerID)
			if err != nil {
				m.Error("校验 Space 成员关系错误", zap.Error(err))
				c.ResponseError(errors.New("校验成员关系错误"))
				return
			}
			if !bothOk {
				c.ResponseError(errors.New("对方不在该 Space 内"))
				return
			}
		} else {
			// 个人空间模式（兼容）：检查好友关系
			sendUserIsFriend, err := m.userService.IsFriend(uid, req.ReceiveChannelID)
			if err != nil {
				m.Error("查询发送者与接受者好友关系错误", zap.Error(err))
				c.ResponseError(errors.New("查询好友关系错误"))
				return
			}
			if !sendUserIsFriend {
				c.ResponseError(errors.New("发送者与接受者不是好友"))
				return
			}
			recvUserIsFriend, err := m.userService.IsFriend(req.ReceiveChannelID, uid)
			if err != nil {
				m.Error("查询接受者与发送者好友关系错误", zap.Error(err))
				c.ResponseError(errors.New("查询接受者与发送者好友关系错误"))
				return
			}
			if !recvUserIsFriend {
				c.ResponseError(errors.New("接受者与发送者不是好友"))
				return
			}
		}
	}
	if req.ReceiveChannelType == common.ChannelTypeGroup.Uint8() {
		isExist, err := m.groupService.ExistMember(req.ReceiveChannelID, uid)
		if err != nil {
			m.Error("查询发送者是否在群内错误", zap.Error(err))
			c.ResponseError(errors.New("查询发送者是否在群内错误"))
			return
		}
		if !isExist {
			c.ResponseError(errors.New("未在群内"))
			return
		}
	}
	// YUJ-644 / Mininglamp-OSS#33: 把 SpaceMiddleware 已校验的发送方 SpaceID 透传给
	// sendMessage，作为 PERSONAL DM 的权威 space_id 注入源（不信客户端 body）。
	senderSpaceID := spacepkg.GetSpaceID(c)
	err = m.sendMessage(req.ReceiveChannelID, req.ReceiveChannelType, uid, req.Payload, senderSpaceID)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// sendMessage 派发消息。senderSpaceID 是 SpaceMiddleware 已校验的发送方 SpaceID
// （来自 X-Space-ID / query），用于 PERSONAL 路径的服务端权威 space_id 注入
// （YUJ-644 / Mininglamp-OSS#33）。空串 senderSpaceID 表示发送方未声明 Space
// （非 Space 模式 / 老客户端兼容），PERSONAL 走老 passthrough 行为。
func (m *Message) sendMessage(channelID string, channelType uint8, fromUID string, payload map[string]interface{}, senderSpaceID string) error {
	// PR#82 R8 (Jerry-Xin 2026-05-19 review on head 244fe9fa): strip any
	// reserved `__obo_*` top-level key from the user-supplied payload
	// BEFORE persistence/dispatch. See sanitizeUserIngressPayload below
	// for the full rationale and unit test surface.
	sanitizeUserIngressPayload(payload, channelID, channelType, fromUID, m.Warn)
	// YUJ-202 / Mininglamp-OSS#94 / #142 — mention pass-through chokepoint.
	// The original Plan X §5 design (docs/2026-05-mention-all-chokepoint-audit.md)
	// rewrote legacy `mention.all=1` to also carry `mention.ais=1` so
	// legacy `@所有人` traffic auto-fanned-out to every AI bot without
	// an SDK update. Mininglamp-OSS/octo-server#142 reverted that
	// inference: legacy `@所有人` MUST NOT trigger bots, so the helper
	// is now a pass-through that preserves whatever `mention.*` shape
	// the client sent. `mention.all`, `mention.humans`, `mention.ais`,
	// and `mention.uids` are all forwarded untouched. The call site is
	// preserved (rather than removed) so any future re-introduction of
	// chokepoint normalization has a single home, and downstream
	// consumers (OBO fan-out, robot dispatch, reminder fan-in) keep the
	// same payload-shape contract. Helper is idempotent and safe on
	// nil / malformed mention shapes — see pkg/mentionrewrite/rewrite.go
	// for the contract.
	payload = RewriteMention(payload)
	// YUJ-219-A / GH#1283 (analysis-report.md §4.5 / §7.4)：
	// 派发前为消息 payload 注入权威 space_id，让客户端 SpaceFilter 拿到可信字段，
	// race 窗口的 fail-open 语义可降级为 fail-closed。
	// YUJ-644：扩展到 PERSONAL（DM）—— 发送方 SpaceMiddleware 已校验的 SpaceID
	// 直接覆盖客户端 payload.space_id，跨 Space 推送时收端 SpaceFilter 拿到权威值
	// 立刻丢弃，不再依赖 channelInfo 缓存命中。
	payload = m.enrichPayloadWithSpaceID(channelID, channelType, payload, senderSpaceID)
	// Mininglamp-OSS/octo-server#144 + PR#145 review follow-up:
	// second-pass mention chokepoint. When mention.ais=1 in a GROUP
	// channel, expand mention.uids to include every bot member of the
	// channel so legacy adapter bots (octo-server#137) that only
	// inspect mention.uids over the WuKongIM websocket still recognise
	// the `@所有 AI` broadcast. PR #138's per-bot UID injection only
	// reaches the bot event queue (/v1/bot/events); this helper covers
	// the WuKongIM dispatch path.
	//
	// ⚠️ PR#145 review (Jerry-Xin / lml2468 / yujiawei 2026-05-23):
	// the expansion MUST run on a clone of `payload`, not on `payload`
	// itself. ExpandAisToBotUIDs mutates the inner `mention` sub-map
	// in place, and the in-memory `payload` is shared with the
	// reminder writer (modules/message/api_reminders.go iterates
	// `mention.uids` to emit one ReminderTypeMentionMe row per UID) —
	// so mutating `payload` here would create one human-visible
	// `[有人@我]` red-dot per server-expanded bot member. The clone is
	// used ONLY for the wire bytes; `payload` retains the original
	// caller-supplied `mention.uids`. See
	// pkg/mentionrewrite/clone.go for the clone contract and
	// pkg/mentionrewrite/expand_ais.go for the expansion contract.
	wirePayload := mentionrewrite.CloneForExpansion(payload)
	wirePayload = mentionrewrite.ExpandAisToBotUIDs(wirePayload, channelType, channelID, m.fetchBotMemberUIDs)
	err := m.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		ChannelID:   channelID,
		ChannelType: channelType,
		FromUID:     fromUID,
		Payload:     []byte(util.ToJson(wirePayload)),
	})
	if err != nil {
		m.Error("发送消息错误", zap.Error(err))
		return errors.New("发送消息错误")
	}
	return nil
}

// enrichPayloadWithSpaceID 在派发消息前给 payload 写入权威 space_id。
//
// 背景 (YUJ-219-A / GH#1283，对应 analysis-report.md §4.5 / §7.4)：
// 客户端 SpaceFilter / filterPersonMessagesBySpace 依赖 payload.space_id
// 判定跨 Space 污染。老路径下该字段只在 PERSONAL DM 由发送端自带，GROUP /
// COMMUNITY_TOPIC 的实时推送完全没有 Space 标签，导致客户端只能靠 channelInfo
// 缓存的 space_id 推导，冷启动 race 窗口里命中 fail-open 分支，跨 Space 消息
// 冒顶。
//
// YUJ-644 / Mininglamp-OSS#33：扩展到 PERSONAL（DM）路径 —— 服务端用
// SpaceMiddleware 已校验的发送方 SpaceID 覆盖客户端上送 payload.space_id，
// 不再信任客户端任何字段。WuKongIM 对 DM 仅用裸 uid 路由（无 Space 概念），
// 客户端 SpaceFilter 是唯一的过滤层，必须有可信信号。
//
// 本函数为后端权威源：
//   - GROUP → 查 group.SpaceID 写入（无条件覆盖）
//   - COMMUNITY_TOPIC → 解析父群 groupNo，再查父群 SpaceID 写入（无条件覆盖）
//   - PERSONAL：senderSpaceID 非空 → 覆盖 payload.space_id（服务端权威）；
//     senderSpaceID 为空 → 老 passthrough（兼容非 Space 部署 / 老客户端）。
//
// 查不到群或解析失败时静默跳过（注入是优化，缺失回落到老语义，不能因此
// 阻断发送）；同一原则：payload 为 nil 时初始化一个空 map，避免调用方踩空。
func (m *Message) enrichPayloadWithSpaceID(channelID string, channelType uint8, payload map[string]interface{}, senderSpaceID string) map[string]interface{} {
	lookup := func(groupNo string) (string, error) {
		g, err := m.groupService.GetGroupWithGroupNo(groupNo)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", nil
		}
		return g.SpaceID, nil
	}
	return enrichPayloadWithSpaceIDCore(channelID, channelType, payload, senderSpaceID, lookup, func(s string, fields ...zap.Field) {
		m.Warn(s, fields...)
	})
}

// enrichPayloadWithSpaceIDCore 是 enrichPayloadWithSpaceID 的纯函数核心，不依赖
// *Message 接收器，便于单测。lookupGroupSpace(groupNo) 返回群的 space_id；
// 查询失败（例如 DB 出错、群不存在）时返回 error，本函数记一条 warn 并跳过注入，
// 保证发送流程不被阻断。logWarn 在生产使用 m.Warn，测试里传入 no-op。
//
// 分支顺序（YUJ-226 / lml P1-2 修复）：**先按 channelType 派发服务端权威路径，
// 再 fallback 到 PERSONAL 的客户端上送**。老版本在最前面做 "sid 存在就 return"
// 的短路，会让 Group/CommunityTopic 消息被客户端伪造的 payload.space_id 绕过，
// 导致跨 Space 信号污染。新版本对 Group / CommunityTopic 一律以群表/父群
// SpaceID 为准，无论客户端是否上送 space_id。
//
// PERSONAL（YUJ-644 / YUJ-660 High-3）：senderSpaceID 是 SpaceMiddleware 已校验
// 的发送方 Space 上下文（X-Space-ID / query?space_id），非空时覆盖 payload.space_id
// 作为服务端权威值；空串时无条件剥离 payload.space_id（YUJ-660 fail-open 修复，
// 见下文 PERSONAL case 注释），不再相信客户端在 SpaceMiddleware opt-in 缺失下
// 提交的任何 space_id。
//
// 内部 emitWarn helper：当 PERSONAL 派发后 payload.space_id 仍为空时记一条结构化
// warn 日志（key=enrich_payload_space_id_empty=true），可作为日志告警的稳态指标。
func enrichPayloadWithSpaceIDCore(
	channelID string,
	channelType uint8,
	payload map[string]interface{},
	senderSpaceID string,
	lookupGroupSpace func(groupNo string) (string, error),
	logWarn func(string, ...zap.Field),
) map[string]interface{} {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	switch channelType {
	case common.ChannelTypeGroup.Uint8():
		// 服务端权威：GROUP 消息的 space_id 以群表为准，无条件覆盖客户端上送值，
		// 防止 sender 给群消息塞错 Space tag（lml P1-2）。
		spaceID, err := lookupGroupSpace(channelID)
		if err != nil {
			if logWarn != nil {
				logWarn("enrichPayloadWithSpaceID: 查群失败，跳过 space_id 注入",
					zap.String("channelID", channelID), zap.Error(err))
			}
			return payload
		}
		if spaceID != "" {
			payload["space_id"] = spaceID
		} else {
			// 老群无 SpaceID：删除客户端可能伪造的 space_id，避免跨 Space 污染。
			delete(payload, "space_id")
		}
		return payload
	case common.ChannelTypeCommunityTopic.Uint8():
		// 子区按父群反推，同样强制覆盖（父群是 Space 权威来源）。
		parentNo, _, perr := thread.ParseChannelID(channelID)
		if perr != nil || parentNo == "" {
			if logWarn != nil {
				logWarn("enrichPayloadWithSpaceID: 解析子区 channelID 失败，跳过 space_id 注入",
					zap.String("channelID", channelID), zap.Error(perr))
			}
			return payload
		}
		spaceID, err := lookupGroupSpace(parentNo)
		if err != nil {
			if logWarn != nil {
				logWarn("enrichPayloadWithSpaceID: 查父群失败，跳过 space_id 注入",
					zap.String("parentGroupNo", parentNo), zap.Error(err))
			}
			return payload
		}
		if spaceID != "" {
			payload["space_id"] = spaceID
		} else {
			delete(payload, "space_id")
		}
		return payload
	case common.ChannelTypePerson.Uint8():
		// YUJ-644 / Mininglamp-OSS#33：PERSONAL DM 用发送方 SpaceMiddleware 已校验
		// 的 SpaceID 覆盖客户端上送，作为客户端 SpaceFilter 的权威信号源。
		if senderSpaceID != "" {
			payload["space_id"] = senderSpaceID
			return payload
		}
		// YUJ-660 (High-3 FAIL-OPEN fix): senderSpaceID == "" 表示发送方未声明 Space
		// (非 Space 部署 / 老客户端没带 X-Space-ID)。在这条路径下任何客户端
		// payload.space_id 都不可信 —— 攻击者只要省略 X-Space-ID 即可塞入伪造值。
		// 服务端无条件剥离，避免 SpaceMiddleware 的 opt-in 语义留下 fail-open 缝隙。
		// 这与 GROUP / COMMUNITY_TOPIC 的"老群无 SpaceID 时删除客户端伪造 space_id"
		// 行为对齐：当服务端无可信权威值时，不允许客户端旁路注入信号。
		_, hadClientSpaceID := payload["space_id"]
		delete(payload, "space_id")
		if logWarn != nil {
			// 监测：未设置 senderSpaceID → 派发后收端走 fail-open 兼容分支。
			// 稳态下应为 0；非零持续上升即说明仍有路径绕过 SpaceMiddleware 透传，
			// 或老客户端在 Space 路由上不带 X-Space-ID。
			logWarn("enrich_payload_space_id_empty",
				zap.Bool("enrich_payload_space_id_empty", true),
				zap.Bool("client_space_id_stripped", hadClientSpaceID),
				zap.String("channelID", channelID),
				zap.Uint8("channelType", channelType),
			)
		}
		return payload
	}
	// 未知 channel_type：保持原样。
	return payload
}

// 消息编辑
func (m *Message) messageEdit(c *wkhttp.Context) {
	var req struct {
		MessageID   string `json:"message_id"`
		MessageSeq  uint32 `json:"message_seq"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		ContentEdit string `json:"content_edit"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("数据格式有误！", err)
		return
	}
	if req.MessageID == "" {
		c.ResponseError(errors.New("消息ID不能为空！"))
		return
	}
	if req.MessageSeq == 0 {
		c.ResponseError(errors.New("消息序号不能为空！"))
		return
	}
	if req.ChannelID == "" {
		c.ResponseError(errors.New("频道ID不能为空！"))
		return
	}

	// 权限检查：只允许编辑自己发送的消息
	loginUID := c.GetLoginUID()
	messageSeqs := []uint32{req.MessageSeq}
	resp, err := m.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, loginUID, messageSeqs)
	if err != nil {
		m.Error("查询消息错误", zap.Error(err))
		c.ResponseError(errors.New("查询消息错误"))
		return
	}
	if resp == nil || len(resp.Messages) == 0 {
		c.ResponseError(errors.New("消息不存在"))
		return
	}
	if resp.Messages[0].FromUID != loginUID {
		c.ResponseError(errors.New("只能编辑自己发送的消息"))
		return
	}
	// TOCTOU 交叉校验：确保权限检查的消息与待编辑的消息是同一条
	if req.MessageID != strconv.FormatInt(resp.Messages[0].MessageID, 10) {
		c.ResponseError(errors.New("消息ID与消息序号不匹配"))
		return
	}

	contentEdit := dbr.NewNullString(req.ContentEdit).String
	contentMD5 := util.MD5(contentEdit)

	exist, err := m.messageExtraDB.existContentEdit(req.MessageID, contentMD5)
	if err != nil {
		m.Error("查询是否存在相同正文失败！", zap.Error(err))
		c.ResponseError(errors.New("查询是否存在相同正文失败！"))
		return
	}
	if exist {
		m.Warn("存在相同编辑正文，不再处理！")
		c.ResponseOK()
		return
	}

	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("开启事务失败！", zap.Error(err))
		c.ResponseError(errors.New("开启事务失败！"))
		return
	}
	defer tx.RollbackUnlessCommitted()
	defer func() {
		if err := recover(); err != nil {
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(c.GetLoginUID(), req.ChannelID)
	}

	version, err := m.genMessageExtraSeq(fakeChannelID)
	if err != nil {
		m.Error("生成消息扩展序列号失败！", zap.Error(err))
		c.ResponseError(errors.New("生成消息扩展序列号失败！"))
		return
	}
	err = m.messageExtraDB.insertOrUpdateContentEditTx(&messageExtraModel{
		MessageID:       req.MessageID,
		MessageSeq:      req.MessageSeq,
		ChannelID:       fakeChannelID,
		ChannelType:     req.ChannelType,
		ContentEdit:     dbr.NewNullString(req.ContentEdit),
		ContentEditHash: contentMD5,
		EditedAt:        int(time.Now().Unix()),
		Version:         version,
	}, tx)
	if err != nil {
		m.Error("添加或修改编辑内容失败！", zap.Error(err))
		c.ResponseError(errors.New("添加或修改编辑内容失败！"))
		return
	}
	msgIds := make([]string, 0)
	msgIds = append(msgIds, req.MessageID)
	// 发布编辑事件
	var eventID int64 = 0
	if m.ctx.GetConfig().ZincSearch.SearchOn {
		eventID, err = m.ctx.EventBegin(&wkevent.Data{
			Event: event.EventUpdateSearchMessage,
			Data: &config.UpdateSearchMessageReq{
				MessageIDs: msgIds,
				ChannelID:  req.ChannelID,
			},
			Type: wkevent.None,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("开启事件失败！", zap.Error(err))
			c.ResponseError(errors.New("开启事件失败！"))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		c.ResponseErrorf("事务提交失败！", err)
		return
	}
	if eventID > 0 {
		m.ctx.EventCommit(eventID)
	}

	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     c.GetLoginUID(),
		CMD:         common.CMDSyncMessageExtra,
	})

	if err != nil {
		m.Error("发送cmd失败！", zap.Error(err))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// 消息已读
func (m *Message) messageReaded(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req struct {
		MessageIDs  []string `json:"message_ids"`
		ChannelID   string   `json:"channel_id"`
		ChannelType uint8    `json:"channel_type"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("数据格式有误！", err)
		return
	}
	if len(req.MessageIDs) == 0 {
		c.ResponseError(errors.New("message_ids不能为空！"))
		return
	}
	// var cloneNo string
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(req.ChannelID, loginUID)
	}
	if len(req.MessageIDs) <= 0 {
		c.ResponseOK()
		return
	}
	messageIDStrs := util.RemoveRepeatedElement(req.MessageIDs)
	messageIdsI := make([]int64, 0, len(messageIDStrs))
	for _, msgID := range messageIDStrs {
		id, _ := strconv.ParseInt(msgID, 10, 64)
		messageIdsI = append(messageIdsI, id)
	}
	syncMsg, err := m.ctx.IMSearchMessages(&config.MsgSearchReq{
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		MessageIds:  messageIdsI,
		LoginUID:    loginUID,
	})
	if err != nil {
		c.ResponseErrorf("查询消息失败！", err)
		return
	}
	if syncMsg == nil || len(syncMsg.Messages) <= 0 {
		m.Warn("没有读取到消息！", zap.Strings("messages", req.MessageIDs))
		c.ResponseError(errors.New("没有读取到消息！"))
		return
	}
	tx, err := m.ctx.DB().Begin()
	if err != nil {
		m.Error("开启事务失败！", zap.Error(err))
		c.ResponseError(errors.New("开启事务失败！"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()

	// 构建批量插入的数据
	readedModels := make([]*memberReadedModel, 0, len(syncMsg.Messages))
	for _, message := range syncMsg.Messages {
		readedModels = append(readedModels, &memberReadedModel{
			MessageID:   message.MessageID,
			ChannelID:   fakeChannelID,
			ChannelType: req.ChannelType,
			UID:         loginUID,
		})
	}
	// 批量插入或更新已读记录
	err = m.memberReadedDB.batchInsertOrUpdateTx(readedModels, tx)
	if err != nil {
		tx.Rollback()
		c.ResponseErrorf("添加已读数据失败！", err)
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		c.ResponseErrorf("提交事务失败！", err)
		return
	}
	// 异步处理 Redis 缓存
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.Error("messageReaded redis cache panic", zap.Any("recover", r), zap.String("stack", string(debug.Stack())))
			}
		}()
		for _, message := range syncMsg.Messages {
			messageIDStr := strconv.FormatInt(message.MessageID, 10)
			jsonStr, err := json.Marshal(&messageReadedCountModel{
				MessageIDStr:   messageIDStr,
				MessageID:      message.MessageID,
				MessageSeq:     message.MessageSeq,
				FromUID:        message.FromUID,
				ChannelID:      fakeChannelID,
				ChannelType:    req.ChannelType,
				LoginUID:       loginUID,
				ReqChannelID:   req.ChannelID,
				ReqChannelType: req.ChannelType,
			})
			if err != nil {
				m.Error("序列化消息错误", zap.Error(err))
				continue
			}

			func() {
				m.mutex.Lock()
				defer m.mutex.Unlock()
				err = m.ctx.GetRedisConn().SetAndExpire(
					fmt.Sprintf("%s%s", CacheReadedCountPrefix, messageIDStr),
					jsonStr,
					time.Hour*24*7,
				)
			}()

			if err != nil {
				m.Error("添加消息扩展数据到缓存失败！",
					zap.Error(err),
					zap.Int64("messageID", message.MessageID),
					zap.String("channelID", fakeChannelID),
				)
			}
		}
	}()
	c.ResponseOK()

}

// 消息回执列表
func (m *Message) messageReceiptList(c *wkhttp.Context) {
	messageIDStr := c.Param("message_id")

	readed := c.Query("readed") // 查询已读未读的消息，0.未读 1.已读
	pIndex, pSize := c.GetPage()

	resps := make([]memberReceiptResp, 0)
	uids := make([]string, 0)
	if readed == "1" {
		memberReadedModels, err := m.memberReadedDB.queryWithMessageIDAndPage(messageIDStr, uint64(pIndex), uint64(pSize))
		if err != nil {
			c.ResponseErrorf("查询已读列表失败！", err)
			return
		}
		if len(memberReadedModels) > 0 {
			for _, memberReadedM := range memberReadedModels {
				uids = append(uids, memberReadedM.UID)
			}
		}
	}
	userResps, err := m.userService.GetUsers(uids)
	if err != nil {
		c.ResponseErrorf("查询用户数据失败！", err)
		return
	}
	userMap := map[string]*user.Resp{}
	if len(userResps) > 0 {
		for _, userResp := range userResps {
			userMap[userResp.UID] = userResp
		}
	}

	for _, uid := range uids {
		userResp := userMap[uid]
		var name string
		if userResp != nil {
			name = userResp.Name
		}
		resps = append(resps, memberReceiptResp{
			UID:  uid,
			Name: name,
		})
	}
	c.Response(resps)

}

//	func (m *Message) getCacheMessageReactionSeq(uid, channelID string, channelType uint8) (int64, error) {
//		versionStr, err := m.ctx.GetRedisConn().Hget(fmt.Sprintf("messageReactionSeq:%s", uid), fmt.Sprintf("%s-%d", channelID, channelType))
//		if err != nil {
//			return 0, err
//		}
//		if versionStr == "" {
//			return 0, nil
//		}
//		version, _ := strconv.ParseInt(versionStr, 10, 64)
//		return version, nil
//	}
func (m *Message) getMessageExtraVersion(uid, source, channelID string, channelType uint8) (int64, error) {
	versionStr, err := m.ctx.GetRedisConn().Hget(fmt.Sprintf("messageExtraVersion:%s%s", uid, source), fmt.Sprintf("%s-%d", channelID, channelType))
	if err != nil {
		return 0, err
	}
	if versionStr == "" {
		return 0, nil
	}
	version, _ := strconv.ParseInt(versionStr, 10, 64)
	return version, nil

}

func (m *Message) setMessageExtraVersion(uid, channelID string, channelType uint8, source string, messageExtraVersion int64) error {
	err := m.ctx.GetRedisConn().Hset(fmt.Sprintf("messageExtraVersion:%s%s", uid, source), fmt.Sprintf("%s-%d", channelID, channelType), fmt.Sprintf("%d", messageExtraVersion))
	if err != nil {
		return err
	}
	return nil
}

// 同步扩展消息数据
func (m *Message) syncMessageExtra(c *wkhttp.Context) {
	var req struct {
		ChannelID    string `json:"channel_id"`
		ChannelType  uint8  `json:"channel_type"`
		ExtraVersion int64  `json:"extra_version"`
		Source       string `json:"source"` // 操作源
		Limit        int    `json:"limit"`  // 数据限制
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("数据格式有误！", err)
		return
	}

	// 群组成员校验：非成员不允许同步消息扩展数据
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		exist, err := m.groupService.ExistMember(req.ChannelID, c.GetLoginUID())
		if err != nil {
			m.Error("查询是否在群内存在失败！", zap.Error(err))
			c.ResponseError(errors.New("查询是否在群内存在失败！"))
			return
		}
		if !exist {
			c.Response(make([]*messageExtraResp, 0))
			return
		}
	}

	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(c.GetLoginUID(), req.ChannelID)
	}
	cacheExtraVersion, err := m.getMessageExtraVersion(c.GetLoginUID(), req.Source, fakeChannelID, req.ChannelType)
	if err != nil {
		c.ResponseErrorf("从缓存中获取消息扩展版本失败！", err)
		return
	}
	extraVersion := req.ExtraVersion
	if cacheExtraVersion >= extraVersion {
		extraVersion = cacheExtraVersion
	} else {
		err = m.setMessageExtraVersion(c.GetLoginUID(), fakeChannelID, req.ChannelType, req.Source, extraVersion)
		if err != nil {
			c.ResponseErrorf("缓存最大的消息扩展版本失败！", err)
			return
		}

	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 10000 {
		limit = 10000
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("频道ID不能为空！"))
		return
	}
	extraModels, err := m.messageExtraDB.sync(extraVersion, fakeChannelID, req.ChannelType, uint64(limit), c.GetLoginUID())
	if err != nil {
		c.ResponseErrorf("同步消息扩展数据失败！", err)
		return
	}
	resps := make([]*messageExtraResp, 0, len(extraModels))
	if len(extraModels) > 0 {
		for _, extraModel := range extraModels {
			resps = append(resps, newMessageExtraResp(extraModel))
		}
	}
	c.Response(resps)
}

// 同步频道消息
func (m *Message) syncChannelMessage(c *wkhttp.Context) {
	var req config.SyncChannelMessageReq
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}

	// 如果当前用户不在群内，则直接返回空消息数组
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		exist, err := m.groupService.ExistMember(req.ChannelID, c.GetLoginUID())
		if err != nil {
			m.Error("查询是否在群内存在失败！", zap.Error(err))
			c.ResponseError(errors.New("查询是否在群内存在失败！"))
			return
		}
		if !exist {
			c.JSON(http.StatusOK, &syncChannelMessageResp{
				StartMessageSeq: req.EndMessageSeq,
				EndMessageSeq:   req.EndMessageSeq,
				PullMode:        req.PullMode,
				Messages:        make([]*MsgSyncResp, 0),
			})
			return
		}
	}
	req.LoginUID = c.GetLoginUID()
	resp, err := m.ctx.IMSyncChannelMessage(req)
	if err != nil {
		m.Error("同步频道内的消息失败！", zap.Error(err), zap.String("req", util.ToJson(req)))
		c.ResponseError(errors.New("同步频道内的消息失败！"))
		return
	}
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() { // 如果是群则需要计算群成员是否变化 如果有变化则将群成员加入到克隆表
		fakeChannelID = common.GetFakeChannelIDWith(req.ChannelID, req.LoginUID)
	}
	channelIds := make([]string, 0)
	channelIds = append(channelIds, fakeChannelID)
	channelSettings, err := m.channelService.GetChannelSettings(channelIds)
	if err != nil {
		m.Error("查询频道设置错误", zap.Error(err), zap.String("req", util.ToJson(req)))
		c.ResponseError(errors.New("查询频道设置错误"))
		return
	}
	var channelOffsetMessageSeq uint32 = 0
	if len(channelSettings) > 0 && channelSettings[0].OffsetMessageSeq > 0 {
		channelOffsetMessageSeq = channelSettings[0].OffsetMessageSeq
	}
	syncResp := newSyncChannelMessageResp(resp, c.GetLoginUID(), req.DeviceUUID, req.ChannelID, req.ChannelType, m.messageExtraDB, m.messageUserExtraDB, m.messageReactionDB, m.channelOffsetDB, m.deviceOffsetDB, channelOffsetMessageSeq)

	// 群消息中的 ThreadCreated 消息：用实时数据覆盖 payload 中的快照字段
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		m.enrichThreadCreatedMessages(syncResp.Messages)
		// 外部来源标识透传：填充顶层 from_is_external / from_source_space_name /
		// from_home_space_id / from_home_space_name (YUJ-63 / #1208)，以及 mergeforward
		// content.users 每个元素的 is_external / source_space_name / home_space_*。
		// 详见 Mininglamp-OSS/octo-server#1188。
		m.enrichExternalMarkers(req.ChannelID, syncResp.Messages)
	}

	// YUJ-219-A / GH#1283 (analysis-report.md §4.1)：
	// 对 Person (DM) 历史消息按已校验的 Space 做消息级过滤。GROUP 路径靠
	// channel_id 本身 Space 隔离，不在此函数处理，避免误杀老群。客户端
	// 仍会做一层 filter 兜底，这里的过滤是后端权威源——只能减，不能加。
	//
	// YUJ-226 / lml P1-1：/v1/message 路由组已挂 SpaceMiddleware，这里只读
	// middleware 写入的 validated spaceID。middleware 已经对 X-Space-ID 的
	// 成员身份做 fail-closed 校验（非成员直接 403），未声明时 spaceID == ""
	// 跳过过滤（向前兼容老客户端）。**不允许**再回退到 c.GetHeader("X-Space-ID")，
	// 否则 hardening 会被绕过。
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if spaceID := spacepkg.GetSpaceID(c); spaceID != "" {
			syncResp.Messages = filterPersonMessagesBySpace(syncResp.Messages, req.ChannelID, spaceID)
		}
	}

	c.Response(syncResp)
}

// 输入中
func (m *Message) typing(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	loginName := c.MustGet("name").(string)
	var req struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(err)
		return
	}
	channelID := req.ChannelID
	channelType := req.ChannelType
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		channelID = loginUID
	}
	// 发送输入中的命令
	err := m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		CMD:         common.CMDTyping,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		Param: map[string]interface{}{
			"from_uid":     loginUID,
			"from_name":    loginName,
			"channel_id":   channelID,
			"channel_type": channelType,
		},
	})
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// 搜索消息
func (m *Message) search(c *wkhttp.Context) {
	var req struct {
		UID         string `json:"uid"` // 搜索的消息限定这某个用户内
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		ContentType int    `json:"content_type"` // 正文类型
		Keyword     string `json:"keyword"`
		SpaceID     string `json:"space_id"` // Space ID（可选）
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(err)
		return
	}
	uid := c.MustGet("uid").(string)
	req.UID = uid

	// 提取 space_id：body > query param > header
	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = c.Query("space_id")
	}
	if spaceID == "" {
		spaceID = c.GetHeader("X-Space-ID")
	}

	headers := map[string]string{"Content-Type": "application/json"}
	if mt := m.ctx.GetConfig().WuKongIM.ManagerToken; mt != "" {
		headers["token"] = mt
	}
	resp, err := network.Post(fmt.Sprintf("%s/message/search", m.ctx.GetConfig().WuKongIM.APIURL), []byte(util.ToJson(req)), headers)
	if err != nil {
		m.Error("调用搜索失败！", zap.Error(err))
		c.ResponseError(errors.New("调用搜索失败！"))
		return
	}
	err = m.handlerIMError(resp)
	if err != nil {
		m.Error("调用搜索错误！", zap.Error(err))
		c.ResponseError(errors.New("调用搜索错误！"))
		return
	}
	var results []map[string]interface{}
	err = util.ReadJsonByByte([]byte(resp.Body), &results)
	if err != nil {
		m.Error("解析搜索数据失败！", zap.Error(err))
		c.ResponseError(errors.New("解析搜索数据失败！"))
		return
	}

	// Space 过滤
	if spaceID != "" && len(results) > 0 {
		results, err = m.filterResultsBySpace(results, spaceID)
		if err != nil {
			m.Error("Space 过滤失败！", zap.Error(err))
			c.ResponseError(errors.New("Space 过滤失败！"))
			return
		}
	}

	c.JSON(http.StatusOK, results)
}

// filterResultsBySpace 按 Space 过滤搜索结果
func (m *Message) filterResultsBySpace(results []map[string]interface{}, spaceID string) ([]map[string]interface{}, error) {
	// 收集群聊 channel_id
	groupNos := make([]string, 0)
	groupNoSet := make(map[string]struct{})
	for _, r := range results {
		ct, _ := r["channel_type"].(float64)
		if int(ct) == 2 {
			chID, _ := r["channel_id"].(string)
			if chID != "" {
				if _, exists := groupNoSet[chID]; !exists {
					groupNoSet[chID] = struct{}{}
					groupNos = append(groupNos, chID)
				}
			}
		}
	}

	// 批量查询群组 space_id
	groupSpaceMap := make(map[string]string) // group_no -> space_id
	if len(groupNos) > 0 {
		groups, err := m.groupService.GetGroups(groupNos)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			groupSpaceMap[g.GroupNo] = g.SpaceID
		}
	}

	// 过滤
	filtered := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		ct, _ := r["channel_type"].(float64)
		channelType := int(ct)
		chID, _ := r["channel_id"].(string)

		switch channelType {
		case 2: // 群聊：匹配群的 space_id
			if groupSpaceMap[chID] == spaceID {
				filtered = append(filtered, r)
			}
		case 1: // DM：解析 payload 中的 space_id
			if m.matchPayloadSpaceID(r, spaceID) {
				filtered = append(filtered, r)
			}
		default:
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

// matchPayloadSpaceID 从消息 payload 中提取 space_id 并匹配
func (m *Message) matchPayloadSpaceID(msg map[string]interface{}, spaceID string) bool {
	payloadStr, _ := msg["payload"].(string)
	if payloadStr == "" {
		return false
	}
	// payload 可能是 base64 编码
	payloadBytes, err := base64.StdEncoding.DecodeString(payloadStr)
	if err != nil {
		// 不是 base64，尝试直接作为 JSON 解析
		payloadBytes = []byte(payloadStr)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return false
	}
	msgSpaceID, _ := payload["space_id"].(string)
	return msgSpaceID == spaceID
}

// 语音消息设置为已读
func (m *Message) voiceReaded(c *wkhttp.Context) {
	var req *voiceReadedReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("数据格式有误！", err)
		return
	}
	if err := req.check(); err != nil {
		c.ResponseError(err)
		return
	}
	loginUID := c.GetLoginUID()

	err := m.messageUserExtraDB.insertOrUpdateVoiceRead(&messageUserExtraModel{
		UID:         loginUID,
		MessageID:   req.MessageID,
		MessageSeq:  req.MessageSeq,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		VoiceReaded: 1,
	})
	if err != nil {
		c.ResponseErrorf("修改语音已读状态失败！", err)
		return
	}
	c.ResponseOK()
}

// 同步回应数据
func (m *Message) syncReaction(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		Seq         int64  `json:"seq"` // 同步序列号
		Limit       uint64 `json:"limit"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	// Verify channel membership before syncing reaction data
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		isMember, err := m.groupService.ExistMember(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询群成员关系错误", zap.Error(err))
			c.ResponseError(errors.New("查询群成员关系错误"))
			return
		}
		if !isMember {
			c.ResponseError(errors.New("没有权限同步此频道的回应数据"))
			return
		}
	} else if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if req.ChannelID != loginUID {
			isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
			if err != nil {
				m.Error("查询好友关系错误", zap.Error(err))
				c.ResponseError(errors.New("查询好友关系错误"))
				return
			}
			if !isFriend {
				c.ResponseError(errors.New("没有权限同步此频道的回应数据"))
				return
			}
		}
	}

	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if !strings.Contains(req.ChannelID, "@") {
			fakeChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	// cacheReactionSeq, err := m.getCacheMessageReactionSeq(loginUID, req.ChannelID, req.ChannelType)
	// if err != nil {
	// 	m.Error("获取缓存messageSeq失败", zap.Error(err))
	// 	c.ResponseError(errors.New("获取缓存messageSeq失败"))
	// 	return
	// }
	// if req.Seq <= cacheReactionSeq {
	// 	req.Seq = cacheReactionSeq
	// }
	list, err := m.messageReactionDB.queryReactionWithChannelAndSeq(fakeChannelID, req.ChannelType, req.Seq, limit)
	if err != nil {
		m.Error("获取缓存seq错误", zap.Error(err))
		c.ResponseError(errors.New("获取缓存seq错误"))
		return
	}

	toChannelID := common.GetToChannelIDWithFakeChannelID(fakeChannelID, loginUID)

	reactions := make([]*reactionResp, 0)
	if len(list) > 0 {
		for _, model := range list {
			reactions = append(reactions, &reactionResp{
				UID:         model.UID,
				Name:        model.Name,
				ChannelID:   toChannelID,
				ChannelType: model.ChannelType,
				Seq:         model.Seq,
				MessageID:   model.MessageID,
				CreatedAt:   model.CreatedAt.String(),
				Emoji:       model.Emoji,
				IsDeleted:   model.IsDeleted,
			})
		}
	}
	c.JSON(http.StatusOK, reactions)
}

// 添加或取消回应
func (m *Message) addOrCancelReaction(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	loginName := c.GetLoginName()
	var req struct {
		MessageID   string `json:"message_id"`   // 消息唯一ID
		ChannelID   string `json:"channel_id"`   // 频道唯一ID
		ChannelType uint8  `json:"channel_type"` // 频道类型
		Emoji       string `json:"emoji"`        // 回应的emoji
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	// Verify channel membership before allowing reaction
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		isMember, err := m.groupService.ExistMember(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询群成员关系错误", zap.Error(err))
			c.ResponseError(errors.New("查询群成员关系错误"))
			return
		}
		if !isMember {
			c.ResponseError(errors.New("没有权限操作此频道"))
			return
		}
	} else if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if req.ChannelID != loginUID {
			isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
			if err != nil {
				m.Error("查询好友关系错误", zap.Error(err))
				c.ResponseError(errors.New("查询好友关系错误"))
				return
			}
			if !isFriend {
				c.ResponseError(errors.New("没有权限操作此频道"))
				return
			}
		}
	}

	model, err := m.messageReactionDB.queryReactionWithUIDAndMessageID(loginUID, req.MessageID)
	if err != nil {
		m.Error("查询登录用户是否回应消息错误", zap.Error(err))
		c.ResponseError(errors.New("查询登录用户是否回应消息错误"))
		return
	}
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(req.ChannelID, loginUID)
	}
	seq, err := m.genMessageReactionSeq(fakeChannelID) // 下次回复seq
	if err != nil {
		c.ResponseError(err)
		return
	}
	if model == nil {
		//新增回应
		err = m.messageReactionDB.insertReaction(&reactionModel{
			ChannelID:   fakeChannelID,
			ChannelType: req.ChannelType,
			UID:         loginUID,
			Name:        loginName,
			MessageID:   req.MessageID,
			Emoji:       req.Emoji,
			Seq:         seq,
			IsDeleted:   0,
		})
		if err != nil {
			m.Error("新增消息回应错误", zap.Error(err))
			c.ResponseError(errors.New("新增消息回应错误"))
			return
		}
	} else {
		model.Seq = seq
		if model.IsDeleted == 1 {
			model.IsDeleted = 0
			if model.Emoji != req.Emoji {
				model.Emoji = req.Emoji
			}
		} else {
			if model.Emoji == req.Emoji {
				model.IsDeleted = 1
			} else {
				model.Emoji = req.Emoji
			}
		}
		err = m.messageReactionDB.updateReactionStatus(model)
		if err != nil {
			m.Error("修改消息回应错误", zap.Error(err))
			c.ResponseError(errors.New("修改消息回应错误"))
			return
		}
	}

	//发送同步消息cmd
	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: uint8(req.ChannelType),
		CMD:         common.CMDSyncMessageReaction,
		FromUID:     loginUID,
	})
	if err != nil {
		m.Error("发送同步命令失败！", zap.Error(err))
		c.ResponseErrorf("发送同步命令失败！", err)
		return
	}

	c.ResponseOK()
}
func (m *Message) handlerIMError(resp *rest.Response) error {
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			resultMap, err := util.JsonToMap(resp.Body)
			if err != nil {
				return err
			}
			if resultMap != nil && resultMap["msg"] != nil {
				return fmt.Errorf("IM Extend服务失败！ -> %s", resultMap["msg"])
			}
		}
		return fmt.Errorf("IM Extend服务返回状态[%d]失败！", resp.StatusCode)
	}
	return nil
}

// 同步消息回执
func (m *Message) syncack(c *wkhttp.Context) {
	uid := c.MustGet("uid").(string)
	lastMessageSeqStr := c.Param("last_message_seq")
	lastMessageSeq, err := strconv.ParseUint(lastMessageSeqStr, 10, 64)
	if err != nil {
		m.Error("last_message_seq格式有误！", zap.String("last_message_seq", lastMessageSeqStr))
		c.ResponseError(errors.New("last_message_seq格式有误！"))
		return
	}
	err = m.ctx.IMSyncMessageAck(&config.SyncackReq{
		UID:            uid,
		LastMessageSeq: uint32(lastMessageSeq),
	})
	if err != nil {
		m.Error("同步消息回执失败！", zap.Error(err), zap.String("uid", uid), zap.String("last_message_seq", lastMessageSeqStr))
		c.ResponseError(errors.New("同步消息回执失败！"))
		return
	}
	c.ResponseOK()
}

// 同步消息
func (m *Message) sync(c *wkhttp.Context) {
	uid := c.MustGet("uid").(string)
	var req syncReq
	if err := c.BindJSON(&req); err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		c.ResponseError(common.ErrData)
		return
	}
	resps, err := m.ctx.IMSyncMessage(&config.MsgSyncReq{
		UID:        uid,
		MessageSeq: req.MaxMessageSeq,
		Limit:      req.Limit,
	})
	if err != nil {
		m.Error("同步消息失败！", zap.Error(err), zap.String("uid", uid))
		c.ResponseError(errors.New("同步消息失败！"))
		return
	}
	messageIDs := make([]string, 0, len(resps))
	for _, message := range resps {
		messageIDs = append(messageIDs, fmt.Sprintf("%d", message.MessageID))
	}

	// 全局扩充数据
	messageExtras, err := m.messageExtraDB.queryWithMessageIDsAndUID(messageIDs, c.GetLoginUID())
	if err != nil {
		log.Error("查询消息扩展字段失败！", zap.Error(err))
	}
	messageExtraMap := map[string]*messageExtraDetailModel{}
	if len(messageExtras) > 0 {
		for _, messageExtra := range messageExtras {
			messageExtraMap[messageExtra.MessageID] = messageExtra
		}
	}
	// 用户扩充数据
	messageUserExtras, err := m.messageUserExtraDB.queryWithMessageIDsAndUID(messageIDs, c.GetLoginUID())
	if err != nil {
		log.Error("查询用户消息扩展字段失败！", zap.Error(err))
	}
	messageUserExtraMap := map[string]*messageUserExtraModel{}
	if len(messageUserExtras) > 0 {
		for _, messageUserExtraM := range messageUserExtras {
			messageUserExtraMap[messageUserExtraM.MessageID] = messageUserExtraM
		}
	}
	// 用户频道偏移
	channelOffsetM, err := m.channelOffsetDB.queryWithUIDAndChannel(c.GetLoginUID(), req.ChannelID, req.ChannelType)
	if err != nil {
		m.Error("查询偏移量失败！", zap.Error(err))
		c.ResponseError(errors.New("查询偏移量失败！"))
		return
	}
	// 频道偏移
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(uid, req.ChannelID)
	}
	channelIds := make([]string, 0)
	channelIds = append(channelIds, fakeChannelID)
	channelSettings, err := m.channelService.GetChannelSettings(channelIds)
	if err != nil {
		m.Error("查询频道设置错误", zap.Error(err), zap.String("req", util.ToJson(req)))
		c.ResponseError(errors.New("查询频道设置错误"))
		return
	}
	var channelOffsetMessageSeq uint32 = 0
	if len(channelSettings) > 0 && channelSettings[0].OffsetMessageSeq > 0 {
		channelOffsetMessageSeq = channelSettings[0].OffsetMessageSeq
	}
	respVos := make([]*MsgSyncResp, 0)
	for _, resp := range resps {
		if channelOffsetM != nil && resp.MessageSeq <= channelOffsetM.MessageSeq {
			continue
		}
		messageIDStr := strconv.FormatInt(resp.MessageID, 10)
		messageExtraM := messageExtraMap[messageIDStr]
		messageUserExtraM := messageUserExtraMap[messageIDStr]
		respVo := &MsgSyncResp{}
		respVo.from(resp, c.GetLoginUID(), messageExtraM, messageUserExtraM, nil, channelOffsetMessageSeq)
		respVos = append(respVos, respVo)
	}

	// YUJ-98 / YUJ-101: 群消息同步路径同样需要回填 msg-level 外部来源字段，
	// 让前端 fromHomeSpaceId / fromHomeSpaceName / fromIsExternal / fromSourceSpaceName
	// getter 在本路径也能拿到值。与 /message/channel/sync 保持一致。
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		m.enrichExternalMarkers(req.ChannelID, respVos)
	}

	c.JSON(http.StatusOK, respVos)
}

// 双向删除
func (m *Message) mutualDelete(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req deleteReq
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if err := req.check(); err != nil {
		c.ResponseError(err)
		return
	}
	messageSeqs := make([]uint32, 0)
	messageSeqs = append(messageSeqs, req.MessageSeq)
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
	}
	resp, err := m.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, loginUID, messageSeqs)
	if err != nil {
		m.Error("查询消息错误", zap.Error(err), zap.String("req", util.ToJson(req)))
		c.ResponseError(errors.New("查询消息错误"))
		return
	}

	if resp == nil || len(resp.Messages) == 0 {
		c.ResponseError(errors.New("消息不存在"))
		return
	}
	var (
		isGroupMember        bool
		isGroupManager       bool
		isParentGroupMember  bool
		isParentGroupManager bool
	)
	switch req.ChannelType {
	case common.ChannelTypeGroup.Uint8():
		isGroupMember, err = m.groupService.ExistMember(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询群成员关系失败", zap.Error(err))
			c.ResponseError(errors.New("查询群成员关系失败"))
			return
		}
		if isGroupMember {
			isGroupManager, err = m.groupService.IsCreatorOrManager(req.ChannelID, loginUID)
			if err != nil {
				m.Error("查询登录用户群内权限错误", zap.Error(err))
				c.ResponseError(errors.New("查询登录用户群内权限错误"))
				return
			}
		}
	case common.ChannelTypeCommunityTopic.Uint8():
		parentGroupNo, _, perr := thread.ParseChannelID(req.ChannelID)
		if perr != nil {
			m.Error("解析子区频道ID失败", zap.Error(perr), zap.String("channelID", req.ChannelID))
			c.ResponseError(errors.New("用户无权删除此消息"))
			return
		}
		isParentGroupMember, err = m.groupService.ExistMember(parentGroupNo, loginUID)
		if err != nil {
			m.Error("查询父群成员关系失败", zap.Error(err))
			c.ResponseError(errors.New("查询父群成员关系失败"))
			return
		}
		if isParentGroupMember {
			isParentGroupManager, err = m.groupService.IsCreatorOrManager(parentGroupNo, loginUID)
			if err != nil {
				m.Error("查询父群管理员身份失败", zap.Error(err))
				c.ResponseError(errors.New("查询父群管理员身份失败"))
				return
			}
		}
	}
	if err := authorizeMutualDelete(
		req.ChannelType,
		resp.Messages[0].FromUID,
		loginUID,
		isGroupMember,
		isGroupManager,
		isParentGroupMember,
		isParentGroupManager,
	); err != nil {
		c.ResponseError(err)
		return
	}
	// TOCTOU 交叉校验：确保权限检查的消息与待删除的消息是同一条
	resolvedMessageID := strconv.FormatInt(resp.Messages[0].MessageID, 10)
	if req.MessageID != resolvedMessageID {
		c.ResponseError(errors.New("消息ID与消息序号不匹配"))
		return
	}
	version, err := m.genMessageExtraSeq(fakeChannelID)
	if err != nil {
		m.Error("生成消息扩展序列号失败！", zap.Error(err))
		c.ResponseError(errors.New("生成消息扩展序列号失败！"))
		return
	}
	err = m.messageExtraDB.insertOrUpdateDeleted(&messageExtraModel{
		MessageID:   resolvedMessageID,
		ChannelID:   fakeChannelID,
		ChannelType: req.ChannelType,
		IsDeleted:   1,
		Version:     version,
	})
	if err != nil {
		m.Error("删除消息错误", zap.Error(err))
		c.ResponseError(errors.New("删除消息错误"))
		return
	}
	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     c.GetLoginUID(),
		CMD:         common.CMDSyncMessageExtra,
	})

	if err != nil {
		m.Error("发送cmd失败！", zap.Error(err))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// 删除消息
func (m *Message) delete(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var reqs []*deleteReq
	if err := c.BindJSON(&reqs); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if len(reqs) == 0 {
		c.ResponseError(errors.New("参数不能为空！"))
		return
	}
	for _, req := range reqs {
		if err := req.check(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	// 验证用户对所涉频道的访问权限
	// 私聊无需校验好友关系：此操作仅写入 messageUserExtraDB（按 loginUID 分区），只影响当前用户视图
	checked := make(map[string]bool)
	for _, req := range reqs {
		key := fmt.Sprintf("%s-%d", req.ChannelID, req.ChannelType)
		if checked[key] {
			continue
		}
		checked[key] = true
		if req.ChannelType == common.ChannelTypeGroup.Uint8() {
			isMember, err := m.groupService.ExistMember(req.ChannelID, loginUID)
			if err != nil {
				m.Error("查询群成员失败", zap.Error(err))
				c.ResponseError(errors.New("查询群成员失败"))
				return
			}
			if !isMember {
				c.ResponseError(errors.New("非频道成员，无权操作"))
				return
			}
		}
	}

	tx, err := m.ctx.DB().Begin()
	if err != nil {
		m.Error("开启事务失败！", zap.Error(err))
		c.ResponseError(errors.New("开启事务失败！"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	for _, req := range reqs {
		err := m.messageUserExtraDB.insertOrUpdateDeletedTx(&messageUserExtraModel{
			UID:              loginUID,
			MessageID:        req.MessageID,
			MessageSeq:       req.MessageSeq,
			ChannelID:        req.ChannelID,
			ChannelType:      req.ChannelType,
			MessageIsDeleted: 1,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("删除消息失败！", zap.Error(err))
			c.ResponseError(errors.New("删除消息失败！"))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		m.Error("提交事务失败！", zap.Error(err))
		c.ResponseError(errors.New("提交事务失败！"))
		return
	}

	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         CMDMessageDeleted,
		Param: map[string]interface{}{
			"messages": reqs,
		},
	})
	if err != nil {
		m.Error("发送命令失败", zap.Error(err))
		c.ResponseError(errors.New("发送命令失败"))
		return
	}

	c.ResponseOK()
}

func (m *Message) genMessageExtraSeq(channelID string) (int64, error) {
	return m.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, channelID))
}
func (m *Message) genMessageReactionSeq(channelID string) (int64, error) {
	return m.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageReactionSeqKey, channelID))
}

// 消息偏移
func (m *Message) offset(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		MessageSeq  uint32 `json:"message_seq"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	channelOffsetM, err := m.channelOffsetDB.queryWithUIDAndChannel(c.GetLoginUID(), req.ChannelID, req.ChannelType)
	if err != nil {
		m.Error("查询频道偏移数据失败！", zap.Error(err))
		c.ResponseError(errors.New("查询频道偏移数据失败！"))
		return
	}
	if channelOffsetM != nil {
		if channelOffsetM.MessageSeq >= req.MessageSeq {
			c.ResponseOK()
			return
		}
	}

	err = m.channelOffsetDB.insertOrUpdate(&channelOffsetModel{
		UID:         c.GetLoginUID(),
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		MessageSeq:  req.MessageSeq,
	})
	if err != nil {
		m.Error("清除失败！", zap.Error(err))
		c.ResponseError(errors.New("清除失败！"))
		return
	}
	// 清除最近会话的未读数（这里不管有没有未读数都调用清除）
	err = m.ctx.IMClearConversationUnread(config.ClearConversationUnreadReq{
		UID:         c.GetLoginUID(),
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		MessageSeq:  req.MessageSeq,
		Unread:      0,
	})
	if err != nil {
		m.Error("清除最近会话未读数失败！", zap.Error(err), zap.String("uid", c.GetLoginUID()), zap.String("channelID", req.ChannelID), zap.Uint8("channelType", req.ChannelType))
	}
	// 清空提醒项
	reminders, err := m.remindersDB.queryWithUIDAndChannel(loginUID, req.ChannelID, req.ChannelType, req.MessageSeq)
	if err != nil {
		m.Error("查询用户提醒项失败！", zap.Error(err))
		c.ResponseError(errors.New("查询用户提醒项失败！"))
		return
	}
	reminderIds := make([]int64, 0)
	if len(reminders) > 0 {
		for _, reminder := range reminders {
			if reminder.MessageSeq <= req.MessageSeq && reminder.Done == 0 {
				reminderIds = append(reminderIds, reminder.Id)
			}
		}
	}

	if len(reminderIds) > 0 {
		tx, err := m.ctx.DB().Begin()
		if err != nil {
			m.Error("开启事务失败！", zap.Error(err))
			c.ResponseError(errors.New("开启事务失败！"))
			return
		}
		defer tx.RollbackUnlessCommitted()
		defer func() {
			if err := recover(); err != nil {
				fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
			}
		}()
		err = m.remindersDB.insertDonesTx(reminderIds, loginUID, tx)
		if err != nil {
			tx.Rollback()
			m.Error("更新提醒项状态失败！", zap.Error(err))
			c.ResponseError(errors.New("更新提醒项状态失败！"))
			return
		}
		for _, id := range reminderIds {
			version, err := m.ctx.GenSeq(common.RemindersKey)
			if err != nil {
				c.ResponseError(err)
				return
			}
			err = m.remindersDB.updateVersionTx(version, id, tx)
			if err != nil {
				tx.Rollback()
				m.Error("更新提醒项版本失败！", zap.Error(err))
				c.ResponseError(errors.New("更新提醒项版本失败！"))
				return
			}
		}
		if err := tx.Commit(); err != nil {
			tx.Rollback()
			m.Error("提交事务失败！", zap.Error(err))
			c.ResponseError(errors.New("提交事务失败！"))
			return
		}
		err = m.ctx.SendCMD(config.MsgCMDReq{
			NoPersist:   true,
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			CMD:         common.CMDSyncReminders,
		})
		if err != nil {
			m.Error("发送cmd[CMDSyncReminders]失败！", zap.Error(err))
		}
	}
	// 发送清空红点的命令
	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   c.GetLoginUID(),
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDConversationUnreadClear,
		Param: map[string]interface{}{
			"channel_id":   req.ChannelID,
			"channel_type": req.ChannelType,
			"unread":       0,
		},
	})
	if err != nil {
		m.Error("命令发送失败！", zap.String("cmd", common.CMDConversationUnreadClear), zap.String("uid", c.GetLoginUID()), zap.String("channelID", req.ChannelID), zap.Uint8("channelType", req.ChannelType))
	}

	c.ResponseOK()
}

// 是否有撤回的权限
func (m *Message) hasRevokePermission(messageM *messageModel, loginUID string) (bool, error) {
	if messageM.FromUID == "" { // 没有fromUID的消息一般是命令类的消息，不被允许撤回
		return false, nil
	}
	if messageM.FromUID == loginUID { // 自己发的消息允许被撤回
		return true, nil
	}
	// YUJ-60: 允许 bot 创建者撤回自己创建的 bot 发的消息（DM / 群都适用）。
	// 放在 FromUID==loginUID 之后，避免非 bot 场景的多余查询；
	// 放在群管理员分支之前，确保 DM 场景也生效。
	if m.robotService != nil {
		creatorUID, err := m.robotService.GetCreatorUID(messageM.FromUID)
		if err != nil {
			// 查询失败不应阻断原有流程，降级继续走后续群管理员分支。
			m.Warn("查询 Bot 创建者失败，跳过 bot-owner 分支",
				zap.Error(err), zap.String("fromUID", messageM.FromUID))
		} else if creatorUID != "" && creatorUID == loginUID {
			return true, nil
		}
	}
	if messageM.ChannelType == common.ChannelTypeGroup.Uint8() { // 管理者或创建者可以撤回其他成员的消息
		loginMember, err := m.groupService.GetMember(messageM.ChannelID, loginUID)
		if err != nil {
			return false, err
		}
		if loginMember == nil {
			return false, nil
		}
		fromMember, err := m.groupService.GetMember(messageM.ChannelID, messageM.FromUID)
		if err != nil {
			return false, err
		}
		if fromMember == nil {
			// 消息发送者已不在群：管理员/创建者可撤回，普通成员不可
			return loginMember.Role != int(common.GroupMemberRoleNormal), nil
		}
		if fromMember.Role == int(common.GroupMemberRoleCreater) || loginMember.Role == int(common.GroupMemberRoleNormal) {
			return false, nil
		}
		if loginMember.Role == int(common.GroupMemberRoleCreater) || (loginMember.Role == int(common.GroupMemberRoleManager) && fromMember.Role == int(common.GroupMemberRoleNormal)) {
			return true, nil
		}

	}

	return false, nil
}

func (m *Message) cancelMentionReminderIfNeed(message *messageModel) {
	setting := config.SettingFromUint8(message.Setting)
	//  如果撤回的是@消息，需要取消提醒
	if !setting.Signal {
		var payloadMap map[string]interface{}
		if err := util.ReadJsonByByte(message.Payload, &payloadMap); err != nil {
			m.Warn("解码消息内容失败！", zap.Error(err))
		}
		if payloadMap != nil {
			if m.hasMention(payloadMap) {
				all, uids := m.getMention(payloadMap)
				if all {
					version, err := m.ctx.GenSeq(common.RemindersKey)
					if err != nil {
						m.Warn("GenSeq failed", zap.Error(err))
						return
					}
					err = m.remindersDB.deleteWithChannel(message.ChannelID, message.ChannelType, message.MessageID, version)
					if err != nil {
						m.Error("删除提醒项失败！", zap.Error(err))
					} else {
						err = m.ctx.SendCMD(config.MsgCMDReq{
							NoPersist:   true,
							ChannelID:   message.ChannelID,
							ChannelType: message.ChannelType,
							CMD:         common.CMDSyncReminders,
						})
						if err != nil {
							m.Error("发送cmd[CMDSyncReminders]失败！", zap.Error(err))
						}
					}
				} else if len(uids) > 0 {
					tx, err := m.ctx.DB().Begin()
					if err != nil {
						m.Error("开启事务失败！", zap.Error(err))
						return
					}
					defer tx.RollbackUnlessCommitted()
					defer func() {
						if err := recover(); err != nil {
							fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
						}
					}()
					for _, uid := range uids {
						version, err := m.ctx.GenSeq(common.RemindersKey)
						if err != nil {
							m.Warn("GenSeq failed", zap.Error(err))
							return
						}
						err = m.remindersDB.deleteWithChannelAndUIDTx(message.ChannelID, message.ChannelType, uid, message.MessageID, version, tx)
						if err != nil {
							m.Error("删除用户提醒项失败！", zap.Error(err))
							tx.Rollback()
							return
						}
					}
					if err := tx.Commit(); err != nil {
						m.Error("提交事务失败！", zap.Error(err))
						tx.RollbackUnlessCommitted()
						return
					}
					err = m.ctx.SendCMD(config.MsgCMDReq{
						NoPersist:   true,
						Subscribers: uids,
						CMD:         common.CMDSyncReminders,
					})
					if err != nil {
						m.Error("发送cmd[CMDSyncReminders]失败！", zap.Error(err))
					}
				}
			}
		}
	}
}

// 撤回消息
func (m *Message) revoke(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	messageID := c.Query("message_id")
	clientMsgNo := c.Query("client_msg_no") // TODO：后续版本不再使用messageID撤回，使用client_msg_no撤回，因为存在重试消息，clientMsgNo一样 但是messageID不一样
	channelID := c.Query("channel_id")
	channelType := c.Query("channel_type")

	if strings.TrimSpace(clientMsgNo) == "" {
		c.ResponseError(errors.New("撤回主键参数错误！"))
		return
	}

	//删除消息
	channelTypeI, _ := strconv.ParseUint(channelType, 10, 64)

	fakeChannelID := channelID
	if uint8(channelTypeI) == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(channelID, c.GetLoginUID())
	}
	cliengMsgNos := make([]string, 0)
	cliengMsgNos = append(cliengMsgNos, clientMsgNo)
	syncMsgs, err := m.ctx.IMSearchMessages(&config.MsgSearchReq{
		LoginUID:     loginUID,
		ChannelID:    channelID,
		ChannelType:  uint8(channelTypeI),
		ClientMsgNos: cliengMsgNos,
	})
	if err != nil {
		m.Error("查询IM消息错误", zap.String("fakeChannelID", fakeChannelID), zap.String("clientMsgNo", clientMsgNo), zap.String("loginUID", c.GetLoginUID()))
		c.ResponseErrorf("查询IM消息错误", err)
		return
	}
	if syncMsgs == nil || len(syncMsgs.Messages) == 0 {
		c.ResponseErrorf("未查询到撤回消息！", err)
		return
	}
	syncMsg := syncMsgs.Messages[0]
	// TOCTOU 交叉校验：若用户传入了 message_id，必须与 clientMsgNo 反查到的 messageID 一致，
	// 防止通过自己消息的 clientMsgNo 配合他人消息的 messageID 撤回任意消息（issue #1048）。
	if err := verifyRevokeMessageID(messageID, syncMsg.MessageID); err != nil {
		c.ResponseError(err)
		return
	}
	// 下游操作统一改用 IM 反查到的可信 channelID / channelType，
	// 防止 clientMsgNo 跨频道非唯一时把撤回广播发到错误频道。
	channelID = syncMsg.ChannelID
	channelTypeI = uint64(syncMsg.ChannelType)
	fakeChannelID = channelID
	if uint8(channelTypeI) == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(channelID, loginUID)
	}
	message := &messageModel{
		ChannelID:   syncMsg.ChannelID,
		ChannelType: syncMsg.ChannelType,
		Setting:     syncMsg.Setting,
		MessageID:   syncMsg.MessageID,
		MessageSeq:  syncMsg.MessageSeq,
		FromUID:     syncMsg.FromUID,
		ClientMsgNo: syncMsg.ClientMsgNo,
		Payload:     syncMsg.Payload,
	}
	allow, err := m.hasRevokePermission(message, c.GetLoginUID())
	if err != nil {
		m.Error("权限判断失败！", zap.Error(err))
		c.ResponseError(errors.New("权限判断失败！"))
		return
	}
	if !allow {
		c.ResponseError(errors.New("无权限撤回此消息！"))
		return
	}

	// 检查撤回时间限制
	// 用户撤回自己消息时受时间限制，管理员/群主撤回他人消息不受限制
	if message.FromUID == c.GetLoginUID() {
		messageTime := time.Unix(int64(syncMsg.Timestamp), 0)
		elapsed := time.Since(messageTime)
		if elapsed.Seconds() > DefaultRevokeTimeout {
			c.ResponseError(errors.New("消息已超过撤回时限！"))
			return
		}
	}

	m.cancelMentionReminderIfNeed(message)

	// 使用服务端反查到的真实 messageID，而非用户输入，避免后续数据库操作作用于不相关消息。
	messageIDStr := strconv.FormatInt(message.MessageID, 10)
	messageExtra, err := m.messageExtraDB.queryWithMessageID(messageIDStr)
	if err != nil {
		m.Error("查询消息扩展错误", zap.Error(err))
		c.ResponseError(errors.New("查询消息扩展错误"))
		return
	}
	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("开启事务失败！", zap.Error(err))
		c.ResponseError(errors.New("开启事务失败！"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	version, err := m.genMessageExtraSeq(fakeChannelID)
	if err != nil {
		m.Error("生成消息扩展序列号失败！", zap.Error(err), zap.String("channelID", fakeChannelID))
		c.ResponseError(errors.New("生成消息扩展序列号失败！"))
		return
	}
	if messageExtra != nil {
		messageExtra.Revoke = 1
		messageExtra.Revoker = loginUID
		messageExtra.Version = version
		err = m.messageExtraDB.updateTx(messageExtra, tx)
		if err != nil {
			tx.Rollback()
			m.Error("更新消息扩展数据失败！", zap.Error(err), zap.String("messageID", messageIDStr), zap.String("channelID", fakeChannelID))
			c.ResponseErrorf("更新消息为撤回状态失败！", err)
			return
		}
	} else {
		err = m.messageExtraDB.insertTx(&messageExtraModel{
			MessageID:   messageIDStr,
			MessageSeq:  message.MessageSeq,
			FromUID:     message.FromUID,
			ChannelID:   fakeChannelID,
			ChannelType: uint8(channelTypeI),
			ReadedCount: 0,
			Revoke:      1,
			Revoker:     loginUID,
			Version:     version,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("新增消息扩展数据失败！", zap.Error(err), zap.String("messageID", messageIDStr), zap.String("channelID", fakeChannelID))
			c.ResponseErrorf("新增消息为撤回状态失败！", err)
			return
		}
	}
	msgIds := make([]string, 0)
	msgIds = append(msgIds, messageIDStr)
	// 发布撤回消息事件
	var eventID int64 = 0
	if m.ctx.GetConfig().ZincSearch.SearchOn {
		eventID, err = m.ctx.EventBegin(&wkevent.Data{
			Event: event.EventUpdateSearchMessage,
			Data: &config.UpdateSearchMessageReq{
				MessageIDs: msgIds,
				ChannelID:  channelID,
			},
			Type: wkevent.None,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("开启事件失败！", zap.Error(err))
			c.ResponseError(errors.New("开启事件失败！"))
			return
		}
	}
	err = m.deletePinnedMessage(channelID, uint8(channelTypeI), msgIds, loginUID, tx)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		c.ResponseErrorf("事务提交失败！", err)
		return
	}
	if eventID > 0 {
		m.ctx.EventCommit(eventID)
	}
	for _, msgID := range msgIds {
		messageIDI, _ := strconv.ParseInt(msgID, 10, 64)
		// 发给指定频道
		err = m.ctx.SendRevoke(&config.MsgRevokeReq{
			Operator:     loginUID,
			OperatorName: c.GetLoginName(),
			FromUID:      loginUID,
			ChannelID:    channelID,
			ChannelType:  uint8(channelTypeI),
			MessageID:    messageIDI,
		})
		if err != nil {
			m.Error("发送撤回消息失败！", zap.Error(err))
			c.ResponseError(errors.New("发送撤回消息失败！"))
			return
		}
	}

	c.ResponseOK()

}

// 同步违禁词
func (m *Message) syncProhibitWords(c *wkhttp.Context) {
	version := c.Query("version")
	maxVersion, _ := strconv.ParseInt(version, 10, 64)
	list, err := m.db.queryProhibitWordsWithVersion(maxVersion)
	if err != nil {
		m.Error("同步违禁词错误", zap.Error(err))
		c.ResponseError(errors.New("同步违禁词错误"))
		return
	}
	result := make([]*ProhibitWordResp, 0)
	if len(list) > 0 {
		for _, word := range list {
			result = append(result, &ProhibitWordResp{
				Id:        word.Id,
				Content:   word.Content,
				IsDeleted: word.IsDeleted,
				CreatedAt: word.CreatedAt.String(),
				Version:   word.Version,
			})
		}
	}
	c.Response(result)
}

// 同步敏感词
func (m *Message) syncSensitiveWords(c *wkhttp.Context) {
	type resp struct {
		Tips    string   `json:"tips"`
		List    []string `json:"list"`
		Version int64    `json:"version"`
	}
	reqVersion, _ := strconv.ParseInt(c.Query("version"), 10, 64)
	resultList := make([]string, 0)
	tips := ""
	if reqVersion < sensitiveWordsVersion {
		resultList = sensitive_words
		tips = "涉及私下交易、转账等资金问题，谨慎对待，谨防上当受骗，点击标题栏头像可投诉！"
	}
	c.Response(&resp{
		Tips:    tips,
		List:    resultList,
		Version: sensitiveWordsVersion,
	})
}

// // 接受IM的消息
// func (m *Message) notify(c *wkhttp.Context) {
// 	data, err := c.GetRawData()
// 	if err != nil {
// 		m.Error("notify读取数据失败！", zap.Error(err))
// 		c.ResponseError(err)
// 		return
// 	}
// 	var msgResps []msgResp
// 	err = util.ReadJsonByByte(data, &msgResps)
// 	if err != nil {
// 		m.Error("读取消息数据失败！", zap.Error(err))
// 		c.ResponseError(err)
// 		return
// 	}
// 	tx, _ := m.db.session.Begin()
// 	defer func() {
// 		if err := recover(); err != nil {
// 			tx.Rollback()
// 			panic(err)
// 		}
// 	}()
// 	messageIDS := make([]string, 0, len(msgResps))
// 	for _, msgResp := range msgResps {
// 		messageIDS = append(messageIDS, strconv.FormatUint(msgResp.MessageID, 10))
// 		messageModel := msgResp.ToModel()
// 		err = m.db.InsertTx(messageModel, tx)
// 		if err != nil {
// 			tx.Rollback()
// 			m.Error("添加消息失败！", zap.Any("msg", msgResp), zap.Error(err))
// 			c.ResponseError(err)
// 			return
// 		}
// 	}
// 	if err := tx.Commit(); err != nil {
// 		tx.Rollback()
// 		m.Error("提交事务失败！", zap.Error(err))
// 		c.ResponseError(err)
// 		return
// 	}
// 	c.Response(messageIDS)
// }

// ---------- vo ----------

type syncChannelMessageResp struct {
	StartMessageSeq uint32          `json:"start_message_seq"` // 开始序列号
	EndMessageSeq   uint32          `json:"end_message_seq"`   // 结束序列号
	PullMode        config.PullMode `json:"pull_mode"`         // 拉取模式
	More            int             `json:"more"`              // 是否还有更多 1.是 0.否
	Messages        []*MsgSyncResp  `json:"messages"`          // 消息数据
}

func newSyncChannelMessageResp(resp *config.SyncChannelMessageResp, loginUID string, deviceUUID string, channelID string, channelType uint8, messageExtraDB *messageExtraDB, messageUserExtraDB *messageUserExtraDB, messageReactionDB *messageReactionDB, channelOffsetDB *channelOffsetDB, deviceOffsetDB *deviceOffsetDB, channelOffsetMessageSeq uint32) *syncChannelMessageResp {
	messages := make([]*MsgSyncResp, 0, len(resp.Messages))
	if len(resp.Messages) > 0 {
		messageIDs := make([]string, 0, len(resp.Messages))
		for _, message := range resp.Messages {
			// 字节超过 LargePayloadThreshold 时跳过 reply 解析，纯性能权衡——避免对超大
			// payload 重复反序列化。新 TruncatedPayload 仅对 Text 按 rune 截
			// （issue #1310），其余类型原样下发，所以这里跳过的代价仅是个别长消息
			// 失去 reply 富化（content 本身完整下发）。
			if len(message.Payload) <= LargePayloadThreshold {
				var payloadMap map[string]interface{}
				err := util.ReadJsonByByte(message.Payload, &payloadMap)
				if err != nil {
					log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(message.Payload)))
				}
				if len(payloadMap) > 0 {
					replyJson := payloadMap["reply"]
					if replyMap, ok := replyJson.(map[string]interface{}); ok {
						if msgId, ok := replyMap["message_id"].(string); ok {
							messageIDs = append(messageIDs, msgId)
						}
					}
				}
			}
			messageIDs = append(messageIDs, fmt.Sprintf("%d", message.MessageID))
		}

		// 消息全局扩张
		messageExtras, err := messageExtraDB.queryWithMessageIDsAndUID(messageIDs, loginUID)
		if err != nil {
			log.Error("查询消息扩展字段失败！", zap.Error(err))
		}
		// 修改消息扩展字段
		for _, message := range resp.Messages {
			// 字节超过阈值时跳过 reply 富化，纯性能权衡——避免对超大 payload 重复反序列化。
			// TruncatedPayload 仅 Text 按 rune 截（issue #1310），其余原样下发，
			// 跳过仅意味着个别长消息失去 reply 富化，content 完整下发。
			if len(message.Payload) > LargePayloadThreshold {
				continue
			}
			var payloadMap map[string]interface{}
			err := util.ReadJsonByByte(message.Payload, &payloadMap)
			if err != nil {
				log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(message.Payload)))
			}
			if len(payloadMap) > 0 {
				replyJson := payloadMap["reply"]
				replyMap, ok := replyJson.(map[string]interface{})
				if !ok {
					continue
				}
				msgId, ok := replyMap["message_id"].(string)
				if !ok {
					continue
				}
				for _, messageExtra := range messageExtras {
					if messageExtra.MessageID == msgId {
						var contentEditMap map[string]interface{}
						if messageExtra.ContentEdit.String != "" {
							err := util.ReadJsonByByte([]byte(messageExtra.ContentEdit.String), &contentEditMap)
							if err != nil {
								log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(messageExtra.ContentEdit.String)))
								continue
							}
							replyMap["payload"] = contentEditMap
							payloadMap["reply"] = replyMap
							message.Payload = []byte(util.ToJson(payloadMap))
						}
						break
					}
				}
			}
		}
		messageExtraMap := map[string]*messageExtraDetailModel{}
		if len(messageExtras) > 0 {
			for _, messageExtra := range messageExtras {
				messageExtraMap[messageExtra.MessageID] = messageExtra
			}
		}

		// 消息用户扩张
		messageUserExtras, err := messageUserExtraDB.queryWithMessageIDsAndUID(messageIDs, loginUID)
		if err != nil {
			log.Error("查询用户消息扩展字段失败！", zap.Error(err))
		}
		messageUserExtraMap := map[string]*messageUserExtraModel{}
		if len(messageUserExtras) > 0 {
			for _, messageUserExtraM := range messageUserExtras {
				messageUserExtraMap[messageUserExtraM.MessageID] = messageUserExtraM
			}
		}

		// 查询消息回应
		messageReaction, err := messageReactionDB.queryWithMessageIDs(messageIDs)
		if err != nil {
			log.Error("查询消息回应数据错误", zap.Error(err))
		}
		messageReactionMap := map[string][]*reactionModel{}
		if len(messageReaction) > 0 {
			for _, reaction := range messageReaction {
				msgReactionList := messageReactionMap[reaction.MessageID]
				if msgReactionList == nil {
					msgReactionList = make([]*reactionModel, 0)
				}
				msgReactionList = append(msgReactionList, reaction)
				messageReactionMap[reaction.MessageID] = msgReactionList
			}
		}

		// 用户频道偏移
		channelOffsetM, err := channelOffsetDB.queryWithUIDAndChannel(loginUID, channelID, channelType)
		if err != nil {
			log.Error("查询频道偏移量失败！", zap.Error(err))
		}

		// 设备偏移
		deviceLastMessageSeq, err := deviceOffsetDB.queryMessageSeq(loginUID, deviceUUID, channelID, channelType)
		if err != nil {
			log.Error("查询设备消息偏移量失败！", zap.Error(err))
		}
		for _, message := range resp.Messages {
			if channelOffsetM != nil && message.MessageSeq <= channelOffsetM.MessageSeq {
				continue
			}
			if message.MessageSeq <= uint32(deviceLastMessageSeq) {
				continue
			}
			messageIDStr := strconv.FormatInt(message.MessageID, 10)
			messageExtra := messageExtraMap[messageIDStr]
			messageUserExtra := messageUserExtraMap[messageIDStr]
			msgResp := &MsgSyncResp{}
			msgResp.from(message, loginUID, messageExtra, messageUserExtra, messageReactionMap[strconv.FormatInt(message.MessageID, 10)], channelOffsetMessageSeq)
			messages = append(messages, msgResp)
		}
	}
	return &syncChannelMessageResp{
		StartMessageSeq: resp.StartMessageSeq,
		EndMessageSeq:   resp.EndMessageSeq,
		PullMode:        resp.PullMode,
		Messages:        messages,
	}
}

// 消息头
type messageHeader struct {
	NoPersist int `json:"no_persist"` // 是否不持久化
	RedDot    int `json:"red_dot"`    // 是否显示红点
	SyncOnce  int `json:"sync_once"`  // 此消息只被同步或被消费一次
}

type syncReq struct {
	MaxMessageSeq uint32 `json:"max_message_seq"` // 客户端最大消息序列号
	Limit         int    `json:"limit"`           // 消息数量限制
	ChannelID     string `json:"channel_id"`      // 频道ID
	ChannelType   uint8  `json:"channel_type"`    // 频道类型
	Reverse       int    `json:"reverse"`         // 是否倒序
	Offset        int64  `json:"offset"`          // 偏移量
}

// type msgResp struct {
// 	MessageID   uint64 `json:"message_id"`   // 服务端的消息ID(全局唯一)
// 	FromUID     string `json:"from_uid"`     // 发送者UID
// 	ChannelID   string `json:"channel_id"`   // 频道ID
// 	ChannelType uint8  `json:"channel_type"` // 频道类型
// 	Timestamp   int64  `json:"timestamp"`    // 服务器消息时间戳(10位，到秒)
// 	Payload     []byte `json:"payload"`      // 消息内容
// }

// func (m msgResp) ToModel() *messageModel {
// 	var payloadMap map[string]interface{}
// 	err := util.ReadJsonByByte(m.Payload, &payloadMap)
// 	if err != nil {
// 		log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(m.Payload)))
// 	}
// 	contentType := 0
// 	if payloadMap != nil {
// 		if payloadMap["type"] != nil {
// 			contentTypeInt64, _ := payloadMap["type"].(json.Number).Int64()
// 			contentType = int(contentTypeInt64)
// 		}
// 		// if payloadMap["content"] != nil {
// 		// 	keyword = payloadMap["content"].(string)
// 		// }
// 	}
// 	return &messageModel{
// 		MessageID:   int64(m.MessageID),
// 		FromUID:     m.FromUID,
// 		ChannelID:   m.ChannelID,
// 		ChannelType: m.ChannelType,
// 		Timestamp:   m.Timestamp,
// 		Payload:     m.Payload,
// 		Type:        contentType,
// 	}
// }

// type replyMsgSyncResp struct {
// 	Root     *config.MessageResp   `json:"root"`
// 	Messages []*config.MessageResp `json:"messages"`
// }

// MgSyncResp 消息同步请求
type MsgSyncResp struct {
	Header       messageHeader `json:"header"`              // 消息头部
	Setting      uint8         `json:"setting"`             // 设置
	MessageID    int64         `json:"message_id"`          // 服务端的消息ID(全局唯一)
	MessageIDStr string        `json:"message_idstr"`       // 服务端的消息ID(全局唯一)字符串形式
	MessageSeq   uint32        `json:"message_seq"`         // 消息序列号 （用户唯一，有序递增）
	ClientMsgNo  string        `json:"client_msg_no"`       // 客户端消息唯一编号
	StreamNo     string        `json:"stream_no,omitempty"` // 流编号
	FromUID      string        `json:"from_uid"`            // 发送者UID
	// 外部来源标识：仅在 /message/channel/sync 群聊路径填充，供前端在外部群渲染来源 Space 徽标。
	// 详见 Mininglamp-OSS/octo-server#1188。
	FromIsExternal      int    `json:"from_is_external"`                 // 发送者是否为外部成员 0.否 1.是
	FromSourceSpaceName string `json:"from_source_space_name,omitempty"` // 发送者来源 Space 名称（为空则前端不渲染）
	// 归属 Space（YUJ-63 / #1208）：外部/内部语义由前端"相对当前查看 Space"判断。
	// 外部成员：from_home_space_id = 发送者来源 space_id；
	// 内部成员：from_home_space_id = 群自身 space_id。
	// 后端 from_is_external / from_source_space_name 原语义保留。
	FromHomeSpaceID   string                 `json:"from_home_space_id,omitempty"`   // 发送者归属 Space ID
	FromHomeSpaceName string                 `json:"from_home_space_name,omitempty"` // 发送者归属 Space 名称
	ToUID             string                 `json:"to_uid,omitempty"`               // 接受者uid
	ChannelID         string                 `json:"channel_id"`                     // 频道ID
	ChannelType       uint8                  `json:"channel_type"`                   // 频道类型
	Expire            uint32                 `json:"expire,omitempty"`               // expire
	Timestamp         int32                  `json:"timestamp"`                      // 服务器消息时间戳(10位，到秒)
	Payload           map[string]interface{} `json:"payload"`                        // 消息内容
	SignalPayload     string                 `json:"signal_payload"`                 // signal 加密后的payload base64编码,TODO: 这里为了兼容没加密的版本，所以新用SignalPayload字段
	ReplyCount        int                    `json:"reply_count,omitempty"`          // 回复集合
	ReplyCountSeq     string                 `json:"reply_count_seq,omitempty"`      // 回复数量seq
	ReplySeq          string                 `json:"reply_seq,omitempty"`            // 回复seq
	Reactions         []*reactionSimpleResp  `json:"reactions,omitempty"`            // 回应数据
	IsDeleted         int                    `json:"is_deleted"`                     // 是否已删除
	VoiceStatus       int                    `json:"voice_status,omitempty"`         // 语音状态 0.未读 1.已读
	Streams           []*streamItemResp      `json:"streams,omitempty"`              // 流数据
	// ---------- 旧字段 这些字段都放到MessageExtra对象里了 ----------
	Readed       int    `json:"readed"`                 // 是否已读（针对于自己）
	Revoke       int    `json:"revoke,omitempty"`       // 是否撤回
	Revoker      string `json:"revoker,omitempty"`      // 消息撤回者
	ReadedCount  int    `json:"readed_count,omitempty"` // 已读数量
	UnreadCount  int    `json:"unread_count,omitempty"` // 未读数量
	ExtraVersion int64  `json:"extra_version"`          // 扩展数据版本号

	// 消息扩展字段
	MessageExtra *messageExtraResp `json:"message_extra,omitempty"` // 消息扩展

}

func (m *MsgSyncResp) from(msgResp *config.MessageResp, loginUID string, messageExtraM *messageExtraDetailModel, messageUserExtraM *messageUserExtraModel, reactionModels []*reactionModel, channelOffsetMessageSeq uint32) {
	m.Header.NoPersist = msgResp.Header.NoPersist
	m.Header.RedDot = msgResp.Header.RedDot
	m.Header.SyncOnce = msgResp.Header.SyncOnce
	m.Setting = msgResp.Setting
	m.MessageID = msgResp.MessageID
	m.MessageIDStr = strconv.FormatInt(msgResp.MessageID, 10)
	m.MessageSeq = msgResp.MessageSeq
	m.ClientMsgNo = msgResp.ClientMsgNo
	m.StreamNo = msgResp.StreamNo
	m.FromUID = msgResp.FromUID
	m.ToUID = msgResp.ToUID
	m.ChannelID = msgResp.ChannelID
	m.ChannelType = msgResp.ChannelType
	m.Expire = msgResp.Expire
	m.Timestamp = msgResp.Timestamp
	if messageExtraM != nil {
		// TODO: 后续这些字段可以废除了 都放MessageExtra对象里了
		m.IsDeleted = messageExtraM.IsDeleted
		m.Revoke = messageExtraM.Revoke
		m.Revoker = messageExtraM.Revoker
		m.ReadedCount = messageExtraM.ReadedCount
		m.Readed = messageExtraM.Readed
		m.ExtraVersion = messageExtraM.Version

		m.MessageExtra = newMessageExtraResp(messageExtraM)
	}

	setting := config.SettingFromUint8(msgResp.Setting)
	var payloadMap map[string]interface{}
	if setting.Signal {
		m.SignalPayload = base64.StdEncoding.EncodeToString(msgResp.Payload)
		payloadMap = map[string]interface{}{
			"type": common.SignalError.Int(),
		}
	} else if len(msgResp.Payload) > LargePayloadThreshold {
		// 超过 caller 阈值进入 TruncatedPayload：Text 按 rune 截，其它类型原样下发，
		// 是否真截断由 TruncatedPayload 内部按 type 决定。
		log.Warn("消息 payload 超过大小阈值，进入 TruncatedPayload 处理",
			zap.Int64("message_id", msgResp.MessageID),
			zap.String("from_uid", msgResp.FromUID),
			zap.String("channel_id", msgResp.ChannelID),
			zap.Int("payload_size", len(msgResp.Payload)))
		payloadMap = TruncatedPayload(msgResp.Payload)
	} else {
		err := util.ReadJsonByByte(msgResp.Payload, &payloadMap)
		if err != nil {
			log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(msgResp.Payload)))
		}
		if len(payloadMap) == 0 {
			payloadMap = map[string]interface{}{
				"type": common.ContentError.Int(),
			}
		}
	}

	// visibles 白名单（截断 / 正常路径共用，避免超大消息绕过权限检查）。
	if visiblesArray, ok := payloadMap["visibles"].([]interface{}); ok && len(visiblesArray) > 0 {
		m.IsDeleted = 1
		for _, limitUID := range visiblesArray {
			if limitUID == loginUID {
				m.IsDeleted = 0
			}
		}
	}

	// type=Text 的 content 强制 string 化，避免 bot 误发 object 导致前端按 string 解析失败。
	CoerceTextPayloadContent(payloadMap)

	if messageUserExtraM != nil {
		if m.IsDeleted == 0 {
			m.IsDeleted = messageUserExtraM.MessageIsDeleted
		}
		m.VoiceStatus = messageUserExtraM.VoiceReaded
	}

	if msgResp.Expire > 0 {
		if time.Now().Unix()-int64(msgResp.Expire) >= int64(msgResp.Timestamp) {
			m.IsDeleted = 1
		}
	}
	if channelOffsetMessageSeq != 0 && msgResp.MessageSeq <= channelOffsetMessageSeq {
		m.IsDeleted = 1
	}
	m.Payload = payloadMap

	msgReactionList := make([]*reactionSimpleResp, 0, len(reactionModels))
	if len(reactionModels) > 0 {
		for _, reaction := range reactionModels {
			msgReactionList = append(msgReactionList, &reactionSimpleResp{
				UID:       reaction.UID,
				Name:      reaction.Name,
				Seq:       reaction.Seq,
				IsDeleted: reaction.IsDeleted,
				Emoji:     reaction.Emoji,
				CreatedAt: reaction.CreatedAt.String(),
			})
		}
	}
	m.Reactions = msgReactionList

	if len(msgResp.Streams) > 0 {
		streams := make([]*streamItemResp, 0, len(msgResp.Streams))
		for _, streamItem := range msgResp.Streams {
			streams = append(streams, newStreamItemResp(streamItem))
		}
		m.Streams = streams
	}

}

type streamItemResp struct {
	StreamSeq   uint32         `json:"stream_seq"`    // 流序号
	ClientMsgNo string         `json:"client_msg_no"` // 客户端消息唯一编号
	Blob        map[string]any `json:"blob"`          // 消息内容
}

func newStreamItemResp(streamItem *config.StreamItemResp) *streamItemResp {
	var blobMap map[string]any
	err := util.ReadJsonByByte(streamItem.Blob, &blobMap)
	if err != nil {
		log.Warn("blob不是json格式！", zap.Error(err), zap.String("blob", string(streamItem.Blob)))
	}
	return &streamItemResp{
		ClientMsgNo: streamItem.ClientMsgNo,
		StreamSeq:   streamItem.StreamSeq,
		Blob:        blobMap,
	}
}

// 回应返回
type reactionResp struct {
	MessageID   string `json:"message_id"`   // 消息编号
	ChannelID   string `json:"channel_id"`   // 频道ID
	ChannelType uint8  `json:"channel_type"` // 频道类型
	Seq         int64  `json:"seq"`          // 回复序列号
	UID         string `json:"uid"`          // 回应用户ID
	Name        string `json:"name"`         // 回应用户名
	Emoji       string `json:"emoji"`        // 回应的emoji
	IsDeleted   int    `json:"is_deleted"`   // 是否删除
	CreatedAt   string `json:"created_at"`
}

// 回应返回
type reactionSimpleResp struct {
	Seq       int64  `json:"seq"`        // 回复序列号
	UID       string `json:"uid"`        // 回应用户ID
	Name      string `json:"name"`       // 回应用户名
	Emoji     string `json:"emoji"`      // 回应的emoji
	IsDeleted int    `json:"is_deleted"` // 是否删除
	CreatedAt string `json:"created_at"`
}

// type userResp struct {
// 	UID       string `json:"uid"`
// 	Name      string `json:"name"`
// 	IsDeleted int    `json:"is_deleted"`
// }

// type syncTotalResp struct {
// 	MessageID   string `json:"message_id"`   // 消息唯一ID
// 	Seq         string `json:"seq"`          // 回复序列号
// 	ChannelID   string `json:"channel_id"`   // 频道唯一ID
// 	ChannelType uint8  `json:"channel_type"` // 频道类型
// 	Count       int    `json:"count"`        // 回复数量
// }

type messageExtraResp struct {
	MessageID       int64                  `json:"message_id"`
	MessageIDStr    string                 `json:"message_id_str"`
	Revoke          int                    `json:"revoke,omitempty"`
	Revoker         string                 `json:"revoker,omitempty"`
	VoiceStatus     int                    `json:"voice_status,omitempty"`
	Readed          int                    `json:"readed,omitempty"`            // 是否已读（针对于自己）
	ReadedCount     int                    `json:"readed_count,omitempty"`      // 已读数量
	ReadedAt        int64                  `json:"readed_at,omitempty"`         // 已读时间
	IsMutualDeleted int                    `json:"is_mutual_deleted,omitempty"` // 双向删除
	IsPinned        int                    `json:"is_pinned,omitempty"`         // 是否置顶
	ContentEdit     map[string]interface{} `json:"content_edit,omitempty"`      // 编辑后的正文
	EditedAt        int                    `json:"edited_at,omitempty"`         // 编辑时间 例如 12:23
	ExtraVersion    int64                  `json:"extra_version"`               // 数据版本
}

func newMessageExtraResp(m *messageExtraDetailModel) *messageExtraResp {

	messageID, _ := strconv.ParseInt(m.MessageID, 10, 64)

	var contentEditMap map[string]interface{}
	if m.ContentEdit.String != "" {
		err := util.ReadJsonByByte([]byte(m.ContentEdit.String), &contentEditMap)
		if err != nil {
			log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(m.ContentEdit.String)))
		}
	}

	var readedAt int64 = 0
	if m.ReadedAt.Valid {
		readedAt = m.ReadedAt.Time.Unix()
	}

	return &messageExtraResp{
		MessageID:       messageID,
		MessageIDStr:    m.MessageID,
		Revoke:          m.Revoke,
		Revoker:         m.Revoker,
		Readed:          m.Readed,
		ReadedAt:        readedAt,
		ReadedCount:     m.ReadedCount,
		ContentEdit:     contentEditMap,
		EditedAt:        m.EditedAt,
		IsMutualDeleted: m.IsDeleted,
		IsPinned:        m.IsPinned,
		ExtraVersion:    m.Version,
	}
}

type memberReceiptResp struct {
	UID  string `json:"uid"`  // 成员uid
	Name string `json:"name"` // 成员名称
}

type ProhibitWordResp struct {
	Id        int64  `json:"id"`
	Content   string `json:"content"`    // 违禁词
	IsDeleted int    `json:"is_deleted"` // 是否删除
	Version   int64  `json:"version"`    // 版本
	CreatedAt string `json:"created_at"` // 时间
}

// payloadMsgType 从 payload 中提取消息类型，兼容 float64 和 json.Number
func payloadMsgType(payload map[string]interface{}) int {
	switch v := payload["type"].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// extractThreadShortIDs 从消息列表中提取 ThreadCreated 消息的 shortID
func extractThreadShortIDs(messages []*MsgSyncResp) []string {
	shortIDs := make([]string, 0)
	for _, msg := range messages {
		if msg.Payload == nil {
			continue
		}
		if payloadMsgType(msg.Payload) != thread.ContentTypeThreadCreated {
			continue
		}
		shortID, _ := msg.Payload["short_id"].(string)
		if shortID == "" {
			continue
		}
		shortIDs = append(shortIDs, shortID)
	}
	return shortIDs
}

// enrichThreadCreatedMessages 遍历群消息，对 ThreadCreated 类型的消息 payload 注入实时 thread 元数据
func (m *Message) enrichThreadCreatedMessages(messages []*MsgSyncResp) {
	shortIDs := extractThreadShortIDs(messages)
	if len(shortIDs) == 0 {
		return
	}

	// 批量查询
	metaMap, err := m.threadDB.QueryThreadMetaByShortIDs(shortIDs)
	if err != nil {
		m.Error("查询子区元数据失败", zap.Error(err))
		return
	}

	// 注入实时数据到 payload
	for _, msg := range messages {
		if msg.Payload == nil {
			continue
		}
		if payloadMsgType(msg.Payload) != thread.ContentTypeThreadCreated {
			continue
		}
		shortID, _ := msg.Payload["short_id"].(string)
		if meta, ok := metaMap[shortID]; ok {
			msg.Payload["message_count"] = meta.MessageCount
			if meta.SourceMessageID != nil {
				msg.Payload["source_message_id"] = *meta.SourceMessageID
			}
		}
	}
}

// applyExternalMarkerToUserItem 给 mergeforward content.users 中的单个 element 写入
// is_external / source_space_name / home_space_id / home_space_name 字段。
// elem 为 map[string]interface{} 才会生效；其他类型（含旧数据缺 uid 的元素）直接跳过，保证向后兼容。
func applyExternalMarkerToUserItem(elem interface{}, markers map[string]group.MemberExternalMarker) {
	userMap, ok := elem.(map[string]interface{})
	if !ok {
		return
	}
	uid, _ := userMap["uid"].(string)
	if uid == "" {
		// 无 uid 的元素无法匹配群成员，写入安全默认值避免前端读到 undefined。
		userMap["is_external"] = 0
		if _, exists := userMap["source_space_name"]; !exists {
			userMap["source_space_name"] = ""
		}
		userMap["home_space_id"] = ""
		userMap["home_space_name"] = ""
		return
	}
	marker, ok := markers[uid]
	if !ok {
		// 出现在 mergeforward 但已不在当前群的用户：标记为非外部，空 source_space_name。
		userMap["is_external"] = 0
		userMap["source_space_name"] = ""
		userMap["home_space_id"] = ""
		userMap["home_space_name"] = ""
		return
	}
	userMap["is_external"] = marker.IsExternal
	userMap["source_space_name"] = marker.SourceSpaceName
	userMap["home_space_id"] = marker.HomeSpaceID
	userMap["home_space_name"] = marker.HomeSpaceName
}

// enrichExternalMarkers 为群聊 /message/channel/sync 返回的每条消息注入外部来源标识。
//  1. 顶层 from_is_external / from_source_space_name（发送者视角）
//  2. from_home_space_id / from_home_space_name（YUJ-63 / #1208，前端相对当前 Space 渲染）
//  3. mergeforward (content type 11) payload.users 每个 element 的 is_external /
//     source_space_name / home_space_id / home_space_name
//
// 只做 O(N) 遍历 + O(1) 查找，整体至多一条 SQL，避免 N+1 JOIN。详见 Mininglamp-OSS/octo-server#1188。
func (m *Message) enrichExternalMarkers(groupNo string, messages []*MsgSyncResp) {
	if groupNo == "" || len(messages) == 0 {
		return
	}
	markers, err := m.groupService.GetMemberExternalMarkers(groupNo)
	if err != nil {
		m.Error("查询群成员外部来源标识失败", zap.Error(err), zap.String("group_no", groupNo))
		return
	}
	applyExternalMarkers(messages, markers)
}

// applyExternalMarkers 把批量查询好的 uid -> MemberExternalMarker 应用到消息数组上。
// 纯函数，不做 IO，便于单测。内部成员（marker.IsExternal == 0）不写 source_space_name，
// 避免无意义字段污染 payload。非群成员的 FromUID / users 元素一律写入安全默认值 0 / ""。
// 同时同步 from_home_space_id / from_home_space_name（YUJ-63 / #1208）：
// 不论内部外部成员，都按 marker.HomeSpaceID / HomeSpaceName 填充，让前端用一致字段做相对渲染。
func applyExternalMarkers(messages []*MsgSyncResp, markers map[string]group.MemberExternalMarker) {
	if len(messages) == 0 {
		return
	}
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if marker, ok := markers[msg.FromUID]; ok {
			msg.FromIsExternal = marker.IsExternal
			if marker.IsExternal == 1 {
				msg.FromSourceSpaceName = marker.SourceSpaceName
			}
			msg.FromHomeSpaceID = marker.HomeSpaceID
			msg.FromHomeSpaceName = marker.HomeSpaceName
		}
		if msg.Payload == nil {
			continue
		}
		if payloadMsgType(msg.Payload) != common.MultipleForward.Int() {
			continue
		}
		usersList, ok := msg.Payload["users"].([]interface{})
		if !ok || len(usersList) == 0 {
			continue
		}
		for _, u := range usersList {
			applyExternalMarkerToUserItem(u, markers)
		}
	}
}
