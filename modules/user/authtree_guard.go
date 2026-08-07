package user

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// requireBoundSpaceMember 把 /users/:uid 钉在 API Key 绑定的那个 Space 内。
//
// 为什么 handler 侧的 space_id 注入救不了这条路由：u.get 的入参只有 :uid 与登录
// UID，GetUserDetail 走的是全局用户表，从不读 pkg/space 的 GetSpaceID。也就是说 uk 树
// 上游的 enforceKeySpace 把 space_id 钉成绑定值之后，这个 handler 根本不消费它——注入
// 是个 no-op，A 空间的 key 能把任意已知 UID 解析成完整资料（short_no、在线与设备状态、
// bot 元数据、实名状态）。租户约束只能在路由自己这一层做，故有本 guard
// （authtree.ScopeRouteGuard）。
//
// 判定与豁免：
//
//	target == 调用者本人 → 放行（自身资料与租户无关）
//	SystemBot(target)   → 放行（botfather / u_10000 / fileHelper 全 Space 可见，
//	                      口径同 space_filter 的 SystemBots 分支）
//	target 是绑定 Space 的活跃成员 → 放行
//	其余（含跨 Space、访客 vst_、incoming webhook iwh_）→ 归并成 not_found
//
// 一律归并成 not_found 而不是 403：403 会把「这个 UID 存在但不在你的 Space」回显出去，
// 等于给一条 UID 枚举信道，与 u.get 自身对不存在用户的返回码保持同码。
//
// 刻意不走 SpaceMiddleware 的 Redis 成员缓存，理由同 enforceKeySpace：撤销必须立即
// 生效，60s 正向 TTL 会重新开出「已被移出 Space 仍可读成员资料」的窗口。
func (u *User) requireBoundSpaceMember() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		u.requireBoundSpaceMemberWithChecker(c, func(spaceID, uid string) (bool, error) {
			return spacepkg.CheckMembership(u.ctx.DB(), spaceID, uid)
		})
	}
}

// requireBoundSpaceMemberWithChecker 是 requireBoundSpaceMember 的可注入实现，
// checkMembership 抽出便于单测替身（成例见 botfather.enforceKeySpaceWithChecker）。
func (u *User) requireBoundSpaceMemberWithChecker(c *wkhttp.Context, checkMembership spacepkg.MembershipChecker) {
	// 绑定为空说明请求没经过 uk 树的 enforceKeySpace（那里已对无绑定 key fail-closed），
	// 或者装配有误。这条路由的租户约束完全依赖绑定值，拿不到就没有可执行的约束，故
	// fail-closed，而不是信任上游一定拦过。
	bound := authtree.BoundSpaceID(c)
	if bound == "" {
		respondUserError(c, errcode.ErrUserNotFound)
		c.Abort()
		return
	}

	target := c.Param("uid")
	if target == "" {
		respondUserError(c, errcode.ErrUserNotFound)
		c.Abort()
		return
	}
	if target == c.GetLoginUID() || spacepkg.IsSystemBot(target) {
		c.Next()
		return
	}

	member, err := checkMembership(bound, target)
	if err != nil {
		u.Error("校验目标用户的绑定 Space 成员身份失败", zap.String("space_id", bound), zap.Error(err))
		respondUserErrorWithStatus(c, errcode.ErrUserQueryFailed)
		c.Abort()
		return
	}
	if !member {
		respondUserError(c, errcode.ErrUserNotFound)
		c.Abort()
		return
	}
	c.Next()
}
