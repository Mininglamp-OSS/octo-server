package featuregate

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"go.uber.org/zap"
)

const (
	// feature_key 的长度与字符集校验统一走 validFeatureKey（registry.go），
	// 与注册表共用同一口径，避免两处漂移。
	maxScopeIDLen     = 64
	maxDescriptionLen = 255
)

// list 列出所有规则及其白名单条目。
func (m *Manager) list(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		m.Warn("featuregate manager list denied", zap.String("uid", c.GetLoginUID()), zap.Error(err))
		gateForbidden(c)
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
		gates = append(gates, toGateResp(ru, scopes, m.registry.isClientVisible(ru.FeatureKey)))
	}
	c.Response(map[string]interface{}{"gates": gates})
}

// update 创建/覆盖一条规则。先全量校验，再 upsert，最后失效缓存。
func (m *Manager) update(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		m.Warn("featuregate manager update denied", zap.String("uid", c.GetLoginUID()), zap.Error(err))
		gateForbidden(c)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if !validFeatureKey(key) {
		gateRequestInvalid(c, "key")
		return
	}
	var req updateGateReq
	if err := c.BindJSON(&req); err != nil {
		gateRequestInvalid(c, "body")
		return
	}
	plan, reason := m.planUpdate(key, req)
	if reason != "" {
		gateRequestInvalid(c, reason)
		return
	}
	if err := m.db.upsertRule(key, plan.mode, plan.percent, plan.bucketBy, plan.description); err != nil {
		m.Error("upsert feature gate failed", zap.String("key", key), zap.Error(err))
		gateOperationFailed(c)
		return
	}
	m.svc.Invalidate(key)
	c.ResponseOK()
}

// updatePlan 是校验通过后要落库的规范化取值。
type updatePlan struct {
	mode        string
	percent     int
	bucketBy    string
	description string
}

// planUpdate 校验并规范化一次 update 请求。reason 非空表示校验失败，其值即
// 回给客户端的 details.reason。
//
// 抽成独立函数而非内联在 handler 里：校验链本身是纯逻辑（唯一的外部依赖是
// 注册表），拆开后 handler 只剩编排，校验也能被单测直接覆盖而不必走 HTTP。
func (m *Manager) planUpdate(key string, req updateGateReq) (updatePlan, string) {
	mode := strings.TrimSpace(req.Mode)
	if !validMode(mode) {
		return updatePlan{}, "mode"
	}
	if req.Percent < 0 || req.Percent > 100 {
		return updatePlan{}, "percent"
	}
	// 空 bucket_by 归一到默认维度后再校验：DB 列有 CHECK 约束，不能落空串进去。
	bucketBy := strings.TrimSpace(req.BucketBy)
	if bucketBy == "" {
		bucketBy = fg.DefaultBucketBy
	}
	if !fg.ValidDimension(bucketBy) {
		return updatePlan{}, "bucket_by"
	}
	// 客户端可见的 key 只能按 user 维度分桶 —— 展示端点没有群/空间上下文。
	// 见 validateClientVisibleDimension 的注释：这里不拦就是一个无报错的错配。
	if m.registry.isClientVisible(key) && !validateClientVisibleDimension(bucketBy) {
		m.Warn("rejected client-visible gate with a dimension the flags endpoint cannot provide",
			zap.String("key", key), zap.String("bucket_by", bucketBy))
		return updatePlan{}, "client_visible_dimension"
	}
	if len(req.Description) > maxDescriptionLen {
		return updatePlan{}, "description"
	}
	return updatePlan{mode: mode, percent: req.Percent, bucketBy: bucketBy, description: req.Description}, ""
}

// addScope 幂等加入一条白名单条目。
func (m *Manager) addScope(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		m.Warn("featuregate manager addScope denied", zap.String("uid", c.GetLoginUID()), zap.Error(err))
		gateForbidden(c)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if !validFeatureKey(key) {
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
		scopeType = fg.DefaultBucketBy // 缺省 group，与初版一致
	}
	if !fg.ValidDimension(scopeType) {
		gateRequestInvalid(c, "scope_type")
		return
	}
	// 同 update：客户端可见的 key 只认 user 维度的白名单条目。允许写入一条永远
	// 不可能命中的 group 条目，等于让运维以为自己开了灰度而实际没有。
	if m.registry.isClientVisible(key) && !validateClientVisibleDimension(scopeType) {
		m.Warn("rejected client-visible gate scope with an unusable dimension",
			zap.String("key", key), zap.String("scope_type", scopeType))
		gateRequestInvalid(c, "client_visible_dimension")
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
		m.Warn("featuregate manager delScope denied", zap.String("uid", c.GetLoginUID()), zap.Error(err))
		gateForbidden(c)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	scopeID := strings.TrimSpace(c.Param("scope_id"))
	if !validFeatureKey(key) {
		gateRequestInvalid(c, "key")
		return
	}
	if scopeID == "" || len(scopeID) > maxScopeIDLen {
		gateRequestInvalid(c, "scope_id")
		return
	}
	scopeType := strings.TrimSpace(c.Query("scope_type"))
	if scopeType == "" {
		scopeType = fg.DefaultBucketBy
	}
	if !fg.ValidDimension(scopeType) {
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
