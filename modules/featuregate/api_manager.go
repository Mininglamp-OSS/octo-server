package featuregate

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"go.uber.org/zap"
)

const (
	// feature_key 的长度与字符集校验统一走 validFeatureKey（registry.go），
	// 与注册表共用同一口径，避免两处漂移。
	maxScopeIDLen = 64
	// maxDescriptionLen 按**字符**计，对齐 VARCHAR(255) 在 utf8mb4 下的语义。
	// 早先按字节校验虽然不会溢出列，却把中文描述砍到约 85 字并拒掉合法输入。
	maxDescriptionLen = 255
	// maxScopesPerKey 是单个 key 的白名单条数上限。
	//
	// 评估路径会把某 key 的全部条目一次读出并整体缓存进 Redis，所以无上限的名单
	// 同时放大查询量与缓存值大小。上限设在写侧而不是给查询加 LIMIT：后者会静默
	// 丢条目，等于把「慢但正确」换成「快但答案错」。真需要给大批人开灰度时，用
	// percent 而不是万人白名单。
	maxScopesPerKey = 1000
)

// scopeIDRe 约束白名单条目的 ID 字面量。
//
// 关键是**禁掉空白与 `/`**：delScope 把 scope_id 当作单个 URL 路径段取（gin 匹配
// 的是已解码路径，`%2F` 也绕不过），所以一个含 `/` 的条目插得进去却永远删不掉。
// 与其在删除侧改协议，不如在写入侧就不让这种条目存在。字符集覆盖了实际会出现的
// uid / group_no / space_id 形态。
var scopeIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:@-]+$`)

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
	m.commitUpdate(c, key, plan)
}

// commitUpdate 做落库前的最后一道校验（存量白名单）并写入。
//
// 存量白名单也要校验，不只是本次请求的字段：注册表是编译期清单、gate 是 DB 行，
// 所以「先有内部 gate 和 group 白名单，之后某次发版把它变成 client-visible」是
// **正常生命周期**而非异常路径。那之后这条 update 若放行，白名单里就全是永远命不中
// 的条目——而运维看到的是名单有行、mode 对、写入 200、日志无声。addScope 侧的检查
// 只覆盖「已是 client-visible 之后新加的条目」，盖不住这个顺序。
func (m *Manager) commitUpdate(c *wkhttp.Context, key string, plan updatePlan) {
	if ok, reason := m.clientVisibleScopesUsable(plan.mode, key); !ok {
		if reason != "" {
			gateRequestInvalid(c, reason)
			return
		}
		gateQueryFailed(c)
		return
	}
	if err := m.db.upsertRule(key, plan.mode, plan.percent, plan.bucketBy, plan.description, c.GetLoginUID()); err != nil {
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
	//
	// 只在**会读这些输入的 mode** 下施加（modeNeedsScopes）。off/on 既不读白名单也
	// 不读 bucket_by（见 Evaluate 的对应分支），对它们校验一个目标状态压根不使用的
	// 前置条件，只会让「关掉一个 gate」比「打开它」更难 —— 而 off 正是本框架的回滚
	// 杠杆。放行不会留下隐患：每一次切进 whitelist/percent 都会重新校验请求里的
	// bucket_by，坏值不可能悄悄生效。
	if modeNeedsScopes(fg.Mode(mode)) && m.registry.isClientVisible(key) && !validateClientVisibleDimension(bucketBy) {
		m.Warn("rejected client-visible gate with a dimension the flags endpoint cannot provide",
			zap.String("key", key), zap.String("bucket_by", bucketBy))
		return updatePlan{}, "client_visible_dimension"
	}
	if utf8.RuneCountInString(req.Description) > maxDescriptionLen {
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
	scopeType, scopeID, reason := m.planScope(key, req)
	if reason != "" {
		gateRequestInvalid(c, reason)
		return
	}
	if ok, reason := m.scopeQuotaAllows(key, scopeType, scopeID); !ok {
		if reason != "" {
			gateRequestInvalid(c, reason)
			return
		}
		gateQueryFailed(c)
		return
	}
	if err := m.db.addScope(key, scopeType, scopeID, c.GetLoginUID()); err != nil {
		m.Error("add feature gate scope failed",
			zap.String("key", key), zap.String("scope_id", scopeID), zap.Error(err))
		gateOperationFailed(c)
		return
	}
	m.svc.Invalidate(key)
	c.ResponseOK()
}

// planScope 校验并规范化一条白名单条目的写入请求。reason 非空表示校验失败。
//
// 与 planUpdate 同样保持纯逻辑（只依赖注册表），便于单测直接覆盖。
func (m *Manager) planScope(key string, req scopeReq) (scopeType, scopeID, reason string) {
	scopeType = strings.TrimSpace(req.ScopeType)
	if scopeType == "" {
		scopeType = fg.DefaultBucketBy // 缺省 group，与初版一致
	}
	if !fg.ValidDimension(scopeType) {
		return "", "", "scope_type"
	}
	// 同 update：客户端可见的 key 只认 user 维度的白名单条目。允许写入一条永远
	// 不可能命中的 group 条目，等于让运维以为自己开了灰度而实际没有。
	if m.registry.isClientVisible(key) && !validateClientVisibleDimension(scopeType) {
		m.Warn("rejected client-visible gate scope with an unusable dimension",
			zap.String("key", key), zap.String("scope_type", scopeType))
		return "", "", "client_visible_dimension"
	}
	scopeID = strings.TrimSpace(req.ScopeID)
	if scopeID == "" || len(scopeID) > maxScopeIDLen || !scopeIDRe.MatchString(scopeID) {
		return "", "", "scope_id"
	}
	return scopeType, scopeID, ""
}

// scopeQuotaAllows 判定这次 addScope 是否被单 key 配额允许。
//
// 返回 (true, "") 放行；(false, reason) 拒绝；(false, "") 查询出错，调用方回 500。
//
// 先判存在再计数：满额时重加一条**已存在**的条目必须仍然幂等成功。先数后插会把它
// 一并拒掉，而那正是运维最可能重试的时刻（网络抖动后重放）。
//
// 这仍是 check-then-act，并发 add 可能略微冲破上限。它是一道用于限制资源规模的
// 整形阈值，不是硬性资源边界，且写侧是 superadmin-only，故不为此引入事务。
func (m *Manager) scopeQuotaAllows(key, scopeType, scopeID string) (bool, string) {
	exists, err := m.db.scopeExists(key, scopeType, scopeID)
	if err != nil {
		m.Error("check feature gate scope existence failed", zap.String("key", key), zap.Error(err))
		return false, ""
	}
	if exists {
		return true, ""
	}
	n, err := m.db.countScopes(key)
	if err != nil {
		m.Error("count feature gate scopes failed", zap.String("key", key), zap.Error(err))
		return false, ""
	}
	if n >= maxScopesPerKey {
		m.Warn("feature gate whitelist quota exceeded",
			zap.String("key", key), zap.Int("count", n), zap.Int("max", maxScopesPerKey))
		return false, "scope_quota"
	}
	return true, ""
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

// clientVisibleScopesUsable 校验某 client-visible key 的**存量**白名单里至少有一条
// 维度可用的条目。
//
// 返回 (true, "") 表示通过；(false, reason) 表示校验失败；(false, "") 表示查询出错，
// 调用方回 500。空名单视为通过——那是合法状态（mode=whitelist 且暂时谁都不放），
// 不是错配。
func (m *Manager) clientVisibleScopesUsable(mode, key string) (bool, string) {
	// off/on 不读白名单，校验它没有意义，还会挡住关停。见 planUpdate 里同源的注释。
	if !modeNeedsScopes(fg.Mode(mode)) {
		return true, ""
	}
	if !m.registry.isClientVisible(key) {
		return true, ""
	}
	scopes, err := m.db.queryScopes(key)
	if err != nil {
		m.Error("query feature gate scopes for client-visible check failed",
			zap.String("key", key), zap.Error(err))
		return false, ""
	}
	if len(scopes) == 0 {
		return true, ""
	}
	for _, s := range scopes {
		if validateClientVisibleDimension(s.ScopeType) {
			return true, ""
		}
	}
	m.Warn("client-visible gate has a whitelist that can never match at the flags endpoint",
		zap.String("key", key), zap.Int("scopes", len(scopes)))
	return false, "client_visible_scopes"
}

// 关于 delScope：它**刻意不做**这项校验。删除最后一条可用条目确实能把白名单重新
// 带回「全都用不上」的状态，但按删除后的预期状态去拦会造成顺序陷阱——运维想把
// {user:u1, group:g1} 清空时，删哪一条都可能先被拒，于是连清理都做不了。
// 这条路径交给读侧兜底：Evaluate 会报 whitelist_dim_unavailable，AllowDisplay 打
// Warn，不再像 round 1 那样两侧皆哑。
