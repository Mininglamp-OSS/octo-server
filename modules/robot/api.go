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
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather/cmdmenu"
	commonmodule "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/mentionrewrite"
	"github.com/Mininglamp-OSS/octo-server/pkg/richtext"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/gookit/goutil/maputil"
	sts "github.com/tencentyun/qcloud-cos-sts-sdk/go"
	"go.uber.org/zap"
)

// PreparedBotTypedEvent is a typed bot event with its sequence and wire payload
// allocated, but not yet visible in the Redis queue. Callers that need to
// combine the queue write with another Redis state transition can commit these
// fields atomically; ordinary callers should keep using EnqueueBotTypedEvent.
type PreparedBotTypedEvent struct {
	EventID  int64
	QueueKey string
	Member   string
}

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
	// EnqueueBotTypedEvent appends a typed (event_type/event_data) event for
	// `robotID` onto the same bot event queue as EnqueueBotEvent, and returns
	// the assigned event_id. It is the typed-event sibling of EnqueueBotEvent
	// (card-message-interaction P2 D5, e.g. event_type="card_action") — it
	// rides the identical GenSeq / ZAdd / Expire chokepoint rather than
	// overloading the message-shaped path, so /v1/bot/events serves organic,
	// synthetic-message, and typed events uniformly. The returned event_id is
	// the queue cursor position (D4 uses it as the idempotency-confirm value;
	// bots key at-least-once idempotency on it per D8). Error only on
	// GenSeq / ZADD failure.
	EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error)
	// PrepareBotTypedEvent allocates and serializes the same queue record as
	// EnqueueBotTypedEvent without making it visible. The returned event is
	// intended for an atomic fenced commit by another module.
	PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (PreparedBotTypedEvent, error)
	// BotCardConfig resolves the bot's effective card capability switches
	// (task bot-setting-store): the bot_setting override, then the
	// system_setting deployment default, then the code default — already
	// AND-ed with the deployment master switch cardmsg.BotEnabled().
	//
	// Both the /v1/bot/card/profile manifest and the sendMessage gate read
	// it, so the advertised capability and what the send path accepts cannot
	// drift. An error means the config could not be read; callers must fail
	// closed rather than substituting defaults — a DB blip must not silently
	// re-open a capability the owner turned off.
	BotCardConfig(robotID string) (BotCardConfig, error)
}

// Service robot 模块对外暴露的只读服务实现，供其它模块注入使用。
// 与 *Robot 共享底层表结构，但不承担消息/事件监听等副作用，
// 因此可以被重复 New 出来而不会导致重复注册 listener。
type Service struct {
	ctx *config.Context
	log.Log
	db           *robotDB
	creatorCache sync.Map // robotID -> creatorUID
	// systemSettings mirrors the Robot field: resolved once at construction so
	// the per-request path never takes common's process-wide singleton mutex.
	systemSettings *commonmodule.SystemSettings
}

// NewService 构造一个只读 robot 服务，满足 IService 接口。
func NewService(ctx *config.Context) IService {
	return &Service{
		ctx:            ctx,
		Log:            log.NewTLog("RobotService"),
		db:             newBotDB(ctx),
		systemSettings: commonmodule.EnsureSystemSettings(ctx),
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

// EnqueueBotTypedEvent — IService — Service variant（card_action 等类型化事件）。
func (s *Service) EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error) {
	return enqueueBotTypedEventGeneric(s.ctx, robotID, eventType, eventData)
}

// EnqueueBotTypedEvent — IService — *Robot variant.
func (rb *Robot) EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error) {
	return enqueueBotTypedEventGeneric(rb.ctx, robotID, eventType, eventData)
}

// PrepareBotTypedEvent — IService — Service variant.
func (s *Service) PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (PreparedBotTypedEvent, error) {
	return prepareBotTypedEvent(s.ctx, robotID, eventType, eventData)
}

// PrepareBotTypedEvent — IService — *Robot variant.
func (rb *Robot) PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (PreparedBotTypedEvent, error) {
	return prepareBotTypedEvent(rb.ctx, robotID, eventType, eventData)
}

// enqueueBotEventGeneric is the write-to-bot-event-queue helper shared by
// EnqueueBotEvent (cross-module synthetic path) and, in typed form, by
// enqueueBotTypedEventGeneric.
//
// It is NOT the only writer, and this comment used to claim otherwise.
// `saveRobotMessage` (modules/robot/event.go) carries its own inline
// GenSeq / ZAdd / Expire copy for the listener fast-path, and both
// `notifyBotJoinedGroup` variants in modules/group write the queue directly
// as well — five ZADD sites in total. PR#685's review caught the doorbell
// being wired to only two of them because the brief trusted this docstring
// instead of the call graph.
//
// If you add a queue writer, it must also ring the doorbell
// (pkg/botevent.Ring) after a successful ZADD, or /v1/bot/events long-poll
// will not wake promptly for it.
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
	// #697: event ids come from the monotonic per-bot allocator, not GenSeq. A
	// failure here fails the enqueue exactly as a GenSeq error did — there is
	// deliberately no fallback, because two live id sources on one queue is the
	// defect being removed. See pkg/botevent/seq.go.
	seq, err := botevent.NextEventID(ctx, robotID)
	if err != nil {
		return err
	}
	messageUpdateJson := util.ToJson(&robotEvent{
		EventID: seq,
		Message: message,
		Expire:  time.Now().Add(ctx.GetConfig().Robot.MessageExpire).Unix(),
	})
	key := botevent.QueueKey(robotID)
	if err := ctx.GetRedisConn().ZAdd(key, float64(seq), messageUpdateJson); err != nil {
		return err
	}
	if err := ctx.GetRedisConn().Expire(key, ctx.GetConfig().Robot.MessageExpire); err != nil {
		// Best-effort TTL refresh — do not fail the enqueue. Mirrors
		// saveRobotMessage which also only logs on Expire failure.
		botevent.Notify(ctx.GetConfig(), robotID)
		return nil
	}
	// Wake any /v1/bot/events long-poll parked on this bot. After the ZADD, so a
	// waiter is never woken toward an event the queue does not have yet; and
	// after the EXPIRE, so a brand-new queue key is never left without a TTL
	// across the ring. Notify does no I/O on this goroutine — see pkg/botevent,
	// which explains why the ring must not be synchronous here.
	botevent.Notify(ctx.GetConfig(), robotID)
	return nil
}

// enqueueBotTypedEventGeneric 是类型化事件（event_type/event_data，如 P2 D5 的
// card_action）入队的共享 helper —— 与 enqueueBotEventGeneric 走同一
// GenSeq / ZAdd / Expire chokepoint，只是承载 EventType/EventData 而非 Message，
// 并把分配到的 event_id（= seq）返回给调用方（D4 用作 confirm 值 / D8 bot 幂等键）。
func enqueueBotTypedEventGeneric(ctx *config.Context, robotID, eventType string, eventData map[string]interface{}) (int64, error) {
	prepared, err := prepareBotTypedEvent(ctx, robotID, eventType, eventData)
	if err != nil {
		return 0, err
	}
	if err := ctx.GetRedisConn().ZAdd(prepared.QueueKey, float64(prepared.EventID), prepared.Member); err != nil {
		return 0, err
	}
	if err := ctx.GetRedisConn().Expire(prepared.QueueKey, ctx.GetConfig().Robot.MessageExpire); err != nil {
		// Best-effort TTL refresh — 与 enqueueBotEventGeneric 一致，不因 TTL
		// 刷新失败而回滚已成功的 ZAdd（event 已入队，event_id 有效）。
		botevent.Notify(ctx.GetConfig(), robotID)
		return prepared.EventID, nil
	}
	botevent.Notify(ctx.GetConfig(), robotID)
	return prepared.EventID, nil
}

func prepareBotTypedEvent(ctx *config.Context, robotID, eventType string, eventData map[string]interface{}) (PreparedBotTypedEvent, error) {
	if ctx == nil {
		return PreparedBotTypedEvent{}, errors.New("robot: nil ctx, cannot prepare typed bot event")
	}
	if strings.TrimSpace(robotID) == "" {
		return PreparedBotTypedEvent{}, errors.New("robot: empty robotID, cannot prepare typed bot event")
	}
	if strings.TrimSpace(eventType) == "" {
		return PreparedBotTypedEvent{}, errors.New("robot: empty eventType, cannot prepare typed bot event")
	}
	// #697: see enqueueBotEventGeneric — monotonic allocator, no GenSeq fallback.
	seq, err := botevent.NextEventID(ctx, robotID)
	if err != nil {
		return PreparedBotTypedEvent{}, err
	}
	messageUpdateJson := util.ToJson(&robotEvent{
		EventID:   seq,
		EventType: eventType,
		EventData: eventData,
		Expire:    time.Now().Add(ctx.GetConfig().Robot.MessageExpire).Unix(),
	})
	return PreparedBotTypedEvent{
		EventID:  seq,
		QueueKey: botevent.QueueKey(robotID),
		Member:   messageUpdateJson,
	}, nil
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
	// seqStore is the shared transactional message_extra version allocator
	// (#627). All message_extra version allocation in this module goes through
	// it; it selects legacy GenSeq vs the transactional channel sequence by the
	// DB-authoritative state row.
	seqStore *msgextraseq.Store
	// systemSettings is the process-wide admin-config snapshot, resolved once
	// here rather than per call. common.EnsureSystemSettings takes a global
	// mutex on every invocation, so calling it per request would put a
	// process-wide lock on the card send path and defeat the lock-free
	// atomic.Pointer read the snapshot is designed around.
	systemSettings *commonmodule.SystemSettings
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
		seqStore:                      msgextraseq.New(ctx),
		systemSettings:                commonmodule.EnsureSystemSettings(ctx),
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
		auth.GET("/robot/my_bots", rb.myBots)                        // 我的 Bot — 已添加好友的 Bot
		// bot 群级免@偏好（octo-server#237）：owner 写/读/列群
		auth.GET("/robot/:robot_id/groups", rb.listGroups)                                  // 列出 bot 所在群 + no_mention
		auth.PUT("/robot/:robot_id/groups/:group_no/mention_pref", rb.setMentionPref)       // UPSERT 群级免@偏好
		auth.DELETE("/robot/:robot_id/groups/:group_no/mention_pref", rb.deleteMentionPref) // 删除回退默认（幂等）
		auth.GET("/robot/:robot_id/groups/:group_no/mention_pref", rb.getMentionPref)       // 读群级免@偏好
		// Bot 级配置（task bot-setting-store）：owner 目录查询 / 批量写 / 删除覆盖。
		//
		// SharedUIDRateLimiter 逐路由挂而非挂在整个 auth 组上：本组已有十余个既有
		// 路由，给整组加限流会改变它们的行为，超出本任务范围。顺序必须在
		// AuthMiddleware 之后——限流器读不到 uid 会静默 fail-open。
		auth.GET("/robot/:robot_id/settings", appwkhttp.SharedUIDRateLimiter(r, rb.ctx), rb.listBotSettings)
		auth.PUT("/robot/:robot_id/settings", appwkhttp.SharedUIDRateLimiter(r, rb.ctx), rb.updateBotSettings)
		auth.DELETE("/robot/:robot_id/settings/:key", appwkhttp.SharedUIDRateLimiter(r, rb.ctx), rb.deleteBotSetting)
	}

	ownedBots := r.Group("/v1", rb.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, rb.ctx))
	{
		ownedBots.GET("/robot/owned_bots", rb.ownedBots)
	}

	// Bot 广场 — Space 内所有 Bot。
	//
	// 从上面的共享 auth 组里摘出来单独成组，是为了补两道此前缺失的防护：
	//
	//  1. SpaceMiddleware。本路由从 query 取 space_id 后直接查库，全链路没有任何
	//     成员身份校验，任何登录用户传一个别的 space_id 就能拿到该空间全部 bot 的
	//     name / description / creator_uid / creator_name / bot_commands——跨租户
	//     读取。中间件恰好也从 query 读 space_id，因此挂上即可，handler 与响应
	//     形状都不用动。
	//  2. SharedUIDRateLimiter。这是个可枚举的列表接口，此前只有全局 IP 限流。
	//
	// 顺序：AuthMiddleware → 限流 → Space 校验。限流必须在 AuthMiddleware 之后
	// 才读得到 uid，否则会静默 fail-open。
	spaceBots := r.Group("/v1", rb.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, rb.ctx), space.SpaceMiddleware(rb.ctx))
	{
		spaceBots.GET("/robot/space_bots", rb.spaceBots)
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
		respondRobotRequestInvalid(c, "")
		return
	}

	// 身份 → 频道 → 内容，与同组 sendMessage 逐条对齐（round-7 评审阻塞项）。
	//
	// 顺序本身是契约的一部分，不是排版：sendMessage 先 allowSendToChannel 再
	// payloadIsVail，即**先确定你有没有资格往这里发，再看你发的是什么**。本分支已经为
	// 同一条原则改过一次——settings 三个端点的属主校验被三位 reviewer 连提三轮，最终
	// 前移到形状校验之前，理由是「对未知/无权资源返回 400 与端点自述的 404/403 语义
	// 矛盾」。这里同理：非群成员发一张卡，该答 channel_send_forbidden（你没资格），
	// 而不是 content_invalid（你东西不对）——后者等于在鉴权之前就对内容表态。
	robotID := c.Param("robot_id")

	// FromUID 此前直接取调用方传入的值。同一个 robotAuth 组里 sendMessage 与 typing 都把
	// 它钉成 c.Param("robot_id")，只有本 handler 是例外，于是一个已鉴权的 Bot 能以**任意
	// uid** 开流。鉴权解决的是「你是谁」，这里却让请求体决定「你说你是谁」。评审判定这条
	// 比卡片那条更重，且与客户端是否渲染卡片无关。
	//
	// 钉完之后拿**钉过的 req.FromUID** 去查频道权限，而不是旁边的 robotID：两者此刻等值，
	// 但校验的必须是真正会被派发的那个字段。若哪天钉住被删或被移到后面，用 robotID 校验会
	// 让频道门继续看到正确身份、而 IM 收到伪造身份，门就成了摆设。一个变量走到底，这种漂移
	// 不可能发生。
	req.FromUID = robotID
	if !rb.allowSendToChannel(req.FromUID, req.ChannelID, req.ChannelType) {
		rb.Warn("robot 无权向该频道开流",
			zap.String("robot_id", robotID), zap.String("channel_id", req.ChannelID))
		httperr.ResponseErrorL(c, errcode.ErrRobotChannelSendForbidden, nil, nil)
		return
	}

	// 流式入口一律不接受卡片 payload。
	//
	// 本 handler 把 req.Payload（裸 []byte）原样转给 WuKongIM：没有 payloadIsVail、
	// 没有 cardmsg.Validate、没有 BotEnabled() 总闸、也没有 per-Bot 门。于是 owner 关掉
	// 展示/交互卡之后，同一个 Bot 换这个端点仍能把 type:17 送出去——本任务对外承诺的是
	// 「每条已鉴权的发卡路径都按有效配置校验」，少一条这个承诺就是假的。
	//
	// **拒绝**而不是补一套完整门禁，是刻意的：补门意味着要在这条路上重建形状校验、
	// URL 白名单、节点/深度上限与 profile 协商，那是把 sendMessage 的整条流水线复制一遍；
	// 而流式消息的用途是增量文本，卡片有 sendMessage 与模板 message/edit 两条正规入口，
	// 没有理由从这里发。拒绝让承诺变真，且不新增一套会和 sendMessage 漂移的判定。
	//
	// 边界：IsCardRawPayload 只认 JSON 数字 type，与 IsCardPayload 同源。本 handler 不做
	// 类型分发，因此不存在 legacy sendMessage 那个「路由强转字符串、校验不认」的夹缝
	// （round-3 P2-1）；`{"type":"17"}` 在这里当普通 payload 转发，见 journal known-gaps。
	if cardmsg.IsCardRawPayload(req.Payload) {
		rb.Warn("stream/start 不接受卡片 payload，已拒绝", zap.String("robot_id", robotID))
		respondRobotContentInvalid(c, "payload")
		return
	}

	// 解散守卫：群或子区解散后禁止开始流式消息
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		if disbanded, err := rb.isGroupDisbanded(req.ChannelID); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		parentGroupNo, err := rb.resolveParentGroupNo(req.ChannelID)
		if err != nil {
			rb.Error("解析子区父群错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
		if disbanded, err := rb.isGroupDisbanded(parentGroupNo); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	}

	streamNo, err := rb.ctx.IMStreamStart(req)
	if err != nil {
		rb.Error("发送stream start消息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotSendFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "")
		return
	}

	// 解散守卫：群或子区解散后禁止结束流式消息
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		if disbanded, err := rb.isGroupDisbanded(req.ChannelID); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		parentGroupNo, err := rb.resolveParentGroupNo(req.ChannelID)
		if err != nil {
			rb.Error("解析子区父群错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
		if disbanded, err := rb.isGroupDisbanded(parentGroupNo); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	}

	err := rb.ctx.IMStreamEnd(req)
	if err != nil {
		rb.Error("发送stream end消息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotSendFailed, nil, nil)
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
			respondRobotAuthCheckFailed(c)
			return
		}
		if robot == nil {
			// Anti-enumeration: the wire collapses to one 401, but log the
			// specific reason so operators retain visibility.
			rb.Warn("robot 鉴权失败：机器人不存在", zap.String("robot_id", robotID))
			respondRobotAuthFailed(c)
			return
		}
		appM, err := rb.appService.GetApp(robot.AppID)
		if err != nil {
			rb.Error("查询app失败！", zap.Error(err), zap.String("appID", robot.AppID))
			respondRobotAuthCheckFailed(c)
			return
		}
		if appM == nil {
			rb.Warn("robot 鉴权失败：app 不存在", zap.String("robot_id", robotID), zap.String("appID", robot.AppID))
			respondRobotAuthFailed(c)
			return
		}
		if !hmac.Equal([]byte(appM.AppKey), []byte(appKey)) {
			rb.Warn("robot 鉴权失败：appKey 不匹配", zap.String("robot_id", robotID), zap.String("appID", robot.AppID))
			respondRobotAuthFailed(c)
			return
		}
		c.Next()
	}
}

func (rb *Robot) typing(c *wkhttp.Context) {
	var req *TypingReq
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		respondRobotRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		respondRobotRequestInvalid(c, "channel_id")
		return
	}
	if req.ChannelType == 0 {
		respondRobotRequestInvalid(c, "channel_type")
		return
	}
	fromUID := c.Param("robot_id")
	if fromUID == "" {
		respondRobotRequestInvalid(c, "from_uid")
		return
	}
	if !rb.allowSendToChannel(fromUID, req.ChannelID, req.ChannelType) {
		httperr.ResponseErrorL(c, errcode.ErrRobotChannelSendForbidden, nil, nil)
		return
	}

	// 解散守卫（企业微信式只读）：群或子区解散后禁止 robot 发送 typing 指示
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		if disbanded, err := rb.isGroupDisbanded(req.ChannelID); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		parentGroupNo, err := rb.resolveParentGroupNo(req.ChannelID)
		if err != nil {
			rb.Error("解析子区父群错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
		if disbanded, err := rb.isGroupDisbanded(parentGroupNo); err != nil {
			rb.Error("查询父群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	}

	err := rb.ctx.SendTyping(req.ChannelID, req.ChannelType, fromUID)
	if err != nil {
		rb.Error("发送typing消息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotSendFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (rb *Robot) sendMessage(c *wkhttp.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cardmsg.MaxSendBodyBytes)
	var messageReq *MessageReq
	if err := c.BindJSON(&messageReq); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		respondRobotRequestInvalid(c, "")
		return
	}
	if strings.TrimSpace(messageReq.ChannelID) == "" {
		respondRobotRequestInvalid(c, "channel_id")
		return
	}
	if messageReq.ChannelType == 0 {
		respondRobotRequestInvalid(c, "channel_type")
		return
	}
	if len(messageReq.Payload) == 0 {
		respondRobotContentInvalid(c, "payload")
		return
	}

	robotID := c.Param("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}
	if !rb.allowSendToChannel(robotID, messageReq.ChannelID, messageReq.ChannelType) {
		httperr.ResponseErrorL(c, errcode.ErrRobotChannelSendForbidden, nil, nil)
		return
	}

	// 解散守卫（企业微信式只读）：群或子区解散后禁止 robot 发送消息
	if messageReq.ChannelType == common.ChannelTypeGroup.Uint8() {
		if disbanded, err := rb.isGroupDisbanded(messageReq.ChannelID); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	} else if messageReq.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		parentGroupNo, err := rb.resolveParentGroupNo(messageReq.ChannelID)
		if err != nil {
			rb.Error("解析子区父群错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
		if disbanded, err := rb.isGroupDisbanded(parentGroupNo); err != nil {
			rb.Error("查询父群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
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
		respondRobotContentInvalid(c, "payload.type")
		return
	}
	contentType := common.ContentType(contentTypeValue)
	if !rb.supportContentType(contentType) {
		respondRobotContentTypeUnsupported(c, int(contentType))
		return
	}

	if !rb.payloadIsVail(payloadResult) {
		respondRobotContentInvalid(c, "payload")
		return
	}

	// per-Bot 卡片策略门（task bot-setting-store，round-2 评审 P1-1）。
	//
	// 本仓有**三个**卡片生产者入口，payloadIsVail 的 InteractiveCard 分支自己就写着
	// 「与 bot_api 的 send gate 对称」。bot_setting 上线时只给 bot_api 那道门加了
	// per-Bot 判定，这条 legacy 路径仍只有部署总闸 + Validate —— 而两条路的身份是**同
	// 一列**（authRobot 按 robot.robot_id 解析，bot_setting.robot_id 与 assertRobotOwner
	// 的 creator_uid 取自同一张表）。于是 owner 关掉展示/交互卡后，同一个 Bot 换这个
	// 端点仍能把卡发出去：一个被某条已鉴权路径忽略的能力开关，就不成其为能力开关。
	//
	// 排在 payloadIsVail 之后：形状先过，策略再判，拒绝形状与本 ingress 既有口径一致
	// （单一 content-invalid，防枚举；具体原因只进日志）。
	if rejectRobotCardBySetting(payloadResult) {
		cfg, cfgErr := rb.BotCardConfig(robotID)
		if cfgErr != nil {
			// fail-closed：配置读不到时不得放行，否则一次 DB 抖动就把 owner 关掉的
			// 能力从这条路放开（与 bot_api 侧同姿态）。
			rb.Error("查询 Bot 卡片配置失败", zap.Error(cfgErr), zap.String("robot_id", robotID))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
		if !robotCardPolicyAllows(payloadResult, cfg) {
			rb.Warn("Bot 卡片能力已关闭，legacy robot ingress 拒绝",
				zap.String("robot_id", robotID),
				zap.Bool("display", cfg.DisplayEnabled),
				zap.Bool("interaction", cfg.InteractionEnabled))
			respondRobotContentInvalid(c, "payload")
			return
		}
	}
	userResp, err := rb.userService.GetUserWithUsername(robotID)
	if err != nil {
		rb.Error("查询机器人的用户信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if userResp == nil {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	// YUJ-644 / Mininglamp-OSS#33: PERSONAL DM 派发前服务端权威 space_id 注入。
	// 设计 / 失败模式见 modules/bot_api/space_inject.go 顶部注释。
	payload := messageReq.Payload
	if messageReq.ChannelType == common.ChannelTypePerson.Uint8() {
		payload = rb.enrichBotPayloadWithSpaceID(robotID, payload)
	}

	// 图文混排 RichText(=14)：派发出口用 content 重算权威顶层 plain，覆盖客户端
	// 不可信 plain（契约 §2），供下游 summary / matter / search / 复制 / 推送 复用。
	// 非 type=14 为 no-op，老消息路径不变。入站 write-strict 校验已由上方
	// payloadIsVail→common.ValidateRichTextPayload 完成（与 user 路径 richtext.Validate
	// 对称）；这里只做 plain 权威生成 + 对真实最终 payload（含 enrichBotPayloadWithSpaceID
	// 注入的顶层字段）的 1MB 复检（PR#232 Jerry-Xin Critical#2）。
	if err := richtext.Finalize(payload); err != nil {
		rb.Error("RichText payload plain 生成/复检失败", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", messageReq.ChannelID))
		respondRobotContentInvalid(c, "payload")
		return
	}

	// card-message-protocol P1 Decision 8：InteractiveCard(=17) 的 server 权威
	// plain 收尾 + 真实出站 payload 512KiB 复检（与上方 richtext.Finalize 同位、
	// 同口径；Decision 9 保证 enrich 只触碰信封顶层键，card 树永不被改写）。
	// 非 type=17 为 no-op。
	if err := cardmsg.Finalize(payload); err != nil {
		rb.Error("InteractiveCard finalize 失败", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", messageReq.ChannelID))
		respondRobotContentInvalid(c, "payload")
		return
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

	// card-message-protocol P1 Decision 3a：ExpandAisToBotUIDs 是 Finalize 之后
	// 唯一会增大 payload 的 mutation（追加频道 bot 成员 UID 到 mention 子表）。
	// Finalize 的 512KiB 复检发生在展开之前，覆盖不到真实出站字节，故对最终
	// wirePayload 再复检一次（PR#543 review：与 bot_api 出站口径对称、与 richtext
	// PR#232「最后一次 mutation 后复检」不变量对齐）。非 type=17 为 no-op。
	if err := cardmsg.RecheckPayloadSize(wirePayload); err != nil {
		rb.Error("InteractiveCard 出站 payload 超限", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", messageReq.ChannelID))
		respondRobotContentInvalid(c, "payload")
		return
	}

	result, err := rb.ctx.SendMessageWithResult(&config.MsgSendReq{
		StreamNo:    messageReq.StreamNo,
		ChannelID:   messageReq.ChannelID,
		ChannelType: messageReq.ChannelType,
		FromUID:     robotID,
		Payload:     []byte(util.ToJson(wirePayload)),
	})
	if err != nil {
		rb.Error("发送robot消息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotSendFailed, nil, nil)
		return
	}
	c.Response(result)
}

func (rb *Robot) supportContentType(contentType common.ContentType) bool {
	switch contentType {
	case common.Text, common.Image, common.GIF, common.Voice,
		common.Video, common.Location, common.Card, common.File,
		common.RichText, common.VectorSticker, common.EmojiSticker,
		cardmsg.InteractiveCard:
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
	case cardmsg.InteractiveCard:
		// card-message-protocol P1：robot 是三个卡片生产者入口之一，与 bot_api
		// 的 send gate 对称（rollout flag + write-strict Validate）。本 ingress
		// 的错误形状是单一 content-invalid 400（防枚举）——flag 关闭 / 白名单 /
		// 大小 / URL 失败的具体原因只进日志。
		//
		// 路由谓词与校验谓词必须是同一个（round-3 评审 P2-1）。本 ingress 是全仓
		// **唯一**用 maputil.Data.Int("type") 分发的入口（bot_api / message / notify
		// 都直接调 IsCardPayload），而 Int 会对字符串跑 strconv.Atoi，IsCardPayload
		// 则只认 float64/int/json.Number、**故意**拒掉字符串 "17"。两者不一致时，
		// {"type":"17"} 会：进本分支 → Validate 因 IsCardPayload=false 直接 return nil
		// （pkg/cardmsg/validate.go）→ rejectRobotCardBySetting 同样判 false 而跳过全部
		// per-Bot 开关 → Finalize no-op。于是一棵没过 URL 白名单、节点/深度上限、
		// profile 协商与能力开关的卡片树被派发出去，只要有客户端对 type 做宽松转换
		// 就能渲染。在这里收口成 IsCardPayload：卡片分支的可达性从此与校验、与门禁
		// 完全同源，不再存在「路由认为是卡片、校验认为不是」的夹缝。
		if !cardmsg.IsCardPayload(payloadResult) {
			rb.Warn("payload.type 非 JSON 数字，不认作卡片（拒绝而非降级放行）")
			return false
		}
		if !cardmsg.BotEnabled() {
			// bot 侧有效门禁：总开关 OCTO_CARD_MESSAGE_ENABLED（Decision 2 rollout
			// gate）AND bot 子开关 OCTO_BOT_CARD_ENABLED；robot 是 bot 生产者之一，
			// 与 bot_api send/edit 及 /v1/bot/card/profile.enabled 同源。
			rb.Warn("卡片消息未启用,robot ingress 拒绝(部署总开关或 bot 子开关关闭)")
			return false
		}
		// PR-C D3：server-only catalog 标记（template_ref / catalog_provenance）
		// 只能由可信边界写入；robot ingress 按键存在显式拒绝（与 bot_api 的
		// reject 口径对称，不走 __obo_* 的静默 strip —— 见 sanitize_robot_ingress.go）。
		if robotCardPayloadForgesCatalogMarker(map[string]interface{}(payloadResult)) {
			rb.Warn("robot 卡片试图伪造 server-only catalog 标记,拒绝")
			return false
		}
		if err := cardmsg.Validate(map[string]interface{}(payloadResult)); err != nil {
			rb.Warn("InteractiveCard payload 校验失败", zap.Error(err))
			return false
		}
		return true
	case common.RichText:
		// 图文混排 RichText(=14)：发送端 write-strict 校验。升级为调
		// common.ValidateRichTextPayload，对序列化后的 payload 做大小上限、
		// content 必填非空、每个 block 结构合法（text 非空 / image url scheme +
		// width/height）的完整契约校验，取代旧的仅 content != nil 浅检。
		raw, err := json.Marshal(map[string]interface{}(payloadResult))
		if err != nil {
			return false
		}
		if _, err := common.ValidateRichTextPayload(raw); err != nil {
			return false
		}
		return true
	}
	return false
}

// 是否允许发送消息到频道
//
// 三个 robot ingress（sendMessage / typing / streamStart）共用这一份判定，所以这里
// 少认一种频道类型，那种类型就在三条路上同时不可用。
//
// **子区（CommunityTopic）此前落在末尾的「未知类型」里被一律拒绝**，而 sendMessage、
// typing、streamStart 三处各自都写着一个解析父群的子区解散守卫——那三个分支因此永远
// 不可达。守卫会去 resolveParentGroupNo 说明作者本就打算让子区能用，是本函数漏了这个
// case，不是那三处多写了。streamStart 补上频道校验时（round-8 评审）这个洞才暴露出来：
// 那条路先前没有校验，于是成了三条里唯一「子区能发」的例外。
//
// 子区的成员资格取决于**父群**：子区 channelID 形如 `<parentGroupNo>____<topicNo>`，
// 权限模型里子区不单独持有成员表。因此解析出父群再查成员，解析是纯字符串操作、不额外碰库。
//
// **但用的谓词比群分支严**，不是同一条规则：群分支是 ExistMember（只看 is_deleted），
// 子区分支是 ExistMemberActive（还要求 status=Normal）。这个不对称是全仓既有口径——
// 子区门禁在 YUJ-4185 被专门加固过，thread / message / messages_search / bot_api 各处
// 都用严格变体，群门禁则仍是松的。所以「被拉黑的 bot 能发父群、不能发子区」是刻意的，
// 不是遗漏。收紧群分支是更大范围的行为变更，另行评估。
//
// 方向上这是**放宽**（type 5：永远拒 → 按父群成员判定），所以今天能发的一样能发；
// 受影响的只有先前被无条件 403 的子区请求。
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
	if channelType == common.ChannelTypeCommunityTopic.Uint8() {
		// 子区：成员资格在父群上。channelID 形状不合法时 fail-closed。
		parentGroupNo, err := rb.resolveParentGroupNo(channelID)
		if err != nil {
			rb.Error("解析子区父群失败！", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", channelID))
			return false
		}
		// **必须是 ExistMemberActive，不是上面群分支那个 ExistMember**（round-8 评审 P1）。
		// ExistMember 只过滤 is_deleted=0，被拉黑成员（status=Blacklist）照样返回 true；
		// ExistMemberActive 额外要求 status=Normal。group/service.go 上该方法的注释点名了
		// 这个场景：「子区(CommunityTopic)读/发门禁用它替代 ExistMember，避免被拉黑用户
		// 越权读/发（YUJ-4185 CR 整改）」，thread / message / messages_search / bot_api
		// 的每一处子区门禁也都用它。
		//
		// 这条路尤其不能松：robot ingress 是服务端直发，不经过 IM datasource，因而拿不到
		// thread 模块的子区拉黑继承（thread/1module.go）——本地这道门就是唯一防线，正是
		// group/db.go 里「用于绕过 IM 层的接口」那句话所指的情形。
		exist, err := rb.groupService.ExistMemberActive(parentGroupNo, robotID)
		if err != nil {
			rb.Error("检查机器人是否是子区父群活跃成员失败！", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", channelID))
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
		respondRobotRequestInvalid(c, "")
		return
	}
	if err := result.Check(); err != nil {
		respondRobotRequestInvalid(c, "")
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
		respondRobotRequestInvalid(c, "")
		return
	}
	if len(req.Username) == 0 {
		respondRobotRequestInvalid(c, "username")
		return
	}
	robotM, err := rb.db.queryWithUsername(req.Username)
	if err != nil {
		rb.Error("查询机器人失败", zap.Error(err), zap.String("username", req.Username))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if robotM == nil {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	if strings.TrimSpace(robotM.AppID) == "" {
		rb.Error("机器人没有app_id", zap.String("username", req.Username))
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
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
		respondRobotInlineQueryTimeout(c)
	}

	rb.inlineQueryEventResultChanMapLock.Lock()
	delete(rb.inlineQueryEventResultChanMap, sid)
	rb.inlineQueryEventResultChanMapLock.Unlock()

	rb.removeInlineQuery(robotID, sid)

}

// #697 (reviewer P1): inline-query event ids MUST come from the same monotonic
// source as the queue's, because both streams are merged into one response.
//
// getEventsResult below reads inlineQueryEventsMap, appends it to the events read
// from robotEvent:{robotID}, sorts the union by EventID, and filters it by the
// caller's cursor. So the two id spaces are not independent — they share a cursor.
//
// An earlier revision of this change gave inline queries their own GenSeq key on
// the belief that nothing consumed them. That was wrong, and the consequence was
// worse than the bug being fixed: a fresh GenSeq key starts near 1000001 while the
// new counter starts low, so one inline event would push the client's cursor into
// the millions and permanently filter out every ordinary event behind it.
//
// Sharing NextEventID keeps them on one source in both modes — one GenSeq
// sequence before activation, one counter after.
func (rb *Robot) addInlineQuery(robotID string, inlineQuery *InlineQuery) {
	seq, err := botevent.NextEventID(rb.ctx, robotID)
	if err != nil {
		rb.Error("allocate inline query event id failed", zap.Error(err), zap.String("robotID", robotID))
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
		respondRobotRequestInvalid(c, "")
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
		respondRobotRequestInvalid(c, "event_id")
		return
	}

	err = rb.removeEvent(robotID, eventID)
	if err != nil {
		rb.Error("移除机器人事件失败", zap.Error(err), zap.Int64("event_id", eventID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "robot_id")
		return
	}

	botCommands, err := rb.db.queryBotCommandsByRobotID(robotID)
	if err != nil {
		rb.Error("查询机器人命令失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}

	if strings.TrimSpace(botCommands) == "" {
		c.Response([]interface{}{})
		return
	}

	// BotFather 自身的菜单是服务端自有文案，按请求协商语言渲染（#335）；库存
	// blob 只是部署默认语言兜底。放在存在性/启用（status=1）/空值门控之后，
	// 三个读取面的覆盖条件保持一致（仅库存非空时覆盖）。其余 bot 的 commands
	// 是创建者内容，照旧原样返回。
	if robotID == cmdmenu.BotFatherUID {
		c.Response(cmdmenu.Commands(octoi18n.OutboundLanguage(c.Request.Context())))
		return
	}

	var commands []interface{}
	if err := json.Unmarshal([]byte(botCommands), &commands); err != nil {
		rb.Error("解析机器人命令失败", zap.Error(err), zap.String("botCommands", botCommands))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "")
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
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			rb.Error("批量查询机器人数据错误", zap.Error(err))
			return
		}
	} else if len(usernames) > 0 {
		robotList, err = rb.db.queryWithUsernames(usernames)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "")
		return
	}

	// 验证操作者是 Bot 创建者
	var creatorUID string
	err := rb.ctx.DB().Select("IFNULL(creator_uid,'')").From("robot").Where("robot_id=? AND status=1", robotID).LoadOne(&creatorUID)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		// A real DB/scan error must not masquerade as 404 — log + 500 (mirrors
		// assertRobotOwner in mention_pref.go).
		rb.Error("查询 robot creator 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if creatorUID == "" {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	if creatorUID != loginUID {
		httperr.ResponseErrorL(c, errcode.ErrRobotCreatorOnly, nil, nil)
		return
	}

	_, err = rb.ctx.DB().Update("robot").Set("description", req.Description).Where("robot_id=?", robotID).Exec()
	if err != nil {
		rb.Error("更新 robot description 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "")
		return
	}

	// 验证操作者是 Bot 创建者
	var creatorUID string
	err := rb.ctx.DB().Select("IFNULL(creator_uid,'')").From("robot").Where("robot_id=? AND status=1", robotID).LoadOne(&creatorUID)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		// A real DB/scan error must not masquerade as 404 — log + 500 (mirrors
		// assertRobotOwner in mention_pref.go).
		rb.Error("查询 robot creator 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if creatorUID == "" {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	if creatorUID != loginUID {
		httperr.ResponseErrorL(c, errcode.ErrRobotCreatorOnly, nil, nil)
		return
	}

	_, err = rb.ctx.DB().Update("robot").Set("auto_approve", req.AutoApprove).Where("robot_id=?", robotID).Exec()
	if err != nil {
		rb.Error("更新 robot auto_approve 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// spaceBots Bot 广场 — 获取 Space 内所有 Bot
func (rb *Robot) spaceBots(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Query("space_id")
	if spaceID == "" {
		respondRobotRequestInvalid(c, "space_id")
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
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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

// ownedBots 我创建的 Bot — 仅返回【登录用户创建、robot.status=1、属于指定 Space 且 space_member.status=1】的 Bot。
// 与 myBots(已加好友) / spaceBots(Space 全部) 区分：owner 语义。
func (rb *Robot) ownedBots(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Query("space_id")
	if spaceID == "" {
		respondRobotRequestInvalid(c, "space_id")
		return
	}

	isMember, err := space.CheckMembership(rb.ctx.DB(), spaceID, loginUID)
	if err != nil {
		rb.Error("校验 Space 成员身份失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrSharedForbidden, nil, nil)
		return
	}

	// creator_uid=loginUID 与 space_id 双重过滤即 space 隔离点，均为占位符，不可绕过。
	// 仅暴露 uid/name/description/bot_commands，绝不返回 token/凭据。
	type ownedBotRow struct {
		UID         string `db:"uid"`
		Name        string `db:"name"`
		Description string `db:"description"`
		BotCommands string `db:"bot_commands"`
	}
	var bots []ownedBotRow
	_, err = rb.ctx.DB().SelectBySql(`
		SELECT r.robot_id as uid, IFNULL(u.name,'') as name,
			IFNULL(r.description,'') as description,
			IFNULL(r.bot_commands,'') as bot_commands
		FROM robot r
		INNER JOIN user u ON u.uid = r.robot_id AND u.robot = 1
		INNER JOIN space_member sm ON sm.uid = r.robot_id AND sm.space_id = ? AND sm.status = 1
		WHERE r.creator_uid = ? AND r.status = 1
		ORDER BY r.created_at DESC
	`, spaceID, loginUID).Load(&bots)
	if err != nil {
		rb.Error("查询我创建的 Bot 列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}

	results := make([]map[string]interface{}, 0, len(bots))
	for _, b := range bots {
		results = append(results, map[string]interface{}{
			"uid":          b.UID,
			"name":         b.Name,
			"description":  b.Description,
			"bot_commands": b.BotCommands,
		})
	}
	c.Response(results)
}

// proxyFile 文件下载代理 — 302 重定向到 presigned URL
func (rb *Robot) proxyFile(c *wkhttp.Context) {
	ph := c.Param("path")
	if ph == "" {
		respondRobotRequestInvalid(c, "path")
		return
	}
	// 去掉前导 /
	ph = strings.TrimPrefix(ph, "/")

	// Sanitize path to prevent directory traversal
	cleaned := filepath.Clean(ph)
	if strings.Contains(cleaned, "..") || strings.ContainsAny(cleaned, "\x00") {
		respondRobotRequestInvalid(c, "path")
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
		httperr.ResponseErrorL(c, errcode.ErrRobotUploadFailed, nil, nil)
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
		// A missing / malformed multipart "file" part is a client error, not an
		// upload-backend failure — surface it as request-invalid (400) with a
		// field detail rather than the Internal=true upload code.
		rb.Warn("读取上传文件失败", zap.Error(err))
		respondRobotRequestInvalid(c, "file")
		return
	}
	defer multipartFile.Close()

	// 文件大小限制 100MB
	const maxSize int64 = 100 * 1024 * 1024
	if fileHeader.Size > maxSize {
		respondRobotFileTooLarge(c, maxSize/1024/1024)
		return
	}

	fileName := fileHeader.Filename
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		httperr.ResponseErrorL(c, errcode.ErrRobotFileTypeUnsupported, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotUploadFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "filename")
		return
	}
	filename = filepath.Base(filename)

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || file.IsBlockedExtension(ext) || !file.IsAllowedExtension(ext) {
		httperr.ResponseErrorL(c, errcode.ErrRobotFileTypeUnsupported, nil, nil)
		return
	}

	cosConfig := rb.ctx.GetConfig().COS
	if cosConfig.SecretID == "" || cosConfig.SecretKey == "" || cosConfig.Bucket == "" {
		rb.Error("COS 配置不完整")
		httperr.ResponseErrorL(c, errcode.ErrRobotUploadFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotUploadFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotUploadFailed, nil, nil)
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
		respondRobotRequestInvalid(c, "filename")
		return
	}
	filename = filepath.Base(filename)

	// fileSize is REQUIRED so the storage layer can sign Content-Length and
	// reject any PUT that exceeds the byte budget — same P0 size-bypass
	// guard the public file API enforces (see modules/file/api.go).
	fileSizeRaw := strings.TrimSpace(c.Query("fileSize"))
	if fileSizeRaw == "" {
		respondRobotRequestInvalid(c, "fileSize")
		return
	}
	fileSize, parseErr := strconv.ParseInt(fileSizeRaw, 10, 64)
	if parseErr != nil || fileSize <= 0 {
		respondRobotRequestInvalid(c, "fileSize")
		return
	}
	if fileSize > file.MaxFileSize {
		rb.Warn("预签名上传 fileSize 超出限制",
			zap.Int64("size", fileSize), zap.Int64("max", file.MaxFileSize))
		respondRobotFileTooLarge(c, file.MaxFileSize/1024/1024)
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || file.IsBlockedExtension(ext) || !file.IsAllowedExtension(ext) {
		httperr.ResponseErrorL(c, errcode.ErrRobotFileTypeUnsupported, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotUploadFailed, nil, nil)
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cardmsg.MaxSendBodyBytes)
	var req struct {
		MessageID   string `json:"message_id"`
		MessageSeq  uint32 `json:"message_seq"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		ContentEdit string `json:"content_edit"`
	}
	if err := c.BindJSON(&req); err != nil {
		rb.Error("数据格式有误！", zap.Error(err))
		respondRobotRequestInvalid(c, "")
		return
	}
	if req.MessageID == "" {
		respondRobotRequestInvalid(c, "message_id")
		return
	}
	if req.MessageSeq == 0 {
		respondRobotRequestInvalid(c, "message_seq")
		return
	}
	if req.ChannelID == "" {
		respondRobotRequestInvalid(c, "channel_id")
		return
	}
	if strings.TrimSpace(req.ContentEdit) == "" {
		respondRobotContentInvalid(c, "content_edit")
		return
	}

	robotID := c.Param("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}

	// 解散守卫（企业微信式只读）：群或子区解散后禁止 robot 编辑消息
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		if disbanded, err := rb.isGroupDisbanded(req.ChannelID); err != nil {
			rb.Error("查询群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	} else if req.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
		parentGroupNo, err := rb.resolveParentGroupNo(req.ChannelID)
		if err != nil {
			rb.Error("解析子区父群错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
		if disbanded, err := rb.isGroupDisbanded(parentGroupNo); err != nil {
			rb.Error("查询父群是否已解散错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		} else if disbanded {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotGroupDisbanded, nil, nil)
			return
		}
	}

	// 权限检查：只允许 Bot 编辑自己发送的消息
	messageSeqs := []uint32{req.MessageSeq}
	resp, err := rb.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, robotID, messageSeqs)
	if err != nil {
		rb.Error("查询消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if resp == nil || len(resp.Messages) == 0 {
		httperr.ResponseErrorL(c, errcode.ErrRobotMessageNotFound, nil, nil)
		return
	}
	if resp.Messages[0].FromUID != robotID {
		httperr.ResponseErrorL(c, errcode.ErrRobotMessageEditForbidden, nil, nil)
		return
	}

	// 图文混排 RichText(=14)：编辑写入口对 content_edit 做与 send 路径对称的
	// write-strict 校验 + 权威 plain 重算（契约 §2，plain 服务端重算不信客户端）。
	// 编辑语义为整体替换 content blocks；非 14 / 非 JSON 体为 no-op。脏/超限 payload
	// 落库前以错误拒绝。MD5 去重 hash 落在 normalize 后的 canonical 体上。
	// card-message-protocol P1 Decision 7：卡片不可变 —— 目标消息为 type-17、
	// 或编辑体为 type-17（把普通消息改写成卡片）都在此拒绝，与 bot_api 编辑路径
	// 共用 cardmsg.RejectsCardEdit 单点谓词（避免两条路拼守卫漂移 —— PR#543 review
	// 发现本路径原先漏查目标是否卡片）。richtext 的 NormalizeContentEdit 是
	// IsRichTextPayload 门控的，卡片体会「原样、零校验」通过（PR#525 round-2
	// finding #1）。resp.Messages[0] 已在上方属主校验取出。
	if cardmsg.RejectsCardEdit(resp.Messages[0].Payload, req.ContentEdit) {
		httperr.ResponseErrorL(c, errcode.ErrRobotCardEditForbidden, nil, nil)
		return
	}
	normalizedEdit, err := richtext.NormalizeContentEdit(req.ContentEdit)
	if err != nil {
		rb.Error("RichText content_edit 校验失败", zap.Error(err), zap.String("messageID", req.MessageID))
		respondRobotContentInvalid(c, "content_edit")
		return
	}
	req.ContentEdit = normalizedEdit

	// 检查是否存在相同编辑内容
	contentEdit := dbr.NewNullString(req.ContentEdit).String
	contentMD5 := util.MD5(contentEdit)

	var existCount int
	err = rb.ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit_hash=?", req.MessageID, contentMD5).LoadOne(&existCount)
	if err != nil {
		rb.Error("查询是否存在相同正文失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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

	// #627 message_extra 版本号在业务事务内经序列行 FOR UPDATE 保留，持锁到 commit，
	// 使提交序=版本序、跨副本唯一。allocator 内部按 DB 状态行分派 legacy/transactional。
	// tx 需在触发 SendCMD（外部副作用）之前 commit，避免客户端在提交前拿到 stale。
	tx, err := rb.ctx.DB().Begin()
	if err != nil {
		rb.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	versions, err := rb.seqStore.ReserveTx(tx, fakeChannelID, req.ChannelType, 1)
	if err != nil {
		rb.Error("生成消息扩展序列号失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	version := versions[0]

	// 写入 message_extra
	_, err = tx.InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version)",
		req.MessageID, req.MessageSeq, fakeChannelID, req.ChannelType, contentEdit, contentMD5, int(time.Now().Unix()), version,
	).Exec()
	if err != nil {
		rb.Error("添加或修改编辑内容失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	if err := tx.Commit(); err != nil {
		rb.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrRobotSendFailed, nil, nil)
		return
	}

	c.ResponseOK()
}

// isGroupDisbanded 查询群是否已解散（status=2），用于 robot 模块的解散守卫。
func (rb *Robot) isGroupDisbanded(groupNo string) (bool, error) {
	var status int
	err := rb.ctx.DB().SelectBySql(
		"SELECT status FROM `group` WHERE group_no=?",
		groupNo,
	).LoadOne(&status)
	if err != nil {
		rb.Error("isGroupDisbanded query failed", zap.String("groupNo", groupNo), zap.Error(err))
		return false, err
	}
	return status == group.GroupStatusDisband, nil
}

// resolveParentGroupNo 从子区 channelID 解析父群号。
// 子区 channelID 格式：groupNo____threadID（4个下划线分隔）
// resolveParentGroupNo 从子区 channelID 解析父群号。
//
// 委托给 thread.ParseChannelID，**不自己写解析**（round-9 评审 P2-2）。原实现用
// `SplitN(channelID, "____", 2)`，只要能切出两段就通过；标准解析器用 `Split`（非
// SplitN）要求**恰好**两段、且两段都非空。差别在本函数只喂解散守卫时无害，但现在它
// 还喂 allowSendToChannel 的子区分支——也就是**鉴权判定**：
//
//	"groupA____topic____extra" → SplitN 给出 parts[0]="groupA"，成员校验按 groupA 过，
//	而派发出去的是原始的非规范 channelID；下游一律走 ParseChannelID，于是那条消息
//	解析失败、投不到任何订阅者。门与投递目标对「什么是合法子区 ID」的判断不一致。
//
// 这与刚修的 P1（子区成员判定用了比全仓更松的谓词）是同一族问题：robot 模块把一个
// 全仓已有的判定又实现了一遍，而且更松。所以这里不再「顺手收紧」，直接用那一份。
//
// 不额外套 thread.IsValidShortID（15–20 位数字）：本函数要保证的性质是「校验成员的
// 群 == 消息实际投递的群」，恰好两段非空即可确定 parts[0] 无歧义；short id 的取值域
// 是另一回事，收紧它的影响面本处无法核实。
func (rb *Robot) resolveParentGroupNo(channelID string) (string, error) {
	groupNo, _, err := thread.ParseChannelID(channelID)
	if err != nil {
		return "", fmt.Errorf("invalid community topic channelID format: %s: %w", channelID, err)
	}
	return groupNo, nil
}
