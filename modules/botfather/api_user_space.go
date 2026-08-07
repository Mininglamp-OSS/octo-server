package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// spaceIDField 是路径参数与 query 参数里 Space 的字段名，两处同名。
const spaceIDField = "space_id"

// enforceKeySpace 把复用到 uk 树的 human handler 钉在 API Key 冻结的那个 Space 上。
//
// 一个 `uk_*` 的租户在签发时就已验证并写入 api_key_space_id，请求不得把它放宽或改向：
//
//	space_id 不等于绑定值   → 403，直接拒绝，不做降级
//	key 主人已不在绑定 Space → 403，fail-closed
//	key 没有绑定 Space      → 403，fail-closed（见下）
//	其余情况               → 把 query 的 space_id 钉成绑定值后放行
//
// 🔴 无绑定 Space 的 key（历史行 space_id 为空串）在这棵子树上一律拒绝。这里没有可执行
// 的租户，放行等于让调用方自己用 query 的 space_id 选租户——对一条以「租户封闭」为卖点
// 的凭据来说是 fail-open 的租户选择：user.search 会落到「按调用方给定 space_id 过滤」
// 分支（该分支只校验目标用户属于该 Space，从不校验调用者），DM 单条读会因空 Space 跳过
// 整个 Space 过滤。这不是向后兼容问题——本中间件只前置在 authtree 贡献的上游能力路由上
// （见 setupUserAPIRoutes 的挂载点），那些路由在本 PR 之前并不存在，无绑定 key 从未能
// 访问它们，收紧不构成回归；既有 /v1/user/bots* 区块不经过本中间件，不受影响。
//
// 成员资格必须在这里复查（YUJ-58 同款结论，与 resolveUKPrincipal 一致）。authUserAPIKey
// 只校验 key status / integration-client enablement / user active，不问主人现在还在不在
// api_key_space_id 冻结的那个 Space。缺这道门时：一个在成员期签发、之后主人被移出该
// Space 的存量 key 仍被无限期信任，而下面的注入会让 user.search 永远走「按 space_id 过滤」
// 分支——该分支只校验**目标**用户是否属于该 Space，从不校验调用者，于是 HaveCommonSpace
// 那道调用者侧的门永远不执行，已失效的成员仍能探测该 Space 现任成员的存在性与资料。
// 刻意不走 SpaceMiddleware 的 Redis 成员缓存：撤销必须立即生效，60s 正向 TTL 会把上面
// 那个窗口重新开出来。
//
// 🔴 注入必须绕开 `c.Query`。gin 在 `c.Query` 首次调用时把 URL.RawQuery 解析进
// Context 私有的 queryCache，此后改写 RawQuery 对 handler 完全不可见——本中间件若用
// `c.Query` 读一次 space_id，就等于亲手把旧值缓存下来，handler 拿到的仍是「没有
// space_id」，user.search 于是落回「任意共同 Space」分支，跨 Space 泄漏。queryCache 是
// 私有字段、无法从外部重置，所以读和写都只能走 `c.Request.URL.Query()`，让 handler 的
// 第一次 `c.Query` 去解析注入后的 RawQuery。
//
// 残留约束：本中间件之前的链路不得调用 gin 的 query 访问器（现状不会——
// authUserAPIKey 只读 Authorization 头，SharedUIDRateLimiter 只读 context 里的 uid）。
// TestUserKeySpaceRuleInjectsBoundSpace 端到端锁住这一点：一旦有中间件前移并读了
// query，该用例立刻失败。
func (bf *BotFather) enforceKeySpace() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		bf.enforceKeySpaceWithChecker(c, func(spaceID, uid string) (bool, error) {
			return spacepkg.CheckMembership(bf.ctx.DB(), spaceID, uid)
		})
	}
}

// enforceKeySpaceWithChecker 是 enforceKeySpace 的可注入实现：checkMembership 抽出便于
// 单测替身（成例见 resolveUKPrincipalWithChecker）。
func (bf *BotFather) enforceKeySpaceWithChecker(c *wkhttp.Context, checkMembership spacepkg.MembershipChecker) {
	bound := authtree.BoundSpaceID(c)
	if bound == "" {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
		c.Abort()
		return
	}

	// 越界判定先做：不匹配无需查库即可拒，免得把 403 路径变成 DB 放大器。
	// 路径参数优先——/space/:space_id/members 这类路由的 Space 总是显式给出的。
	query := c.Request.URL.Query()
	requested := c.Param(spaceIDField)
	if requested == "" {
		requested = query.Get(spaceIDField)
	}
	if requested != "" && requested != bound {
		bf.rejectSpaceMismatch(c)
		return
	}

	// uid 缺失说明中间件链装配有误（authUserAPIKey 必先落 api_key_uid）；CheckMembership
	// 对空 uid 返回 false，于是走下面的 403，方向是 fail-closed。
	member, err := checkMembership(bound, getAPIKeyUID(c))
	if err != nil {
		bf.Error("校验 uk 绑定 Space 成员身份失败", zap.String("space_id", bound), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		c.Abort()
		return
	}
	if !member {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
		c.Abort()
		return
	}

	// 单一来源：无论 space_id 原本缺失、来自 query、还是来自路径参数，都把 query 钉成
	// bound。路径命中时也覆盖，是为了让 handler 不可能同时看到两个不一致的来源——今天
	// 没有 handler 既读 :space_id 又读 query space_id，但一旦有，读哪个都必须得到同一
	// 个已校验的值。
	query.Set(spaceIDField, bound)
	c.Request.URL.RawQuery = query.Encode()
	c.Next()
}

// rejectSpaceMismatch 对跨 Space 请求统一回 403。不回显请求值与绑定值：调用方只需要
// 知道「这个 key 不能操作那个 Space」，回显会把绑定租户泄露给拿到 key 但不知其作用域的
// 一方。
func (bf *BotFather) rejectSpaceMismatch(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
	c.Abort()
}
