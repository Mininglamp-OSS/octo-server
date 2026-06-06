package common

import (
	"context"
	"encoding/base64"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// Shared SystemSettings instance. EnsureSystemSettings is the single entry
// point — every caller (Common.New, NewManager, modules/user/*, modules/base/
// common.EmailService) goes through it so the in-memory snapshot is shared
// across the process. Otherwise the admin-write Reload would only update one
// instance and other modules would keep serving stale values.
var (
	sharedMu             sync.Mutex
	sharedSystemSettings *SystemSettings
)

// EnsureSystemSettings returns the process-wide SystemSettings instance,
// constructing it on first call. Safe to call from any goroutine.
//
// Failed initial Load is non-fatal: an empty-snapshot instance is stored
// and the background auto-reload (started here) will retry every
// reloadTTL. Until then all getters fall back to yaml — degraded mode,
// not a hard failure. A successful subsequent reload self-heals.
func EnsureSystemSettings(ctx *config.Context) *SystemSettings {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedSystemSettings != nil {
		return sharedSystemSettings
	}
	s := NewSystemSettings(ctx, newSystemSettingDB(ctx))
	if err := s.Load(); err != nil {
		s.Error("initial SystemSettings load failed; auto-reload will retry",
			zap.Error(err))
	}
	// Self-healing in case Load failed above, and multi-instance sync for
	// admin writes on peer servers. Lifetime tied to the process: context.
	// Background is intentional — server has no cancellation handle to
	// thread through here, and the goroutine is harmless to leak at
	// shutdown.
	s.StartAutoReload(context.Background())
	sharedSystemSettings = s
	return sharedSystemSettings
}

// (resetSharedSystemSettingsForTest was removed: octo-lib's
// register.GetModules caches the moduleList with sync.Once for the lifetime
// of a test binary, so the Manager's stored *SystemSettings is bound to
// the first ctx. Resetting the package-level singleton produces a fresh
// instance that the Manager does NOT see, which historically led to
// confusing test failures. Tests should instead reuse the singleton
// captured by NewManager and mutate state through it. See
// TestManagerSystemSetting_BoolEmptyValueResetsToYaml for the pattern.)

// defaultReloadTTL is how often the background goroutine pulls a fresh
// snapshot from system_setting. 60s is the agreed budget for multi-instance
// drift: an admin-side change becomes visible on every server within one TTL.
const defaultReloadTTL = 60 * time.Second

// SystemSettings is the read path for admin-tunable global config.
//
// Lookup model:
//   - Snapshot is an immutable map[string]string ("category.key" → value),
//     swapped atomically by Load / Reload. Readers go through atomic.Pointer
//     and never take a lock; SMTP send (high-frequency) does not block on
//     admin writes.
//   - Empty DB value means "not configured" and falls back to the matching
//     yaml field on *config.Config.
//   - Encrypted values are decrypted at snapshot-build time and cached in
//     plaintext form in the map; the high-frequency read path never calls
//     the cipher. Decryption failure logs an error and skips the entry, so
//     the getter falls back to yaml rather than serving a corrupt value.
type SystemSettings struct {
	ctx       *config.Context
	db        *systemSettingDB
	snapshot  atomic.Pointer[map[string]string]
	reloadTTL time.Duration
	log.Log
}

// NewSystemSettings builds a helper with an empty initial snapshot.
// Callers must invoke Load() once at startup before serving traffic;
// Reload() is safe to call at any time (admin write path uses it).
func NewSystemSettings(ctx *config.Context, db *systemSettingDB) *SystemSettings {
	s := &SystemSettings{
		ctx:       ctx,
		db:        db,
		reloadTTL: defaultReloadTTL,
		Log:       log.NewTLog("SystemSettings"),
	}
	empty := map[string]string{}
	s.snapshot.Store(&empty)
	return s
}

// Load reads every row from system_setting and atomically replaces the
// snapshot. Used at startup and by Reload (which is just an alias for
// "load now" with logging semantics).
func (s *SystemSettings) Load() error {
	rows, err := s.db.listAll()
	if err != nil {
		return err
	}
	next := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ValueType == settingTypeEncrypted {
			if row.Value == "" {
				continue // empty → fall back to yaml
			}
			plaintext, err := decryptKey(row.Value)
			if err != nil {
				s.Error("decrypt system_setting failed; falling back to yaml",
					zap.String("category", row.Category),
					zap.String("key", row.KeyName),
					zap.Error(err))
				continue
			}
			next[schemaKey(row.Category, row.KeyName)] = plaintext
			continue
		}
		next[schemaKey(row.Category, row.KeyName)] = row.Value
	}
	s.snapshot.Store(&next)
	return nil
}

// Reload is the admin-write hook: after the manager API upserts new values
// it calls this so the change is visible on this instance immediately
// (other instances pick it up within reloadTTL).
func (s *SystemSettings) Reload() error {
	return s.Load()
}

// StartAutoReload kicks off a goroutine that re-loads the snapshot every
// reloadTTL until ctx is canceled. Intended to be called once at startup
// (with a long-lived context). Errors are logged but do not stop the loop.
//
// Production callers pass context.Background() — the goroutine therefore
// runs for the lifetime of the process and shuts down with it. The
// ctx.Done() arm exists to make this swappable: if a server-shutdown
// context is ever plumbed through, no code change is needed here. The
// defer ticker.Stop() is reached only on that future cancellation; with
// context.Background() it is unreachable but kept so the function stays
// correct under either invocation.
func (s *SystemSettings) StartAutoReload(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.reloadTTL)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Load(); err != nil {
					s.Error("auto-reload system_setting failed", zap.Error(err))
				}
			}
		}
	}()
}

// ----- generic getters -----

func (s *SystemSettings) lookup(category, key string) (string, bool) {
	// Defensive: NewSystemSettings always seeds a non-nil map, but a
	// zero-value SystemSettings literal (e.g. tests that bypass the
	// constructor) would crash here without this guard.
	snapPtr := s.snapshot.Load()
	if snapPtr == nil {
		return "", false
	}
	v, ok := (*snapPtr)[schemaKey(category, key)]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (s *SystemSettings) getBool(category, key string, fallback bool) bool {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	switch v {
	case "1", "true", "TRUE":
		return true
	case "0", "false", "FALSE":
		return false
	default:
		return fallback
	}
}

func (s *SystemSettings) getString(category, key string, fallback string) string {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	return v
}

func (s *SystemSettings) getInt(category, key string, fallback int) int {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// getIntClamped is getInt with range enforcement: a value outside
// [settingIntMin, settingIntMax] — which the admin write path rejects, but a
// direct DB edit could still introduce — falls back to the code default rather
// than being served verbatim. Defence in depth for the int settings (D-289).
func (s *SystemSettings) getIntClamped(category, key string, fallback int) int {
	v := s.getInt(category, key, fallback)
	if v < settingIntMin || v > settingIntMax {
		return fallback
	}
	return v
}

func (s *SystemSettings) getFloat(category, key string, fallback float64) float64 {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func (s *SystemSettings) getEncrypted(category, key string, fallback string) string {
	// Encrypted values are stored decrypted in the snapshot, so a plain
	// lookup is sufficient. The dedicated method exists so callers — and
	// readers — can see the difference between "stored as encrypted" and
	// "stored as string".
	return s.getString(category, key, fallback)
}

// ----- typed getters (the 7 settings shipped this iteration) -----

// RegisterOff returns whether registration is globally disabled.
// DB value wins over cfg.Register.Off when set.
func (s *SystemSettings) RegisterOff() bool {
	return s.getBool("register", "off", s.ctx.GetConfig().Register.Off)
}

// RegisterOnlyChina returns whether only China-region phone numbers may register.
func (s *SystemSettings) RegisterOnlyChina() bool {
	return s.getBool("register", "only_china", s.ctx.GetConfig().Register.OnlyChina)
}

// RegisterUsernameOn returns whether username-based registration is enabled.
func (s *SystemSettings) RegisterUsernameOn() bool {
	return s.getBool("register", "username_on", s.ctx.GetConfig().Register.UsernameOn)
}

// RegisterEmailOn returns whether email-based registration / login is enabled.
func (s *SystemSettings) RegisterEmailOn() bool {
	return s.getBool("register", "email_on", s.ctx.GetConfig().Register.EmailOn)
}

// LocalLoginOff returns whether local-account login entry points should be
// disabled. When true, frontend hides the local login UI and backend rejects
// requests to /v1/user/login, /v1/user/usernamelogin, /v1/user/emaillogin and
// their companion code-send endpoints. Password-recovery flows and third-party
// /SSO (GitHub, Gitee, OIDC) are not affected — this toggle is meant for
// deployments that have adopted SSO and want to force users through it.
//
// Default false (no yaml fallback): plain self-hosted deployments without DB
// override keep the historical "local login enabled" behavior.
//
// Safety override: even if the DB says local_off=1, this getter returns false
// when no third-party login (OIDC / GitHub / Gitee) is actually configured.
// Without the override an admin who flips the switch before wiring up an IdP
// would lock everyone — including themselves — out of the system. The
// override always picks "open" so the deployment stays accessible while ops
// fixes the missing SSO config. The hazard is surfaced via startup log
// (logLocalLoginOffSafetyOverride) so it isn't silently swallowed.
func (s *SystemSettings) LocalLoginOff() bool {
	if !s.getBool("login", "local_off", false) {
		return false
	}
	return anyThirdPartyLoginConfigured(s.ctx.GetConfig())
}

// anyThirdPartyLoginConfigured reports whether at least one external login
// provider has the credentials it needs to handle a real auth round-trip.
// LocalLoginOff guards on this so flipping the master switch without wiring
// up an IdP can never brick the deployment.
//
// Checked providers:
//   - OIDC: must be enabled AND all hard-required env present (see
//     isOIDCFullyConfigured). DM_OIDC_ENABLED=true alone is insufficient —
//     missing issuer / client_id / etc. makes the callback 4xx/5xx at
//     runtime, effectively no usable SSO.
//   - GitHub: client_id AND client_secret in yaml/env (both required for
//     the OAuth code exchange in api_github.go).
//   - Gitee:  client_id AND client_secret in yaml/env (same shape).
func anyThirdPartyLoginConfigured(cfg *config.Config) bool {
	if isOIDCFullyConfigured() {
		return true
	}
	if cfg.Github.ClientID != "" && cfg.Github.ClientSecret != "" {
		return true
	}
	if cfg.Gitee.ClientID != "" && cfg.Gitee.ClientSecret != "" {
		return true
	}
	return false
}

// oidcProviderIDRe mirrors modules/oidc/config.go:providerIDRe. Kept in sync
// by the reciprocal comments on both sides (see loadProvider's required block).
// A literal duplication, not a regex compiled from a shared string, because
// the alternative (extracting to a leaf package) would touch ~10 files for
// one shared regex; the maintenance cost is one extra place to update if
// the rule ever changes.
var oidcProviderIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// isOIDCFullyConfigured mirrors the fatal checks inside
// modules/oidc/config.go:loadProvider — including the provider-ID regex,
// because an invalid ID makes LoadConfig fail, leaves oidc.cfg=nil, and
// causes the OIDC routes to be registered as 404/disabled at request time.
// Skipping the regex would let local_off=1 + invalid PROVIDER_ID slip past
// the safety override and lock everyone out.
//
// Why duplicated instead of importing modules/oidc:
//   modules/common ← system_settings.go would need to import modules/oidc,
//   but modules/oidc transitively imports modules/user → modules/common,
//   creating a cycle. Extracting oidc.LoadConfig into its own leaf package
//   was considered and rejected as out-of-scope churn for this PR. The
//   trade-off is mirroring the required-env list here; modules/oidc/
//   config.go carries a reciprocal comment so adding a new required env
//   prompts updating both places.
//
// Mirrored requirements (keep in sync with modules/oidc/config.go):
//   - DM_OIDC_ENABLED  parsed by strconv.ParseBool — accepts 1/0/t/T/true/
//     True/TRUE/f/F/false/etc, matching oidc/config.go:getBool exactly.
//     Earlier strings.ToLower-style parsing diverged on "t"/"T".
//   - DM_OIDC_PROVIDER_ID             default "oidc"; must match providerIDRe
//   - DM_OIDC_PROVIDER_ISSUER         (alias DM_OIDC_AEGIS_ISSUER)
//   - DM_OIDC_PROVIDER_CLIENT_ID      (alias DM_OIDC_AEGIS_CLIENT_ID)
//   - DM_OIDC_PROVIDER_CLIENT_SECRET  (alias DM_OIDC_AEGIS_CLIENT_SECRET)
//   - DM_OIDC_PROVIDER_REDIRECT_URI   (alias DM_OIDC_AEGIS_REDIRECT_URI)
//   - DM_OIDC_RT_ENC_KEY              (base64, 32 bytes after decode)
//
// We intentionally do NOT replicate non-fatal checks (scope strings,
// durations) — those don't make LoadConfig fail and don't disable the
// callback path.
func isOIDCFullyConfigured() bool {
	v := os.Getenv("DM_OIDC_ENABLED")
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil || !enabled {
		return false
	}
	required := []struct {
		primary, alias string
	}{
		{"DM_OIDC_PROVIDER_ISSUER", "DM_OIDC_AEGIS_ISSUER"},
		{"DM_OIDC_PROVIDER_CLIENT_ID", "DM_OIDC_AEGIS_CLIENT_ID"},
		{"DM_OIDC_PROVIDER_CLIENT_SECRET", "DM_OIDC_AEGIS_CLIENT_SECRET"},
		{"DM_OIDC_PROVIDER_REDIRECT_URI", "DM_OIDC_AEGIS_REDIRECT_URI"},
	}
	for _, r := range required {
		if os.Getenv(r.primary) == "" && os.Getenv(r.alias) == "" {
			return false
		}
	}
	// Provider ID: empty falls back to "oidc" (matches loadProvider default),
	// non-empty must satisfy the same regex or LoadConfig fails fatally.
	providerID := os.Getenv("DM_OIDC_PROVIDER_ID")
	if providerID == "" {
		providerID = "oidc"
	}
	if !oidcProviderIDRe.MatchString(providerID) {
		return false
	}
	// RT key must base64-decode to 32 bytes (AES-256). Just non-empty is not
	// enough — oidc/config.go rejects wrong-length keys at boot, our guard
	// should be at least as strict so a deployment that would fail to boot
	// can't be marked "configured".
	keyB64 := os.Getenv("DM_OIDC_RT_ENC_KEY")
	if keyB64 == "" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return false
	}
	return true
}

// LogLocalLoginOffSafetyOverrideIfActive emits a single error-level log entry
// when local_off is intended to be on but no third-party login is configured —
// the exact state where LocalLoginOff() silently returns false to keep the
// deployment from locking itself. The log is the only signal ops have that
// the admin's intent is currently being overridden; without it the
// inconsistency is invisible until someone wonders why local login still
// works after flipping the switch.
//
// Why localOff is a parameter, not read from snapshot here:
//   Callers know the intended value with stronger guarantees than the
//   shared snapshot. The manager-write path can pass the just-validated
//   request value (independent of whether Reload succeeded — PR #104 P2
//   from yujiawei). Startup passes the freshly-loaded snapshot value.
//   Reading the snapshot directly inside this method would silently miss
//   the warning when Reload fails right after a write, exactly when ops
//   most needs the signal.
//
// Callers: invoke once at server startup (Common.Route) after Load
// completes, and from the manager update handler after a write that
// touched login.local_off (passing the plan's value).
func (s *SystemSettings) LogLocalLoginOffSafetyOverrideIfActive(localOff bool) {
	if !localOff {
		return
	}
	if anyThirdPartyLoginConfigured(s.ctx.GetConfig()) {
		return
	}
	s.Error("login.local_off=1 但未配置任何第三方登录 (OIDC / GitHub / Gitee); " +
		"已自动回退为允许本地登录,避免锁死;请尽快补齐第三方登录配置后再开启此开关")
}

// RawLocalLoginOffFromSnapshot returns the snapshot's raw DB value for
// login.local_off without applying the SSO-safety override. Used by callers
// that need to feed LogLocalLoginOffSafetyOverrideIfActive at startup (the
// snapshot has just been loaded, so freshness isn't a concern). Exposed
// publicly because the field-level `getBool` is package-private and the
// only external need is this one logging path.
func (s *SystemSettings) RawLocalLoginOffFromSnapshot() bool {
	return s.getBool("login", "local_off", false)
}

// envSpaceDisableUserCreate 与 modules/space/api.go:envDisableUserCreateSpace
// 保持同名,镜像在 common 包以避免反向依赖 (space 已 import common)。新增/修改
// env 解析规则时两处同步,语义就是: 1/true/yes/on (任意大小写,允许前后空格)
// 视为 ON，其余皆 OFF。
const envSpaceDisableUserCreate = "DM_SPACE_DISABLE_USER_CREATE"

// SpaceDisableUserCreate reports whether the user-facing「创建空间」入口应被
// 关闭。完整 fallback 链(按优先级):
//
//	1. DB 行存在且 value 非空 → 走 getBool 解析(1/true/TRUE → true;
//	   0/false/FALSE → false; 未知字面量 → false)。**不再回退到 env** —— 与
//	   其他 bool 设置一致,未知字面量等同 "admin 不希望关闭"。
//	2. DB 行不存在,或 value="" → env DM_SPACE_DISABLE_USER_CREATE
//	3. 都缺失 → false (保持开放)
//
// 注：manager 写接口对 bool 值已做规范化(只接受 0/1/true/false 及大小写
// 变体),正常路径不会出现未知字面量;此规则覆盖的是有人绕过 API 直接改 DB
// 的边缘场景。
//
// DB 是单一真源：admin 在管理台显式 toggle 立刻生效（Reload 内存快照），
// 多实例 60s 内收敛。env 仅作历史部署兼容入口；新部署应直接走 system_setting。
//
// 与 modules/space/api.go:IsUserCreateDisabled 保持等价语义 —— 后者仍是
// env-only 的低层解析器,留给没有 ctx 的调用方与 yaml 模式;实际请求路径走本
// 方法（modules/space/api.go:createSpace）。
//
// 实现细节：DB 路径委托给 getBool 以与其他 bool 设置共享解析规则,避免双写
// 字面量集合(reviewer H1)。"DB 行是否存在"由独立 lookup 决定,从而区分
// "DB 缺行 → env" 与 "DB 值=0 → 强制 false 压制 env" 两个语义。
func (s *SystemSettings) SpaceDisableUserCreate() bool {
	if _, ok := s.lookup("space", "disable_user_create"); ok {
		// 走与所有其他 bool 设置一致的字面量解析;未知字面量会落到 fallback=false,
		// 与 "DB 显式写了 0" 语义一致 —— 都视为 admin 不希望关闭。
		return s.getBool("space", "disable_user_create", false)
	}
	return parseSpaceDisableUserCreateEnv(os.Getenv(envSpaceDisableUserCreate))
}

// parseSpaceDisableUserCreateEnv 与 modules/space/api.go:IsUserCreateDisabled
// 的解析逻辑保持一致(1/true/yes/on,大小写不敏感,允许前后空格)。两处镜像而
// 非提到 leaf package,理由同 LocalLoginOff/OIDC: 一个 helper 不值得为它引
// 入一层新包。修改任何一处时两边同步,否则同一开关在两个出口语义会漂移。
func parseSpaceDisableUserCreateEnv(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ----- sidebar recent-tab activity filter (issue #289) -----

// SidebarRecentFilterGroupDays returns the recent-tab activity window for group
// conversations, in days. 0 disables the window (all groups returned). Defaults
// to defaultSidebarRecentFilterGroupDays (3) — today's hard-coded behaviour.
func (s *SystemSettings) SidebarRecentFilterGroupDays() int {
	return s.getIntClamped("sidebar", "recent_filter_group_days", defaultSidebarRecentFilterGroupDays)
}

// SidebarRecentFilterThreadDays returns the recent-tab activity window for
// thread (community topic) conversations, in days. 0 disables the window.
func (s *SystemSettings) SidebarRecentFilterThreadDays() int {
	return s.getIntClamped("sidebar", "recent_filter_thread_days", defaultSidebarRecentFilterThreadDays)
}

// SidebarRecentFilterPersonDays returns the recent-tab activity window for DM
// conversations, in days. Defaults to 0, which keeps today's "DMs are always
// shown regardless of age" behaviour; the per-type default makes the historical
// hard-coded `!isDM` exemption data-driven.
func (s *SystemSettings) SidebarRecentFilterPersonDays() int {
	return s.getIntClamped("sidebar", "recent_filter_person_days", defaultSidebarRecentFilterPersonDays)
}

// SupportEmail returns the From address used by the SMTP sender.
func (s *SystemSettings) SupportEmail() string {
	return s.getString("support", "email", s.ctx.GetConfig().Support.Email)
}

// SupportEmailSmtp returns the SMTP host:port endpoint.
func (s *SystemSettings) SupportEmailSmtp() string {
	return s.getString("support", "email_smtp", s.ctx.GetConfig().Support.EmailSmtp)
}

// SupportEmailPwd returns the (decrypted) SMTP password. If the stored
// ciphertext fails to decrypt at Load time, the snapshot omits the key and
// this getter returns the yaml fallback.
func (s *SystemSettings) SupportEmailPwd() string {
	return s.getEncrypted("support", "email_pwd", s.ctx.GetConfig().Support.EmailPwd)
}

// ----- incomingwebhook settings (总开关 + 核心阈值) -----
//
// 这些 env 名 / 默认值是 modules/incomingwebhook 的「单一真源」：incomingwebhook 侧
// 通过下面的 getter 读取（不再各自读 env），从而让 system_setting 的 effective_value
// 能反映完整的 DB → env → code-default 回退链。修改 env 名或默认值时，需同步
// systemSettingSchema 的 incomingwebhook 行；reciprocal sync 注释见
// modules/incomingwebhook/api.go 的 New / allowPerWebhook / create。
const (
	envIncomingWebhookEnabled         = "DM_INCOMINGWEBHOOK_ENABLED"
	envIncomingWebhookPerWebhookRPS   = "DM_INCOMINGWEBHOOK_RPS"
	envIncomingWebhookPerWebhookBurst = "DM_INCOMINGWEBHOOK_BURST"
	envIncomingWebhookMaxPerGroup     = "DM_INCOMINGWEBHOOK_MAX_PER_GROUP"

	defaultIncomingWebhookEnabled         = true
	defaultIncomingWebhookPerWebhookRPS   = 5.0
	defaultIncomingWebhookPerWebhookBurst = 10
	defaultIncomingWebhookMaxPerGroup     = 10
)

// IncomingWebhookEnabled 是群入站 Webhook 功能的总开关。关闭后 push 端点返回 404、
// 管理写操作（create/update/delete/regenerate）被拒绝，仅保留 list 只读。
// 回退链：DB → env(DM_INCOMINGWEBHOOK_ENABLED) → 默认开启(true)。
func (s *SystemSettings) IncomingWebhookEnabled() bool {
	return s.getBool("incomingwebhook", "enabled", incomingWebhookEnabledEnvDefault())
}

// IncomingWebhookPerWebhookRPS 单个 webhook 令牌桶速率(rps)。DB → env → 默认 5。
//
// 读侧防御（D-289 同型，覆盖直接改库的旁路）：rps 必须是正有限值；NaN/±Inf/≤0 一律
// 回退到 env/默认。否则 allowPerWebhook 的 `rps<=0` 短路会把限流器静默关掉，NaN 还会
// 让 Redis Lua 脚本报错而 fail-open——正是这个 getter 要兜住的。写侧也已拒绝
// （settingTypeFloat + Positive，见 api_manager_system_setting.go），此处是纵深防御。
func (s *SystemSettings) IncomingWebhookPerWebhookRPS() float64 {
	// env fallback 同样消毒：wkhttp.ParseRPSFromEnv 用 strconv.ParseFloat，会接受
	// NaN / +Inf（DM_INCOMINGWEBHOOK_RPS=NaN 原样透出），所以 def 本身可能非有限。
	// 若 env 给出非有限/≤0 的 def，回退到永远合法的 code default，避免它穿过下面的
	// clamp 继续把 NaN 喂给限流器（Jerry-Xin #292 review）。
	def := wkhttp.ParseRPSFromEnv(envIncomingWebhookPerWebhookRPS, defaultIncomingWebhookPerWebhookRPS)
	if math.IsNaN(def) || math.IsInf(def, 0) || def <= 0 {
		def = defaultIncomingWebhookPerWebhookRPS
	}
	v := s.getFloat("incomingwebhook", "per_webhook_rps", def)
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return def
	}
	return v
}

// IncomingWebhookPerWebhookBurst 单个 webhook 令牌桶突发上限。DB → env → 默认 10。
// 读侧防御：≤0 回退默认（同 RPS，避免 `burst<=0` 短路静默关掉限流器）。
func (s *SystemSettings) IncomingWebhookPerWebhookBurst() int {
	def := wkhttp.ParseBurstFromEnv(envIncomingWebhookPerWebhookBurst, defaultIncomingWebhookPerWebhookBurst)
	v := s.getInt("incomingwebhook", "per_webhook_burst", def)
	if v <= 0 {
		return def
	}
	return v
}

// IncomingWebhookMaxPerGroup 单个群可创建的 webhook 数量上限。DB → env → 默认 10。
// 读侧防御：≤0 回退默认（max_per_group=0 会让每次 create 都 ErrQuotaExceeded，是
// 总开关之外一种更难诊断的「暗关」）。
func (s *SystemSettings) IncomingWebhookMaxPerGroup() int {
	def := incomingWebhookMaxPerGroupEnvDefault()
	v := s.getInt("incomingwebhook", "max_per_group", def)
	if v <= 0 {
		return def
	}
	return v
}

// incomingWebhookEnabledEnvDefault 解析 DM_INCOMINGWEBHOOK_ENABLED（缺省/无法识别
// 视为开启），作为 DB 未配置时的 fallback。比 getBool 的 DB 解析更宽松，接受
// 1/0/true/false/yes/no/on/off（大小写不敏感、允许前后空格）。
func incomingWebhookEnabledEnvDefault() bool {
	v := strings.TrimSpace(os.Getenv(envIncomingWebhookEnabled))
	if v == "" {
		return defaultIncomingWebhookEnabled
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return defaultIncomingWebhookEnabled
}

// incomingWebhookMaxPerGroupEnvDefault 解析 DM_INCOMINGWEBHOOK_MAX_PER_GROUP；仅
// 接受正整数，否则回退默认值（语义与迁移前 modules/incomingwebhook.maxPerGroup 一致）。
func incomingWebhookMaxPerGroupEnvDefault() int {
	if v := os.Getenv(envIncomingWebhookMaxPerGroup); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultIncomingWebhookMaxPerGroup
}
