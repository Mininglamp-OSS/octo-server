package featuregate

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"go.uber.org/zap"
)

const (
	maxKeyLen         = 64
	maxScopeIDLen     = 64
	maxDescriptionLen = 255
)

// list 列出所有规则及其白名单条目。
func (m *Manager) list(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}
	rules, err := m.db.listRules()
	if err != nil {
		m.Error("list feature gates failed", zap.Error(err))
		gateQueryFailed(c)
		return
	}
	gates := make([]gateResp, 0, len(rules))
	for _, ru := range rules {
		scopes, err := m.db.queryScopes(ru.FeatureKey)
		if err != nil {
			m.Error("list feature gate scopes failed", zap.String("key", ru.FeatureKey), zap.Error(err))
			gateQueryFailed(c)
			return
		}
		gates = append(gates, toGateResp(ru, scopes))
	}
	c.Response(map[string]interface{}{"gates": gates})
}

// update 创建/覆盖一条规则。先全量校验，再 upsert，最后失效缓存。
func (m *Manager) update(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" || len(key) > maxKeyLen {
		gateRequestInvalid(c, "key")
		return
	}
	var req updateGateReq
	if err := c.BindJSON(&req); err != nil {
		gateRequestInvalid(c, "body")
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if !validMode(mode) {
		gateRequestInvalid(c, "mode")
		return
	}
	if req.Percent < 0 || req.Percent > 100 {
		gateRequestInvalid(c, "percent")
		return
	}
	if len(req.Description) > maxDescriptionLen {
		gateRequestInvalid(c, "description")
		return
	}
	if err := m.db.upsertRule(key, mode, req.Percent, req.Description); err != nil {
		m.Error("upsert feature gate failed", zap.String("key", key), zap.Error(err))
		gateOperationFailed(c)
		return
	}
	m.svc.Invalidate(key)
	c.ResponseOK()
}

// addScope 幂等加入一条白名单条目。
func (m *Manager) addScope(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" || len(key) > maxKeyLen {
		gateRequestInvalid(c, "key")
		return
	}
	var req scopeReq
	if err := c.BindJSON(&req); err != nil {
		gateRequestInvalid(c, "body")
		return
	}
	scopeType := strings.TrimSpace(req.ScopeType)
	if scopeType == "" {
		scopeType = fg.ScopeTypeGroup // 缺省 group
	}
	if !validScopeType(scopeType) {
		gateRequestInvalid(c, "scope_type")
		return
	}
	scopeID := strings.TrimSpace(req.ScopeID)
	if scopeID == "" || len(scopeID) > maxScopeIDLen {
		gateRequestInvalid(c, "scope_id")
		return
	}
	if err := m.db.addScope(key, scopeType, scopeID); err != nil {
		m.Error("add feature gate scope failed",
			zap.String("key", key), zap.String("scope_id", scopeID), zap.Error(err))
		gateOperationFailed(c)
		return
	}
	m.svc.Invalidate(key)
	c.ResponseOK()
}

// delScope 删除一条白名单条目；条目不存在返回 404。scope_type 走 query，缺省 group。
func (m *Manager) delScope(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	scopeID := strings.TrimSpace(c.Param("scope_id"))
	if key == "" || len(key) > maxKeyLen || scopeID == "" {
		gateRequestInvalid(c, "scope_id")
		return
	}
	scopeType := strings.TrimSpace(c.Query("scope_type"))
	if scopeType == "" {
		scopeType = fg.ScopeTypeGroup
	}
	if !validScopeType(scopeType) {
		gateRequestInvalid(c, "scope_type")
		return
	}
	n, err := m.db.deleteScope(key, scopeType, scopeID)
	if err != nil {
		m.Error("delete feature gate scope failed",
			zap.String("key", key), zap.String("scope_id", scopeID), zap.Error(err))
		gateOperationFailed(c)
		return
	}
	if n == 0 {
		gateNotFound(c)
		return
	}
	m.svc.Invalidate(key)
	c.ResponseOK()
}
