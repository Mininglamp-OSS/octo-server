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

// New 构造路由模块。
func New(ctx *config.Context) *IncomingWebhook {
	w := &IncomingWebhook{
		ctx:       ctx,
		Log:       log.NewTLog("IncomingWebhook"),
		db:        newDB(ctx),
		groupDB:   group.NewDB(ctx),
		rateRedis: sharedRateRedis(ctx.GetConfig()),
		auditSem:  make(chan struct{}, auditConcurrency()),
		floor:     newLocalFloor(),
		settings:  commonmod.EnsureSystemSettings(ctx),
	}
	// 群解散级联禁用所有 webhook
	w.ctx.AddEventListener(event.GroupDisband, w.handleGroupDisband)
	return w
}

// Route 注册路由。
func (w *IncomingWebhook) Route(r *wkhttp.WKHttp) {
	// 管理类：登录用户 + 群管理员校验。认证路由默认挂 SharedUIDRateLimiter（须在
	// AuthMiddleware 之后，否则读不到 uid 会静默 fail-open），与全局 IP floor 叠加，
	// 给 create/regenerate 等敏感写操作补 per-login-user 限流。
	mgr := r.Group("/v1/groups", w.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, w.ctx))
	{
		// 总开关(system_setting incomingwebhook.enabled)关闭时，写操作一律 403 拒绝，
		// 仅保留 list 只读——运维仍可查看/排查已存在配置。requireMgmtEnabled 不挂在 list 上。
		mgr.POST("/:group_no/incoming-webhooks", w.requireMgmtEnabled(), w.create)
		mgr.GET("/:group_no/incoming-webhooks", w.list)
		mgr.PUT("/:group_no/incoming-webhooks/:webhook_id", w.requireMgmtEnabled(), w.update)
		mgr.DELETE("/:group_no/incoming-webhooks/:webhook_id", w.requireMgmtEnabled(), w.delete)
		mgr.POST("/:group_no/incoming-webhooks/:webhook_id/regenerate", w.requireMgmtEnabled(), w.regenerate)
	}

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
		push.POST("/incoming-webhooks/:webhook_id/:token", w.requirePushEnabled(), w.localFloorMiddleware(), ipLimit, w.ipFailureGateMiddleware(), w.push)
	}
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

// requireGroupAdmin 校验登录用户是否为群主或管理员，是则返回 (loginUID, true)；
// 否则已写入 4xx 响应。
func (w *IncomingWebhook) requireGroupAdmin(c *wkhttp.Context, groupNo string) (string, bool) {
	loginUID := c.MustGet("uid").(string)
	ok, err := w.groupDB.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		w.Error("query group manager failed", zap.Error(err))
		mgmtQueryFailed(c)
		return "", false
	}
	if !ok {
		mgmtForbidden(c)
		return "", false
	}
	return loginUID, true
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
	loginUID, ok := w.requireGroupAdmin(c, groupNo)
	if !ok {
		return
	}

	var req createReq
	if err := c.BindJSON(&req); err != nil {
		mgmtRequestInvalid(c, "body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		mgmtRequestInvalid(c, "name")
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

	m := &incomingWebhookModel{
		WebhookID:  generateWebhookID(),
		TokenHash:  hash,
		GroupNo:    groupNo,
		SpaceID:    g.SpaceID,
		Name:       req.Name,
		Avatar:     req.Avatar,
		CreatorUID: loginUID,
		Status:     statusEnabled,
	}
	// 配额校验 + 写入在事务内原子完成；FOR UPDATE 锁住 group_no 范围，防止并发越限。
	//
	// TOCTOU 说明：requireActiveGroup 的 status 检查是 insert 事务之前的非事务读，
	// 事务内仅靠 group 行锁串行化、不重查 status。极小窗口内群被解散仍可能写入一条
	// status=1 的行，但这**不构成安全问题**：该 webhook 永远推不出消息——push 路径的
	// requireActiveGroup 重查才是权威闸（群非 Normal 一律 401），且 disband 级联会把
	// status 翻 0。故此处不在事务内重读 group.status，避免给热路径加锁负担。
	maxWH := w.settings.IncomingWebhookMaxPerGroup()
	if err := w.db.insertWithQuota(m, maxWH); err != nil {
		if errors.Is(err, ErrQuotaExceeded) {
			mgmtQuotaExceeded(c, maxWH)
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
	}
	c.Response(resp)
}

func (w *IncomingWebhook) list(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	if _, ok := w.requireGroupAdmin(c, groupNo); !ok {
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
	if _, ok := w.requireGroupAdmin(c, groupNo); !ok {
		return
	}

	m, ok := w.queryManageable(c, groupNo, webhookID)
	if !ok {
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
		if name == "" || len(name) > 64 {
			mgmtRequestInvalid(c, "name")
			return
		}
		fields["name"] = name
	}
	if req.Avatar != nil {
		fields["avatar"] = *req.Avatar
	}
	if req.Status != nil {
		// 仅接受启用/禁用；statusDeleted(2) 不可经 update 设置——删除只能走 DELETE
		// 端点（软删除），update 也不能复活已删除行（见下方 queryManageable）。
		if *req.Status != statusDisabled && *req.Status != statusEnabled {
			mgmtRequestInvalid(c, "status")
			return
		}
		// 启用 webhook 前必须确认群仍处于 Normal —— 阻断 disband → re-enable 复活路径。
		// 禁用（status=0）始终允许，便于管理员主动关停。
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
	if _, ok := w.requireGroupAdmin(c, groupNo); !ok {
		return
	}
	if _, ok := w.queryManageable(c, groupNo, webhookID); !ok {
		return
	}
	if err := w.db.deleteByWebhookID(webhookID); err != nil {
		w.Error("delete webhook failed", zap.Error(err))
		mgmtOperationFailed(c)
		return
	}
	c.ResponseOK()
}

func (w *IncomingWebhook) regenerate(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	webhookID := c.Param("webhook_id")
	if _, ok := w.requireGroupAdmin(c, groupNo); !ok {
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
	if _, ok := w.queryManageable(c, groupNo, webhookID); !ok {
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

func (w *IncomingWebhook) push(c *wkhttp.Context) {
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

	// 1) 查 webhook（queryByWebhookID 已把 ErrNotFound 吸收为 nil/nil）
	m, err := w.db.queryByWebhookID(webhookID)
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
	g, err := w.requireActiveGroup(m.GroupNo)
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
		pushRateLimited(c)
		return
	}

	// 4) 读 body 并按统一上限拒绝过大请求。LimitReader 多读 1 字节用于判超。
	limit := maxBytes()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(limit)+1))
	if err != nil {
		pushPayloadInvalid(c, "body")
		return
	}
	if len(body) > limit {
		pushPayloadTooLarge(c)
		return
	}

	var req pushPayloadReq
	if err := json.Unmarshal(body, &req); err != nil {
		pushPayloadInvalid(c, "json")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		pushPayloadInvalid(c, "content")
		return
	}
	// content 语义长度上限（按 rune 计），独立于 8KB 字节 body cap：防止单条消息正文
	// 过长污染所有客户端渲染。超限按 413 拒绝，与 body 超限同语义。
	if utf8.RuneCountInString(req.Content) > maxContentRunes() {
		pushPayloadTooLarge(c)
		return
	}

	// 5) 构造 payload 并发送
	payload := buildPayload(m, &req)
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
		pushDeliveryFailed(c)
		return
	}

	// 6) 异步审计 + markUsed（失败不影响响应），并发受 auditSem 限制
	var msgID int64
	if resp != nil {
		msgID = resp.MessageID
	}
	// 审计用同一可信 IP（clientIP），而非 gin 可伪造的 c.ClientIP()。
	w.submitRecordSuccess(m, len(body), ip, msgID)

	c.Response(map[string]interface{}{
		"status":     0,
		"message_id": msgID,
	})
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
func buildPayload(m *incomingWebhookModel, req *pushPayloadReq) map[string]interface{} {
	name := req.Username
	if name == "" {
		name = m.Name
	}
	avatar := req.AvatarURL
	if avatar == "" {
		avatar = m.Avatar
	}
	name = truncateUTF8(name, maxFromNameBytes)
	avatar = truncateUTF8(avatar, maxFromAvatarBytes)
	return map[string]interface{}{
		"type":    int(common.Text),
		"content": req.Content,
		"from": map[string]interface{}{
			"kind":       "webhook",
			"webhook_id": m.WebhookID,
			"name":       name,
			"avatar":     avatar,
		},
		// space_id 必须由服务端从 group 表派生，不接受调用方覆盖，
		// 防止 webhook 消息被伪造到其他 Space。
		"space_id": m.SpaceID,
	}
}

// submitRecordSuccess 把审计任务投递给有界并发池：未达上限时异步执行；已达上限时
// **丢弃**本次审计（仅 Warn）。如此审计占用的 DB 连接总数恒 ≤ auditSem 容量，不会在
// 洪峰下与主流量抢连接池。审计为非关键路径，溢出丢弃优于回落到请求 goroutine 同步执行
// （后者请求并发无界时会重新压垮连接池）。
func (w *IncomingWebhook) submitRecordSuccess(m *incomingWebhookModel, byteSize int, ip string, msgID int64) {
	select {
	case w.auditSem <- struct{}{}:
		go func() {
			defer func() { <-w.auditSem }()
			w.recordSuccess(m, byteSize, ip, msgID)
		}()
	default:
		// 并发已达上限：丢弃审计，保证总 DB 并发有界、不抢占主流量连接池。
		w.Warn("audit dropped: concurrency cap reached",
			zap.String("webhook_id", m.WebhookID))
	}
}

// auditWriteTimeout 限定一次审计（markUsed + insertAudit 两次写）的总耗时上限。
// recordSuccess 始终跑在独立 goroutine 上（submitRecordSuccess 满载时直接丢弃、不回落
// 到请求 goroutine），所以这个超时**不影响 push 响应延迟**；它的作用是封顶单个 detached
// 审计 goroutine 在 DB 饱和/故障时持有连接池连接的时长，避免慢 DB 下连接被长期占用。
// 3s 足够正常写入，又能在故障时快速放手（审计本就是非关键路径，失败仅记日志）。
const auditWriteTimeout = 3 * time.Second

// recordSuccess 写审计 + 累加调用计数。失败仅记日志，不阻塞主流程。
func (w *IncomingWebhook) recordSuccess(m *incomingWebhookModel, byteSize int, ip string, msgID int64) {
	defer func() {
		if r := recover(); r != nil {
			w.Error("recordSuccess panic", zap.Any("recover", r))
		}
	}()
	// 两次写共用一个截止时间，封顶单个审计 goroutine 在 DB 饱和/故障时持有连接的时长。
	ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
	defer cancel()
	if err := w.db.markUsed(ctx, m.WebhookID, time.Now()); err != nil {
		w.Warn("markUsed failed", zap.String("webhook_id", m.WebhookID), zap.Error(err))
	}
	audit := &auditModel{
		WebhookID: m.WebhookID,
		GroupNo:   m.GroupNo,
		IP:        ip,
		ByteSize:  byteSize,
		MessageID: msgID,
	}
	if err := w.db.insertAudit(ctx, audit); err != nil {
		w.Warn("insert audit failed", zap.String("webhook_id", m.WebhookID), zap.Error(err))
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
	// 故意 commit(nil)：disable 失败也不重试，避免阻塞事件队列。
	// 异步窗口期由 push 路径的 requireActiveGroup 兜底（belt + suspenders）：
	// 即便此处尚未把 webhook.status 改为 0，推送也会因群非 Normal 而 401。
	commit(nil)
}
