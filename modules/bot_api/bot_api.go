package bot_api

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
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
	ctx                   *config.Context
	db                    *botAPIDB
	userService           user.IService
	fileService           file.IService
	groupService          group.IService
	userDB                *user.DB
	threadService         thread.IService
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
	robotService          robot.IService
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
	log.Log
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

	ba := &BotAPI{
		ctx:                   ctx,
		db:                    newBotAPIDB(ctx),
		userService:           user.NewService(ctx),
		fileService:           file.NewService(ctx),
		groupService:          group.NewService(ctx),
		userDB:                user.NewDB(ctx),
		threadService:         thread.NewService(ctx),
		robotService:          robot.NewService(ctx),
		speechClient:          voice_adapter.NewSpeechClient(speechURL, speechKey, time.Duration(timeoutSec)*time.Second),
		maxVoiceContextLength: maxCtxLen,
		maxBodySize:           maxBodySize,
		maxFileSize:           maxFileSize,
		Log:                   log.NewTLog("BotAPI"),
	}
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
	// register endpoint (token needed but not via authBot group — handled inline)
	r.POST("/v1/bot/register", ba.register)

	// Bot API endpoints (unified auth middleware)
	botAPI := r.Group("/v1/bot", ba.authBot())
	{
		botAPI.POST("/sendMessage", ba.sendMessage)
		botAPI.POST("/typing", ba.typing)
		botAPI.POST("/readReceipt", ba.readReceipt)
		botAPI.POST("/events", ba.getEvents)
		botAPI.POST("/events/:event_id/ack", ba.eventAck)
		botAPI.POST("/heartbeat", ba.heartbeat)
		botAPI.POST("/messages/sync", ba.syncMessages)
		botAPI.GET("/groups", ba.getGroups)
		botAPI.GET("/groups/:group_no", ba.getGroupInfo)
		botAPI.GET("/groups/:group_no/members", ba.getGroupMembers)
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
		botAPI.GET("/user/info", ba.getUserInfo)
		// Voice context API (User Bot only)
		botAPI.PUT("/voice/context", ba.botPutVoiceContext)
		botAPI.GET("/voice/context", ba.botGetVoiceContext)
		botAPI.DELETE("/voice/context", ba.botDeleteVoiceContext)
		botAPI.POST("/voice/transcribe", ba.botTranscribe)
	}

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
