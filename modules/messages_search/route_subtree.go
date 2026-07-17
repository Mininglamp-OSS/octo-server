package messages_search

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// 本文件是 YUJ-49（#B）的入口接线层：把同一套 _search* handler 暴露给 bot / uk 两条
// 新路由树，并把 principal 解析（决策十）下沉到路由入口。设计要点（决策六/七/十）：
//
//   - 一套 handler、一个 Handler 实例（Shared 单例）承载 web / bot / uk 三条路由树，
//     共享同一份限流桶与 sender 缓存；三者仅入口鉴权 + principal 解析不同。
//   - bot_api / botfather 各自提供「鉴权 + principal 解析」中间件，MountSubtree 追加
//     搜索专属链（searchRateLimiter → auditMiddleware → backendGate）并挂载全部端点，
//     与 /v1/messages（web）的端点集合完全一致（同一 routeMounters）。
//   - SpaceMiddleware 刻意不进 bot/uk 链：bot 无 space_member（spaceID 恒空），uk 的
//     spaceID 走 api_key_space_id（principal 读取），故两条链都不挂 Space 门
//     （对齐 space_inject.go:148「/v1/bot 不挂 SpaceMiddleware」）。
//
// principal 的三态区分下沉在此：命中哪条路由 + on_behalf_of 是否存在 →
//   - bot 路由 + 无 on_behalf_of = as-bot（user_bot）；
//   - bot 路由 + on_behalf_of   = as-user(OBO)（grantor）；
//   - uk 路由                    = as-user 直接身份（uk）。
// 具体解析由本文件导出的构造器完成，接线由 bot_api / botfather 的入口中间件驱动。

var (
	sharedOnce sync.Once
	sharedH    *Handler
)

// Shared 返回进程内唯一的搜索 Handler。web（本模块 SetupAPI）、bot（bot_api）、
// uk（botfather）三条路由树共用它，从而共享限流桶 / sender 缓存 / 后端模式解析——
// 单个 bot 的搜索配额是一致的一份，而非按入口分裂。首个调用方（模块装载顺序决定）
// 用其 *config.Context 构造；后续调用返回同一实例（各模块拿到的 ctx 是同一个）。
func Shared(ctx *config.Context) *Handler {
	sharedOnce.Do(func() {
		sharedH = New(ctx)
	})
	return sharedH
}

// MountSubtree 在 r 上以 prefix 为前缀挂载全部 _search* 端点，前置 front（调用方的
// 鉴权 + principal 解析中间件），再接搜索专属链 searchRateLimiter → auditMiddleware →
// backendGate，最后经 routeMounters 注册与 /v1/messages 完全一致的端点集合。
//
// front 约定：必须在放行前对成功请求调用 SetPrincipal（bot=user_bot/obo、uk=uk），
// 因为 searchRateLimiter（取限流键）与 auditMiddleware（取审计主体）都依赖 principal。
// front 内部若鉴权/授权失败，应自行响应并 Abort，链在此中止。
//
// backendGate 位于链尾（鉴权 + 限流 + 审计之后），与 web 侧一致：即便后端为
// disabled/zinc，拒绝也照样计量与审计，杜绝无度量的「搜索关闭」枚举旁路（V9）。
func (h *Handler) MountSubtree(r *wkhttp.WKHttp, prefix string, front ...wkhttp.HandlerFunc) {
	chain := make([]wkhttp.HandlerFunc, 0, len(front)+3)
	chain = append(chain, front...)
	chain = append(chain, h.searchRateLimiter(), h.auditMiddleware(), h.backendGate())
	g := r.Group(prefix, chain...)
	for _, mount := range routeMounters {
		mount(h, g)
	}
}

// ---- principal 解析：供 bot_api / botfather 的入口中间件调用（决策十）----

// SetPrincipal 把已解析的搜索主体写入 context，handler 经 h.principal(c) 读取。
// bot / uk / obo 路由显式调用；web 真人路由不调用，靠 Handler.principal 惰性默认。
func SetPrincipal(c *wkhttp.Context, p Principal) { setPrincipal(c, p) }

// AuthenticateUserBot 解析 as-bot 主体：subjectUID = botUID（要求上游 authBot() 已在
// context 落 robot_id）。App Bot 返回 ErrAppBotSearchDenied（一期不支持，决策五，
// 调用方须显式拒绝、不得静默放行）。
func AuthenticateUserBot(c *wkhttp.Context) (Principal, error) { return authenticateUserBot(c) }

// AuthenticateUK 解析 uk 主体：subjectUID = keyModel.UID、spaceID = api_key_space_id
//（要求上游 authUserAPIKey() 已落 api_key_uid / api_key_space_id）。
func AuthenticateUK(c *wkhttp.Context) (Principal, error) { return authenticateUK(c) }

// NewOBOPrincipal 组装 as-user(OBO) 主体：subjectUID = grantorUID（走真人分支）、
// 限流/审计按 botUID、审计并记 grantorUID。grant + scope + grantorCanReadChannel 的
// 实时权限校验（TOCTOU 与发消息侧一致）由调用方在此之前完成（#F 复用
// bot_api/obo_check.go）；本构造器只从两个已校验入参组装载体。
func NewOBOPrincipal(botUID, grantorUID, spaceID string) (Principal, error) {
	return authenticateOBO(botUID, grantorUID, spaceID)
}

// 导出鉴权错误哨兵，供入口中间件按类型映射响应（App Bot 拒绝 vs 一般未鉴权）。
var (
	ErrPrincipalUnauthenticated = errPrincipalUnauthenticated
	ErrAppBotSearchDenied       = errPrincipalAppBotDenied
)

// PrincipalKind 返回本请求已解析主体的类型名（user / user_bot / obo / uk），未解析
// 返回空串。供入口接线的单测断言「命中哪条路由 + on_behalf_of → 对应 principal」。
func PrincipalKind(c *wkhttp.Context) string {
	if v, ok := c.Get(principalCtxKey); ok {
		if p, ok := v.(Principal); ok && p != nil {
			return p.Kind().String()
		}
	}
	return ""
}
