package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
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
//
// resolveUKPrincipal 内联补齐人类路由 SpaceMiddleware 的等价成员资格门（YUJ-58）：
// authUserAPIKey 只校验 key status / integration-client enablement / user active，不重查
// key 主人是否仍是 api_key_space_id 冻结的 Space 成员。缺此门时，一个在成员期签发、之后
// 主人被移出该 Space 的存量 key 仍被无限期信任，可经 global DM 枚举检索该 Space 当前成员的
// DM 历史（buildAllowlist→enumerateDMPeers 只信任 allowlist、不重校验调用者本人的 space
// 成员资格）。人类 web 路由此时会被 SpaceMiddleware 的成员资格检查拒绝。

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
//
// 成员资格校验委托给 spacepkg.CheckMembership（与人类路由 SpaceMiddleware 同款语义：
// join space.status=1 + space_member.status=1），checker 经 resolveUKPrincipalWithChecker
// 注入以便单测无需 DB。
func (bf *BotFather) resolveUKPrincipal(c *wkhttp.Context) {
	bf.resolveUKPrincipalWithChecker(c, func(spaceID, uid string) (bool, error) {
		return spacepkg.CheckMembership(bf.ctx.DB(), spaceID, uid)
	})
}

// resolveUKPrincipalWithChecker 是 resolveUKPrincipal 的可注入实现：checkMembership 抽出
// 便于单测替身。校验落在 uk 主体鉴权路径（YUJ-58 要求三：归一化，勿在 enumerateDMPeers
// 内联特判绕过谓词一致性）。
func (bf *BotFather) resolveUKPrincipalWithChecker(c *wkhttp.Context, checkMembership spacepkg.MembershipChecker) {
	p, err := messages_search.AuthenticateUK(c)
	if err != nil {
		// api_key_uid 缺失——authUserAPIKey 之后不应发生，fail-closed。
		respondBotfatherAuthFailed(c)
		return
	}

	// 仅当 key 绑定了 Space（spaceID 非空）时校验成员资格：无 Space 的 uk 无成员资格可查，
	// 与人类 SpaceMiddleware「无 space_id 则跳过」对齐；其空 Space 由下游 RequiresSpaceScope
	// 的必填门 fail-close，不在此拦截。
	if spaceID := p.SpaceID(); spaceID != "" {
		member, mErr := checkMembership(spaceID, p.SubjectUID())
		if mErr != nil {
			bf.Error("uk 搜索校验 Space 成员身份失败", zap.String("space_id", spaceID), zap.Error(mErr))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}
		if !member {
			// 与人类 SpaceMiddleware 同款 fail-closed（403）：key 主人已被移出 / 禁用该 Space，
			// 或该 Space 已停用。存量 active key 不再被无限期信任。
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
			c.Abort()
			return
		}
	}

	messages_search.SetPrincipal(c, p)
	c.Next()
}
