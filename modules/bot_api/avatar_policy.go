package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// Every avatar route is explicitly classified. authBot runs this for all Bot
// trees, including contributed message, file, webhook and search routes. A new
// route is denied until assigned a capability; User/App Bot behavior is intact.
var avatarRoutes = map[string]botpolicy.Capability{
	"POST /v1/bot/heartbeat":                                              botpolicy.Runtime,
	"POST /v1/bot/sendMessage":                                            botpolicy.Message,
	"POST /v1/bot/typing":                                                 botpolicy.Message,
	"POST /v1/bot/readReceipt":                                            botpolicy.Message,
	"POST /v1/bot/messages/sync":                                          botpolicy.Message,
	"POST /v1/bot/events":                                                 botpolicy.Runtime,
	"POST /v1/bot/events/:event_id/ack":                                   botpolicy.Runtime,
	"GET /v1/bot/groups":                                                  botpolicy.ReadGroup,
	"GET /v1/bot/groups/:group_no":                                        botpolicy.ReadGroup,
	"GET /v1/bot/groups/:group_no/members":                                botpolicy.ReadGroup,
	"GET /v1/bot/groups/:group_no/mention_pref":                           botpolicy.ReadGroup,
	"GET /v1/bot/groups/:group_no/md":                                     botpolicy.ReadGroup,
	"GET /v1/bot/groups/:group_no/messages/:message_id":                   botpolicy.Message,
	"GET /v1/bot/messages/person/:peer_uid/:message_id":                   botpolicy.Message,
	"GET /v1/bot/groups/:group_no/threads":                                botpolicy.ReadThread,
	"GET /v1/bot/groups/:group_no/threads/:short_id":                      botpolicy.ReadThread,
	"GET /v1/bot/groups/:group_no/threads/:short_id/members":              botpolicy.ReadThread,
	"GET /v1/bot/groups/:group_no/threads/:short_id/md":                   botpolicy.ReadThread,
	"GET /v1/bot/groups/:group_no/threads/:short_id/messages/:message_id": botpolicy.Message,
	"POST /v1/bot/setCommands":                                            botpolicy.Commands,
	"POST /v1/bot/file/upload":                                            botpolicy.File,
	"POST /v1/bot/upload":                                                 botpolicy.File,
	"GET /v1/bot/file/download/*path":                                     botpolicy.File,
	"GET /v1/bot/upload/credentials":                                      botpolicy.File,
	"GET /v1/bot/upload/presigned":                                        botpolicy.File,
	"GET /v1/botfile/*path":                                               botpolicy.File,
	"POST /v1/botfile/upload":                                             botpolicy.File,
	"POST /v1/bot/message/edit":                                           botpolicy.Card,
	"POST /v1/bot/message/card/revisions/clear":                           botpolicy.Card,
	"GET /v1/bot/card/profile":                                            botpolicy.Card,
}

func avatarRouteAllowed(method, route string) bool {
	capability, exists := avatarRoutes[method+" "+route]
	return exists && botpolicy.Allows(botpolicy.Avatar, capability)
}

func (ba *BotAPI) authorizeAvatarRoute(c *wkhttp.Context) bool {
	if !avatarRouteAllowed(c.Request.Method, c.FullPath()) {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAvatarUnsupported, nil, nil)
		c.Abort()
		return false
	}
	channelID, channelType := c.Param("group_no"), uint8(2)
	if shortID := c.Param("short_id"); shortID != "" {
		channelID += "____" + shortID
		channelType = 5
	}
	if peer := c.Param("peer_uid"); peer != "" {
		channelID = peer
		channelType = 1
	}
	if channelID == "" {
		return true
	}
	allowed, err := botpolicy.CanAccessChannel(ba.ctx.DB(), getRobotIDFromContext(c), channelID, channelType, c.GetHeader("X-Space-ID"))
	if err != nil {
		ba.Error("avatar route resource authorization failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		c.Abort()
		return false
	}
	if !allowed {
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		c.Abort()
		return false
	}
	return true
}
