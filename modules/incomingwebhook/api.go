package incomingwebhook

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/go-redis/redis"
	"go.uber.org/zap"
)

// 默认配置（可被环境变量覆盖）。
const (
	envBodyMax         = "DM_INCOMINGWEBHOOK_MAX_BYTES"
	envIngressIPRPS    = "DM_INCOMINGWEBHOOK_IP_RPS"
	envIngressIPBurst  = "DM_INCOMINGWEBHOOK_IP_BURST"
	envIPFailRPS       = "DM_INCOMINGWEBHOOK_IP_FAIL_RPS"
	envIPFailBurst     = "DM_INCOMINGWEBHOOK_IP_FAIL_BURST"
	envMaxContentRunes = "DM_INCOMINGWEBHOOK_MAX_CONTENT_RUNES"

	defaultMaxBytes = 8 * 1024
	// 总开关(enabled) 与 per_webhook rps/burst、max_per_group 已迁移到 system_setting
	// （单一真源在 modules/common.SystemSettings.IncomingWebhook*），运行时可经管理台
	// 动态调；env DM_INCOMINGWEBHOOK_{ENABLED,RPS,BURST,MAX_PER_GROUP} 仍作 fallback。
	// per-IP 请求限流（StrictIPRateLimitMiddleware，计入全部请求）的默认值。刻意高于
	// 旧值(30/60)、但仍低于进程级 floor(200/400)：合法共享/固定 IP 的正常推送量(受
	// per-webhook 5rps 约束，单 IP 多 webhook 聚合一般 ≪100rps)不被误杀，同时把"单 IP
	// 持多有效 token"的洪流封在 floor 之下，避免一个 IP 吃满全局 floor 挤占其它租户。
	defaultIngressIPRPS   = 100.0
	defaultIngressIPBurst = 200
	defaultIPFailRPS      = 30.0
	defaultIPFailBurst    = 60
	// content 的语义长度上限（rune 数）。8KB body cap 是字节传输上限，这里再加一道
	// 业务上限：单条消息正文过长既影响客户端渲染，也无 IM 语义。默认 4000 rune
	// 介于 Discord(~2k) 与 Slack(~40k) 之间，可经 env 调整。
	defaultMaxContentRunes = 4000
)

// 撤回权限说明：webhook 消息的 FromUID 形如 "iwh_xxx"，永远不是群成员。
// 当群主/管理员调撤回 API 时，message.hasRevokePermission 走 fromMember==nil
// 兜底分支允许撤回；普通成员（包括 webhook 创建者）走否定分支。这条契约依赖
// message 模块的现有实现，未来若 message 重构 hasRevokePermission，需要在此处
// 同步加测试或改为显式 "iwh_" 前缀分支。

// IncomingWebhook 群入站 Webhook 路由层。
type IncomingWebhook struct {
	ctx *config.Context
	log.Log
	db        *incomingWebhookDB
	groupDB   *group.DB
	rateRedis *redis.Client
	// auditSem 给 push 成功后的异步审计(recordSuccess)限并发：每次推送有两次 DB 写，
	// 无界 `go recordSuccess` 在 Redis 限流 fail-open + 推送洪峰下会无限堆 goroutine、
	// 压垮 DB 连接池。用带缓冲 channel 作信号量给审计的 DB 操作总并发封顶——满了就**丢弃**
	// 本次审计（仅 Warn），而不是回落到请求 goroutine 同步执行。审计是非关键路径（失败
	// 本就只记日志），丢弃换来的是：审计占用的 DB 连接数恒 ≤ 桶容量，洪峰下不会和主流量
	// 抢连接池。同步回落则会让每个请求 goroutine 各占一条连接，在限流全 fail-open、请求
	// 并发本身无界时重新压垮连接池——正是这个信号量要避免的（yujiawei review P2）。
	auditSem chan struct{}
	// floor 是 push 端点的 Redis-independent 进程级限流地板：两个 Redis 限流器在
	// Redis 故障时 fail-open，floor 用纯内存令牌桶兜底，保证单实例推送速率始终有界。
	floor *localFloor
	// settings 是进程级共享的 system_setting 快照（admin 可动态调）。本模块的总开关
	// (enabled) 与核心阈值(per_webhook rps/burst、max_per_group) 都走它读取：DB →
	// env(DM_INCOMINGWEBHOOK_*) → code-default。admin 在管理台改值后 Reload 立即生效，
	// 多实例 60s 内收敛，无需重启。其余阈值(IP/失败预算/floor/body/content)仍走 env。
	settings *commonmod.SystemSettings
	// webhookCache / groupCache 是 push 热路径的进程内短 TTL 缓存（#284 item 2）：
	// 分别缓存 webhook 行与「群 Normal」结果，命中即 0 DB 读。变更路径（update/delete/
	// regenerate/handleGroupDisband）即时失效本实例条目，跨实例由 TTL 兜底——staleness
	// 契约见 cache.go。只缓存存在/Normal 的正向结果，不做负缓存。
	webhookCache *ttlCache[*incomingWebhookModel]
	groupCache   *ttlCache[*group.Model]
	// memberCache 缓存「创建者仍是群内（内部、正常）成员」的正向结果，key 为
	// groupNo+"|"+uid，条目值为「创建者当前是否群管理员」（供 push 覆盖判权）。
	// push 路径的创建者在群闸（cachedCreatorMembership）用它把每次推送的
	// group_member 点读压到 0；【负结果绝不缓存】（安全不变量，退群后最多 stale
	// 一个 TTL，之后懒级联禁用把 webhook 翻为 disabled，彻底关闸）。
	memberCache *ttlCache[bool]
}

// maxConcurrentAudit 限制异步审计 goroutine 的最大并发数（默认值，可被 env 覆盖）。
const (
	envAuditConcurrency     = "DM_INCOMINGWEBHOOK_AUDIT_CONCURRENCY"
	defaultAuditConcurrency = 64
)

func auditConcurrency() int {
	if v := os.Getenv(envAuditConcurrency); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultAuditConcurrency
}

// rateRedisOnce 让限流用的 redis client 在进程内单例化，避免每次 New() 都开新连接池
// 在测试或多次注册场景下泄漏（参考 pkg/wkhttp/ratelimit_helper.go 的 SharedUIDRateLimiter）。
var (
	rateRedisOnce   sync.Once
	rateRedisClient *redis.Client
)

func sharedRateRedis(cfg *config.Config) *redis.Client {
	rateRedisOnce.Do(func() {
		// 通过 octoredis.MustBuildOptions 构造，确保 cfg.DB.RedisTLS 启用时
		// （AWS ElastiCache / Azure Cache 等托管 TLS Redis）TLSConfig 不被遗漏。
		// 否则限流 client 连不上 TLS-only Redis，per-IP / per-webhook 两个限流器
		// 都会 fail-open，未认证 push 端点的反扫描/防洪泛保护被静默关闭。
		// PoolSize 显式设 10：令牌桶 Lua 脚本是短事务，与 main.go / user / group /
		// space / integration 等其它限流 client 的全局约定保持一致。Redis 故障/连接池
		// 打满导致 fail-open 的兜底由进程内 localFloor 负责，不在此处放大连接池。
		rateRedisClient = redis.NewClient(octoredis.MustBuildOptions(cfg, func(o *redis.Options) {
			o.MaxRetries = 1
			o.PoolSize = 10
		}))
	})
	return rateRedisClient
}

// New 构造路由模块（完整实例：注册 push/管理路由 + GroupDisband 事件监听）。
func New(ctx *config.Context) *IncomingWebhook {
	w := newIncomingWebhook(ctx)
	// 群解散级联禁用所有 webhook。只在模块主实例上注册——bot 管理面
	// （NewManagementFacade）不重复订阅，避免一次 disband 触发两份级联写。
	w.ctx.AddEventListener(event.GroupDisband, w.handleGroupDisband)
	return w
}

// NewManagementFacade 构造 bot_api 挂载管理端点（MountManagementRoutes）用的轻量
// 实例：与 New 同构（同一套 handler / DB / 审计池），但不注册事件监听（disband
// 级联由模块主实例负责），也不会被挂 push 路由。
//
// ⚠️ 缓存一致性契约：facade 不挂 push 路由，它持有的几份热路径缓存实际从不被读取
// （仅因复用同一构造器而存在，开销可忽略）。要点只有一个：bot 面的管理写操作
// （update/delete/regenerate）无法失效【主实例】的 push 缓存，主实例最多 stale 一个
// TTL（默认 3s）——与既有「跨实例无主动失效、TTL 兜底」的 staleness 契约（cache.go）
// 同级，等同把 bot 面视作一个对等实例；需要更快收敛就调小
// DM_INCOMINGWEBHOOK_CACHE_TTL_MS。
//
// 刻意【不】按 ctx 记忆化共享单实例：octo-lib 的模块注册器以首个 ctx 调用各模块的
// 创建闭包，按-ctx 记忆化会把测试进程里的多个 test server 折叠成单实例，串掉
// per-server 的 env 派生配置（内存限流地板、缓存 TTL 等）。
func NewManagementFacade(ctx *config.Context) *IncomingWebhook {
	return newIncomingWebhook(ctx)
}

func newIncomingWebhook(ctx *config.Context) *IncomingWebhook {
	cacheTTL, cacheMax := cacheTTL(), cacheMax()
	return &IncomingWebhook{
		ctx:          ctx,
		Log:          log.NewTLog("IncomingWebhook"),
		db:           newDB(ctx),
		groupDB:      group.NewDB(ctx),
		rateRedis:    sharedRateRedis(ctx.GetConfig()),
		auditSem:     make(chan struct{}, auditConcurrency()),
		floor:        newLocalFloor(),
		settings:     commonmod.EnsureSystemSettings(ctx),
		webhookCache: newTTLCache[*incomingWebhookModel](cacheTTL, cacheMax),
		groupCache:   newTTLCache[*group.Model](cacheTTL, cacheMax),
		memberCache:  newTTLCache[bool](cacheTTL, cacheMax),
	}
}

// Route 注册路由。
func (w *IncomingWebhook) Route(r *wkhttp.WKHttp) {
	// 管理类：登录用户 + 群成员/角色校验（resolveActor）。认证路由默认挂
	// SharedUIDRateLimiter（须在 AuthMiddleware 之后，否则读不到 uid 会静默
	// fail-open），与全局 IP floor 叠加，给 create/regenerate 等敏感写操作补
	// per-login-user 限流。bot 侧的同一组端点由 bot_api 模块经
	// MountManagementRoutes 挂到 /v1/bot/groups/:group_no/incoming-webhooks。
	mgr := r.Group("/v1/groups/:group_no/incoming-webhooks",
		w.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, w.ctx))
	w.MountManagementRoutes(mgr)

	// 推送类：URL 内 token 鉴权，无 AuthMiddleware。四层限流，由粗到细：
	//  1) localFloorMiddleware —— 纯内存、不依赖 Redis 的进程级地板，先挡洪峰；Redis
	//     故障时仍限速，避免对 DB + WuKongIM 的洪泛放大。内含两段（均不依赖 Redis）：
	//     先按 IP 的内存令牌桶(默认 100rps，与下方 Redis per-IP 限流持平)，再按全局进程桶
	//     (默认 200rps)。per-IP 段在前，
	//     使单个滥用 IP 至多吃掉它那份地板配额，避免一个 IP 抽干全局桶、误杀其它 IP 的
	//     合法推送(#287)；全局段仍封顶 Redis 故障下的分布式洪流(多 IP)，是地板的本意。
	//  2) ipLimit (StrictIPRateLimitMiddleware) —— 按 IP 对【全部】请求限流(默认 100rps，
	//     低于 floor)，给"单 IP 持多有效 token"的洪流封一个硬天花板，防止一个 IP 吃满
	//     全局 floor 挤占其它租户。阈值高于旧值，合法共享/固定 IP 的正常量不被误杀。
	//  3) ipFailureGateMiddleware —— 按 IP 的"鉴权失败预算"闸(默认 60)：只读 peek，把扫
	//     token 的 IP 在烧光失败预算后【在打 DB 之前】快速切断。合法推送(有效 Key)不消耗
	//     该预算，故比第 2 层更早、更精准地反扫描，且不误伤合法流量。
	//  4) allowPerWebhook(handler 内，按 webhook_id) —— 单个 webhook 的合法流量整形(5rps)。
	ipRPS := wkhttp.ParseRPSFromEnv(envIngressIPRPS, defaultIngressIPRPS)
	ipBurst := wkhttp.ParseBurstFromEnv(envIngressIPBurst, defaultIngressIPBurst)
	ipLimit := r.StrictIPRateLimitMiddleware(context.Background(), w.rateRedis, "incoming_webhook", ipRPS, ipBurst)

	push := r.Group("/v1")
	{
		// requirePushEnabled 在最前：总开关关闭时直接 404，最廉价地短路（甚至不进 floor）。
		// 所有推送形态（native / github / wecom / gitlab / feishu）共享同一条中间件链与
		// 同一组限流桶：适配器只是 body 解析方式不同，不是新的攻击面，不单独开配额
		// （见 adapter.go）。
		chain := func(h wkhttp.HandlerFunc) []wkhttp.HandlerFunc {
			return []wkhttp.HandlerFunc{w.requirePushEnabled(), w.localFloorMiddleware(), ipLimit, w.ipFailureGateMiddleware(), h}
		}
		push.POST("/incoming-webhooks/:webhook_id/:token", chain(w.push)...)
		// 平台适配器（#297 Phase 3/4、#426）：GitHub / 企业微信 / Multica / GitLab / 飞书。
		// 鉴权、限流、群校验与 native 完全一致，仅 body 解析不同（adapter_*.go）。
		push.POST("/incoming-webhooks/:webhook_id/:token/github", chain(w.pushGitHub)...)
		push.POST("/incoming-webhooks/:webhook_id/:token/wecom", chain(w.pushWeCom)...)
		push.POST("/incoming-webhooks/:webhook_id/:token/multica", chain(w.pushMultica)...)
		push.POST("/incoming-webhooks/:webhook_id/:token/gitlab", chain(w.pushGitLab)...)
		push.POST("/incoming-webhooks/:webhook_id/:token/feishu", chain(w.pushFeishu)...)
	}
}

// MountManagementRoutes 把管理端点（create/list/update/delete/regenerate/
// deliveries/test）注册到 g 上。g 的路径必须已携带 :group_no（形如
// /v1/groups/:group_no/incoming-webhooks），且调用方负责挂好鉴权中间件并保证
// 处理器可从 c.MustGet("uid") 取到操作者身份：
//   - 用户侧（本模块 Route）：AuthMiddleware 写入登录 uid；
//   - bot 侧（bot_api 模块）：authBot 后由适配中间件把 robot_id 写入 "uid"。
//
// 权限矩阵由 resolveActor + 各 handler 的所有权判断统一实施，与挂载面无关。
func (w *IncomingWebhook) MountManagementRoutes(g *wkhttp.RouterGroup) {
	// 总开关(system_setting incomingwebhook.enabled)关闭时，写操作一律 403 拒绝，
	// 仅保留 list 只读——运维仍可查看/排查已存在配置。requireMgmtEnabled 不挂在 list 上。
	g.POST("", w.requireMgmtEnabled(), w.create)
	g.GET("", w.list)
	g.PUT("/:webhook_id", w.requireMgmtEnabled(), w.update)
	g.DELETE("/:webhook_id", w.requireMgmtEnabled(), w.delete)
	g.POST("/:webhook_id/regenerate", w.requireMgmtEnabled(), w.regenerate)
	// 排障：最近投递记录（成功+失败）。只读，与 list 一致不挂 requireMgmtEnabled。
	g.GET("/:webhook_id/deliveries", w.deliveries)
	// 测试推送：创建者/管理员一键发一条样例消息验证配置。写操作，挂总开关闸。
	g.POST("/:webhook_id/test", w.requireMgmtEnabled(), w.testPush)
}

// requirePushEnabled 在总开关(system_setting incomingwebhook.enabled)关闭时让 push
// 端点返回 404。这是「功能全局停用」语义，对所有请求一致（不区分 webhook 是否存在），
// 因此与 push 路径的反枚举不变量不冲突。
func (w *IncomingWebhook) requirePushEnabled() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if !w.settings.IncomingWebhookEnabled() {
			pushDisabled(c)
			return
		}
		c.Next()
	}
}

// requireMgmtEnabled 在总开关关闭时拒绝所有管理写操作（create/update/delete/
// regenerate）并返回 403；list 只读不挂此闸。挂在 AuthMiddleware 之后，故仅对已认证的
// 群管理员生效——总开关是「功能是否开放」而非鉴权，403 语义恰当。
func (w *IncomingWebhook) requireMgmtEnabled() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if !w.settings.IncomingWebhookEnabled() {
			mgmtFeatureDisabled(c)
			return
		}
		c.Next()
	}
}

// ============================================================
// 配置读取（每次读 env，便于运行时调参）
// ============================================================

func maxBytes() int {
	if v := os.Getenv(envBodyMax); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxBytes
}

func maxContentRunes() int {
	if v := os.Getenv(envMaxContentRunes); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxContentRunes
}

// ipFailRPS / ipFailBurst bound the per-IP AUTH-FAILURE budget (not request
// volume): how fast / how many failed-auth attempts an IP may make before the
// push gate starts rejecting it. Tunable via DM_INCOMINGWEBHOOK_IP_FAIL_RPS /
// _BURST.
func ipFailRPS() float64 {
	return wkhttp.ParseRPSFromEnv(envIPFailRPS, defaultIPFailRPS)
}

func ipFailBurst() int {
	return wkhttp.ParseBurstFromEnv(envIPFailBurst, defaultIPFailBurst)
}

// ============================================================
// 工具函数
// ============================================================

func generateToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	token = hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	hash = hex.EncodeToString(sum[:])
	return token, hash, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// generateWebhookID 用 16 字节随机数构造 webhook 的公开 ID（URL 路径段）。
// 不截断 UUID 时间戳前缀，避免高并发下毫秒级碰撞。
func generateWebhookID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败概率极低；退化到 UUID 仍可保证唯一性。
		return webhookIDPrefix + strings.ReplaceAll(util.GenerUUID(), "-", "")
	}
	return webhookIDPrefix + hex.EncodeToString(buf)
}

// memberWebhookNamePrefix 是非管理员（成员/bot）所设 webhook 展示名的强制前缀：
// 成员可以自定义名称，但名称必须以 "Webhook-" 开头（缺省自动命名本就是该形态），
// 防止成员把 webhook 命名成"HR 公告"或他人姓名冒充真实发送者（PR #340 review，
// yujiawei P2）。管理员命名不受限（历史行为，管理员本就可信）。
const memberWebhookNamePrefix = "Webhook-"

// prefixedWebhookName 给非管理员提交的名称补强制前缀；已带前缀则原样返回（幂等，
// 避免成员保存自己 webhook 时被二次加前缀）。
func prefixedWebhookName(name string) string {
	if strings.HasPrefix(name, memberWebhookNamePrefix) {
		return name
	}
	return memberWebhookNamePrefix + name
}

// autoWebhookName 在创建时未提供名称的情况下生成服务端默认名：
// 前缀 + webhook_id 随机段前 6 位 hex。确定性、可与 webhook_id 对账，
// 不引入第二个随机源。
func autoWebhookName(webhookID string) string {
	suffix := strings.TrimPrefix(webhookID, webhookIDPrefix)
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return memberWebhookNamePrefix + suffix
}

func toResp(m *incomingWebhookModel) webhookResp {
	r := webhookResp{
		WebhookID:  m.WebhookID,
		GroupNo:    m.GroupNo,
		Name:       m.Name,
		Avatar:     m.Avatar,
		CreatorUID: m.CreatorUID,
		Status:     m.Status,
		CallCount:  m.CallCount,
		CreatedAt:  time.Time(m.CreatedAt).Unix(),
	}
	if m.LastUsedAt.Valid {
		r.LastUsedAt = m.LastUsedAt.Time.Unix()
	}
	return r
}

// publicURL 构造对外推送 URL（不含 host，由前端拼接基础域名）。
func publicURL(webhookID, token string) string {
	return fmt.Sprintf("/v1/incoming-webhooks/%s/%s", webhookID, token)
}

// publicURLs 构造各推送形态的对外路径（#297 onboarding 项 / Phase 4 / #426）：native 即
// 历史契约的 url 字段，github / wecom / multica / gitlab / feishu 为平台适配器后缀。
// 与 publicURL 一样不含 host。
func publicURLs(webhookID, token string) map[string]string {
	base := publicURL(webhookID, token)
	return map[string]string{
		"native":  base,
		"github":  base + "/github",
		"wecom":   base + "/wecom",
		"multica": base + "/multica",
		"gitlab":  base + "/gitlab",
		"feishu":  base + "/feishu",
	}
}

// ============================================================
// 鉴权辅助
// ============================================================

// requireActiveGroup 查询群并校验状态为 Normal；非 Normal（含已禁用/已解散/不存在）
// 一律按 404 拒绝。所有"会让 webhook 进入可推送状态"的写操作（create / update 启用 /
// regenerate）以及 push 路径都必须先过这一关，确保 disband 后没有窗口期可被复活或继续推送。
func (w *IncomingWebhook) requireActiveGroup(groupNo string) (*group.Model, error) {
	g, err := w.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		return nil, fmt.Errorf("query group: %w", err)
	}
	if g == nil || g.Status != group.GroupStatusNormal {
		return nil, nil
	}
	return g, nil
}

// cachedQueryByWebhookID 是 push 热路径用的带缓存 webhook 点读：命中即 0 DB 读。
// 只缓存存在的行（DB 故障与未命中都不写缓存，也不做负缓存——见 cache.go）。
// 仅供 push 使用；管理写路径（update/delete/regenerate）仍走未缓存的 db.queryByWebhookID
// 直读最新状态，避免基于陈旧快照做复活/越权判断。
func (w *IncomingWebhook) cachedQueryByWebhookID(webhookID string) (*incomingWebhookModel, error) {
	if m, ok := w.webhookCache.get(webhookID); ok {
		return m, nil
	}
	// 先捕获代际再读 DB：若读取期间有并发 invalidate（update/delete/regenerate），
	// setIfGen 会丢弃这次回填，避免用变更前的旧行复活缓存（读-后-失效竞态）。
	gen := w.webhookCache.loadGen()
	m, err := w.db.queryByWebhookID(webhookID)
	if err != nil {
		return nil, err
	}
	if m != nil {
		w.webhookCache.setIfGen(webhookID, m, gen)
	}
	return m, nil
}

// cachedRequireActiveGroup 带缓存的群活跃校验，语义同 requireActiveGroup（群存在且
// Normal 返回非 nil，否则 (nil,nil)）。只缓存 Normal 结果——非 Normal/不存在不缓存，
// 既免去负缓存复杂度，也让"群刚解散"在 disband 失效/TTL 后立即走 DB 复核而非被粘住。
// 仅供 push 使用；管理写路径仍走未缓存的 requireActiveGroup。
//
// ⚠️ 群【管理员禁用】(Normal→Disabled, event.GroupUpdate) 不在失效矩阵内：admin 禁用后
// 群闸在所有实例上最多 stale 一个 TTL 才生效（解散是即时的）。这是经维护者确认接受的
// 取舍，详见 cache.go 顶部契约注释；TestPush_GroupAdminDisable_TTLBounded 钉住此语义。
func (w *IncomingWebhook) cachedRequireActiveGroup(groupNo string) (*group.Model, error) {
	if g, ok := w.groupCache.get(groupNo); ok {
		return g, nil
	}
	// 同 cachedQueryByWebhookID：代际守卫关闭 disband 失效与在途 miss 读的回填竞态。
	gen := w.groupCache.loadGen()
	g, err := w.requireActiveGroup(groupNo)
	if err != nil {
		return nil, err
	}
	if g != nil {
		w.groupCache.setIfGen(groupNo, g, gen)
	}
	return g, nil
}

// cachedCreatorMembership 带缓存判断创建者是否仍是群的内部正常成员、以及当前是否
// 群管理员，push 热路径专用。缓存【只存在于 member=true 时】（条目命中即 member=true，
// 条目值为 isAdmin）——负结果绝不缓存（false-never-cached 是创建者退群闸的安全不变量，
// 由 TestCreatorMembershipCache_NeverCachesNegative 钉住），退群后最多 stale 一个 TTL。
// 退群没有跨模块事件可订阅（group.memberremove 事件常量无发布方），所以这里是
// 该规则的权威闸：闸不过 → push 401 + 懒级联禁用（见 handlePush）。
//
// isAdmin 供 push 的展示身份覆盖判权（resolveFromIdentity）：创建者【当前】是管理员
// 才允许 username/avatar_url 覆盖。采用"当前角色"而非创建时快照——免迁移列，且管理员
// 被降级后其 webhook 的覆盖能力随之收回（权限跟随现任角色，语义更严）；角色变更的
// 生效延迟同样 ≤ 一个 TTL。
func (w *IncomingWebhook) cachedCreatorMembership(groupNo, uid string) (member, admin bool, err error) {
	key := groupNo + "|" + uid
	if isAdmin, hit := w.memberCache.get(key); hit {
		return true, isAdmin, nil
	}
	gen := w.memberCache.loadGen()
	member, admin, err = w.db.queryMemberRole(groupNo, uid)
	if err != nil {
		return false, false, err
	}
	if member {
		w.memberCache.setIfGen(key, admin, gen)
	}
	return member, admin, nil
}

// mgmtActor 是管理端点的操作者身份：用户登录态或 bot token 鉴权后的统一抽象。
//   - isAdmin：群主或管理员（QueryIsGroupManagerOrCreator，人/bot 同一查询）——
//     可管理群内任意 webhook、可自定义展示身份、不受 per-creator 配额限制；
//   - 非 admin 的内部正常成员（含成员 bot）：可创建（展示身份服务端固定）、
//     只能管理自己创建的（creator_uid == uid）。
type mgmtActor struct {
	uid     string
	isAdmin bool
}

// resolveActor 解析并校验操作者：必须是群的【内部、正常状态】成员（管理员判定
// 自带该 fail-safe 过滤）。失败时已写入 4xx/5xx 响应并返回 ok=false。
// uid 来源于 "uid" 上下文键：用户路由由 AuthMiddleware 写入；bot 路由由 bot_api
// 的适配中间件把 robot_id 写入同名键，两个挂载面共用本判定。
func (w *IncomingWebhook) resolveActor(c *wkhttp.Context, groupNo string) (mgmtActor, bool) {
	uid := c.MustGet("uid").(string)
	// 单查询同时拿成员资格 + 管理员身份（queryMemberRole 与
	// group.QueryIsGroupManagerOrCreator 同一组 fail-safe 过滤），省掉非管理员
	// 路径的二连击（PR #340 review，Octo-Q F1）。
	isMember, isAdmin, err := w.db.queryMemberRole(groupNo, uid)
	if err != nil {
		w.Error("query group member failed", zap.Error(err))
		mgmtQueryFailed(c)
		return mgmtActor{}, false
	}
	if !isMember {
		mgmtForbidden(c)
		return mgmtActor{}, false
	}
	return mgmtActor{uid: uid, isAdmin: isAdmin}, true
}

// requireOwnership 校验 actor 对 m 的管理权：管理员放行任意，普通成员/bot 仅放行
// 自己创建的；否则写 403。返回 false 时调用方应立即返回。
//
// 刻意用 403 而非 404：list 对全员只读可见，webhook 的存在性在群内不是秘密，
// "看得到但不能动"用 Forbidden 语义更诚实（跨群/不存在仍由 queryManageable 404）。
func (w *IncomingWebhook) requireOwnership(c *wkhttp.Context, actor mgmtActor, m *incomingWebhookModel) bool {
	if actor.isAdmin || m.CreatorUID == actor.uid {
		return true
	}
	mgmtForbidden(c)
	return false
}

// requireCreatorInGroup 校验 webhook 创建者仍是群的内部正常成员。enable /
// regenerate / testPush 这些"让 webhook 可推送/保持可推送"的操作都必须过这一关：
// 创建者退群后 webhook 永久失效（push 路径懒级联禁用兜底），只能删除重建。
// 失败时已写入响应（409 或 5xx）。
func (w *IncomingWebhook) requireCreatorInGroup(c *wkhttp.Context, m *incomingWebhookModel) bool {
	ok, _, err := w.db.queryMemberRole(m.GroupNo, m.CreatorUID)
	if err != nil {
		w.Error("query creator membership failed", zap.Error(err))
		mgmtQueryFailed(c)
		return false
	}
	if !ok {
		mgmtCreatorLeft(c)
		return false
	}
	return true
}

// queryManageable 查询属于 groupNo 且未被软删除的 webhook，供管理端写操作（update /
// delete / regenerate）复用。未命中 / 跨群 / 已软删除（statusDeleted）一律按 not-found
// 写响应；查询故障写 5xx。任一情况返回 (nil, false)，调用方据此提前返回。
//
// 把"已删除视为不存在"集中在此一处，保证三个写端点不会遗漏软删除判断而误操作或复活
// 已删除的 webhook（#254）。
func (w *IncomingWebhook) queryManageable(c *wkhttp.Context, groupNo, webhookID string) (*incomingWebhookModel, bool) {
	m, err := w.db.queryByWebhookID(webhookID)
	if err != nil {
		w.Error("query webhook failed", zap.Error(err))
		mgmtQueryFailed(c)
		return nil, false
	}
	if m == nil || m.GroupNo != groupNo || m.Status == statusDeleted {
		mgmtNotFound(c)
		return nil, false
	}
	return m, true
}

// ============================================================
// 管理端点
// ============================================================

func (w *IncomingWebhook) create(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	actor, ok := w.resolveActor(c, groupNo)
	if !ok {
		return
	}

	var req createReq
	if err := c.BindJSON(&req); err != nil {
		mgmtRequestInvalid(c, "body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	// 成员/bot 自定义名称强制带 "Webhook-" 前缀（先补前缀再做长度校验，上限对
	// 最终落库值生效）；管理员命名不受限。恰好等于裸前缀（无有效内容）视同未填，
	// 走下方的自动命名（PR #340 review，yujiawei P2#9）。
	if !actor.isAdmin && req.Name != "" {
		req.Name = prefixedWebhookName(req.Name)
		if req.Name == memberWebhookNamePrefix {
			req.Name = ""
		}
	}
	if len(req.Name) > 64 {
		mgmtRequestInvalid(c, "name")
		return
	}
	// 头像仅管理员可设置：普通成员/bot 的 webhook 走头像端点的确定性默认头像
	// （bot 13 色 palette），不接受自定义，防止任意成员借头像伪装他人。
	if !actor.isAdmin && req.Avatar != "" {
		mgmtRequestInvalid(c, "avatar")
		return
	}

	// 查询 group 拿 space_id；同时确保群处于 Normal 状态。
	// 已解散/已禁用的群禁止创建新 webhook，避免 disband 后被 stale 管理员复活。
	g, err := w.requireActiveGroup(groupNo)
	if err != nil {
		w.Error("query group failed", zap.Error(err))
		mgmtQueryFailed(c)
		return
	}
	if g == nil {
		mgmtGroupNotFound(c)
		return
	}

	token, hash, err := generateToken()
	if err != nil {
		w.Error("generate token failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}

	webhookID := generateWebhookID()
	if req.Name == "" {
		// 名称缺省时服务端自动命名（Webhook-xxxxxx，后缀取自 webhook_id，可追溯）。
		req.Name = autoWebhookName(webhookID)
	}

	m := &incomingWebhookModel{
		WebhookID:  webhookID,
		TokenHash:  hash,
		GroupNo:    groupNo,
		SpaceID:    g.SpaceID,
		Name:       req.Name,
		Avatar:     req.Avatar,
		CreatorUID: actor.uid,
		Status:     statusEnabled,
	}
	// 配额校验 + 写入在事务内原子完成；FOR UPDATE 锁住 group_no 范围，防止并发越限。
	//
	// TOCTOU 说明：requireActiveGroup 的 status 检查是 insert 事务之前的非事务读，
	// 事务内仅靠 group 行锁串行化、不重查 status。极小窗口内群被解散仍可能写入一条
	// status=1 的行，但这**不构成安全问题**：该 webhook 永远推不出消息——push 路径的
	// requireActiveGroup 重查才是权威闸（群非 Normal 一律 401），且 disband 级联会把
	// status 翻 0。故此处不在事务内重读 group.status，避免给热路径加锁负担。
	//
	// 配额双层：群级 max_per_group 对所有人生效；per-creator 仅约束普通成员/bot
	// （管理员能删任意 webhook，对其限个人额度无安全意义）。
	maxWH := w.settings.IncomingWebhookMaxPerGroup()
	maxPerCreator := 0
	if !actor.isAdmin {
		maxPerCreator = w.settings.IncomingWebhookMaxPerCreator()
	}
	if err := w.db.insertWithQuota(m, maxWH, maxPerCreator); err != nil {
		if errors.Is(err, ErrQuotaExceeded) {
			mgmtQuotaExceeded(c, maxWH)
			return
		}
		if errors.Is(err, ErrCreatorQuotaExceeded) {
			mgmtCreatorQuotaExceeded(c, maxPerCreator)
			return
		}
		w.Error("insert webhook failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}

	resp := createResp{
		webhookResp: toResp(m),
		Token:       token,
		URL:         publicURL(m.WebhookID, token),
		URLs:        publicURLs(m.WebhookID, token),
	}
	c.Response(resp)
}

// list 对任意（内部、正常）群成员只读开放：响应不含 token / token_hash / 推送 URL，
// 看得到不等于推得了；成员据 creator_uid 识别哪些是自己创建的。
func (w *IncomingWebhook) list(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	if _, ok := w.resolveActor(c, groupNo); !ok {
		return
	}
	list, err := w.db.queryByGroupNo(groupNo)
	if err != nil {
		w.Error("list webhooks failed", zap.Error(err))
		mgmtQueryFailed(c)
		return
	}
	resps := make([]webhookResp, 0, len(list))
	for _, m := range list {
		resps = append(resps, toResp(m))
	}
	c.Response(map[string]interface{}{"list": resps})
}

func (w *IncomingWebhook) update(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	webhookID := c.Param("webhook_id")
	actor, ok := w.resolveActor(c, groupNo)
	if !ok {
		return
	}

	m, ok := w.queryManageable(c, groupNo, webhookID)
	if !ok {
		return
	}
	if !w.requireOwnership(c, actor, m) {
		return
	}

	var req updateReq
	if err := c.BindJSON(&req); err != nil {
		mgmtRequestInvalid(c, "body")
		return
	}

	fields := map[string]interface{}{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			mgmtRequestInvalid(c, "name")
			return
		}
		// 与 create 同口径：成员/bot 改名强制带前缀（幂等），长度校验对最终值生效；
		// 裸前缀（无有效内容）按空名拒绝——update 没有自动命名回退（既有名字是已
		// 确立的身份，静默换成自动名会让调用方意外）。
		if !actor.isAdmin {
			name = prefixedWebhookName(name)
			if name == memberWebhookNamePrefix {
				mgmtRequestInvalid(c, "name")
				return
			}
		}
		if len(name) > 64 {
			mgmtRequestInvalid(c, "name")
			return
		}
		fields["name"] = name
	}
	if req.Avatar != nil {
		// 头像仅管理员可改（与 create 一致）：成员/bot 的 webhook 固定走头像端点的
		// 确定性默认头像。
		if !actor.isAdmin {
			mgmtRequestInvalid(c, "avatar")
			return
		}
		fields["avatar"] = *req.Avatar
	}
	if req.Status != nil {
		// 仅接受启用/禁用；statusDeleted(2) 不可经 update 设置——删除只能走 DELETE
		// 端点（软删除），update 也不能复活已删除行（见下方 queryManageable）。
		if *req.Status != statusDisabled && *req.Status != statusEnabled {
			mgmtRequestInvalid(c, "status")
			return
		}
		// 启用 webhook 前必须确认群仍处于 Normal —— 阻断 disband → re-enable 复活路径；
		// 且创建者必须仍在群内 —— 阻断"创建者退群后被第三方复活"的旁路（push 的懒级联
		// 禁用 + 此闸构成 belt & suspenders）。禁用（status=0）始终允许，便于主动关停。
		if *req.Status == statusEnabled {
			g, err := w.requireActiveGroup(groupNo)
			if err != nil {
				w.Error("query group failed", zap.Error(err))
				mgmtQueryFailed(c)
				return
			}
			if g == nil {
				mgmtGroupNotFound(c)
				return
			}
			if !w.requireCreatorInGroup(c, m) {
				return
			}
		}
		fields["status"] = *req.Status
	}
	if len(fields) == 0 {
		c.Response(toResp(m))
		return
	}
	if err := w.db.updateFields(webhookID, fields); err != nil {
		w.Error("update webhook failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}
	// 本实例即时失效缓存：状态/名称/头像变更后，push 路径不得再命中旧快照（禁用尤其
	// 关键——必须及时停推）。跨实例由 TTL 兜底。
	w.webhookCache.invalidate(webhookID)
	updated, qErr := w.db.queryByWebhookID(webhookID)
	if qErr != nil || updated == nil {
		// 回读失败/行消失：无法确认更新结果（可能已落库，也可能因并发软删除而落空），
		// 不返回可能失真的更新前快照，按 5xx 交客户端重试，不谎报成功。
		w.Error("re-read after update failed", zap.Error(qErr))
		mgmtOperationFailed(c)
		return
	}
	// 并发软删除竞态：updateFields 的 status != statusDeleted 守卫保证不会把已删除行的
	// 字段写回（杜绝复活）。若回读到 statusDeleted，说明本次 update 与 DELETE 并发且
	// DELETE 胜出——按 not-found 返回，与"删除即不可再操作"一致。
	if updated.Status == statusDeleted {
		mgmtNotFound(c)
		return
	}
	c.Response(toResp(updated))
}

func (w *IncomingWebhook) delete(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	webhookID := c.Param("webhook_id")
	actor, ok := w.resolveActor(c, groupNo)
	if !ok {
		return
	}
	m, ok := w.queryManageable(c, groupNo, webhookID)
	if !ok {
		return
	}
	// 删除不挂 requireCreatorInGroup：创建者退群后管理员必须仍能清理（#member-perms）。
	if !w.requireOwnership(c, actor, m) {
		return
	}
	if err := w.db.deleteByWebhookID(webhookID); err != nil {
		w.Error("delete webhook failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}
	// 软删后即时失效本实例缓存，push 不得再命中旧的 enabled 快照继续推送。
	w.webhookCache.invalidate(webhookID)
	c.ResponseOK()
}

func (w *IncomingWebhook) regenerate(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	webhookID := c.Param("webhook_id")
	actor, ok := w.resolveActor(c, groupNo)
	if !ok {
		return
	}
	// 与 create / update(启用) 保持一致：群非 Normal 不允许颁发新 token。
	g, err := w.requireActiveGroup(groupNo)
	if err != nil {
		w.Error("query group failed", zap.Error(err))
		mgmtQueryFailed(c)
		return
	}
	if g == nil {
		mgmtGroupNotFound(c)
		return
	}
	m, ok := w.queryManageable(c, groupNo, webhookID)
	if !ok {
		return
	}
	if !w.requireOwnership(c, actor, m) {
		return
	}
	// 创建者已退群的 webhook 不再颁发新 token（它无论如何推不出消息，新 token 只会
	// 造成"为什么一直 401"的排障困惑）。
	if !w.requireCreatorInGroup(c, m) {
		return
	}
	token, hash, err := generateToken()
	if err != nil {
		w.Error("generate token failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}
	if err := w.db.updateFields(webhookID, map[string]interface{}{"token_hash": hash}); err != nil {
		w.Error("update token_hash failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}
	// token 轮换后即时失效本实例缓存：旧 token_hash 不得再被 push 命中（否则旧 token 在
	// 本实例仍可推送）。跨实例 TTL 窗口内旧 token 仍短暂有效，是已接受的 staleness 契约。
	w.webhookCache.invalidate(webhookID)
	// 并发软删除竞态：updateFields 的 status != statusDeleted 守卫保证不会给已删除的
	// webhook 写新 token_hash。回读确认行仍存活，避免向客户端返回一个实际未落库、
	// 指向已删除行的"新 token"。
	updated, qErr := w.db.queryByWebhookID(webhookID)
	if qErr != nil || updated == nil {
		// 回读失败/行消失：token 是否落库无法确认，按 5xx 让客户端重试，不误报 404。
		w.Error("re-read after regenerate failed", zap.Error(qErr))
		mgmtOperationFailed(c)
		return
	}
	if updated.Status == statusDeleted {
		// 与并发 DELETE 竞争且 DELETE 胜出：token_hash 未写入已删除行，按 not-found。
		mgmtNotFound(c)
		return
	}
	c.Response(createResp{
		webhookResp: toResp(updated),
		Token:       token,
		URL:         publicURL(webhookID, token),
		URLs:        publicURLs(webhookID, token),
	})
}

// ============================================================
// 排障 / 测试端点（管理端，群管理员）
// ============================================================

// deliveries 返回条数控制。
const (
	defaultDeliveriesLimit = 50
	maxDeliveriesLimit     = 100
)

// testPushMessage 返回「测试推送」文案，按出站语言本地化。webhook 消息是普通消息体、
// 不走 i18n 错误信封（那是错误响应专用），故采用与 outbound 邮件一致的做法：用
// i18n.OutboundLanguage(ctx) 解析协商语言，再选对应文案。支持矩阵为 en-US / zh-CN
// （默认 zh-CN）。
func testPushMessage(ctx context.Context) string {
	if i18n.OutboundLanguage(ctx) == "en-US" {
		return "✅ Incoming Webhook test message: setup works, the delivery path is live."
	}
	return "✅ Incoming Webhook 测试消息：配置成功，链路已打通。"
}

// deliveries 返回某 webhook 最近的投递记录（成功+失败），供发送方排障。只读、
// 【创建者或群管理员】可见——投递元数据（失败原因/字节数/时间）只与排障相关，
// 其他成员没有查看他人 webhook 投递明细的业务需要；绝不返回 token。失败记录的
// reason/http_status 与 push 路径返回给调用方的响应一致，便于对照定位。
func (w *IncomingWebhook) deliveries(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	webhookID := c.Param("webhook_id")
	actor, ok := w.resolveActor(c, groupNo)
	if !ok {
		return
	}
	// 复用 queryManageable：webhook 必须属于该群且未软删除（跨群/不存在→404）。
	m, ok := w.queryManageable(c, groupNo, webhookID)
	if !ok {
		return
	}
	if !w.requireOwnership(c, actor, m) {
		return
	}
	limit := defaultDeliveriesLimit
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxDeliveriesLimit {
		limit = maxDeliveriesLimit
	}
	list, err := w.db.queryRecentAudits(webhookID, limit)
	if err != nil {
		w.Error("query deliveries failed", zap.String("webhook_id", webhookID), zap.Error(err))
		mgmtQueryFailed(c)
		return
	}
	resps := make([]deliveryResp, 0, len(list))
	for _, a := range list {
		resps = append(resps, deliveryRespFrom(a))
	}
	c.Response(map[string]interface{}{"list": resps})
}

func deliveryRespFrom(a *auditModel) deliveryResp {
	return deliveryResp{
		Status:     a.Status,
		Reason:     a.Reason,
		HTTPStatus: a.HTTPStatus,
		Adapter:    a.Adapter,
		ByteSize:   a.ByteSize,
		MessageID:  a.MessageID,
		CreatedAt:  time.Time(a.CreatedAt).Unix(),
	}
}

// testPush 由创建者或群管理员触发，向群里发一条固定文案的测试消息，端到端验证
// webhook 配置（群可达、消息能投递）。走与正式推送相同的 buildPayload(text) →
// SendMessage 链路，并记一条 adapter=test 的成功投递，便于在 deliveries 里与真实
// 流量区分。
func (w *IncomingWebhook) testPush(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	webhookID := c.Param("webhook_id")
	actor, ok := w.resolveActor(c, groupNo)
	if !ok {
		return
	}
	// 群必须仍为 Normal —— 不向已解散/禁用群发测试消息（与 create/enable 一致）。
	g, err := w.requireActiveGroup(groupNo)
	if err != nil {
		w.Error("query group failed", zap.Error(err))
		mgmtQueryFailed(c)
		return
	}
	if g == nil {
		mgmtGroupNotFound(c)
		return
	}
	m, ok := w.queryManageable(c, groupNo, webhookID)
	if !ok {
		return
	}
	if !w.requireOwnership(c, actor, m) {
		return
	}
	// 与 push 路径的创建者在群闸同口径：创建者已退群的 webhook 连测试消息也不发。
	if !w.requireCreatorInGroup(c, m) {
		return
	}

	msg := testPushMessage(c.Request.Context())
	req := &pushPayloadReq{Content: msg}
	// 测试推送的请求体不含覆盖字段，覆盖判权传 false 即可（展示固定为 webhook 配置）。
	payload := buildPayload(m, req, false)
	ip := clientIP(c.Request)
	resp, err := w.ctx.SendMessageWithResult(&config.MsgSendReq{
		Header:      config.MsgHeader{RedDot: 1},
		ChannelID:   m.GroupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     m.WebhookID,
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		w.Error("send test webhook message failed",
			zap.String("webhook_id", m.WebhookID), zap.Error(err))
		// 记一条 adapter=test 的失败投递，让排障故事对称（成功/失败都可见）。bumpUsed=false。
		w.submitDelivery(&auditModel{
			WebhookID: m.WebhookID, GroupNo: m.GroupNo, IP: ip, ByteSize: len(msg),
			Status: auditFailed, Reason: "delivery_failed",
			HTTPStatus: http.StatusInternalServerError, Adapter: adapterTest,
		}, false)
		mgmtOperationFailed(c)
		return
	}
	var msgID int64
	if resp != nil {
		msgID = resp.MessageID
	}
	// bumpUsed=false：测试推送不计入 call_count / last_used_at（adapter=test 已可区分）。
	w.submitSuccess(m, len(msg), ip, msgID, adapterTest, false)
	c.Response(map[string]interface{}{
		"status":     0,
		"message_id": msgID,
	})
}

// ============================================================
// 推送端点
// ============================================================

// failAuth records a per-IP auth failure (a token-scan signal) then returns the
// uniform 401. Used only on genuine auth-failure branches — unknown/disabled
// webhook, bad token, malformed request — never on server-side (DB) errors or
// post-authentication state failures (valid token, group not Normal), so those
// never penalize the caller's IP.
func (w *IncomingWebhook) failAuth(c *wkhttp.Context, ip string) {
	w.penalizeIPFailure(ip)
	pushUnauthorized(c)
}

// push / pushGitHub / pushWeCom / pushMultica / pushGitLab / pushFeishu 是各推送形态的
// 路由入口，全部走 handlePush 流水线，仅在 adapter（body 解析 / bodyLimit / successExtra）
// 上分叉。
func (w *IncomingWebhook) push(c *wkhttp.Context)        { w.handlePush(c, nativeAdapter) }
func (w *IncomingWebhook) pushGitHub(c *wkhttp.Context)  { w.handlePush(c, githubAdapter) }
func (w *IncomingWebhook) pushWeCom(c *wkhttp.Context)   { w.handlePush(c, wecomAdapter) }
func (w *IncomingWebhook) pushMultica(c *wkhttp.Context) { w.handlePush(c, multicaAdapter) }
func (w *IncomingWebhook) pushGitLab(c *wkhttp.Context)  { w.handlePush(c, gitlabAdapter) }
func (w *IncomingWebhook) pushFeishu(c *wkhttp.Context)  { w.handlePush(c, feishuAdapter) }

func (w *IncomingWebhook) handlePush(c *wkhttp.Context, ad pushAdapter) {
	// 仅用于"鉴权失败才计入"的 per-IP 失败预算（见 failAuth / ipFailureGateMiddleware）。
	// 用 clientIP（信任代理追加的 X-Real-Ip / 最右 XFF），而非 gin c.ClientIP()——后者在
	// wkhttp 的 trust-all-proxies 默认下取最左 XFF（客户端可伪造），会让扫描者每次伪造
	// 新 IP 从而绕过失败预算。
	ip := clientIP(c.Request)

	webhookID := c.Param("webhook_id")
	token := c.Param("token")
	if webhookID == "" || token == "" {
		// 缺参/畸形请求——算作扫描信号，计入 IP 失败预算。
		w.failAuth(c, ip)
		return
	}

	// 1) 查 webhook（cachedQueryByWebhookID 命中即 0 DB 读；未命中回落 db.queryByWebhookID，
	//    后者已把 ErrNotFound 吸收为 nil/nil）
	m, err := w.cachedQueryByWebhookID(webhookID)
	if err != nil {
		// 服务端故障，不是调用方扫描——绝不计入 IP 失败预算（否则 DB 抖动会误封 IP）。
		w.Error("query webhook failed", zap.Error(err))
		pushUnauthorized(c)
		return
	}
	if m == nil || m.Status == statusDeleted {
		// 未知或【已软删除】的 webhook——没有合法调用方会往不存在/已删除的 URL 推送，
		// 是明确的扫描/滥用信号，计入 IP 失败预算（否则一个泄露的已删 URL 可无限刷、
		// 每次一次 DB 读却永不触发失败门）。
		w.failAuth(c, ip)
		return
	}
	if m.Status != statusEnabled {
		// webhook 存在但被【禁用】（statusDisabled）——可能是持有有效 token 的合法调用方
		// 在其 webhook 刚被管理员禁用后继续推送，无法在 token 校验前区分，故对禁用态保留
		// 宽限：【不】计入 IP 失败预算，避免误封共享 IP（响应仍是同一 401，保持反枚举）。
		pushUnauthorized(c)
		return
	}

	// 2) 常量时间比对 token
	expected := hashToken(token)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(m.TokenHash)) != 1 {
		// token 不匹配——鉴权失败信号，计入 IP 失败预算。
		w.failAuth(c, ip)
		return
	}

	// 2.5) 群必须仍处于 Normal —— 兜底 handleGroupDisband 的异步窗口期，
	// 也防止对已解散群继续推送消息。统一返回 401（响应体不区分原因——防探测的主防线；
	// 时序非恒定，仅尽力而为，见 errcode/incomingwebhook.go 注释）。
	g, err := w.cachedRequireActiveGroup(m.GroupNo)
	if err != nil {
		w.Error("query group on push failed",
			zap.String("webhook_id", m.WebhookID), zap.Error(err))
		pushUnauthorized(c)
		return
	}
	if g == nil {
		pushUnauthorized(c)
		return
	}

	// 3) per-webhook 限流；Redis 故障时显式 fail-open，避免 Redis 抖动导致全量推送被拒。
	allowed, err := w.allowPerWebhook(c.Request.Context(), webhookID)
	if err != nil {
		w.warnDegraded("per-webhook rate limit redis failed, fail-open", err)
		allowed = true
	}
	if !allowed {
		// 刻意【不】记 rate_limited 审计：429 + X-RateLimit-*/Retry-After 头已把节流信息
		// 给到调用方；而 rate_limited 是唯一天然高频的失败类型（其余失败都在限流闸之后、
		// 已被 per-webhook 5rps 收住），逐条落审计会在重试风暴时放大 DB 写入与 auditSem 溢出
		// 的 Warn 日志，反噬限流「廉价丢弃」的本意。管理员可从 deliveries 里成功记录的稀疏/
		// 中断间接看出节流（review 跟进）。
		pushRateLimited(c)
		return
	}

	// 3.5) 创建者必须仍是群的内部正常成员（#member-perms）：成员/bot 可自助创建
	// webhook 后，"退群即失效"是该权限模型的安全底线（离开的人不能继续向群里发声）。
	// 退群没有可订阅的跨模块事件，这里即权威闸；闸不过时【懒级联禁用】该 webhook
	// （status→disabled，幂等、不触碰软删除行），让管理列表如实反映失效状态，后续
	// push 在 status 闸即被拒。响应仍是统一 401（持有效 token 的调用方，与群闸同
	// 口径，不计 IP 失败预算、不入审计）。
	//
	// 刻意放在 per-webhook 限流【之后】：缓存只存正向结果，创建者已退群的 webhook 每次
	// push 都会回源 group_member 点读 + 对等实例上重复幂等 disable 写，挂在 5rps 限流闸
	// 之后让这部分 DB 负载继承限流上限（PR #340 review，yujiawei P2#2；闸在 token 鉴权
	// 之后，语义安全）。
	//
	// creatorIsAdmin 顺带取自同一查询：决定 push 请求的 username/avatar_url 展示覆盖
	// 是否生效（见 resolveFromIdentity——创建者当前是管理员才允许覆盖，堵住成员经
	// push 路径绕过管理面前缀/头像限制的冒充旁路，PR #340 review，yujiawei P1）。
	creatorIsMember, creatorIsAdmin, err := w.cachedCreatorMembership(m.GroupNo, m.CreatorUID)
	if err != nil {
		// 服务端故障：拒绝但不禁用（fail closed on push, no destructive write）。
		w.Error("query creator membership on push failed",
			zap.String("webhook_id", m.WebhookID), zap.Error(err))
		pushUnauthorized(c)
		return
	}
	if !creatorIsMember {
		if dErr := w.db.disableEnabledByWebhookID(m.WebhookID); dErr != nil {
			w.Warn("lazy-disable webhook on creator-left failed",
				zap.String("webhook_id", m.WebhookID), zap.Error(dErr))
		}
		w.webhookCache.invalidate(m.WebhookID)
		pushUnauthorized(c)
		return
	}

	// 3.6) 平台 header token 二次校验（目前仅 GitLab 的 X-Gitlab-Token，须等于 URL token）。
	// 此闸在 URL token 已验证、creator 成员资格已确认之后——能到这里说明调用方已持有
	// webhook 真正的密钥，故 header 不匹配是【配置错误】而非枚举探测：落审计
	// （reason=token，byteSize=0，body 尚未读）便于管理员在 deliveries 里定位，返回统一
	// 401（不计 IP 失败预算——调用方持有效 URL token，非攻击者）。
	if ad.verifyToken != nil && !ad.verifyToken(c.Request.Header, token) {
		w.submitFailure(m, 0, ip, ad.name, "token", http.StatusUnauthorized)
		pushUnauthorized(c)
		return
	}

	// 4) 读 body 并按【该形态】的上限拒绝过大请求。LimitReader 多读 1 字节用于判超。
	// native / wecom 是调用方编写的 body（8KiB 足够且应当约束）；github 是平台生成的
	// 事件 JSON，普遍超过 8KiB 且发送方无法修短，用更宽的专属上限（见 pushAdapter.
	// bodyLimit）。此处已通过 token 鉴权 + per-webhook 限流，宽上限不构成放大面。
	limit := ad.bodyLimit()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(limit)+1))
	if err != nil {
		w.submitFailure(m, 0, ip, ad.name, "body", http.StatusBadRequest)
		pushPayloadInvalid(c, "body")
		return
	}
	if len(body) > limit {
		w.submitFailure(m, len(body), ip, ad.name, "too_large", http.StatusRequestEntityTooLarge)
		pushPayloadTooLarge(c)
		return
	}

	// 5) 适配器解析：native 直接反序列化 pushPayloadReq；github/wecom 把平台格式翻译
	//    成等价请求（见 adapter.go），之后三种形态共用同一条构造/投递/审计路径。
	req, skipReason, invalidReason := ad.parse(c.Request.Header, body)
	if invalidReason != "" {
		w.submitFailure(m, len(body), ip, ad.name, invalidReason, http.StatusBadRequest)
		pushPayloadInvalid(c, invalidReason)
		return
	}
	if skipReason != "" {
		// 已接收、刻意不投递（GitHub ping / 渲染子集之外的事件）：返回 200 让平台侧
		// 显示投递成功，落 auditSkipped 让群管理员在 deliveries 里确认链路连通。
		w.submitSkipped(m, len(body), ip, ad.name, skipReason)
		c.Response(successBody(ad, 0, skipReason))
		return
	}

	// 6) 按 msg_type 构造 payload。缺省/"text" 走历史纯文本路径（content 必填，客户端
	//    按 markdown 渲染），完全向后兼容；"richtext" 走图文混排：blocks 翻译为 octo
	//    原生 RichText(=14) 并由 richtext.Validate/Finalize 权威校验。
	var payload map[string]interface{}
	switch strings.ToLower(strings.TrimSpace(req.MsgType)) {
	case "", msgTypeText:
		// "text" 是 content 的别名：content 为空时回退，降低从既有集成迁移的改造成本。
		if req.Content == "" {
			req.Content = req.Text
		}
		if strings.TrimSpace(req.Content) == "" {
			w.submitFailure(m, len(body), ip, ad.name, "content", http.StatusBadRequest)
			pushPayloadInvalid(c, "content")
			return
		}
		// content 语义长度上限（按 rune 计），独立于 8KB 字节 body cap：防止单条消息
		// 正文过长污染所有客户端渲染。超限按 413 拒绝，与 body 超限同语义。
		if utf8.RuneCountInString(req.Content) > maxContentRunes() {
			w.submitFailure(m, len(body), ip, ad.name, "too_large", http.StatusRequestEntityTooLarge)
			pushPayloadTooLarge(c)
			return
		}
		payload = buildPayload(m, req, creatorIsAdmin)
	case msgTypeRichText:
		// 注意：richtext 路径【不】套用纯文本的 maxContentRunes(4000) 语义上限——富文本
		// 由块结构 + 1MB 序列化上限约束，默认 8KB body cap 下不可能逾越。这是与文本路径的
		// 有意不对称（若运维上调 body cap，富文本仍受 1MB 兜底，不会无界）。
		p, err := buildRichTextPayload(m, req, creatorIsAdmin)
		if err != nil {
			// 仅 >1MB 映射 413（与 body/content 超限同语义）；其余结构性非法（空 content /
			// 空 text 块 / 非 http(s) 图片 url / 缺图片宽高 / 未知块类型 / 超块数上限）
			// 一律 400 invalid，reason=blocks 供调用方定位。
			if errors.Is(err, common.ErrRichTextPayloadTooLarge) {
				w.submitFailure(m, len(body), ip, ad.name, "too_large", http.StatusRequestEntityTooLarge)
				pushPayloadTooLarge(c)
				return
			}
			w.submitFailure(m, len(body), ip, ad.name, "blocks", http.StatusBadRequest)
			pushPayloadInvalid(c, "blocks")
			return
		}
		payload = p
	default:
		w.submitFailure(m, len(body), ip, ad.name, "msg_type", http.StatusBadRequest)
		pushPayloadInvalid(c, "msg_type")
		return
	}

	resp, err := w.ctx.SendMessageWithResult(&config.MsgSendReq{
		// RedDot=1 让 webhook 消息触发未读红点和推送，与 botfather/robot 一致。
		Header:      config.MsgHeader{RedDot: 1},
		ChannelID:   m.GroupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		// WebhookID 已经自带 "iwh_" 前缀，这里直接用即可，避免双前缀。
		FromUID: m.WebhookID,
		Payload: []byte(util.ToJson(payload)),
	})
	if err != nil {
		w.Error("send incoming webhook message failed",
			zap.String("webhook_id", m.WebhookID), zap.Error(err))
		w.submitFailure(m, len(body), ip, ad.name, "delivery_failed", http.StatusBadGateway)
		pushDeliveryFailed(c)
		return
	}

	// 7) 异步审计 + markUsed（失败不影响响应），并发受 auditSem 限制
	var msgID int64
	if resp != nil {
		msgID = resp.MessageID
	}
	// 审计用同一可信 IP（clientIP），而非 gin 可伪造的 c.ClientIP()。bumpUsed=true：
	// 真实推送成功累加 call_count / last_used_at。
	w.submitSuccess(m, len(body), ip, msgID, ad.name, true)

	c.Response(successBody(ad, msgID, ""))
}

// 与 create/update 路径的 webhook 名称/头像列长度约束一致，避免 push 路径成为绕过。
const (
	maxFromNameBytes   = 64
	maxFromAvatarBytes = 255
)

// truncateUTF8 在 max 字节处裁剪，回退到上一 rune 边界避免破坏多字节字符。
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for i := max; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	// 兜底：max 落在首个 rune 内部（max < 首 rune 宽度）时无回退边界。
	// 当前 64/255 字节上限远大于任何 rune 宽度，这条不可达；但若未来把上限调到
	// 个位数，返回空串也好过 s[:max] 切出半个 rune。
	return ""
}

// buildPayload 把 webhook 请求映射到群消息 payload。
//   - WuKongIM 只有 Text 类型，所有 webhook 消息都用 Text(1) 投递。
//   - 注入 from.kind=webhook 元信息，便于客户端识别非真实用户消息；
//     客户端可统一按 markdown 渲染 webhook 消息（无 markdown 时退化为纯文本）。
//   - @all/@here 降级为纯文本：调用方写在 content 里的字面量保留，不附 mention 字段。
//
// 安全：
//   - 调用方 req.Extra 一律**丢弃**，不进入持久化 payload。原因：message 模块对
//     顶层 payload 字段（如 visibles / mention / reminder 等）按服务端控制语义解释，
//     让外部 token 持有者写这些字段会绕过群可见性 / 通知策略。如需扩展，请在此处
//     显式列入允许字段（且明确该字段无访问控制语义），不要再走透传。
//   - req.Username / req.AvatarURL 服务端裁剪到 create 侧同样的字节上限。push 路径
//     原本只受 8KB body cap 约束，调用方可塞 KB 级字符串污染所有客户端 from.* 渲染。
func buildPayload(m *incomingWebhookModel, req *pushPayloadReq, allowOverride bool) map[string]interface{} {
	name, avatar := resolveFromIdentity(m, req, allowOverride)
	return map[string]interface{}{
		"type":    int(common.Text),
		"content": req.Content,
		"from": map[string]interface{}{
			"kind":       extraKindValue,
			"webhook_id": m.WebhookID,
			"name":       name,
			"avatar":     avatar,
		},
		// space_id 必须由服务端从 group 表派生，不接受调用方覆盖，
		// 防止 webhook 消息被伪造到其他 Space。
		"space_id": m.SpaceID,
	}
}

// resolveFromIdentity 解析 webhook 消息的展示发送者名/头像。
//
// allowOverride 是 push 请求 Username/AvatarURL 覆盖的判权结果（创建者【当前】是
// 群管理员，取自 cachedCreatorMembership）：
//   - true（管理员创建/持有）：覆盖优先，否则回落到 webhook 自身配置——历史的
//     Slack/GitHub 兼容行为，管理员可信；
//   - false（成员/bot 的 webhook）：覆盖一律【忽略】，固定用存量 Name/Avatar。
//     没有这道闸，管理面的 Webhook- 前缀与头像锁就会被 push 路径整体绕过——成员拿着
//     自己 webhook 的 token 即可以"HR 公告"+任意头像发声（PR #340 review，yujiawei
//     P1：don't ship a half-control）。存量 Name 必然已带前缀（create/update 强制），
//     无需在此重复加工。
//
// 两者都裁剪到与 create 侧一致的字节上限，防止 push 路径成为绕过列长度约束的旁路。
// 文本与富文本两条路径共用此函数，保证 from.* 渲染口径一致。
func resolveFromIdentity(m *incomingWebhookModel, req *pushPayloadReq, allowOverride bool) (name, avatar string) {
	if allowOverride {
		name = req.Username
		avatar = req.AvatarURL
	}
	if name == "" {
		name = m.Name
	}
	if avatar == "" {
		avatar = m.Avatar
	}
	return truncateUTF8(name, maxFromNameBytes), truncateUTF8(avatar, maxFromAvatarBytes)
}

// submitSuccess 记录一次成功投递。adapter 标记来源（native 推送 / test 测试推送）。
// bumpUsed 控制是否累加 call_count / 刷新 last_used_at：native 真实推送为 true，
// 管理端「测试推送」为 false——测试不是真实流量，不应污染管理列表展示的使用量。
func (w *IncomingWebhook) submitSuccess(m *incomingWebhookModel, byteSize int, ip string, msgID int64, adapter string, bumpUsed bool) {
	w.submitDelivery(&auditModel{
		WebhookID:  m.WebhookID,
		GroupNo:    m.GroupNo,
		IP:         ip,
		ByteSize:   byteSize,
		MessageID:  msgID,
		Status:     auditSuccess,
		HTTPStatus: http.StatusOK,
		Adapter:    adapter,
	}, bumpUsed)
}

// submitFailure 记录一次【鉴权通过后】的失败投递（payload 非法/体积过大/投递失败）。
// 不累加调用计数(bumpUsed=false)——call_count 语义是「成功调用次数」。reason/httpStatus
// 与 push 路径返回给调用方的响应保持一致，供 deliveries 端点排障；adapter 标记推送形态。
//
// 刻意【不】覆盖 rate_limited（429）：它在限流闸处直接返回、不入审计——见 push 路径中
// !allowed 分支的说明（天然高频，逐条落库会反噬限流的廉价丢弃）。
//
// ⚠️ 仅在 webhook 已通过 token 鉴权且群为 Normal 之后调用：鉴权失败（未知/错 token/
// 已解散群）绝不落本表，只进 IP 失败预算，维持 push 路径的反枚举不变量。
func (w *IncomingWebhook) submitFailure(m *incomingWebhookModel, byteSize int, ip, adapter, reason string, httpStatus int) {
	w.submitDelivery(&auditModel{
		WebhookID:  m.WebhookID,
		GroupNo:    m.GroupNo,
		IP:         ip,
		ByteSize:   byteSize,
		Status:     auditFailed,
		Reason:     reason,
		HTTPStatus: httpStatus,
		Adapter:    adapter,
	}, false)
}

// submitSkipped 记录一次「已接收、刻意不投递」的结果（GitHub ping / 渲染子集之外的
// 事件类型）。响应是 200，但没有消息进群——单列 auditSkipped 状态而非伪装成成功或
// 失败（语义见 model.go auditSkipped）。bumpUsed=false：call_count 语义是「成功投递
// 次数」。鉴权前置约束与 submitFailure 相同（反枚举不变量）。
func (w *IncomingWebhook) submitSkipped(m *incomingWebhookModel, byteSize int, ip, adapter, reason string) {
	w.submitDelivery(&auditModel{
		WebhookID:  m.WebhookID,
		GroupNo:    m.GroupNo,
		IP:         ip,
		ByteSize:   byteSize,
		Status:     auditSkipped,
		Reason:     reason,
		HTTPStatus: http.StatusOK,
		Adapter:    adapter,
	}, false)
}

// submitDelivery 把审计任务投递给有界并发池：未达上限时异步执行；已达上限时**丢弃**
// 本次审计（仅 Warn）。如此审计占用的 DB 连接总数恒 ≤ auditSem 容量，不会在洪峰下与
// 主流量抢连接池。审计为非关键路径，溢出丢弃优于回落到请求 goroutine 同步执行（后者
// 请求并发无界时会重新压垮连接池）。
func (w *IncomingWebhook) submitDelivery(audit *auditModel, bumpUsed bool) {
	select {
	case w.auditSem <- struct{}{}:
		go func() {
			defer func() { <-w.auditSem }()
			w.recordDelivery(audit, bumpUsed)
		}()
	default:
		// 并发已达上限：丢弃审计，保证总 DB 并发有界、不抢占主流量连接池。
		w.Warn("audit dropped: concurrency cap reached",
			zap.String("webhook_id", audit.WebhookID))
	}
}

// auditWriteTimeout 限定一次审计（markUsed + insertAudit 最多两次写）的总耗时上限。
// recordDelivery 始终跑在独立 goroutine 上（submitDelivery 满载时直接丢弃、不回落到
// 请求 goroutine），所以这个超时**不影响 push 响应延迟**；它的作用是封顶单个 detached
// 审计 goroutine 在 DB 饱和/故障时持有连接池连接的时长，避免慢 DB 下连接被长期占用。
// 3s 足够正常写入，又能在故障时快速放手（审计本就是非关键路径，失败仅记日志）。
const auditWriteTimeout = 3 * time.Second

// recordDelivery 写一条投递审计；bumpUsed 时额外累加调用计数（仅成功路径）。失败仅记
// 日志，不阻塞主流程。
func (w *IncomingWebhook) recordDelivery(audit *auditModel, bumpUsed bool) {
	defer func() {
		if r := recover(); r != nil {
			w.Error("recordDelivery panic", zap.Any("recover", r))
		}
	}()
	// 两次写共用一个截止时间，封顶单个审计 goroutine 在 DB 饱和/故障时持有连接的时长。
	ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
	defer cancel()
	if bumpUsed {
		if err := w.db.markUsed(ctx, audit.WebhookID, time.Now()); err != nil {
			w.Warn("markUsed failed", zap.String("webhook_id", audit.WebhookID), zap.Error(err))
		}
	}
	if err := w.db.insertAudit(ctx, audit); err != nil {
		w.Warn("insert audit failed", zap.String("webhook_id", audit.WebhookID), zap.Error(err))
	}
}

// handleGroupDisband 群解散时禁用所有 webhook（事件 payload 包含 group_no）。
func (w *IncomingWebhook) handleGroupDisband(data []byte, commit config.EventCommit) {
	var req config.MsgGroupDisband
	if err := json.Unmarshal(data, &req); err != nil || req.GroupNo == "" {
		commit(nil) // 忽略错误事件，不阻塞队列
		return
	}
	if err := w.db.disableByGroupNo(req.GroupNo); err != nil {
		w.Warn("disable webhooks on group disband failed",
			zap.String("group_no", req.GroupNo), zap.Error(err))
	}
	// 即时失效本实例的群缓存：下次 push 的 cachedRequireActiveGroup 会 miss → 直查 DB →
	// 群非 Normal → 拒绝。这一道就足以挡住本群所有 webhook（即便它们的 webhook 行缓存仍
	// 短暂 stale 为 enabled，群闸也会拦下），无需按 webhookID 逐条失效。跨实例 TTL 兜底。
	w.groupCache.invalidate(req.GroupNo)
	// 故意 commit(nil)：disable 失败也不重试，避免阻塞事件队列。
	// 异步窗口期由 push 路径的 requireActiveGroup 兜底（belt + suspenders）：
	// 即便此处尚未把 webhook.status 改为 0，推送也会因群非 Normal 而 401。
	commit(nil)
}
