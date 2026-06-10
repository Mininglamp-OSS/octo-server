package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// registerIncomingWebhookRoutes 把群入站 Webhook 的管理端点（create/list/update/
// delete/regenerate/deliveries/test）挂到 bot token 鉴权面：
//
//	/v1/bot/groups/:group_no/incoming-webhooks[...]
//
// 处理器与用户路由（/v1/groups/:group_no/incoming-webhooks）共用同一套实现
// （incomingwebhook.NewManagementFacade，一套 Service、两个门）。权限矩阵一致：
// bot 是群管理员（group_member.role）则与人类管理员同权；普通成员 bot 只能创建并
// 管理自己（robot_id 即 creator_uid）创建的 webhook。bot 面写操作对 push 缓存的
// 失效以 TTL 兜底（见 NewManagementFacade 的契约注释）。
//
// 中间件链：authBot（bf_/app_ token → robot_id）→ botActorUID（把 robot_id 写入
// 共享处理器读取的 "uid" 键）→ SharedUIDRateLimiter（须在 uid 写入之后，否则读不到
// uid 静默 fail-open；bot 与登录用户共用 per-uid 桶语义，robot_id 即桶 key）。
func (ba *BotAPI) registerIncomingWebhookRoutes(r *wkhttp.WKHttp) {
	iwh := incomingwebhook.NewManagementFacade(ba.ctx)
	g := r.Group("/v1/bot/groups/:group_no/incoming-webhooks",
		ba.authBot(), ba.botActorUID(), appwkhttp.SharedUIDRateLimiter(r, ba.ctx))
	iwh.MountManagementRoutes(g)
}

// botActorUID 把 authBot 解析出的 robot_id 适配到 "uid" 上下文键（incomingwebhook
// 共享处理器的操作者身份来源，与用户路由的 AuthMiddleware 同键）。robot_id 缺失说明
// 中间件链装配有误，按内部断言失败处理（500），并 Abort 阻断后续 handler。
func (ba *BotAPI) botActorUID() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		robotID := getRobotIDFromContext(c)
		if robotID == "" {
			ba.respondBotAPIIdentityMissing(c)
			c.Abort()
			return
		}
		c.Set("uid", robotID)
		c.Next()
	}
}
