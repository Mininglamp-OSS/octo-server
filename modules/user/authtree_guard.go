package user

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// groupNoField 是 u.get 读的第二个调用方可控 query 参数名。
const groupNoField = "group_no"

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
//
// 折掉 vst_ 还顺带挡住了一个副作用：u.get 的访客分支会改写 c.Request.URL.Path 再
// HandleContext 重入路由，那是一个路径重写原语，不该出现在自动化凭据的可达面上。它今天
// 不可达**正是因为**访客没有 space_member 行而被这里折成 not_found；将来若放宽豁免名单，
// 必须单独把访客分支挡住，别让它顺势变成可达。
//
// 🔴 u.get 读两个调用方可控的输入，两个都要处置，不能只钉 :uid。group_no（query）会让
// handler 额外返回该群下 target 的 GroupMember 块，以及 is_external / source_space_id /
// source_space_name / home_space_*——**另一个 Space 的 ID 与可读名字**——外加 vercode
// （friend-add 流程里当 invite_vercode 用，是能力不是标识符）；而 GetGroupMember 与
// IsShowShortNo 都不校验调用者是否在那个群。于是一把绑定 A 的 key 加个
// ?group_no=<B 的群> 就能取走 B 空间的元数据。uk 树上没有任何能力宣称需要 group-scoped
// 用户详情，所以直接把它从 query 里删掉：fail-closed by construction，比校验它更难写错，
// 且 human 路由不受影响。
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

	// group_no 是这条路由的第二个租户锚点，且无人校验它（见头注释）。删而不校验：
	// 读写都必须走 c.Request.URL.Query()，不能用 c.Query —— 那一次调用会把 RawQuery
	// 解析进 gin 私有的 queryCache，此后改写 RawQuery 对 handler 完全不可见，删除就成了
	// 无效操作（同一个陷阱见 enforceKeySpace 的注释）。
	if query := c.Request.URL.Query(); query.Get(groupNoField) != "" {
		query.Del(groupNoField)
		c.Request.URL.RawQuery = query.Encode()
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
