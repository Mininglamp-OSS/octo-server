package featuregate

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// Manager 是 featuregate 的路由入口：superadmin 管理端点 + 用户侧只读下发端点。
// 读规则/白名单走 db，写后调 svc.Invalidate 让缓存秒级失效。
type Manager struct {
	ctx *config.Context
	log.Log
	db       *gateDB
	svc      *Service
	registry *clientRegistry
	flags    *flagsAPI
}

// NewManager 构造模块入口。svc 既用于写后失效缓存，也是用户侧端点的判定来源，
// 与业务模块的运行时评估共享同一 Redis/DB。
func NewManager(ctx *config.Context) *Manager {
	return newManagerWithRegistry(ctx, defaultClientRegistry)
}

// newManagerWithRegistry 是 NewManager 的可注入变体。生产只有 defaultClientRegistry
// 一份；测试用它注入自己的注册表，从而在不往生产清单里塞假 key 的前提下覆盖
// 「客户端可见 key」相关的分支。
func newManagerWithRegistry(ctx *config.Context, registry *clientRegistry) *Manager {
	svc := NewService(ctx)
	return &Manager{
		ctx:      ctx,
		Log:      log.NewTLog("featureGateManager"),
		db:       newDB(ctx),
		svc:      svc,
		registry: registry,
		flags:    newFlagsAPI(ctx, svc, registry),
	}
}

// Route 注册两组路由：
//
//	/v1/manager/featuregate  superadmin 管理面（每个 handler 内再校验角色）
//	/v1/featuregate          已登录用户的只读下发面
//
// 两组都在 AuthMiddleware 之后挂 SharedUIDRateLimiter —— 挂在鉴权中间件之前
// 读不到 uid，会静默 fail-open。
//
// 用户侧端点用 /v1/featuregate/... 而非 /v1/user/...：路径首段跟模块名走
// （同 /v1/common/appconfig、/v1/sticker/user），从路由能直接定位到代码。
func (m *Manager) Route(r *wkhttp.WKHttp) {
	m.routeManager(r)
	m.routeUser(r)
}

// routeManager 挂 superadmin 管理面。角色校验在每个 handler 内再做一次。
func (m *Manager) routeManager(r *wkhttp.WKHttp) {
	g := r.Group("/v1/manager/featuregate", m.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, m.ctx))
	g.GET("/gates", m.list)                              // 列出所有规则（含白名单）
	g.PUT("/gates/:key", m.update)                       // 改 mode/percent/bucket_by/description
	g.POST("/gates/:key/scopes", m.addScope)             // 加白名单条目
	g.DELETE("/gates/:key/scopes/:scope_id", m.delScope) // 删白名单条目
}

// routeUser 挂已登录用户的只读下发面。
func (m *Manager) routeUser(r *wkhttp.WKHttp) {
	g := r.Group("/v1/featuregate", m.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, m.ctx))
	g.GET("/flags", m.flags.get) // 当前登录用户的灰度位
}

// ---- 请求/响应模型 ----

type updateGateReq struct {
	Mode    string `json:"mode"`
	Percent int    `json:"percent"`
	// BucketBy 是 percent 分桶维度；空 = 沿用默认（group）。
	BucketBy    string `json:"bucket_by"`
	Description string `json:"description"`
}

type scopeReq struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
}

type scopeResp struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	UpdatedBy string `json:"updated_by"`
}

type gateResp struct {
	FeatureKey  string `json:"feature_key"`
	Mode        string `json:"mode"`
	Percent     int    `json:"percent"`
	BucketBy    string `json:"bucket_by"`
	Description string `json:"description"`
	// UpdatedBy 是最近修改该规则的管理员 uid。一个能对全体用户关功能的开关必须
	// 留下操作人，否则事后无从追溯是谁在什么时候翻的。
	UpdatedBy string      `json:"updated_by"`
	Scopes    []scopeResp `json:"scopes"`
	// ClientVisible 标示该 key 是否会被下发到客户端。运维据此知道这条规则受
	// 「维度必须是 user」的额外约束，也知道改它会影响终端展示。
	ClientVisible bool `json:"client_visible"`
	// KillSwitchEnv 回显该 key 的 env 杀开关名，省得运维手工推导大小写/连字符。
	KillSwitchEnv string `json:"kill_switch_env"`
}

func toGateResp(g *gateModel, scopes []*scopeModel, clientVisible bool) gateResp {
	sr := make([]scopeResp, 0, len(scopes))
	for _, s := range scopes {
		sr = append(sr, scopeResp{ScopeType: s.ScopeType, ScopeID: s.ScopeID, UpdatedBy: s.UpdatedBy})
	}
	return gateResp{
		FeatureKey: g.FeatureKey,
		Mode:       g.Mode,
		Percent:    g.Percent,
		// 空值在回显里也归一成实际生效的维度，避免运维看到空白去猜默认是什么。
		BucketBy:      fg.BucketDimension(fg.Rule{BucketBy: g.BucketBy}),
		Description:   g.Description,
		UpdatedBy:     g.UpdatedBy,
		Scopes:        sr,
		ClientVisible: clientVisible,
		KillSwitchEnv: KillSwitchEnv(g.FeatureKey),
	}
}

// ---- 字段校验 ----

func validMode(mode string) bool {
	switch fg.Mode(mode) {
	case fg.ModeOff, fg.ModeOn, fg.ModeWhitelist, fg.ModePercent:
		return true
	}
	return false
}

// clientVisibleDimension 是【客户端展示端点】唯一能提供的维度。
//
// GET /v1/featuregate/flags 只有登录用户的 UID：它不接收也不推断空间/群上下文
// （一个用户可属多个空间，"当前空间"在那条请求里没有确定答案）。因此凡是会下发到
// 客户端的 key，其分桶维度与白名单维度都必须是 user。
const clientVisibleDimension = fg.ScopeTypeUser

// validateClientVisibleDimension 是写侧的错配拦截：对已注册为客户端可见的
// feature_key，拒绝它用上一个展示端点提供不了的维度。
//
// 不拦的后果是一个**无报错的静默错配**：配 bucket_by=group 时，展示端点评估到的
// GroupNo 为空，若按空串照算则所有用户落进同一个桶，管理台显示的 50% 实际会变成
// 全体开或全体关。读侧（AllowDisplay）另有 fail-closed 兜底 —— 两侧都要：只做
// 写侧会被直接改库绕过，只做读侧则运维以为自己配置生效了。
//
// **按 mode 门控**（见 planUpdate 里的 modeNeedsScopes 判断）：off/on 既不读白名单也
// 不读 bucket_by，对它们施加此校验会挡住关停，而 off 是本框架的回滚杠杆。代价是
// off/on 的行里可能留下一个未经校验的 bucket_by——这不构成隐患，因为每次切进
// whitelist/percent 都会重新校验请求里的值，坏值不可能悄悄生效。
func validateClientVisibleDimension(dim string) bool {
	return dim == clientVisibleDimension
}
