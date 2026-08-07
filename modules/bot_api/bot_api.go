package bot_api

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardrevision"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
)

const (
	// heartbeat Redis key prefix and TTL
	heartbeatKeyPrefix = "bot:heartbeat:"
	heartbeatTTL       = 60

	// robotEventPrefix for events queue
	robotEventPrefix = "robotEvent:"
)

// BotAPI is the public Bot API gateway module.
// It handles all bot-facing endpoints (/v1/bot/*) with unified auth.
type BotAPI struct {
	ctx           *config.Context
	db            *botAPIDB
	userService   user.IService
	fileService   file.IService
	groupService  group.IService
	userDB        *user.DB
	threadService thread.IService
	// robotService gives the OBO fan-out path a way to enqueue synthetic
	// events directly into a grantee bot's /v1/bot/events queue. The
	// webhook layer drops NoPersist=1 messages before NotifyMessagesListeners
	// (modules/webhook/api.go handleMessageNotify), and the OBO fan-out
	// copy intentionally sets NoPersist=1 to keep the copy out of chat
	// history. Without a direct enqueue, the fan-out copy reaches
	// WuKongIM but never reaches the bot — see YUJ-1424 / PR#82
	// Jerry-Xin review blocker. fanoutForMessage calls robotService
	// AFTER dispatchFanout succeeds so we only enqueue events that
	// WuKongIM actually accepted.
	robotService robot.IService
	// cardConfig resolves the per-Bot card capability switches (task
	// bot-setting-store). Production points it at the same robotService
	// instance above; it is a separate, one-method field so focused tests can
	// inject a fake without implementing all of robot.IService — same reason
	// modules/file declares stickerSystemSettings instead of taking the whole
	// *common.SystemSettings.
	//
	// A nil value is a wiring error, not a permissive default: the card paths
	// fail closed rather than assume every switch is on.
	cardConfig botCardConfigResolver
	// ackFilteredEvent overrides the auto-ACK write in filterAppBotEvents.
	// Production leaves it nil (see BotAPI.ackEvent); tests set it to inject a
	// Redis whose reads succeed while writes fail, which is the state the
	// long-poll loop's forward-progress guarantee exists to survive and which a
	// healthy Redis cannot be talked into producing.
	ackFilteredEvent func(key string, eventID string) error
	// cardRevisions is the D10 card revision history store (shared table
	// octo_message_card_revision; written here on bot card edits + clear).
	cardRevisions *cardrevision.Store
	// cardMutator owns the card_seq CAS primitive shared with first-party
	// callback finalization. Existing tests that construct BotAPI literals keep
	// the legacy local fallback in cardSeqCASWrite when this field is nil.
	cardMutator botAPICardMutator
	// cardTemplates is the deployment-global, code-reviewed Bot allowlist over
	// the broader cardtmpl Registry. Production construction fails closed when
	// any advertised ref is missing or internally inconsistent.
	cardTemplates *botCardTemplateCatalog
	// seqStore is the shared transactional message_extra version allocator (#627),
	// used by the local card-write fallbacks when cardMutator is nil.
	seqStore              *msgextraseq.Store
	speechClient          *voice_adapter.SpeechClient
	maxVoiceContextLength int
	maxBodySize           int64
	maxFileSize           int64
	// spaceQuerier overrides ba.db for resolveBotActiveSpaceID (test injection).
	// nil in production; tests set it to stub the DB call deterministically.
	spaceQuerier botSpaceQuerier
	// dispatchOverride lets tests intercept the IM dispatch call to capture the
	// final MsgSendReq (including server-authoritative payload.space_id).
	// nil in production; the real path goes through ba.ctx.SendMessageWithResult.
	dispatchOverride func(*config.MsgSendReq) (*config.MsgSendResp, error)
	// oboStoreOverride lets unit tests inject an in-memory oboStore so
	// checkOBO / REST handlers / fan-out can run without standing up MySQL.
	// nil in production; the real path uses ba.db (which satisfies oboStore).
	// See modules/bot_api/obo_db.go for the interface contract.
	oboStoreOverride oboStore
	// oboFanoutDispatch lets unit tests intercept the per-grantee copy that
	// the fan-out listener would otherwise hand to ba.ctx.SendMessage. The
	// production path delegates to ba.dispatchMsgSendReq so the existing
	// dispatchOverride hook keeps capturing sends in handler tests.
	// nil in production.
	oboFanoutDispatch func(*config.MsgSendReq) error
	// oboFanoutBotEnqueue lets unit tests intercept the bot-event-queue
	// enqueue that fanoutForMessage performs after a successful dispatch.
	// Without this seam, fan-out tests would need a live Redis to assert
	// the synthetic event reaches /v1/bot/events. The production path
	// goes through ba.robotService.EnqueueBotEvent (see YUJ-1424 / PR#82
	// Jerry-Xin blocker for why direct enqueue is necessary at all).
	// nil in production → robotService path runs.
	oboFanoutBotEnqueue func(robotID string, message *config.MessageResp) error
	// oboChannelAccessOverride lets unit tests stub the grantor channel-
	// access check used by oboCreateScope (PR#82 review P0 — channel-wiretap
	// fix). Production path runs grantorCanReadChannel, which queries
	// group_member + userService.IsFriend; tests that build BotAPI without
	// a live DB session set this hook to deterministically accept or reject
	// (uid, channel_id, channel_type) without touching MySQL.
	// nil in production → the real DB-backed check runs.
	oboChannelAccessOverride func(uid, channelID string, channelType uint8) (bool, error)
	// oboGroupMemberOverride — PR#121 R8 (YUJ-1673). Test seam for
	// `userIsGroupMember`, the (uid, group_no) → bool lookup used by
	// Gate 4 (bot already in group) and by `grantorCanReadChannel`
	// for ChannelTypeGroup / ChannelTypeCommunityTopic. Production
	// path runs the `group_member` SELECT against MySQL; unit tests
	// that build BotAPI without a live DB session set this hook to
	// deterministically answer membership questions for the explicit-
	// scope fan-out paths that previously could not be exercised in
	// pure-Go tests (only the implicit-scope SQL-feeder filter was
	// reachable). nil in production → the real DB-backed check runs.
	oboGroupMemberOverride func(uid, groupNo string) (bool, error)
	// oboDisplayNameLookup — YUJ-1465 / Mininglamp-OSS/octo-server#108
	// (OBO v2). Test seam for resolving a uid → display name when the
	// fan-out path builds the synthetic `obo_system_hint` string. Returns
	// "" for unknown uids so the hint falls back to the bare uid. Empty
	// override → the production path queries the `user` table.
	oboDisplayNameLookup func(uid string) string
	// oboGroupNameLookup — YUJ-1465 / Mininglamp-OSS/octo-server#108
	// (OBO v2). Test seam for resolving a (group_no | thread channel id)
	// to a human group name. The fan-out path only consults this for
	// group / community-topic origin channels; DMs use the peer name
	// instead. Returns "" for unknown channels so the hint falls back
	// to the bare channel id. Empty override → the production path
	// queries the `group` table (with parent-group resolution for
	// community topics).
	oboGroupNameLookup func(channelID string, channelType uint8) string
	// typingCMDDispatch — YUJ-1465 / Mininglamp-OSS/octo-server#108.
	// Test seam for the typing handler's ctx.SendCMD call. Lets unit
	// tests capture the dispatched CMD (including the resolved
	// `from_uid`) without standing up a live WuKongIM. Production
	// path (nil override) goes through ba.ctx.SendCMD verbatim, so
	// behaviour outside of tests is unchanged.
	typingCMDDispatch func(req config.MsgCMDReq) error
	// friendCheckOverride lets unit tests stub userService.IsFriend for the
	// friend-gate decision in checkSendPermission / syncMessages, and for
	// the OBO friend-gate bypass (see obo_friend_gate.go). Production path
	// uses ba.userService.IsFriend; tests that build BotAPI without a live
	// user service set this hook to deterministically accept or reject
	// (uid, toUID) without touching MySQL. PR#82 R6 P0 — managed-persona
	// OBO friend-gate bypass needs to be testable end-to-end without the
	// full user-service stack.
	// nil in production → the real userService.IsFriend runs.
	friendCheckOverride func(uid, toUID string) (bool, error)
	// groupStatusQueryOverride lets focused permission tests inject the
	// group.status lookup without a live MySQL session. Production preserves the
	// raw lookup error so errors.Is(dbr.ErrNotFound) remains available upstream.
	groupStatusQueryOverride func(groupNo string) (int, error)
	// spaceMemberQueryOverride lets focused App Bot permission tests exercise a
	// failed Space membership lookup without a live MySQL session. The helper
	// returns the error without logging identifiers; the permission observer owns
	// the single desensitized terminal log.
	spaceMemberQueryOverride func(uid, spaceID string) (bool, error)
	// groupMemberCountOverride lets focused permission tests distinguish an
	// empty active membership from a query failure without a live MySQL session.
	groupMemberCountOverride func(groupNo, robotID string) (int, error)
	// sendPermissionMetrics records terminal permission-check failures with
	// bounded stage/reason labels. Struct-literal tests may leave it nil and use
	// the package default registered once at startup.
	sendPermissionMetrics *botSendPermissionMetrics
	// searchHandler 是共享的消息搜索 Handler（messages_search.Shared），bot 搜索路由
	// /v1/bot/messages/_search* 复用它（YUJ-49 / #B）。与 web、uk 入口共用同一实例，
	// 从而共享限流桶与 sender 缓存。principal 由本模块的 resolveSearchPrincipal 按
	// on_behalf_of 区分 as-bot / as-user(OBO)。
	searchHandler *messages_search.Handler
	// rateLimiters 持有三条 per-bot 限流通道（business / heartbeat / register，
	// issue #696）。为 nil 时所有限流中间件直接放行——单测里用 piecemeal 构造的
	// BotAPI 没有 ctx/Redis，不应因为限流而变红。
	rateLimiters *botRateLimiters
	log.Log
}

// botAPICardMutator is the narrow mutation surface used by raw and Registry
// edit modes. The interface keeps handler tests deterministic while production
// uses carddispatch.CardMutator for ownership, lifecycle, CAS, revision, and
// CMD synchronization.
type botAPICardMutator interface {
	Snapshot(context.Context, carddispatch.CardMutationTarget) (carddispatch.CardMutationSnapshot, error)
	Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error)
	WriteCAS(carddispatch.CardMutationCASRequest) (bool, error)
}

// dispatchMsgSendReq sends a built MsgSendReq to WuKongIM via ba.ctx, OR to a
// test-injected dispatchOverride for handler-level integration tests.
func (ba *BotAPI) dispatchMsgSendReq(req *config.MsgSendReq) (*config.MsgSendResp, error) {
	if ba.dispatchOverride != nil {
		return ba.dispatchOverride(req)
	}
	return ba.ctx.SendMessageWithResult(req)
}

// NewBotAPI creates the Bot API gateway module.
func NewBotAPI(ctx *config.Context) *BotAPI {
	speechURL := os.Getenv(voice_adapter.EnvSpeechServiceURL)
	speechKey := os.Getenv(voice_adapter.EnvSpeechAPIKey)
	timeoutSec := voice_adapter.DefaultTimeoutSec
	if v := os.Getenv(voice_adapter.EnvSpeechTimeout); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutSec = n
		}
	}
	maxCtxLen := 10000
	if v := os.Getenv("SPEECH_MAX_CONTEXT_LENGTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxCtxLen = n
		}
	} else if v := os.Getenv("VOICE_MAX_VOICE_CONTEXT_LENGTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxCtxLen = n
		}
	}
	maxBodySize := int64(5 << 20)
	if v := os.Getenv(voice_adapter.EnvSpeechMaxBodySize); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxBodySize = n
		}
	}
	maxFileSize := int64(3 << 20)
	if v := os.Getenv("SPEECH_MAX_FILE_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxFileSize = n
		}
	}

	var cardTemplates *botCardTemplateCatalog
	if runtimeCatalog := cardtmpl.DefaultCatalog(); runtimeCatalog != nil {
		var err error
		cardTemplates, err = newBotCardTemplateCatalogWithPolicy(runtimeCatalog, defaultBotTemplatePolicy())
		if err != nil {
			panic(fmt.Errorf("install Bot card template catalog: %w", err))
		}
	}

	robotService := robot.NewService(ctx)

	ba := &BotAPI{
		ctx:                   ctx,
		db:                    newBotAPIDB(ctx),
		userService:           user.NewService(ctx),
		fileService:           file.NewService(ctx),
		groupService:          group.NewService(ctx),
		userDB:                user.NewDB(ctx),
		threadService:         thread.NewService(ctx),
		robotService:          robotService,
		cardConfig:            robotService,
		cardRevisions:         cardrevision.NewStore(ctx.DB()),
		cardMutator:           carddispatch.NewCardMutator(ctx),
		cardTemplates:         cardTemplates,
		seqStore:              msgextraseq.New(ctx),
		speechClient:          voice_adapter.NewSpeechClient(speechURL, speechKey, time.Duration(timeoutSec)*time.Second),
		maxVoiceContextLength: maxCtxLen,
		maxBodySize:           maxBodySize,
		maxFileSize:           maxFileSize,
		sendPermissionMetrics: defaultBotSendPermissionMetrics,
		rateLimiters:          newBotRateLimiters(ctx.GetConfig()),
		searchHandler:         messages_search.Shared(ctx),
		Log:                   log.NewTLog("BotAPI"),
	}
	// Resolve the /v1/bot/events long-poll hold budget here rather than on the
	// first long-poll request, so an unusable OCTO_BOT_EVENTS_MAX_HOLDS is
	// reported in the startup log instead of surfacing in production traffic.
	ba.resolveEventHoldBudget()
	// YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone fan-out.
	// Subscribed AFTER the dependency wiring above so oboMessagesListen
	// can safely consult ba.db (oboStore). Idempotent: the listener
	// short-circuits when no grants exist for the message's channel.
	if ctx != nil {
		ctx.AddMessagesListener(ba.oboMessagesListen)
	}
	return ba
}

// Route registers all Bot API routes.
func (ba *BotAPI) Route(r *wkhttp.WKHttp) {
	// register endpoint (token needed but not via authBot group — handled inline).
	//
	// **两层限流,顺序载荷性**:先 per-IP,再 per-token。
	//
	// (1) per-token 指纹(issue #696):此处在 authBot 之前、还没有 bot 身份,而 token
	//     就在请求里。这一维度命中事故形态——token 失效的 bot 以约 4 rps 持续重试
	//     register,按 token 分桶能把这类风暴关进它自己的桶,不波及同 IP 的健康 bot。
	//
	// (2) per-IP strict(code review 补入):**单靠 (1) 有 keyspace 放大漏洞**。
	//     token 由客户端提供且在限流之前不做有效性校验,攻击者每次换一个随机 token
	//     即可换一个新桶——不仅绕过 (1),更糟的是令牌桶 Lua 在首次判定时就会
	//     HMSET+EXPIRE 建 key(bucket.go),于是 live key 数 = 请求速率 × TTL(约 40s)。
	//     只靠全局 per-IP 桶(1500 rps)兜底意味着最坏 6 万个 key,而生产 Redis
	//     **未设 maxmemory**,没有 LRU 淘汰、OOM 直接被 OS kill。
	//     这道 strict 桶把 key 生成速率从 1500/s 压到 100/s(40s 窗口内约 4000 个),
	//     且必须排在 (1) **之前**——否则 key 已经建好了,挡也来不及。
	//
	//     参数走 env、不热调:IP 层是防滥用底线,不需要按业务负载反复收敛。
	//     **但也不能按平峰量裁**:定这个值的时候 register 还在全局桶里,原上限是全局的
	//     1500 rps,而它同时是**自愈路径**——车队集体重连时压得太紧,就是在唯一要紧的
	//     时刻限流恢复能力。它现已一并移出全局桶,所以这 100 rps 是这条路径**唯一**的
	//     上限,没有外层兜底。定值理由见 ratelimit.go 的常量注释。
	registerIPRPS, registerIPBurst := ipLimitParams(
		envRegisterIPRPS, defaultRegisterIPRPS, envRegisterIPBurst, defaultRegisterIPBurst)
	registerIPLimit := r.StrictIPRateLimitMiddleware(
		context.Background(), sharedRateLimitRedis(ba.ctx.GetConfig()),
		"bot_register", registerIPRPS, registerIPBurst)
	// **用 r.Any 而非 r.POST**:全局限流的豁免只看路径不看方法（lib 的
	// `excludeSet[c.Request.URL.Path]`），若底线只挂在 POST 上，非 POST 请求会
	// 既跳过全局桶又碰不到底线（第二轮 code review P1-2）。onlyPOST 在过桶之后收口。
	// 注意 `r.Any` 只覆盖 gin 的九个方法，扩展方法（PROPFIND 等）仍绕过——
	// 残留范围与为何不在本仓关闭，见 ratelimit.go onlyPOST 的注释。
	r.Any("/v1/bot/register",
		registerIPLimit,
		onlyPOST,
		ba.rateLimitMiddleware(
			func(l *botRateLimiters) *ratelimit.Limiter { return l.register },
			func(c *wkhttp.Context) string { return botTokenFingerprint(extractBotToken(c)) },
			"bot",
		),
		ba.register)

	// heartbeat 刻意**不在** botAPI 组内：它必须有独立于业务流量的配额，
	// 否则一次 7-8 条消息的推理爆发就能顺带把心跳挤掉、进而断联（issue #696）。
	// 同时它已被移出全局 per-IP 桶（main.go globalRateLimitExcludePaths），
	// 否则同出网 IP 的邻居仍能把它饿死。
	//
	// **三层，顺序载荷性**（code review P1 修正）：
	//
	// (1) per-IP strict —— 必须排在 `authBot` **之前**。这是 exclude 的对价：
	//     移出全局桶也移走了它唯一的未鉴权层防护，而 (3) 挂在鉴权之后、无效 token
	//     在 `authBot` 里就 abort 了，永远走不到那道桶。缺这一层，攻击者可用任意
	//     无效 token 无限触发 `authBot` 的 Redis/DB 查询——原本被全局桶挡住的 DDoS
	//     面因为 exclude 反而被打开。这一层**没有 enabled 开关**，因为 (3) 默认关闭。
	// (2) authBot + requireBotIdentity —— 鉴权并断言 bot 身份已落。
	//     用 requireBotIdentity 而非 botActorUID:限流维度取自 "robot_id",不需要
	//     把 robotID 别名成 "uid"（见 ratelimit.go requireBotIdentity 的注释）。
	// (3) per-bot 桶 —— 防单个已鉴权 bot 滥用；可热调、可影子、默认关闭。
	heartbeatIPRPS, heartbeatIPBurst := ipLimitParams(
		envHeartbeatIPRPS, defaultHeartbeatIPRPS, envHeartbeatIPBurst, defaultHeartbeatIPBurst)
	heartbeatIPLimit := r.StrictIPRateLimitMiddleware(
		context.Background(), sharedRateLimitRedis(ba.ctx.GetConfig()),
		"bot_heartbeat", heartbeatIPRPS, heartbeatIPBurst)
	// **r.Any 而非 r.POST**:同 register，豁免只看路径不看方法,底线必须盖住 gin 路由
	// 得到的每个方法,否则 `GET /v1/bot/heartbeat` 既跳过全局桶又碰不到底线
	// （第二轮 code review P1-2）。扩展方法的残留见 ratelimit.go onlyPOST 注释。
	r.Any("/v1/bot/heartbeat",
		heartbeatIPLimit,
		onlyPOST,
		ba.authBot(),
		ba.requireBotIdentity(),
		ba.rateLimitMiddleware(
			func(l *botRateLimiters) *ratelimit.Limiter { return l.heartbeat },
			func(c *wkhttp.Context) string { return getRobotIDFromContext(c) },
			"bot",
		),
		ba.heartbeat)

	// Bot API endpoints (unified auth middleware)
	//
	// requireBotIdentity 只**断言** `authBot` 已写入 bot 身份（缺失即 fail-closed 中止），
	// 刻意**不**写 `c.Set("uid", robotID)`。per-bot 限流的维度取自 `getRobotIDFromContext`
	// （即 authBot 写的 "robot_id"），与 `uid` 无关——初版误用了 `botActorUID()`
	// 并在注释里声称"供限流取维度",两者都是错的:那个别名会让主组内任何调
	// `GetLoginUID()` 的 handler 静默拿到 robotID 而不是登录用户，是白担的授权混淆风险
	// （第二轮 code review P2-2）。
	//
	// 顺序是载荷性的：authBot → requireBotIdentity → 限流。限流中间件在拿不到身份时
	// 旁路放行（OutcomeNoKey），顺序颠倒会让它静默失效。
	botAPI := r.Group("/v1/bot",
		ba.authBot(),
		ba.requireBotIdentity(),
		ba.rateLimitMiddleware(
			func(l *botRateLimiters) *ratelimit.Limiter { return l.business },
			func(c *wkhttp.Context) string { return getRobotIDFromContext(c) },
			"bot",
		),
	)
	{
		botAPI.POST("/sendMessage", ba.sendMessage)
		botAPI.POST("/typing", ba.typing)
		botAPI.POST("/readReceipt", ba.readReceipt)
		botAPI.POST("/events", ba.getEvents)
		botAPI.POST("/events/:event_id/ack", ba.eventAck)
		botAPI.POST("/messages/sync", ba.syncMessages)
		botAPI.GET("/groups", ba.getGroups)
		botAPI.GET("/resolve/targets", ba.botResolveTargets)
		botAPI.GET("/groups/:group_no", ba.getGroupInfo)
		botAPI.GET("/groups/:group_no/members", ba.getGroupMembers)
		botAPI.GET("/groups/:group_no/mention_pref", ba.getMentionPref) // 群级免@偏好读（octo-server#237）
		botAPI.GET("/groups/:group_no/md", ba.getGroupMd)
		botAPI.PUT("/groups/:group_no/md", ba.updateGroupMd)
		botAPI.GET("/space/members", ba.botSpaceMembers)
		botAPI.POST("/createGroup", ba.botGroupCreate)
		botAPI.PUT("/groups/:group_no/info", ba.botGroupUpdate)
		botAPI.POST("/groups/:group_no/members/add", ba.botGroupMemberAdd)
		botAPI.POST("/groups/:group_no/members/remove", ba.botGroupMemberRemove)
		// Thread API
		botAPI.POST("/groups/:group_no/threads", ba.botCreateThread)
		botAPI.GET("/groups/:group_no/threads", ba.botListThreads)
		botAPI.GET("/groups/:group_no/threads/:short_id", ba.botGetThread)
		botAPI.DELETE("/groups/:group_no/threads/:short_id", ba.botDeleteThread)
		botAPI.GET("/groups/:group_no/threads/:short_id/members", ba.botListThreadMembers)
		botAPI.POST("/groups/:group_no/threads/:short_id/join", ba.botJoinThread)
		botAPI.POST("/groups/:group_no/threads/:short_id/leave", ba.botLeaveThread)
		botAPI.GET("/groups/:group_no/threads/:short_id/md", ba.botGetThreadMd)
		botAPI.PUT("/groups/:group_no/threads/:short_id/md", ba.botUpdateThreadMd)
		botAPI.POST("/setCommands", ba.setCommands)
		// File API
		botAPI.POST("/file/upload", ba.botUploadFile)
		botAPI.POST("/upload", ba.botUploadFile)
		botAPI.GET("/file/download/*path", ba.botFileDownload)
		botAPI.GET("/upload/credentials", ba.botUploadCredentials)
		botAPI.GET("/upload/presigned", ba.botUploadPresigned)
		botAPI.POST("/message/edit", ba.botMessageEdit)
		botAPI.POST("/message/card/revisions/clear", ba.botCardRevisionsClear) // D10.6 清除卡片修订(写墓碑)
		botAPI.GET("/card/profile", ba.botCardProfile)                         // D12 卡片能力清单(feature detection)
		botAPI.GET("/user/info", ba.getUserInfo)
		// Voice context API (User Bot only)
		botAPI.PUT("/voice/context", ba.botPutVoiceContext)
		botAPI.GET("/voice/context", ba.botGetVoiceContext)
		botAPI.DELETE("/voice/context", ba.botDeleteVoiceContext)
		botAPI.POST("/voice/transcribe", ba.botTranscribe)
		// OBO bot-token read (Mininglamp-OSS/octo-server#135 / YUJ-1762).
		// Adapter-facing endpoint that returns the active grant whose
		// grantee is the calling bot, including `persona_prompt`. Kept
		// inside the bot-token group (not /v1/obo/*) because the auth
		// posture is "the bot is asking about itself" — fundamentally
		// different from the user-token /v1/obo/* CRUD which mutates
		// grants on behalf of a logged-in grantor.
		botAPI.GET("/obo-grant", ba.oboBotGetGrant)
	}

	// 由 message 模块贡献的群内单条消息查询（见 pkg/authtree 的 why）。botActorUID 只挂在
	// 这些复用路由上、不挂整个 botAPI 组：组级别名会让组内任何调 GetLoginUID() 的 handler
	// 静默拿到 robotID，是白担的授权混淆风险；而这些 handler 的 actor 本就应当是 bot 自己。
	//
	// appBotDMSpaceGuard 补上读侧的 App Bot 租户门：复用的 getPersonMessage 只做
	// friend/blacklist 判定，不看 App Bot 的 scope=space 绑定，比发送侧宽（详见该函数注释）。
	authtree.Mount(authtree.TreeBotToken, r, authtree.MountOn(botAPI, ba.botActorUID(), ba.appBotDMSpaceGuard()))

	// Bot File API (separate group for wildcard conflict avoidance)
	botFileAPI := r.Group("/v1/botfile", ba.authBot())
	{
		botFileAPI.GET("/*path", ba.botProxyFile)
		botFileAPI.POST("/upload", ba.botUploadFile)
	}

	// YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone (OBO) REST.
	// User-token endpoints under /v1/obo. Implementation in obo_api.go;
	// the call is split out so this Route function doesn't grow further.
	ba.registerOBORoutes(r)

	// 群入站 Webhook 管理（bot token 面），与用户路由共用 incomingwebhook 实例与
	// 权限矩阵（管理员 bot 同权人类管理员）。实现在 incoming_webhook.go。
	ba.registerIncomingWebhookRoutes(r)

	// 消息搜索（YUJ-49 / #B）：/v1/bot/messages/_search*，authBot 鉴权，principal 由
	// on_behalf_of 区分 as-bot / as-user(OBO)。实现在 search_route.go。
	ba.mountSearchRoutes(r)
}

// ==================== Helper Functions ====================

// resolveSpaceChannelID handles Bot API channel_id resolution.
// DM(channel_type=1): WuKongIM uses bare uid without Space prefix.
// Group: returned as-is.
// resolveSpaceChannelID is a placeholder for future Space-aware channel resolution.
// Currently a no-op: WuKongIM handles DM routing without Space prefix in channel_id.
// The Space prefix (s{spaceID}_{uid}) is only needed for IM whitelist operations,
// which are handled in applyBot/createFriendRelation.
func (ba *BotAPI) resolveSpaceChannelID(robotID, channelID string, channelType uint8) string {
	return channelID
}

// resolveBotDisplayName queries the bot's display name, falls back to robotID.
func (ba *BotAPI) resolveBotDisplayName(robotID string) string {
	botUser, err := ba.userDB.QueryByUID(robotID)
	if err == nil && botUser != nil && botUser.Name != "" {
		return botUser.Name
	}
	return robotID
}

// clearTypingThrottle resets the typing throttle state (called after bot sends a message).
func (ba *BotAPI) clearTypingThrottle(robotID string, channelID string, channelType uint8) {
	if ba.ctx == nil {
		// Test contexts may not wire ctx; nothing to clear.
		return
	}
	typingStartKey := fmt.Sprintf("typing_start:%s:%s:%d", robotID, channelID, channelType)
	typingCountKey := fmt.Sprintf("typing_count:%s:%s:%d", robotID, channelID, channelType)
	ba.ctx.GetRedisConn().Del(typingStartKey)
	ba.ctx.GetRedisConn().Del(typingCountKey)
}
