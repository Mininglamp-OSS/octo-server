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
// 注入实现依赖 gin 的 query 缓存尚未建立：`c.Query` 首次调用才会解析并缓存
// URL.RawQuery，此中间件之前的链路（authUserAPIKey 只读 Authorization 头、
// SharedUIDRateLimiter 只读 context 里的 uid）都不读 query，故改写 RawQuery 后
// handler 读到的是注入后的值。TestUserKeySpaceRuleInjectsBoundSpace 端到端锁住这一点，
// 若日后有中间件前移到此处之前并读了 query，该测试会立刻失败。
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

		if requested := c.Query(spaceIDField); requested != "" {
			if requested != bound {
				bf.rejectSpaceMismatch(c)
				return
			}
			c.Next()
			return
		}

		q := c.Request.URL.Query()
		q.Set(spaceIDField, bound)
		c.Request.URL.RawQuery = q.Encode()
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
