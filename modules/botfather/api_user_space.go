package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
)

// spaceIDField 是路径参数与 query 参数里 Space 的字段名，两处同名。
const spaceIDField = "space_id"

// enforceKeySpace 把复用到 uk 树的 human handler 钉在 API Key 冻结的那个 Space 上。
//
// 一个 `uk_*` 的租户在签发时就已验证并写入 api_key_space_id，请求不得把它放宽或改向：
//
//	请求未带 space_id  → 注入绑定值，使按 space_id 分支的 handler（user.search）留在租户内
//	space_id 等于绑定值 → 放行
//	space_id 不等于绑定值 → 403，直接拒绝，不做降级
//
// 无绑定 Space 的 key（历史行的 space_id 为空串）没有可执行的租户，原样放行——这与
// /v1/user/bots* 既有 handler 在绑定为空时跳过 Space 校验的口径一致，不新增放宽面。
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
		bound := authtree.BoundSpaceID(c)
		if bound == "" {
			c.Next()
			return
		}

		// 路径参数优先：/space/:space_id/members 这类路由的 Space 总是显式给出的。
		if requested := c.Param(spaceIDField); requested != "" {
			if requested != bound {
				bf.rejectSpaceMismatch(c)
				return
			}
			c.Next()
			return
		}

		query := c.Request.URL.Query()
		if requested := query.Get(spaceIDField); requested != "" {
			if requested != bound {
				bf.rejectSpaceMismatch(c)
				return
			}
			c.Next()
			return
		}

		query.Set(spaceIDField, bound)
		c.Request.URL.RawQuery = query.Encode()
		c.Next()
	}
}

// rejectSpaceMismatch 对跨 Space 请求统一回 403。不回显请求值与绑定值：调用方只需要
// 知道「这个 key 不能操作那个 Space」，回显会把绑定租户泄露给拿到 key 但不知其作用域的
// 一方。具体差异只进日志。
func (bf *BotFather) rejectSpaceMismatch(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
	c.Abort()
}
