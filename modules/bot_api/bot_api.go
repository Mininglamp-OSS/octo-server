package bot_api

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/modules/voice"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
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
	voiceDB       *voice.VoiceDB
	voiceSvc      *voice.VoiceService
	voiceCfg      *voice.VoiceConfig
	// spaceQuerier overrides ba.db for resolveBotActiveSpaceID (test injection).
	// nil in production; tests set it to stub the DB call deterministically.
	spaceQuerier botSpaceQuerier
	// dispatchOverride lets tests intercept the IM dispatch call to capture the
	// final MsgSendReq (including server-authoritative payload.space_id).
	// nil in production; the real path goes through ba.ctx.SendMessageWithResult.
	dispatchOverride func(*config.MsgSendReq) (*config.MsgSendResp, error)
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
	voiceCfg := voice.NewVoiceConfigFromEnv()
	return &BotAPI{
		ctx:           ctx,
		db:            newBotAPIDB(ctx),
		userService:   user.NewService(ctx),
		fileService:   file.NewService(ctx),
		groupService:  group.NewService(ctx),
		userDB:        user.NewDB(ctx),
		threadService: thread.NewService(ctx),
		voiceDB:       voice.NewVoiceDB(ctx),
		voiceSvc:      voice.NewVoiceService(voiceCfg),
		voiceCfg:      voiceCfg,
		Log:           log.NewTLog("BotAPI"),
	}
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


