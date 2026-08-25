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
	// 缓存键带 schema 版本号：给 fg.Rule 加字段后，上一版二进制写入的缓存值会缺
	// 新字段，读出来是零值并在 TTL 内（最长 60s）被当作真实配置使用。版本号一升，
	// 新旧二进制读写不同 keyspace，这个窗口就不存在了。改 Rule 结构时必须同步升。
	cacheSchemaVersion = "v1"
	ruleKeyPrefix      = "featuregate:rule:" + cacheSchemaVersion + ":"
	scopeKeyPrefix     = "featuregate:scope:" + cacheSchemaVersion + ":"
	cacheTTL           = 60 * time.Second
	// nilSentinel 缓存「DB 也没有此规则」，防止未注册 key 每次穿透到 DB。
	nilSentinel = "__nil__"
)

// Dims 是评估维度，pkg/featuregate.Dims 的别名 —— 调用方只 import 本模块即可
// 构造维度，无需再引入纯逻辑包。
type Dims = fg.Dims

// Service 是 featuregate 的带 IO 编排层：在 pkg/featuregate 纯逻辑之上叠加
// DB 真源 + Redis 读缓存 + env kill switch + 按端固定的 fail 兜底。
//
// fail 策略不可按规则配置，而是按调用端固定：
//
//	AllowCreate  写时闸门     fail-closed  （误拒 < 数据裸奔）
//	AllowPush    推送总开关   fail-open    （误推 < 中断存量推送）
//	AllowDisplay 展示位下发   fail-closed，且把「存储故障」与「确定性的关」分开回报
//
// 这是刻意的安全不对称，不暴露为运维旋钮以免被配反。
//
// 由调用方直接 NewService(ctx) 构造，不经 module 注册，与 group.NewDB(ctx) 同
// 模式，保持依赖图单向无环。
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
	Mode     string `json:"mode"`
	Percent  int    `json:"percent"`
	BucketBy string `json:"bucket_by,omitempty"`
}

// AllowCreate 是写时闸门（fail-closed）：env kill / 存储故障 / 无规则 一律拒绝，
// 否则交给纯函数评估。用于 create 类操作 —— 配置拿不到时拒绝新功能，胜过裸奔。
func (s *Service) AllowCreate(ctx context.Context, key string, dims fg.Dims) bool {
	if killSwitchOn(key) {
		return false
	}
	rule, scopes, err := s.loadRuleAndScopes(key)
	if err != nil {
		s.Warn("featuregate load failed; create fail-closed",
			zap.String("key", key), zap.Error(err))
		return false
	}
	if rule == nil {
		return false // 未注册规则 → fail-closed
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
	rule, err := s.loadRule(key)
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

// AllowDisplay 是客户端展示位判定，返回 (allow, ok) 两态：
//
//   - ok == false ：**存储故障**（DB/Redis）导致判定不可得。调用方应把该 key
//     从响应中【省略】，让客户端保留上一次的值。绝不能下发 false —— 响应只有
//     布尔，客户端分不清「真的关」和「服务端抖了一下」，一次 Redis 抖动就会让
//     全体用户丢功能。
//   - ok == true  ：allow 是**确定性**结论。规则不存在 → false（未配置即关），
//     维度不可用 → false，kill switch → false。
//
// 为什么 kill switch 必须走确定性 false 而不是省略：省略意味着客户端保留旧值，
// 那么对一个已经放量的功能按下 kill 开关，客户端界面永远不会收敛——紧急关停
// 在展示面上完全失效。kill 的语义是「立刻确定性地关」，只能是 false。
//
// 维度不可用（如展示端点只有 UID，规则却配了 bucket_by=group）同样是确定性
// 结论而非可用性问题，故也走 false，并打 Warn 让运维能发现这个错配。
func (s *Service) AllowDisplay(ctx context.Context, key string, dims fg.Dims) (allow bool, ok bool) {
	if killSwitchOn(key) {
		return false, true
	}
	rule, scopes, err := s.loadRuleAndScopes(key)
	if err != nil {
		s.Warn("featuregate load failed; display key omitted from response",
			zap.String("key", key), zap.Error(err))
		return false, false
	}
	if rule == nil {
		return false, true // 未注册规则 → 确定性的关
	}
	decision := fg.Evaluate(*rule, scopes, dims)
	if decision.DimensionUnusable {
		// 配置与调用点不匹配：规则引用的某个维度这里提供不了。
		//
		// 用 DimensionUnusable 而非只认 ReasonDimUnavailable，是为了把三种错配一网
		// 打尽：分桶维度缺失（fail-closed）、白名单条目全都用不上（同样 fail-closed）、
		// 以及 percent 0%/100% 短路——后者**决策仍然正确**所以不改结果，但配置确实
		// 是错的，不报的话运维会在把 100 调到 50 的那一刻突然全员掉线。
		s.Warn("featuregate display rule references a dimension this call site cannot provide",
			zap.String("key", key),
			zap.String("reason", decision.Reason),
			zap.Bool("allow", decision.Allow),
			zap.String("bucket_by", fg.BucketDimension(*rule)))
	}
	return decision.Allow, true
}

// loadRuleAndScopes 载入规则与（按需的）白名单。
//
// 「哪些 mode 需要白名单」在此处是**唯一定义**，AllowCreate 与 AllowDisplay 共用。
// 初版把这个条件内联在 AllowCreate 里写作 `if rule.Mode == ModeWhitelist`，当
// percent 也开始支持白名单后，那种写法极易漏改一处，症状是白名单静默失效——写入
// 成功、读路径拿到空 scopes。收敛到一处就没有「漏改哪一处」这回事。
func (s *Service) loadRuleAndScopes(key string) (*fg.Rule, []fg.Scope, error) {
	rule, err := s.loadRule(key)
	if err != nil || rule == nil {
		return nil, nil, err
	}
	if !modeNeedsScopes(rule.Mode) {
		return rule, nil, nil
	}
	scopes, err := s.loadScopes(key)
	if err != nil {
		return nil, nil, err
	}
	return rule, scopes, nil
}

// modeNeedsScopes 报告某 mode 的判定是否要读白名单。与
// pkg/featuregate.Evaluate 的白名单作用域严格对应：whitelist 与 percent 生效，
// off（不可穿透）与 on（无意义）不需要。
func modeNeedsScopes(mode fg.Mode) bool {
	return mode == fg.ModeWhitelist || mode == fg.ModePercent
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

// loadRule 读规则。**不接收 context**：octo-lib 的 redis.Conn 没有任何 ctx 版本方法
// （实测 0 个），所以 Redis 那半边根本无法遵守取消/超时。与其一半遵守一半假装、
// 让下一个调用者误以为 ctx 生效，不如让签名如实反映能力。
//
// Redis 命中即用；miss / 缓存损坏都走 fetchRuleAndCache（查 DB
// 并回填，损坏值被新值覆盖，避免 TTL 内反复穿透）；Redis 真实故障则绕过缓存直查
// DB（不回填）。
func (s *Service) loadRule(key string) (*fg.Rule, error) {
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
		return &fg.Rule{Key: key, Mode: fg.Mode(cr.Mode), Percent: cr.Percent, BucketBy: cr.BucketBy}, nil
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
		Mode: string(rule.Mode), Percent: rule.Percent, BucketBy: rule.BucketBy,
	}))
	return rule, nil
}

// loadScopes 读白名单条目：语义同 loadRule。空列表序列化为 "[]"（非空串），与
// miss（空串）天然区分，不会反复回查；miss / 缓存损坏都走 fetchScopesAndCache。
func (s *Service) loadScopes(key string) ([]fg.Scope, error) {
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
	return &fg.Rule{Key: key, Mode: fg.Mode(m.Mode), Percent: m.Percent, BucketBy: m.BucketBy}, nil
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

// envKillPrefix / envKillSuffix 拼出 env 终极开关名。前缀取 OCTO_ 是本仓当前约定
// （DM_ 系列均为遗留），并按 OCTO_<模块>_<项> 的既有样式带上模块名。
const (
	envKillPrefix = "OCTO_FEATUREGATE_"
	envKillSuffix = "_KILL"
)

// killSwitchOn 读 env 终极开关。不查任何存储，DB/Redis 全挂时仍可一键停。
func killSwitchOn(key string) bool {
	return os.Getenv(KillSwitchEnv(key)) == "1"
}

// KillSwitchEnv 返回某 feature_key 对应的 kill 开关环境变量名，形如
// incoming_webhook_push → OCTO_FEATUREGATE_INCOMING_WEBHOOK_PUSH_KILL。
// 导出是为了让管理端/文档/测试与判定共用同一套推导，不各写一遍字符串拼接。
//
// 这里**不做任何字符折叠**，映射因此是单射：feature_key 被 validFeatureKey 限定在
// [a-z][a-z0-9_]*，ToUpper 在该字符集上一一对应，所以两条不同的 gate 不可能落到
// 同一个 env 名上。早先允许连字符并把 `-` 折成 `_` 时，`docs-beta` 与 `docs_beta`
// 会共用一个开关——停一个会把另一个一起停掉。禁掉连字符 + 不折叠，这类碰撞在
// 结构上消失，而不是靠约定回避。
//
// 注意这条推导把 feature_key 绑进了运维词汇表：改 feature_key 就等于改环境变量
// 名。这正是对外 JSON 用独立 client_key 的原因之一（见 registry.go）。
func KillSwitchEnv(key string) string {
	return envKillPrefix + strings.ToUpper(key) + envKillSuffix
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "" // 极罕见；返回空串使下次仍 miss 重试，而非缓存坏值
	}
	return string(b)
}
