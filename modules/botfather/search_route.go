package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// 消息搜索的 uk 入口（YUJ-49 / #B，决策十正式接线）。
//
// 把 messages_search 的全部 _search* 端点挂到 /v1/user/messages，以 authUserAPIKey()
// 鉴权，principal=uk：subjectUID = keyModel.UID（直接真人身份）、spaceID =
// api_key_space_id、限流/审计主体 = key UID。uk 无 bot、无 OBO scope 收窄，可读谓词走
// 真人可达集（复用 messages_search 现有真人分支 checkChannelAccess / buildAllowlist）。
//
// 中间件链对齐 setupUserAPIRoutes（authUserAPIKey + SharedUIDRateLimiter）再接搜索专属
// 链，无 SpaceMiddleware（spaceID 走 principal 的 api_key_space_id）：
//
//	authUserAPIKey → SharedUIDRateLimiter → resolveUKPrincipal → searchRateLimiter → audit → backendGate

// mountSearchRoutes 挂载 uk 搜索子树。由 Route() 调用。
func (bf *BotFather) mountSearchRoutes(r *wkhttp.WKHttp) {
	bf.searchHandler.MountSubtree(r, "/v1/user/messages",
		bf.authUserAPIKey(),
		appwkhttp.SharedUIDRateLimiter(r, bf.ctx),
		bf.resolveUKPrincipal,
	)
}

// resolveUKPrincipal 是 uk 搜索的 principal 解析中间件（authUserAPIKey 之后、
// searchRateLimiter 之前运行）。authUserAPIKey 已把 api_key_uid / api_key_space_id 落
// context，这里据此组装 uk 主体并写入，供后续限流/审计/handler 统一读取。
func (bf *BotFather) resolveUKPrincipal(c *wkhttp.Context) {
	p, err := messages_search.AuthenticateUK(c)
	if err != nil {
		// api_key_uid 缺失——authUserAPIKey 之后不应发生，fail-closed。
		respondBotfatherAuthFailed(c)
		return
	}
	messages_search.SetPrincipal(c, p)
	c.Next()
}
