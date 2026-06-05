package featuregate

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	octoredis "github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"go.uber.org/zap"
)

// 缓存键前缀与兜底 TTL。Redis 是规则的读缓存（DB 才是真源）；管理端写后主动
// DEL 失效以达成秒级全实例一致，TTL 仅作 DEL 漏掉时的最终一致兜底。
const (
	ruleKeyPrefix  = "ft:rule:"
	scopeKeyPrefix = "ft:scope:"
	cacheTTL       = 60 * time.Second
	// nilSentinel 缓存「DB 也没有此规则」，防止未注册 key 每次穿透到 DB。
	nilSentinel = "__nil__"
)

// Dims 是评估维度，pkg/featuregate.Dims 的别名 —— 调用方（incomingwebhook 等）
// 只 import 本模块即可构造维度，无需再引入纯逻辑包。
type Dims = fg.Dims

// Service 是 featuregate 的带 IO 编排层：在 pkg/featuregate 纯逻辑之上叠加
// DB 真源 + Redis 读缓存 + env kill switch + 按端固定的 fail 兜底。
//
// fail 策略不可按规则配置：create 一律 fail-closed、push 一律 fail-open（见
// AllowCreate / AllowPush）。这是刻意的安全不对称（create 误拒 < 数据裸奔；
// push 误推 < 中断存量推送），不暴露为运维旋钮以免被配反。
//
// 由调用方（如 incomingwebhook）直接 NewService(ctx) 构造，不经 module 注册，
// 与 group.NewDB(ctx) 同模式，保持依赖图单向无环。
type Service struct {
	db    *gateDB
	redis *octoredis.Conn
	log.Log
}

// NewService 构造 Service，自取 DB 与 Redis。
func NewService(ctx *config.Context) *Service {
	return &Service{
		db:    newDB(ctx),
		redis: ctx.GetRedisConn(),
		Log:   log.NewTLog("FeatureGate"),
	}
}

// cachedRule 是规则在 Redis 中的序列化形态（不含 key —— key 已在 Redis key 里）。
type cachedRule struct {
	Mode    string `json:"mode"`
	Percent int    `json:"percent"`
}

// AllowCreate 是写时闸门（fail-closed）：env kill / 存储故障 / 无规则 一律拒绝，
// 否则交给纯函数评估。用于 create 类操作 —— 配置拿不到时拒绝新功能，胜过裸奔。
func (s *Service) AllowCreate(ctx context.Context, key string, dims fg.Dims) bool {
	if killSwitchOn(key) {
		return false
	}
	rule, err := s.loadRule(ctx, key)
	if err != nil {
		s.Warn("featuregate load rule failed; create fail-closed",
			zap.String("key", key), zap.Error(err))
		return false
	}
	if rule == nil {
		return false // 未注册规则 → fail-closed
	}
	var scopes []fg.Scope
	if rule.Mode == fg.ModeWhitelist {
		scopes, err = s.loadScopes(ctx, key)
		if err != nil {
			s.Warn("featuregate load scopes failed; create fail-closed",
				zap.String("key", key), zap.Error(err))
			return false
		}
	}
	return fg.Evaluate(*rule, scopes, dims).Allow
}

// AllowPush 是推送总开关（fail-open）：仅在 env kill 或规则被显式置为 off 时
// 拒绝；存储故障 / 无规则 / 其余 mode 一律放行。push 是群广播热路径，绝不能因
// 灰度框架故障中断存量推送，也不做维度灰度（按群灰度对群广播无意义）。
func (s *Service) AllowPush(ctx context.Context, key string) bool {
	if killSwitchOn(key) {
		return false
	}
	rule, err := s.loadRule(ctx, key)
	if err != nil {
		s.Warn("featuregate load rule failed; push fail-open",
			zap.String("key", key), zap.Error(err))
		return true
	}
	if rule == nil {
		return true // 未注册规则 → fail-open
	}
	// off 是管理员主动关停（确定性），与「故障 fail-open」区分开。
	return rule.Mode != fg.ModeOff
}

// Invalidate 失效某 key 的规则与白名单缓存。管理端写操作提交后调用，使变更秒级
// 在所有实例生效（Redis 共享，DEL 后各实例下次读 miss 回填新值）。
func (s *Service) Invalidate(key string) {
	if err := s.redis.Del(ruleKeyPrefix + key); err != nil {
		s.Warn("featuregate invalidate rule cache failed",
			zap.String("key", key), zap.Error(err))
	}
	if err := s.redis.Del(scopeKeyPrefix + key); err != nil {
		s.Warn("featuregate invalidate scope cache failed",
			zap.String("key", key), zap.Error(err))
	}
}

// loadRule 读规则：Redis 命中即用；miss / 缓存损坏都走 fetchRuleAndCache（查 DB
// 并回填，损坏值被新值覆盖，避免 TTL 内反复穿透——push 热路径每请求读规则，放大
// 明显）；Redis 真实故障则绕过缓存直查 DB（不回填）。
func (s *Service) loadRule(_ context.Context, key string) (*fg.Rule, error) {
	cached, err := s.redis.GetString(ruleKeyPrefix + key)
	if err != nil {
		return s.queryRuleFromDB(key) // Redis 故障：绕过缓存，DB 失败时由调用方走 fail 语义
	}
	switch cached {
	case "":
		return s.fetchRuleAndCache(key)
	case nilSentinel:
		return nil, nil
	default:
		var cr cachedRule
		if err := json.Unmarshal([]byte(cached), &cr); err != nil {
			s.Warn("featuregate rule cache corrupt; refetch from DB",
				zap.String("key", key), zap.Error(err))
			return s.fetchRuleAndCache(key) // 回填覆盖损坏值
		}
		return &fg.Rule{Key: key, Mode: fg.Mode(cr.Mode), Percent: cr.Percent}, nil
	}
}

// fetchRuleAndCache 查 DB 并回填缓存（无规则回填 nilSentinel 防穿透）。
func (s *Service) fetchRuleAndCache(key string) (*fg.Rule, error) {
	rule, err := s.queryRuleFromDB(key)
	if err != nil {
		return nil, err
	}
	if rule == nil {
		s.cacheSet(ruleKeyPrefix+key, nilSentinel)
		return nil, nil
	}
	s.cacheSet(ruleKeyPrefix+key, mustJSON(cachedRule{
		Mode: string(rule.Mode), Percent: rule.Percent,
	}))
	return rule, nil
}

// loadScopes 读白名单条目：语义同 loadRule。空列表序列化为 "[]"（非空串），与
// miss（空串）天然区分，不会反复回查；miss / 缓存损坏都走 fetchScopesAndCache。
func (s *Service) loadScopes(_ context.Context, key string) ([]fg.Scope, error) {
	cached, err := s.redis.GetString(scopeKeyPrefix + key)
	if err != nil {
		return s.queryScopesFromDB(key)
	}
	if cached == "" {
		return s.fetchScopesAndCache(key)
	}
	var scopes []fg.Scope
	if err := json.Unmarshal([]byte(cached), &scopes); err != nil {
		s.Warn("featuregate scope cache corrupt; refetch from DB",
			zap.String("key", key), zap.Error(err))
		return s.fetchScopesAndCache(key) // 回填覆盖损坏值
	}
	return scopes, nil
}

// fetchScopesAndCache 查 DB 并回填白名单缓存。
func (s *Service) fetchScopesAndCache(key string) ([]fg.Scope, error) {
	scopes, err := s.queryScopesFromDB(key)
	if err != nil {
		return nil, err
	}
	s.cacheSet(scopeKeyPrefix+key, mustJSON(scopes))
	return scopes, nil
}

func (s *Service) queryRuleFromDB(key string) (*fg.Rule, error) {
	m, err := s.db.queryRule(key)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return &fg.Rule{Key: key, Mode: fg.Mode(m.Mode), Percent: m.Percent}, nil
}

func (s *Service) queryScopesFromDB(key string) ([]fg.Scope, error) {
	rows, err := s.db.queryScopes(key)
	if err != nil {
		return nil, err
	}
	scopes := make([]fg.Scope, 0, len(rows))
	for _, r := range rows {
		scopes = append(scopes, fg.Scope{Type: r.ScopeType, ID: r.ScopeID})
	}
	return scopes, nil
}

// cacheSet 写缓存，失败仅 Warn（不阻断主流程，TTL/下次 miss 自愈）。空串跳过，
// 避免把「未配置」状态写成可被误判为 miss 的值。
func (s *Service) cacheSet(k, v string) {
	if v == "" {
		return
	}
	if err := s.redis.SetAndExpire(k, v, cacheTTL); err != nil {
		s.Warn("featuregate cache set failed", zap.String("key", k), zap.Error(err))
	}
}

// killSwitchOn 读 env 终极开关 DM_FT_<KEY>_KILL。不查任何存储，DB/Redis 全挂
// 时仍可一键停。KEY 为 feature_key 的大写形态（incoming_webhook_push →
// DM_FT_INCOMING_WEBHOOK_PUSH_KILL）。
func killSwitchOn(key string) bool {
	env := "DM_FT_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_")) + "_KILL"
	return os.Getenv(env) == "1"
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "" // 极罕见；返回空串使下次仍 miss 重试，而非缓存坏值
	}
	return string(b)
}
