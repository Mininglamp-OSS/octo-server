package featuregate

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// Manager 是 featuregate 的 superadmin 管理端点。读规则/白名单走 db，写后调
// svc.Invalidate 让缓存秒级失效。
type Manager struct {
	ctx *config.Context
	log.Log
	db  *gateDB
	svc *Service
}

// NewManager 构造管理端。svc 仅用于写后失效缓存，与运行时评估共享同一 Redis/DB。
func NewManager(ctx *config.Context) *Manager {
	return &Manager{
		ctx: ctx,
		Log: log.NewTLog("featureGateManager"),
		db:  newDB(ctx),
		svc: NewService(ctx),
	}
}

// Route 注册管理路由。AuthMiddleware 之后挂 SharedUIDRateLimiter（须在其后才能
// 读到 uid），每个 handler 内再做 superadmin 校验。
func (m *Manager) Route(r *wkhttp.WKHttp) {
	g := r.Group("/v1/manager/featuregate", m.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, m.ctx))
	{
		g.GET("/gates", m.list)                              // 列出所有规则（含白名单）
		g.PUT("/gates/:key", m.update)                       // 改 mode/percent/description
		g.POST("/gates/:key/scopes", m.addScope)             // 加白名单条目
		g.DELETE("/gates/:key/scopes/:scope_id", m.delScope) // 删白名单条目
	}
}

// ---- 请求/响应模型 ----

type updateGateReq struct {
	Mode        string `json:"mode"`
	Percent     int    `json:"percent"`
	Description string `json:"description"`
}

type scopeReq struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
}

type scopeResp struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
}

type gateResp struct {
	FeatureKey  string      `json:"feature_key"`
	Mode        string      `json:"mode"`
	Percent     int         `json:"percent"`
	Description string      `json:"description"`
	Scopes      []scopeResp `json:"scopes"`
}

func toGateResp(g *gateModel, scopes []*scopeModel) gateResp {
	sr := make([]scopeResp, 0, len(scopes))
	for _, s := range scopes {
		sr = append(sr, scopeResp{ScopeType: s.ScopeType, ScopeID: s.ScopeID})
	}
	return gateResp{
		FeatureKey:  g.FeatureKey,
		Mode:        g.Mode,
		Percent:     g.Percent,
		Description: g.Description,
		Scopes:      sr,
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

func validScopeType(st string) bool {
	return st == fg.ScopeTypeGroup || st == fg.ScopeTypeSpace
}
