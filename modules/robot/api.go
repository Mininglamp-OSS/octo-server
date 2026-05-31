package robot

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"io"
	"mime"
	"path/filepath"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/gookit/goutil/maputil"
	sts "github.com/tencentyun/qcloud-cos-sts-sdk/go"
	"go.uber.org/zap"
)

// IService 为其他模块提供的窄接口，避免持有完整 *Robot 以及由此产生的循环依赖。
// YUJ-60: 允许 bot 创建者撤回自己 bot 发的消息时，由 message 模块注入并调用。
//
// YUJ-1424 (PR#82 Jerry-Xin review blocker, 2026-05-20): EnqueueBotEvent
// exposes the bot event queue write so cross-module callers (specifically
// the OBO fan-out path in modules/bot_api) can deliver synthetic events
// without going through WuKongIM → webhook → NotifyMessagesListeners.
// The webhook drops NoPersist=1 messages before notifying listeners
// (modules/webhook/api.go handleMessageNotify, by design — see the
// content-type-contract comment in modules/bot_api/obo_fanout.go), so
// the OBO fan-out copy (which intentionally sets NoPersist=1 to keep the
// copy out of chat history) never reaches the bot event queue. Direct
// enqueue bypasses that filter.
type IService interface {
	// GetCreatorUID 带缓存地查询机器人的创建者 UID。
	// 机器人不存在或无 creator_uid 时返回空字符串及 nil error；
	// 仅在底层查询异常时才返回 error。
	GetCreatorUID(robotID string) (string, error)
	// EnqueueBotEvent appends a synthetic event for `robotID` to the bot
	// event queue consumed by /v1/bot/events. Mirrors the schema used by
	// (*Robot).saveRobotMessage so /v1/bot/events serves both organic and
	// synthetic events transparently. Returns an error only when the
	// Redis ZADD / GenSeq call fails.
	EnqueueBotEvent(robotID string, message *config.MessageResp) error
	// ExistRobot reports whether `uid` identifies an active robot
	// (robot.status=1). Mininglamp-OSS/octo-server#144: the ingress
	// chokepoint that expands `mention.ais=1` into `mention.uids` uses
	// this to filter the channel's group-member list down to the bot
	// subset, so legacy adapter bots that only inspect `mention.uids`
	// still receive the `@所有 AI` broadcast over the WuKongIM payload.
	//
	// Returns false (no error) for unknown / disabled robots — callers
	// can treat any non-nil error as a "lookup failed" and skip the
	// expansion best-effort (an unexpanded broadcast is no worse than
	// the pre-#144 state).
	ExistRobot(uid string) (bool, error)
}

// Service robot 模块对外暴露的只读服务实现，供其它模块注入使用。
// 与 *Robot 共享底层表结构，但不承担消息/事件监听等副作用，
// 因此可以被重复 New 出来而不会导致重复注册 listener。
type Service struct {
	ctx          *config.Context
	db           *robotDB
	creatorCache sync.Map // robotID -> creatorUID
}

// NewService 构造一个只读 robot 服务，满足 IService 接口。
func NewService(ctx *config.Context) IService {
	return &Service{
		ctx: ctx,
		db:  newBotDB(ctx),
	}
}

// GetCreatorUID 查询机器人的创建者 UID，带 sync.Map 缓存。
// 未命中（bot 不存在）时返回空串 + nil，调用方据此判定为“非 bot / 无 owner”。
func (s *Service) GetCreatorUID(robotID string) (string, error) {
	if v, ok := s.creatorCache.Load(robotID); ok {
		return v.(string), nil
	}
	uid, err := s.db.queryCreatorUID(robotID)
	if err != nil {
		// 未查到记录 → 视为“不是有效 bot”，缓存空串避免反复 DB 查询。
		if errors.Is(err, dbr.ErrNotFound) {
			s.creatorCache.Store(robotID, "")
			return "", nil
		}
		return "", err
	}
	s.creatorCache.Store(robotID, uid)
	return uid, nil
}

// GetCreatorUID 让 *Robot 同时实现 IService，便于已有 Robot 实例的场景直接复用。
// 内部委托给已有的 getCreatorUID（含 sync.Map 缓存）。
func (rb *Robot) GetCreatorUID(robotID string) (string, error) {
	uid, err := rb.getCreatorUID(robotID)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return uid, nil
}

// EnqueueBotEvent — IService — synthetic-event delivery path. See the
// IService docstring for the YUJ-1424 / PR#82 R-blocker rationale. The
// queue schema (key, score, payload shape, expiry) MUST match
// (*Robot).saveRobotMessage exactly; if that helper's wire format ever
// changes, update both sites in lockstep so /v1/bot/events serves
// synthetic and organic events identically.
func (s *Service) EnqueueBotEvent(robotID string, message *config.MessageResp) error {
	return enqueueBotEventGeneric(s.ctx, robotID, message)
}

// EnqueueBotEvent — IService — *Robot variant. Delegates to the same
// helper used by saveRobotMessage / Service.EnqueueBotEvent so the
// queue write semantics cannot drift between the listener fast-path and
// the cross-module synthetic path.
func (rb *Robot) EnqueueBotEvent(robotID string, message *config.MessageResp) error {
	return enqueueBotEventGeneric(rb.ctx, robotID, message)
}

// ExistRobot — IService — Service variant. Delegates to the same
// robotDB.exist helper used by /v1/manager/robots etc., scoped to
// `status=1` (active robots only). See the IService docstring for the
// Mininglamp-OSS/octo-server#144 rationale.
func (s *Service) ExistRobot(uid string) (bool, error) {
	if strings.TrimSpace(uid) == "" {
		return false, nil
	}
	return s.db.exist(uid)
}

// ExistRobot — IService — *Robot variant. Delegates to the embedded
// robotDB.exist so existing *Robot instances satisfy the wider
// IService surface introduced for Mininglamp-OSS/octo-server#144.
func (rb *Robot) ExistRobot(uid string) (bool, error) {
	if strings.TrimSpace(uid) == "" {
		return false, nil
	}
	return rb.db.exist(uid)
}

// enqueueBotEventGeneric is the shared write-to-bot-event-queue helper
// used by saveRobotMessage (listener path) and EnqueueBotEvent (cross-
// module synthetic path). Centralizing the GenSeq / ZAdd / Expire shape
// here means the bot event consumer (/v1/bot/events) sees identical
// records regardless of which path produced them.
func enqueueBotEventGeneric(ctx *config.Context, robotID string, message *config.MessageResp) error {
	if ctx == nil {
		return errors.New("robot: nil ctx, cannot enqueue bot event")
	}
	if strings.TrimSpace(robotID) == "" {
		return errors.New("robot: empty robotID, cannot enqueue bot event")
	}
	if message == nil {
		return errors.New("robot: nil message, cannot enqueue bot event")
	}
	// YUJ-2531 / Mininglamp-OSS/octo-server#208: bot-delivery chokepoint
	// (synthetic-event path). Mirror saveRobotMessage: strip any bare
	// legacy `mention.all=1` and inject `mention.humans=1` on a copy so
	// the bot event queue never carries the legacy broadcast flag.
	if normalized := stripBareMentionAllForBot(message.Payload); !bytes.Equal(normalized, message.Payload) {
		cp := *message
		cp.Payload = normalized
		message = &cp
	}
	seq, err := ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))
	if err != nil {
		return err
	}
	messageUpdateJson := util.ToJson(&robotEvent{
		EventID: seq,
		Message: message,
		Expire:  time.Now().Add(ctx.GetConfig().Robot.MessageExpire).Unix(),
	})
	key := fmt.Sprintf("robotEvent:%s", robotID)
	if err := ctx.GetRedisConn().ZAdd(key, float64(seq), messageUpdateJson); err != nil {
		return err
	}
	if err := ctx.GetRedisConn().Expire(key, ctx.GetConfig().Robot.MessageExpire); err != nil {
		// Best-effort TTL refresh — do not fail the enqueue. Mirrors
		// saveRobotMessage which also only logs on Expire failure.
		return nil
	}
	return nil
}

type Robot struct {
	ctx *config.Context
	log.Log
	db                                robotDB
	robotEventPrefix                  string
	userService                       user.IService
	appService                        app.IService
	groupService                      group.IService
	fileService                       file.IService
	inlineQueryEventsMap              map[string][]*robotEvent // inlineQuery事件
	inlineQueryEventsMapLock          sync.RWMutex
	inlineQueryEventResultChanMap     map[string]chan *InlineQueryResult
	inlineQueryEventResultChanMapLock sync.RWMutex
	mentionRegexp                     *regexp.Regexp
	creatorCache                      sync.Map      // robotID -> creatorUID 缓存
	msgSem                            chan struct{} // semaphore to limit concurrent message processing goroutines
	// spaceQuerier overrides &rb.db for enrichBotPayloadWithSpaceID (test injection).
	// nil in production; tests set it to stub the DB call deterministically.
	spaceQuerier robotSpaceQuerier
}

func New(ctx *config.Context) *Robot {
	rb := &Robot{
		ctx:                           ctx,
		Log:                           log.NewTLog("Robot"),
		db:                            *newBotDB(ctx),
		robotEventPrefix:              "robotEvent:",
		userService:                   user.NewService(ctx),
		appService:                    app.NewService(ctx),
		groupService:                  group.NewService(ctx),
		fileService:                   file.NewService(ctx),
		inlineQueryEventsMap:          map[string][]*robotEvent{},
		inlineQueryEventResultChanMap: map[string]chan *InlineQueryResult{},
		mentionRegexp:                 regexp.MustCompile(`@\S+`),
		msgSem:                        make(chan struct{}, 100), // limit concurrent message processing goroutines
	}
	ctx.AddMessagesListener(rb.messagesListen)

	ctx.AddMessagesListener(rb.robotMessageListen)

	return rb
}

// Route 路由配置
func (rb *Robot) Route(r *wkhttp.WKHttp) {

	auth := r.Group("/v1", rb.ctx.AuthMiddleware(r))
	{
		auth.POST("/robot/sync", rb.sync)                            // 同步机器人菜单
		auth.POST("/robot/inline_query", rb.inlineQuery)             // 机器人行内搜索
		auth.GET("/robot/commands", rb.getCommands)                  // 查询机器人命令列表
		auth.PUT("/robot/:robot_id/description", rb.setDescription)  // 设置 Bot 简介
		auth.PUT("/robot/:robot_id/auto_approve", rb.setAutoApprove) // 设置是否自动通过好友申请
		auth.GET("/robot/space_bots", rb.spaceBots)                  // Bot 广场 — Space 内所有 Bot
		auth.GET("/robot/my_bots", rb.myBots)                        // 我的 Bot — 已添加好友的 Bot
	}

	robotAuth := r.Group("/v1/robots/:robot_id/:app_key", rb.authRobot()) // :robot_id即user的username
	{
		robotAuth.GET("/events", rb.getEventsForGet)                  // 获取事件
		robotAuth.POST("/events", rb.getEventsForPost)                // 获取事件（POST方式）
		robotAuth.POST("/events/:event_id/ack", rb.eventAck)          // 事件确认
		robotAuth.POST("/answerInlineQuery", rb.answerInlineQuery)    // 响应inlineQuery
		robotAuth.POST("/sendMessage", rb.sendMessage)                // 发送消息
		robotAuth.POST("/typing", rb.typing)                          // 输入中
		robotAuth.POST("/stream/start", rb.streamStart)               // 流式消息开启
		robotAuth.POST("/stream/end", rb.streamEnd)                   // 流式消息结束
		robotAuth.GET("/file/*path", rb.proxyFile)                    // 文件下载代理
		robotAuth.POST("/upload", rb.botUploadFile)                   // 文件上传
		robotAuth.GET("/upload/credentials", rb.botUploadCredentials) // STS 临时密钥签发
		robotAuth.GET("/upload/presigned", rb.botUploadPresigned)     // 预签名上传 URL 签发
		robotAuth.POST("/message/edit", rb.botMessageEdit)            // Bot 编辑消息
		// GROUP.md routes are in botfather module (/v1/bot/groups/:group_no/md)

	}

	if err := rb.insertSystemRobot(); err != nil {
		rb.Error("初始化系统机器人失败", zap.Error(err))
	}
}

func (rb *Robot) streamStart(c *wkhttp.Context) {
	var req config.MessageStreamStartReq
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}

	streamNo, err := rb.ctx.IMStreamStart(req)
	if err != nil {
		rb.Error("发送stream start消息失败！", zap.Error(err))
		c.ResponseError(errors.New("发送stream start消息失败！"))
		return
	}
	c.Response(gin.H{
		"stream_no": streamNo,
	})
}

func (rb *Robot) streamEnd(c *wkhttp.Context) {
	var req config.MessageStreamEndReq
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	err := rb.ctx.IMStreamEnd(req)
	if err != nil {
		rb.Error("发送stream end消息失败！", zap.Error(err))
		c.ResponseError(errors.New("发送stream end消息失败！"))
		return
	}
	c.ResponseOK()
}

func (rb *Robot) authRobot() wkhttp.HandlerFunc {

	return func(c *wkhttp.Context) {
		robotID := c.Param("robot_id")
		appKey := c.Param("app_key")

		robot, err := rb.db.queryVaildRobotWithRobtID(robotID)
		if err != nil {
			rb.Error("查询robot失败！", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"msg": "查询robot失败！",
			})
			return
		}
		if robot == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"msg": "机器人不存在！",
			})
			return
		}
		appM, err := rb.appService.GetApp(robot.AppID)
		if err != nil {
			rb.Error("查询app失败！", zap.Error(err), zap.String("appID", robot.AppID))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"msg": "查询app失败！",
			})
			return
		}
		if appM == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"msg": "app不存在！",
			})
			return
		}
		if !hmac.Equal([]byte(appM.AppKey), []byte(appKey)) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"msg": "appKey不正确！",
			})
			return
		}
		c.Next()
	}
}

func (rb *Robot) typing(c *wkhttp.Context) {
	var req *TypingReq
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空！"))
		return
	}
	if req.ChannelType == 0 {
		c.ResponseError(errors.New("channel_type不能为空！"))
		return
	}
	fromUID := c.Param("robot_id")
	if fromUID == "" {
		c.ResponseError(errors.New("from_uid不能为空！"))
		return
	}
	if !rb.allowSendToChannel(fromUID, req.ChannelID, req.ChannelType) {
		c.ResponseError(errors.New("不允许发送消息到此频道！"))
		return
	}
	err := rb.ctx.SendTyping(req.ChannelID, req.ChannelType, fromUID)
	if err != nil {
		rb.Error("发送typing消息失败！", zap.Error(err))
		c.ResponseError(errors.New("发送typing消息失败！"))
		return
	}
	c.ResponseOK()
}

func (rb *Robot) sendMessage(c *wkhttp.Context) {
	var messageReq *MessageReq
	if err := c.BindJSON(&messageReq); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if strings.TrimSpace(messageReq.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空！"))
		return
	}
	if messageReq.ChannelType == 0 {
		c.ResponseError(errors.New("channel_type不能为空！"))
		return
	}
	if len(messageReq.Payload) == 0 {
		c.ResponseError(errors.New("payload不能为空！"))
		return
	}

	robotID := c.Param("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("robot_id不能为空！"))
		return
	}
	if !rb.allowSendToChannel(robotID, messageReq.ChannelID, messageReq.ChannelType) {
		c.ResponseError(errors.New("不允许发送消息到此频道！"))
		return
	}

	// YUJ-1393 / PR#82 review #2 R1 (Jerry-Xin 2026-05-19 follow-up):
	// strip any reserved `__obo_*` top-level key from the robot-supplied
	// payload BEFORE validation / dispatch. The legacy robot endpoint
	// was previously the only one of the three ingress points (user /
	// bot / robot) that let `__obo_processed__: true` through unmodified,
	// which a misbehaving / malicious robot script could exploit to
	// suppress its own persona-clone fan-out copy (fan-out gate 3 in
	// modules/bot_api/obo_fanout.go drops any payload carrying the
	// marker). See modules/robot/sanitize_robot_ingress.go for the full
	// rationale, the test surface, and why this ingress follows the
	// silent-strip precedent set by the user API rather than the loud
	// 4xx-reject precedent set by the bot API.
	sanitizeRobotIngressPayload(messageReq.Payload, messageReq.ChannelID, messageReq.ChannelType, robotID, rb.Warn)

	payloadResult := maputil.Data(messageReq.Payload)
	contentTypeValue := payloadResult.Int("type")
	if contentTypeValue == 0 {
		c.ResponseError(errors.New("payload.type不能为空！"))
		return
	}
	contentType := common.ContentType(contentTypeValue)
	if !rb.supportContentType(contentType) {
		c.ResponseError(fmt.Errorf("不支持的type[%d]", contentType))
		return
	}

	if !rb.payloadIsVail(payloadResult) {
		c.ResponseError(fmt.Errorf("无效的payload[%s]", util.ToJson(messageReq.Payload)))
		return
	}
	userResp, err := rb.userService.GetUserWithUsername(robotID)
	if err != nil {
		rb.Error("查询机器人的用户信息失败！", zap.Error(err))
		c.ResponseError(fmt.Errorf("获取机器人[%s]信息失败！", robotID))
		return
	}
	if userResp == nil {
		c.ResponseError(fmt.Errorf("机器人[%s]不存在！", robotID))
		return
	}
	// YUJ-644 / Mininglamp-OSS#33: PERSONAL DM 派发前服务端权威 space_id 注入。
	// 设计 / 失败模式见 modules/bot_api/space_inject.go 顶部注释。
	payload := messageReq.Payload
	if messageReq.ChannelType == common.ChannelTypePerson.Uint8() {
		payload = rb.enrichBotPayloadWithSpaceID(robotID, payload)
	}

	// YUJ-202 / Mininglamp-OSS#94 / #142 — mention pass-through
	// chokepoint. Same contract as the user and bot API ingresses:
	// post-#142 the helper no longer infers `mention.ais=1` from
	// legacy `mention.all=1` (legacy `@所有人` MUST NOT trigger bots);
	// it now forwards `mention.all`, `mention.humans`, `mention.ais`,
	// and `mention.uids` untouched. The call site is preserved so any
	// future chokepoint normalization lands in one place across the
	// three ingresses. ⚠️ F2 (PR#70 Jerry-Xin correctness-critical
	// review): MUST stay OUTSIDE the `ChannelTypePerson` conditional
	// above so group / community-topic mention payloads always reach
	// the chokepoint. Helper is idempotent and safe on nil —
	// see pkg/mentionrewrite.
	payload = mentionrewrite.RewriteMention(payload)

	// Mininglamp-OSS/octo-server#144 + PR#145 review follow-up:
	// second-pass mention chokepoint (sister call to the user and bot
	// ingresses). When mention.ais=1 in a GROUP channel, expand
	// mention.uids to include every bot member of the channel so
	// legacy adapter bots (#137) on the WuKongIM websocket recognise
	// the `@所有 AI` broadcast. PR #138 only rewrites the
	// /v1/bot/events queue path; this helper covers the websocket
	// dispatch path.
	//
	// ⚠️ PR#145 review (Jerry-Xin / lml2468 / yujiawei 2026-05-23):
	// the expansion MUST run on a clone of `payload`, not on `payload`
	// itself. ExpandAisToBotUIDs mutates the inner `mention` sub-map
	// in place, and the in-memory `payload` is shared with the
	// persisted message_extra row + the reminder writer at
	// modules/message/api_reminders.go (which iterates `mention.uids`
	// to emit one ReminderTypeMentionMe row per UID) — mutating it
	// here would create one human-visible `[有人@我]` reminder per
	// server-expanded bot member. The clone is used ONLY for the wire
	// bytes; `payload` retains the original caller-supplied
	// `mention.uids`. See pkg/mentionrewrite/clone.go for the clone
	// contract.
	wirePayload := mentionrewrite.CloneForExpansion(payload)
	wirePayload = mentionrewrite.ExpandAisToBotUIDs(wirePayload, messageReq.ChannelType, messageReq.ChannelID, rb.fetchBotMemberUIDs)

	result, err := rb.ctx.SendMessageWithResult(&config.MsgSendReq{
		StreamNo:    messageReq.StreamNo,
		ChannelID:   messageReq.ChannelID,
		ChannelType: messageReq.ChannelType,
		FromUID:     robotID,
		Payload:     []byte(util.ToJson(wirePayload)),
	})
	if err != nil {
		rb.Error("发送robot消息失败！", zap.Error(err))
		c.ResponseError(errors.New("发送消息失败！"))
		return
	}
	c.Response(result)
}

func (rb *Robot) supportContentType(contentType common.ContentType) bool {
	switch contentType {
	case common.Text, common.Image, common.GIF, common.Voice,
		common.Video, common.Location, common.Card, common.File,
		common.RichText, common.VectorSticker, common.EmojiSticker:
		return true
	}
	return false
}

func (rb *Robot) payloadIsVail(payloadResult maputil.Data) bool {
	contentType := common.ContentType(payloadResult.Int("type"))
	switch contentType {
	case common.Text:
		return payloadResult.Get("content") != nil
	case common.Image, common.GIF, common.VectorSticker, common.EmojiSticker:
		return payloadResult.Get("url") != nil
	case common.Voice:
		return payloadResult.Get("url") != nil
	case common.Video:
		return payloadResult.Get("url") != nil
	case common.Location:
		return payloadResult.Get("latitude") != nil && payloadResult.Get("longitude") != nil
	case common.Card:
		return payloadResult.Get("uid") != nil || payloadResult.Get("name") != nil
	case common.File:
		return payloadResult.Get("url") != nil
	case common.RichText:
		return payloadResult.Get("content") != nil
	}
	return false
}

// 是否允许发送消息到频道
func (rb *Robot) allowSendToChannel(robotID string, channelID string, channelType uint8) bool {
	if channelType == common.ChannelTypePerson.Uint8() {
		// 个人频道允许机器人发送消息
		return true
	}
	if channelType == common.ChannelTypeGroup.Uint8() {
		// 群组频道需要检查机器人是否是群成员
		exist, err := rb.groupService.ExistMember(channelID, robotID)
		if err != nil {
			rb.Error("检查机器人是否是频道成员失败！", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", channelID))
			return false
		}
		return exist
	}
	// 未知频道类型，拒绝发送
	return false
}

func (rb *Robot) answerInlineQuery(c *wkhttp.Context) {
	var result *InlineQueryResult
	if err := c.BindJSON(&result); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if err := result.Check(); err != nil {
		c.ResponseError(err)
		return
	}
	rb.inlineQueryEventResultChanMapLock.Lock()
	resultChan := rb.inlineQueryEventResultChanMap[result.InlineQuerySID]
	rb.inlineQueryEventResultChanMapLock.Unlock()
	if resultChan != nil {
		select {
		case resultChan <- result:
		default:
		}
	}
	c.ResponseOK()
}

func (rb *Robot) inlineQuery(c *wkhttp.Context) {
	var req struct {
		Offset      string `json:"offset"`
		Query       string `json:"query"`
		Username    string `json:"username"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if len(req.Username) == 0 {
		c.ResponseError(errors.New("username不能为空！"))
		return
	}
	robotM, err := rb.db.queryWithUsername(req.Username)
	if err != nil {
		c.ResponseErrorf("查询机器人失败！", err)
		return
	}
	if robotM == nil {
		c.ResponseError(errors.New("机器人不存在！"))
		return
	}
	if strings.TrimSpace(robotM.AppID) == "" {
		rb.Error("机器人没有app_id", zap.String("username", req.Username))
		c.ResponseError(errors.New("机器人没有app_id！"))
		return
	}
	robotID := robotM.RobotID
	sid := util.GenerUUID()
	inlineQuery := &InlineQuery{
		SID:         sid,
		Query:       req.Query,
		FromUID:     c.GetLoginUID(),
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		Offset:      req.Offset,
	}

	rb.addInlineQuery(robotID, inlineQuery)

	resultChan := make(chan *InlineQueryResult)

	rb.inlineQueryEventResultChanMapLock.Lock()
	rb.inlineQueryEventResultChanMap[sid] = resultChan
	rb.inlineQueryEventResultChanMapLock.Unlock()

	select {
	case result := <-resultChan:
		c.JSON(http.StatusOK, result)
	case <-time.After(time.Second * 20):
		c.AbortWithStatus(http.StatusRequestTimeout)
	}

	rb.inlineQueryEventResultChanMapLock.Lock()
	delete(rb.inlineQueryEventResultChanMap, sid)
	rb.inlineQueryEventResultChanMapLock.Unlock()

	rb.removeInlineQuery(robotID, sid)

}

func (rb *Robot) addInlineQuery(robotID string, inlineQuery *InlineQuery) {
	seq, err := rb.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))
	if err != nil {
		rb.Error("GenSeq failed", zap.Error(err))
		return
	}
	rb.inlineQueryEventsMapLock.Lock()
	events := rb.inlineQueryEventsMap[robotID]
	if events == nil {
		events = make([]*robotEvent, 0)
	}
	events = append(events, &robotEvent{
		EventID:     seq,
		InlineQuery: inlineQuery,
		Expire:      time.Now().Add(rb.ctx.GetConfig().Robot.InlineQueryTimeout).Unix(),
	})
	rb.inlineQueryEventsMap[robotID] = events
	rb.inlineQueryEventsMapLock.Unlock()
}

func (rb *Robot) removeInlineQuery(robotID, sid string) {
	rb.inlineQueryEventsMapLock.Lock()
	defer func() {
		rb.inlineQueryEventsMapLock.Unlock()
	}()
	events := rb.inlineQueryEventsMap[robotID]
	if len(events) == 0 {
		return
	}
	removeIdx := -1
	for idx, event := range events {
		if event.InlineQuery.SID == sid {
			removeIdx = idx
			break
		}
	}
	if removeIdx != -1 {
		events = append(events[:removeIdx], events[removeIdx+1:]...)
		rb.inlineQueryEventsMap[robotID] = events
	}
}

type robotEventSortSlice []*robotEvent

func (r robotEventSortSlice) Len() int {
	return len(r)
}

func (r robotEventSortSlice) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

func (r robotEventSortSlice) Less(i, j int) bool {
	return r[i].EventID < r[j].EventID
}

func (rb *Robot) getEventsResult(robotID string, eventID int64, limit int64) ([]*robotEventResp, error) {

	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	robotEventJsons, err := rb.ctx.GetRedisConn().ZRangeByScore(fmt.Sprintf("%s%s", rb.robotEventPrefix, robotID), redis.ZRangeBy{
		Max:   "+inf",
		Min:   fmt.Sprintf("%d", eventID),
		Count: limit,
	})
	if err != nil {
		return nil, err
	}
	rb.inlineQueryEventsMapLock.RLock()
	robotEvents := rb.inlineQueryEventsMap[robotID]
	rb.inlineQueryEventsMapLock.RUnlock()
	newRobotEvents := make([]*robotEvent, 0, len(robotEvents)+int(limit))

	results := make([]*robotEventResp, 0, len(robotEvents)+int(limit))

	if len(robotEvents) > 0 {
		newRobotEvents = append(newRobotEvents, robotEvents...)
	}

	if len(robotEventJsons) > 0 {
		for _, robotEventJson := range robotEventJsons {
			var robotEvent = &robotEvent{}
			err = util.ReadJsonByByte([]byte(robotEventJson), &robotEvent)
			if err != nil {
				rb.Error("机器人消息解码失败！", zap.Error(err))
				continue
			}
			newRobotEvents = append(newRobotEvents, robotEvent)
		}
	}
	if len(newRobotEvents) > 0 {
		robotEventsSlice := robotEventSortSlice(newRobotEvents)
		sort.Sort(robotEventsSlice)
		if int64(len(robotEventsSlice)) > limit {
			robotEventsSlice = robotEventsSlice[0:limit]
		}
		for _, robotEvent := range robotEventsSlice {
			if robotEvent.EventID <= eventID {
				continue
			}
			robotEventResp := &robotEventResp{}
			robotEventResp.from(robotEvent)
			results = append(results, robotEventResp)
		}
	}
	return results, nil

}

// 移除指定事件
func (rb *Robot) removeEvent(robotID string, eventID int64) error {
	err := rb.ctx.GetRedisConn().ZRemRangeByScore(fmt.Sprintf("%s%s", rb.robotEventPrefix, robotID), fmt.Sprintf("%d", eventID), fmt.Sprintf("%d", eventID))
	return err
}

func (rb *Robot) getEventsForPost(c *wkhttp.Context) {
	robotID := c.Param("robot_id")
	var req struct {
		Limit   int64 `json:"limit"`
		EventID int64 `json:"event_id"`
	}
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	results, err := rb.getEventsResult(robotID, req.EventID, req.Limit)
	if err != nil {
		c.Response(gin.H{
			"status": 0,
			"msg":    err.Error(),
		})
		return
	}
	c.Response(gin.H{
		"status":  1,
		"results": results,
	})
}

func (rb *Robot) getEventsForGet(c *wkhttp.Context) {
	robotID := c.Param("robot_id")
	eventID := c.Query("event_id")
	limit, err := strconv.ParseInt(c.Query("limit"), 10, 64)
	if err != nil {
		limit = 0
		rb.Warn("解析limit参数失败", zap.Error(err), zap.String("value", c.Query("limit")))
	}
	eventIDI64, err := strconv.ParseInt(eventID, 10, 64)
	if err != nil {
		eventIDI64 = 0
		rb.Warn("解析event_id参数失败", zap.Error(err), zap.String("value", eventID))
	}

	results, err := rb.getEventsResult(robotID, eventIDI64, limit)
	if err != nil {
		c.Response(gin.H{
			"status": 0,
			"msg":    err.Error(),
		})
		return
	}

	c.Response(gin.H{
		"status":  1,
		"results": results,
	})

}

func (rb *Robot) eventAck(c *wkhttp.Context) {
	robotID := c.Param("robot_id")
	eventID, err := strconv.ParseInt(c.Param("event_id"), 10, 64)
	if err != nil {
		rb.Error("解析event_id参数失败", zap.Error(err), zap.String("value", c.Param("event_id")))
		c.ResponseError(errors.New("event_id格式错误"))
		return
	}

	err = rb.removeEvent(robotID, eventID)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.ResponseOK()

}

func (rb *Robot) insertSystemRobot() error {
	robotID := rb.ctx.GetConfig().Account.SystemUID
	m, err := rb.db.queryRobotWithRobtID(robotID)
	if err != nil {
		rb.Error("查询系统机器人错误", zap.Error(err))
		return err
	}
	if m == nil {
		tx, err := rb.db.session.Begin()
		if err != nil {
			rb.Error("开启事物错误", zap.Error(err))
			return err
		}
		defer func() {
			if err := recover(); err != nil {
				tx.Rollback()
				fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
			}
		}()
		robotVersion, err := rb.ctx.GenSeq(common.RobotSeqKey)
		if err != nil {
			tx.Rollback()
			rb.Error("GenSeq failed", zap.Error(err))
			return err
		}
		err = rb.db.insertTx(&robot{
			RobotID: robotID,
			Status:  int(Enable),
			Token:   util.GenerUUID(),
			Version: robotVersion,
		}, tx)
		if err != nil {
			tx.Rollback()
			rb.Error("添加系统机器人错误", zap.Error(err))
			return err
		}
		list := make([]*menu, 0)
		for _, m := range systemRobotMap {
			list = append(list, &menu{
				RobotID: robotID,
				CMD:     m.CMD,
				Remark:  m.Remark,
				Type:    m.Type,
			})
		}
		for _, menu := range list {
			err = rb.db.insertMenuTx(menu, tx)
			if err != nil {
				tx.Rollback()
				rb.Error("添加系统机器人菜单错误", zap.Error(err))
				return err
			}
		}
		err = tx.Commit()
		if err != nil {
			tx.RollbackUnlessCommitted()
			rb.Error("添加系统机器人事物提交失败", zap.Error(err))
			return err
		}
	}
	return nil
}

// 查询机器人命令列表
func (rb *Robot) getCommands(c *wkhttp.Context) {
	robotID := c.Query("robot_id")
	if strings.TrimSpace(robotID) == "" {
		c.ResponseError(errors.New("robot_id不能为空"))
		return
	}

	botCommands, err := rb.db.queryBotCommandsByRobotID(robotID)
	if err != nil {
		rb.Error("查询机器人命令失败", zap.Error(err))
		c.ResponseError(errors.New("查询机器人命令失败"))
		return
	}

	if strings.TrimSpace(botCommands) == "" {
		c.Response([]interface{}{})
		return
	}

	var commands []interface{}
	if err := json.Unmarshal([]byte(botCommands), &commands); err != nil {
		rb.Error("解析机器人命令失败", zap.Error(err), zap.String("botCommands", botCommands))
		c.ResponseError(errors.New("机器人命令数据损坏"))
		return
	}
	c.Response(commands)
}

// 同步机器人菜单
func (rb *Robot) sync(c *wkhttp.Context) {
	type req struct {
		RobotID  string `json:"robot_id"` // TODO: robotID为了兼容老版本，新版用username
		Version  int64  `json:"version"`
		Username string `json:"username"`
	}
	var reqs []*req
	if err := c.BindJSON(&reqs); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}

	robotIDs := make([]string, 0)
	usernames := make([]string, 0)
	for _, reqModel := range reqs {
		if strings.TrimSpace(reqModel.RobotID) != "" {
			robotIDs = append(robotIDs, reqModel.RobotID)
		}
		if strings.TrimSpace(reqModel.Username) != "" {
			usernames = append(usernames, reqModel.Username)
		}
	}

	result := make([]*syncResp, 0)
	var robotList []*robot
	var err error
	if len(robotIDs) > 0 {
		robotList, err = rb.db.queryWithIDs(robotIDs)
		if err != nil {
			c.ResponseError(errors.New("批量查询机器人数据错误"))
			rb.Error("批量查询机器人数据错误", zap.Error(err))
			return
		}
	} else if len(usernames) > 0 {
		robotList, err = rb.db.queryWithUsernames(usernames)
		if err != nil {
			c.ResponseError(errors.New("批量通过username查询机器人数据错误"))
			rb.Error("批量通过username查询机器人数据错误", zap.Error(err))
			return
		}
	}

	respRobotIDs := make([]string, 0)
	for _, reqModel := range reqs {
		for _, robot := range robotList {
			if ((len(robotIDs) > 0 && reqModel.RobotID == robot.RobotID) || (len(usernames) > 0 && reqModel.Username == robot.Username)) && reqModel.Version < robot.Version {
				respRobotIDs = append(respRobotIDs, robot.RobotID)
				break
			}
		}
	}
	if len(respRobotIDs) == 0 {
		c.Response(result)
		return
	}
	menus, err := rb.db.queryMenusWithRobotIDs(respRobotIDs)
	if err != nil {
		c.ResponseError(errors.New("批量查询机器人菜单数据错误"))
		rb.Error("批量查询机器人菜单数据错误", zap.Error(err))
		return
	}
	for _, robotID := range respRobotIDs {
		var version int64
		var status int
		var created_at string
		var updated_at string
		var username string
		var placeholder string
		var inlineOn int
		for _, robot := range robotList {
			if robotID == robot.RobotID {
				version = robot.Version
				status = robot.Status
				created_at = robot.CreatedAt.String()
				updated_at = robot.UpdatedAt.String()
				username = robot.Username
				placeholder = robot.Placeholder
				inlineOn = robot.InlineOn
				break
			}
		}
		robotMenus := make([]*menuResp, 0)
		for _, menu := range menus {
			if menu.RobotID == robotID {
				robotMenus = append(robotMenus, &menuResp{
					RobotID:   robotID,
					CMD:       menu.CMD,
					Remark:    menu.Remark,
					Type:      menu.Type,
					CreatedAt: menu.CreatedAt.String(),
					UpdatedAt: menu.UpdatedAt.String(),
				})
			}
		}
		result = append(result, &syncResp{
			RobotID:     robotID,
			Username:    username,
			Placeholder: placeholder,
			InlineOn:    inlineOn,
			Status:      status,
			Version:     version,
			CreatedAt:   created_at,
			UpdatedAt:   updated_at,
			Menus:       robotMenus,
		})
	}
	c.Response(result)
}

type syncResp struct {
	RobotID     string      `json:"robot_id"`
	Username    string      `json:"username"`
	InlineOn    int         `json:"inline_on"`
	Placeholder string      `json:"placeholder"`
	Status      int         `json:"status"`
	Version     int64       `json:"version"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
	Menus       []*menuResp `json:"menus"`
}
type menuResp struct {
	CMD       string `json:"cmd"`
	Remark    string `json:"remark"`
	Type      string `json:"type"`
	RobotID   string `json:"robot_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type robotEventResp struct {
	EventID     int64                   `json:"event_id,omitempty"`   // 更新ID
	Message     *simpleRobotMessageResp `json:"message,omitempty"`    // 消息对象
	InlineQuery *InlineQuery            `json:"inline_query"`         // 查询
	EventType   string                  `json:"event_type,omitempty"` // 自定义事件类型
	EventData   map[string]interface{}  `json:"event_data,omitempty"` // 自定义事件数据
}

func (s *robotEventResp) from(resp *robotEvent) {
	s.EventID = resp.EventID
	if resp.Message != nil {
		simpleRobotMessageResp := &simpleRobotMessageResp{}
		simpleRobotMessageResp.from(resp.Message)
		s.Message = simpleRobotMessageResp
	}
	if resp.InlineQuery != nil {
		s.InlineQuery = resp.InlineQuery
	}
	if resp.EventType != "" {
		s.EventType = resp.EventType
		s.EventData = resp.EventData
	}
}

type simpleRobotMessageResp struct {
	MessageID   int64       `json:"message_id"`             // 服务端的消息ID(全局唯一)
	MessageSeq  uint32      `json:"message_seq"`            // 消息序列号 （用户唯一，有序递增）
	FromUID     string      `json:"from_uid"`               // 发送者UID
	ChannelID   string      `json:"channel_id,omitempty"`   // 频道ID
	ChannelType uint8       `json:"channel_type,omitempty"` // 频道类型
	Timestamp   int32       `json:"timestamp"`              // 服务器消息时间戳(10位，到秒)
	Payload     interface{} `json:"payload"`                // 消息正文
}

func (s *simpleRobotMessageResp) from(messageResp *config.MessageResp) {
	s.MessageID = messageResp.MessageID
	s.MessageSeq = messageResp.MessageSeq
	s.FromUID = messageResp.FromUID
	if messageResp.ChannelType != common.ChannelTypePerson.Uint8() {
		s.ChannelID = messageResp.ChannelID
		s.ChannelType = messageResp.ChannelType
	}
	s.Timestamp = messageResp.Timestamp
	var payloadMap map[string]interface{}
	if err := util.ReadJsonByByte(messageResp.Payload, &payloadMap); err != nil {
		log.Warn("解码消息正文失败", zap.Error(err))
	}
	s.Payload = payloadMap
}

// setDescription 设置 Bot 简介
func (rb *Robot) setDescription(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")

	var req struct {
		Description string `json:"description"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("参数错误"))
		return
	}

	// 验证操作者是 Bot 创建者
	var creatorUID string
	err := rb.ctx.DB().Select("IFNULL(creator_uid,'')").From("robot").Where("robot_id=? AND status=1", robotID).LoadOne(&creatorUID)
	if err != nil || creatorUID == "" {
		c.ResponseError(errors.New("机器人不存在"))
		return
	}
	if creatorUID != loginUID {
		c.ResponseError(errors.New("只有创建者可以修改"))
		return
	}

	_, err = rb.ctx.DB().Update("robot").Set("description", req.Description).Where("robot_id=?", robotID).Exec()
	if err != nil {
		c.ResponseError(errors.New("更新失败"))
		return
	}
	c.ResponseOK()
}

// setAutoApprove 设置是否自动通过好友申请
func (rb *Robot) setAutoApprove(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")

	var req struct {
		AutoApprove int `json:"auto_approve"` // 0:需审批 1:自动通过
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("参数错误"))
		return
	}

	// 验证操作者是 Bot 创建者
	var creatorUID string
	err := rb.ctx.DB().Select("IFNULL(creator_uid,'')").From("robot").Where("robot_id=? AND status=1", robotID).LoadOne(&creatorUID)
	if err != nil || creatorUID == "" {
		c.ResponseError(errors.New("机器人不存在"))
		return
	}
	if creatorUID != loginUID {
		c.ResponseError(errors.New("只有创建者可以修改"))
		return
	}

	_, err = rb.ctx.DB().Update("robot").Set("auto_approve", req.AutoApprove).Where("robot_id=?", robotID).Exec()
	if err != nil {
		c.ResponseError(errors.New("更新失败"))
		return
	}
	c.ResponseOK()
}

// spaceBots Bot 广场 — 获取 Space 内所有 Bot
func (rb *Robot) spaceBots(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Query("space_id")
	if spaceID == "" {
		c.ResponseError(errors.New("space_id 不能为空"))
		return
	}

	// 查询 Space 内所有 Bot（space_member + user + robot）
	type spaceBotRow struct {
		UID         string `db:"uid"`
		Name        string `db:"name"`
		Description string `db:"description"`
		CreatorUID  string `db:"creator_uid"`
		BotCommands string `db:"bot_commands"`
		AutoApprove int    `db:"auto_approve"`
	}
	var bots []spaceBotRow
	_, err := rb.ctx.DB().SelectBySql(`
		SELECT sm.uid, IFNULL(u.name,'') as name, 
			IFNULL(r.description,'') as description, 
			IFNULL(r.creator_uid,'') as creator_uid,
			IFNULL(r.bot_commands,'') as bot_commands,
			IFNULL(r.auto_approve,0) as auto_approve
		FROM space_member sm
		INNER JOIN user u ON sm.uid = u.uid AND u.robot = 1
		INNER JOIN robot r ON r.robot_id = sm.uid AND r.status = 1
		WHERE sm.space_id = ? AND sm.status = 1 AND sm.uid != 'botfather'
		ORDER BY u.created_at DESC
	`, spaceID).Load(&bots)
	if err != nil {
		rb.Error("查询 Space Bot 列表失败", zap.Error(err))
		c.ResponseError(errors.New("查询失败"))
		return
	}

	// 批量查好友关系
	botUIDs := make([]string, 0, len(bots))
	for _, b := range bots {
		botUIDs = append(botUIDs, b.UID)
	}
	friendMap := make(map[string]bool)
	applyMap := make(map[string]int) // 0=待审批
	if len(botUIDs) > 0 {
		// 好友关系
		type friendRow struct {
			ToUID string `db:"to_uid"`
		}
		var friends []friendRow
		_, _ = rb.ctx.DB().SelectBySql(
			"SELECT to_uid FROM friend WHERE uid = ? AND to_uid IN ? AND is_deleted = 0",
			loginUID, botUIDs,
		).Load(&friends)
		for _, f := range friends {
			friendMap[f.ToUID] = true
		}
		// 好友申请状态
		type applyRow struct {
			ToUID  string `db:"to_uid"`
			Status int    `db:"status"`
		}
		var applies []applyRow
		_, _ = rb.ctx.DB().SelectBySql(
			"SELECT to_uid, status FROM friend_apply WHERE uid = ? AND to_uid IN ?",
			loginUID, botUIDs,
		).Load(&applies)
		for _, a := range applies {
			applyMap[a.ToUID] = a.Status
		}
	}

	// 批量查创建者名称
	creatorUIDs := make([]string, 0)
	creatorUIDSet := make(map[string]bool)
	for _, b := range bots {
		if b.CreatorUID != "" && !creatorUIDSet[b.CreatorUID] {
			creatorUIDs = append(creatorUIDs, b.CreatorUID)
			creatorUIDSet[b.CreatorUID] = true
		}
	}
	creatorNameMap := make(map[string]string)
	if len(creatorUIDs) > 0 {
		type nameRow struct {
			UID  string `db:"uid"`
			Name string `db:"name"`
		}
		var names []nameRow
		_, _ = rb.ctx.DB().SelectBySql(
			"SELECT uid, name FROM user WHERE uid IN ?", creatorUIDs,
		).Load(&names)
		for _, n := range names {
			creatorNameMap[n.UID] = n.Name
		}
	}

	results := make([]map[string]interface{}, 0, len(bots))
	for _, b := range bots {
		status := "not_added" // 未添加
		if friendMap[b.UID] {
			status = "added" // 已添加
		} else if _, ok := applyMap[b.UID]; ok {
			status = "pending" // 审批中
		}
		results = append(results, map[string]interface{}{
			"uid":          b.UID,
			"name":         b.Name,
			"description":  b.Description,
			"creator_uid":  b.CreatorUID,
			"creator_name": creatorNameMap[b.CreatorUID],
			"bot_commands": b.BotCommands,
			"auto_approve": b.AutoApprove,
			"status":       status,
		})
	}
	c.Response(results)
}

// myBots 我的 Bot — 已添加好友的 Bot
func (rb *Robot) myBots(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Query("space_id")

	type myBotRow struct {
		UID         string `db:"uid"`
		Name        string `db:"name"`
		Description string `db:"description"`
		CreatorUID  string `db:"creator_uid"`
		BotCommands string `db:"bot_commands"`
	}
	var bots []myBotRow

	query := `
		SELECT f.to_uid as uid, IFNULL(u.name,'') as name,
			IFNULL(r.description,'') as description,
			IFNULL(r.creator_uid,'') as creator_uid,
			IFNULL(r.bot_commands,'') as bot_commands
		FROM friend f
		INNER JOIN user u ON f.to_uid = u.uid AND u.robot = 1
		INNER JOIN robot r ON r.robot_id = f.to_uid AND r.status = 1
		WHERE f.uid = ? AND f.is_deleted = 0 AND f.to_uid != 'botfather'`
	args := []interface{}{loginUID}

	if spaceID != "" {
		query += ` AND f.to_uid IN (SELECT uid FROM space_member WHERE space_id = ? AND status = 1)`
		args = append(args, spaceID)
	}

	query += ` ORDER BY f.created_at DESC`

	_, err := rb.ctx.DB().SelectBySql(query, args...).Load(&bots)
	if err != nil {
		rb.Error("查询我的 Bot 列表失败", zap.Error(err))
		c.ResponseError(errors.New("查询失败"))
		return
	}

	// 批量查创建者名称
	creatorUIDs := make([]string, 0)
	creatorUIDSet := make(map[string]bool)
	for _, b := range bots {
		if b.CreatorUID != "" && !creatorUIDSet[b.CreatorUID] {
			creatorUIDs = append(creatorUIDs, b.CreatorUID)
			creatorUIDSet[b.CreatorUID] = true
		}
	}
	creatorNameMap := make(map[string]string)
	if len(creatorUIDs) > 0 {
		type nameRow struct {
			UID  string `db:"uid"`
			Name string `db:"name"`
		}
		var names []nameRow
		_, _ = rb.ctx.DB().SelectBySql(
			"SELECT uid, name FROM user WHERE uid IN ?", creatorUIDs,
		).Load(&names)
		for _, n := range names {
			creatorNameMap[n.UID] = n.Name
		}
	}

	results := make([]map[string]interface{}, 0, len(bots))
	for _, b := range bots {
		results = append(results, map[string]interface{}{
			"uid":          b.UID,
			"name":         b.Name,
			"description":  b.Description,
			"creator_uid":  b.CreatorUID,
			"creator_name": creatorNameMap[b.CreatorUID],
			"bot_commands": b.BotCommands,
		})
	}
	c.Response(results)
}

// proxyFile 文件下载代理 — 302 重定向到 presigned URL
func (rb *Robot) proxyFile(c *wkhttp.Context) {
	ph := c.Param("path")
	if ph == "" {
		c.ResponseError(errors.New("文件路径不能为空"))
		return
	}
	// 去掉前导 /
	ph = strings.TrimPrefix(ph, "/")

	// Sanitize path to prevent directory traversal
	cleaned := filepath.Clean(ph)
	if strings.Contains(cleaned, "..") || strings.ContainsAny(cleaned, "\x00") {
		c.ResponseErrorWithStatus(errors.New("文件路径无效"), http.StatusBadRequest)
		return
	}
	ph = cleaned

	filename := c.Query("filename")
	if filename == "" {
		filename = pkgutil.ExtractFilenameFromPath(ph)
	}

	downloadURL, err := rb.fileService.DownloadURL(ph, filename)
	if err != nil {
		rb.Error("获取文件下载URL失败", zap.Error(err), zap.String("path", ph))
		c.ResponseError(errors.New("获取文件失败"))
		return
	}
	c.Redirect(http.StatusFound, downloadURL)
}

// botUploadFile Bot 文件上传
func (rb *Robot) botUploadFile(c *wkhttp.Context) {
	fileType := c.DefaultQuery("type", "chat")
	uploadPath := c.Query("path")

	multipartFile, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		rb.Error("读取上传文件失败", zap.Error(err))
		c.ResponseError(errors.New("读取文件失败"))
		return
	}
	defer multipartFile.Close()

	// 文件大小限制 100MB
	const maxSize int64 = 100 * 1024 * 1024
	if fileHeader.Size > maxSize {
		c.ResponseError(fmt.Errorf("文件大小不能超过%dMB", maxSize/1024/1024))
		return
	}

	fileName := fileHeader.Filename
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		c.ResponseError(errors.New("文件必须包含扩展名"))
		return
	}

	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	path := uploadPath
	if path == "" {
		path = fmt.Sprintf("/%d/%s%s", time.Now().Unix(), util.GenerUUID(), filepath.Ext(fileName))
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	storagePath := fmt.Sprintf("%s%s", fileType, path)
	contentDisposition := file.BuildContentDisposition(fileName)
	_, err = rb.fileService.UploadFile(storagePath, contentType, contentDisposition, func(w io.Writer) error {
		_, err := io.Copy(w, multipartFile)
		return err
	})
	if err != nil {
		rb.Error("上传文件失败", zap.Error(err))
		c.ResponseError(errors.New("上传文件失败"))
		return
	}

	fullURL, err := rb.fileService.DownloadURL(storagePath, "")
	if err != nil {
		rb.Warn("生成下载URL失败，回退到相对路径", zap.Error(err))
		fullURL = fmt.Sprintf("file/preview/%s%s", fileType, path)
	}
	c.Response(gin.H{
		"url":  fullURL,
		"name": fileName,
		"size": fileHeader.Size,
	})
}

// botUploadCredentials 签发 STS 临时密钥，供客户端直传 COS
func (rb *Robot) botUploadCredentials(c *wkhttp.Context) {
	filename := c.Query("filename")
	if strings.TrimSpace(filename) == "" {
		c.ResponseError(errors.New("filename 不能为空"))
		return
	}
	filename = filepath.Base(filename)

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || file.IsBlockedExtension(ext) || !file.IsAllowedExtension(ext) {
		c.ResponseError(errors.New("不支持的文件类型"))
		return
	}

	cosConfig := rb.ctx.GetConfig().COS
	if cosConfig.SecretID == "" || cosConfig.SecretKey == "" || cosConfig.Bucket == "" {
		rb.Error("COS 配置不完整")
		c.ResponseError(errors.New("COS 未配置"))
		return
	}

	prefix := strings.TrimSpace(cosConfig.Prefix)
	// Use UUID-based key (pure ASCII) to avoid double-encoding by HTTP clients.
	fnExt := strings.ToLower(filepath.Ext(filename))
	objectPath := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), fnExt)
	var key string
	if prefix != "" {
		key = path.Join(prefix, objectPath)
	} else {
		key = objectPath
	}

	bucket := cosConfig.Bucket
	region := cosConfig.Region

	appId := ""
	if idx := strings.LastIndex(bucket, "-"); idx > 0 {
		appId = bucket[idx+1:]
	}
	if appId == "" {
		rb.Error("无法从 bucket 名称中提取 appId", zap.String("bucket", bucket))
		c.ResponseError(errors.New("COS 配置错误：bucket 格式不正确"))
		return
	}

	client := sts.NewClient(cosConfig.SecretID, cosConfig.SecretKey, nil)
	opt := &sts.CredentialOptions{
		DurationSeconds: 1800,
		Region:          region,
		Policy: &sts.CredentialPolicy{
			Statement: []sts.CredentialPolicyStatement{
				{
					Action:   []string{"cos:PutObject"},
					Effect:   "allow",
					Resource: []string{fmt.Sprintf("qcs::cos:%s:uid/%s:%s/%s", region, appId, bucket, key)},
				},
			},
		},
	}

	res, err := client.GetCredential(opt)
	if err != nil {
		rb.Error("获取 STS 临时密钥失败", zap.Error(err))
		c.ResponseError(errors.New("获取临时密钥失败"))
		return
	}

	c.Response(gin.H{
		"bucket": bucket,
		"region": region,
		"key":    key,
		"credentials": gin.H{
			"tmpSecretId":  res.Credentials.TmpSecretID,
			"tmpSecretKey": res.Credentials.TmpSecretKey,
			"sessionToken": res.Credentials.SessionToken,
		},
		"startTime":   res.StartTime,
		"expiredTime": res.ExpiredTime,
		"cdnBaseUrl":  cosConfig.BucketURL,
	})
}

// botUploadPresigned 签发预签名 PUT URL，供客户端直传文件
func (rb *Robot) botUploadPresigned(c *wkhttp.Context) {
	filename := c.Query("filename")
	if strings.TrimSpace(filename) == "" {
		c.ResponseError(errors.New("filename 不能为空"))
		return
	}
	filename = filepath.Base(filename)

	// fileSize is REQUIRED so the storage layer can sign Content-Length and
	// reject any PUT that exceeds the byte budget — same P0 size-bypass
	// guard the public file API enforces (see modules/file/api.go).
	fileSizeRaw := strings.TrimSpace(c.Query("fileSize"))
	if fileSizeRaw == "" {
		c.ResponseError(errors.New("fileSize 参数必填，且不能超过最大限制"))
		return
	}
	fileSize, parseErr := strconv.ParseInt(fileSizeRaw, 10, 64)
	if parseErr != nil || fileSize <= 0 {
		c.ResponseError(errors.New("fileSize 参数必须为正整数（字节）"))
		return
	}
	if fileSize > file.MaxFileSize {
		rb.Warn("预签名上传 fileSize 超出限制",
			zap.Int64("size", fileSize), zap.Int64("max", file.MaxFileSize))
		c.ResponseError(fmt.Errorf("文件大小不能超过%dMB", file.MaxFileSize/1024/1024))
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || file.IsBlockedExtension(ext) || !file.IsAllowedExtension(ext) {
		c.ResponseError(errors.New("不支持的文件类型"))
		return
	}

	// Use UUID-based key (pure ASCII) to avoid double-encoding by HTTP clients.
	objectPath := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), ext)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	contentDisposition := file.BuildContentDisposition(filename)
	expiry := 30 * time.Minute
	uploadURL, downloadURL, err := rb.fileService.PresignedPutURL(objectPath, contentType, contentDisposition, fileSize, expiry)
	if err != nil {
		rb.Error("生成预签名上传URL失败", zap.Error(err))
		c.ResponseError(errors.New("生成上传URL失败"))
		return
	}

	resp := gin.H{
		"method":      "PUT",
		"uploadUrl":   uploadURL,
		"downloadUrl": downloadURL,
		"contentType": contentType,
		"key":         objectPath,
		"expiresIn":   int(expiry.Seconds()),
		"expiredTime": time.Now().Add(expiry).Unix(),
		"maxFileSize": fileSize,
	}
	// Content-Disposition is signed into the canonical headers on
	// SigV4 backends (MinIO/COS), so the browser MUST echo this exact
	// value at PUT time or the gateway returns 403 SignatureDoesNotMatch.
	// Mirror the main file endpoint at modules/file/api.go.
	if contentDisposition != "" {
		resp["contentDisposition"] = contentDisposition
	}
	c.Response(resp)
}

// botMessageEdit Bot 编辑自己发送的消息
func (rb *Robot) botMessageEdit(c *wkhttp.Context) {
	var req struct {
		MessageID   string `json:"message_id"`
		MessageSeq  uint32 `json:"message_seq"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		ContentEdit string `json:"content_edit"`
	}
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if req.MessageID == "" {
		c.ResponseError(errors.New("message_id 不能为空"))
		return
	}
	if req.MessageSeq == 0 {
		c.ResponseError(errors.New("message_seq 不能为空"))
		return
	}
	if req.ChannelID == "" {
		c.ResponseError(errors.New("channel_id 不能为空"))
		return
	}
	if strings.TrimSpace(req.ContentEdit) == "" {
		c.ResponseError(errors.New("content_edit 不能为空"))
		return
	}

	robotID := c.Param("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("robot_id 不能为空"))
		return
	}

	// 权限检查：只允许 Bot 编辑自己发送的消息
	messageSeqs := []uint32{req.MessageSeq}
	resp, err := rb.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, robotID, messageSeqs)
	if err != nil {
		rb.Error("查询消息错误", zap.Error(err))
		c.ResponseError(errors.New("查询消息错误"))
		return
	}
	if resp == nil || len(resp.Messages) == 0 {
		c.ResponseError(errors.New("消息不存在"))
		return
	}
	if resp.Messages[0].FromUID != robotID {
		c.ResponseError(errors.New("只能编辑自己发送的消息"))
		return
	}

	// 检查是否存在相同编辑内容
	contentEdit := dbr.NewNullString(req.ContentEdit).String
	contentMD5 := util.MD5(contentEdit)

	var existCount int
	err = rb.ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit_hash=?", req.MessageID, contentMD5).LoadOne(&existCount)
	if err != nil {
		rb.Error("查询是否存在相同正文失败！", zap.Error(err))
		c.ResponseError(errors.New("查询是否存在相同正文失败！"))
		return
	}
	if existCount > 0 {
		rb.Warn("存在相同编辑正文，不再处理！")
		c.ResponseOK()
		return
	}

	// 计算 fakeChannelID
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(robotID, req.ChannelID)
	}

	// 生成 message_extra 版本号
	version, err := rb.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, fakeChannelID))
	if err != nil {
		rb.Error("生成消息扩展序列号失败！", zap.Error(err))
		c.ResponseError(errors.New("生成消息扩展序列号失败！"))
		return
	}

	// 写入 message_extra
	_, err = rb.ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version)",
		req.MessageID, req.MessageSeq, fakeChannelID, req.ChannelType, contentEdit, contentMD5, int(time.Now().Unix()), version,
	).Exec()
	if err != nil {
		rb.Error("添加或修改编辑内容失败！", zap.Error(err))
		c.ResponseError(errors.New("添加或修改编辑内容失败！"))
		return
	}

	// 发送 CMD 同步消息扩展到客户端
	err = rb.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     robotID,
		CMD:         common.CMDSyncMessageExtra,
	})
	if err != nil {
		rb.Error("发送 CMD 同步失败！", zap.Error(err))
		c.ResponseError(errors.New("发送同步命令失败"))
		return
	}

	c.ResponseOK()
}
