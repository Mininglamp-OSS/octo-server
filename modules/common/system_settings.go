package common

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
	// stickerClampWarned 去重 clamp getter 的越界 Warn(review R6)。key 形如
	// "sticker.upload_max_size_kb=99999>5120",同一 (key, 越界值) 在进程周期
	// 内只 log 一次;admin 改到别的越界值会重新 log 一条。避免读侧热路径
	// 刷屏,同时保留 operator 可观测性。
	stickerClampWarned sync.Map
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
	return parseSettingBool(v, fallback)
}

// parseSettingBool applies the canonical system_setting bool literal rules
// (1/true/TRUE → true, 0/false/FALSE → false, anything else → fallback).
// Shared by getBool and the atomic SpaceWelcomeConfig reader so both spell the
// parse the same way.
func parseSettingBool(v string, fallback bool) bool {
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
//
//	modules/common ← system_settings.go would need to import modules/oidc,
//	but modules/oidc transitively imports modules/user → modules/common,
//	creating a cycle. Extracting oidc.LoadConfig into its own leaf package
//	was considered and rejected as out-of-scope churn for this PR. The
//	trade-off is mirroring the required-env list here; modules/oidc/
//	config.go carries a reciprocal comment so adding a new required env
//	prompts updating both places.
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
//
//	Callers know the intended value with stronger guarantees than the
//	shared snapshot. The manager-write path can pass the just-validated
//	request value (independent of whether Reload succeeded — PR #104 P2
//	from yujiawei). Startup passes the freshly-loaded snapshot value.
//	Reading the snapshot directly inside this method would silently miss
//	the warning when Reload fails right after a write, exactly when ops
//	most needs the signal.
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
//  1. DB 行存在且 value 非空 → 走 getBool 解析(1/true/TRUE → true;
//     0/false/FALSE → false; 未知字面量 → false)。**不再回退到 env** —— 与
//     其他 bool 设置一致,未知字面量等同 "admin 不希望关闭"。
//  2. DB 行不存在,或 value="" → env DM_SPACE_DISABLE_USER_CREATE
//  3. 都缺失 → false (保持开放)
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
	envIncomingWebhookMaxPerCreator   = "DM_INCOMINGWEBHOOK_MAX_PER_CREATOR"
	// 控制非管理员成员创建的 webhook 是否可用广播型 @（@所有人 / @所有 AI）。
	envIncomingWebhookMemberCanBroadcast = "OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST"

	defaultIncomingWebhookEnabled            = true
	defaultIncomingWebhookPerWebhookRPS      = 5.0
	defaultIncomingWebhookPerWebhookBurst    = 10
	defaultIncomingWebhookMaxPerGroup        = 10
	defaultIncomingWebhookMaxPerCreator      = 5
	defaultIncomingWebhookMemberCanBroadcast = true
)

// IncomingWebhookEnabled 是群入站 Webhook 功能的总开关。关闭后 push 端点返回 404、
// 管理写操作（create/update/delete/regenerate）被拒绝，仅保留 list 只读。
// 回退链：DB → env(DM_INCOMINGWEBHOOK_ENABLED) → 默认开启(true)。
func (s *SystemSettings) IncomingWebhookEnabled() bool {
	return s.getBool("incomingwebhook", "enabled", incomingWebhookEnabledEnvDefault())
}

// IncomingWebhookMemberCanBroadcast 控制【非管理员成员】创建的 webhook 是否可使用广播型
// @（@所有人 / @所有 AI）。关闭后，成员建的 webhook 即便已置 allow_mention_* 能力位，其
// 广播也在 push 读路径被剥离（mention_ignored 回报）；【管理员创建】的 webhook 不受影响。
// 因为是 push 读侧 AND（参见 incomingwebhook.buildMention），翻此开关可【即时收回】全部成员
// 广播、无需迁移存量列。回退链：DB → env(OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST) → 默认开启(true)。
func (s *SystemSettings) IncomingWebhookMemberCanBroadcast() bool {
	return s.getBool("incomingwebhook", "member_can_broadcast", incomingWebhookMemberCanBroadcastEnvDefault())
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

// incomingWebhookMemberCanBroadcastEnvDefault 解析 OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST
// （缺省/无法识别视为开启），作为 DB 未配置时的 fallback。语义同 incomingWebhookEnabledEnvDefault。
func incomingWebhookMemberCanBroadcastEnvDefault() bool {
	v := strings.TrimSpace(os.Getenv(envIncomingWebhookMemberCanBroadcast))
	if v == "" {
		return defaultIncomingWebhookMemberCanBroadcast
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return defaultIncomingWebhookMemberCanBroadcast
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

// IncomingWebhookMaxPerCreator 单个普通成员/bot 在一个群内可创建的 webhook 数量
// 上限（群主/管理员豁免，仅受群级 max_per_group 约束）。DB → env → 默认 5。
// 读侧防御：≤0 回退默认（同 max_per_group，避免误配成"任何成员都建不了"的暗关）。
func (s *SystemSettings) IncomingWebhookMaxPerCreator() int {
	def := incomingWebhookMaxPerCreatorEnvDefault()
	v := s.getInt("incomingwebhook", "max_per_creator", def)
	if v <= 0 {
		return def
	}
	return v
}

// incomingWebhookMaxPerCreatorEnvDefault 解析 DM_INCOMINGWEBHOOK_MAX_PER_CREATOR；
// 仅接受正整数，否则回退默认值。
func incomingWebhookMaxPerCreatorEnvDefault() int {
	if v := os.Getenv(envIncomingWebhookMaxPerCreator); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultIncomingWebhookMaxPerCreator
}

// ---------------------------------------------------------------------------
// App Bot auth cache (issue #309)
// ---------------------------------------------------------------------------

const (
	// defaultAppBotAuthCacheTTLSeconds is the safety-net expiry (seconds) for the
	// shared Redis App Bot auth cache. Revocation is instant via the shared DEL;
	// this TTL only bounds drift / the narrow re-populate race (see
	// modules/bot_api/registry_redis.go). 60s keeps the worst-case staleness
	// window (a failed DEL, or the re-populate race) tight while still serving
	// active tokens from cache between DB re-validations. Kept in sync with
	// defaultAppBotAuthCacheTTL in modules/bot_api/registry_redis.go.
	defaultAppBotAuthCacheTTLSeconds = 60
	// appBotAuthCacheTTLMinSeconds / Max bound an admin override to a sane window
	// (does not use getIntClamped, whose [0,3650] range is tuned for "days").
	// Revocation propagates instantly via the shared tombstone, so this TTL is only
	// an orphan / failed-revocation-write backstop — the max is kept tight (10 min)
	// so a misconfiguration can't widen the worst-case revoked-token window.
	appBotAuthCacheTTLMinSeconds = 30
	appBotAuthCacheTTLMaxSeconds = 600
)

// AppBotAuthCacheTTLSeconds is the safety-net TTL (seconds) written with each
// shared App Bot auth cache key. Read from system_setting (category "app_bot",
// key "auth_cache_ttl_seconds"); hot-reloaded with the rest of the snapshot, so
// an operator can retune it without a deploy. Out-of-range values fall back to
// the code default rather than being served verbatim (defence in depth).
func (s *SystemSettings) AppBotAuthCacheTTLSeconds() int {
	v := s.getInt("app_bot", "auth_cache_ttl_seconds", defaultAppBotAuthCacheTTLSeconds)
	if v < appBotAuthCacheTTLMinSeconds || v > appBotAuthCacheTTLMaxSeconds {
		return defaultAppBotAuthCacheTTLSeconds
	}
	return v
}

// ---------------------------------------------------------------------------
// Custom stickers (modules/sticker)
// ---------------------------------------------------------------------------

// defaultStickerUserMaxCount is the per-user custom-sticker cap when the admin
// has not overridden system_setting sticker.user_max_count. Admin-tunable via
// POST /v1/manager/common/system_setting; hot-reloaded with the snapshot.
const defaultStickerUserMaxCount = 100

// StickerUserMaxCount is the maximum number of custom stickers a single user may
// keep. Read-side defence: a non-positive value (only reachable via a direct DB
// edit — the admin write path enforces Positive) falls back to the default
// rather than silently locking the user out of adding any sticker.
func (s *SystemSettings) StickerUserMaxCount() int {
	v := s.getInt("sticker", "user_max_count", defaultStickerUserMaxCount)
	if v <= 0 {
		return defaultStickerUserMaxCount
	}
	return v
}

// StickerCustomEnabled reports whether clients should show the custom-sticker
// management entry. This is a presentation toggle only; server-side CRUD
// authorization remains governed by the /v1/sticker/user route middleware and
// handler checks. Default false supports a controlled client rollout.
func (s *SystemSettings) StickerCustomEnabled() bool {
	return s.getBool("sticker", "custom_enabled", false)
}

// StickerHandleRequired reports whether custom-sticker registration must reject a
// missing upload handle (POST /v1/sticker/user). This is the enforcement POLICY,
// deliberately independent of the signing CAPABILITY (OCTO_MASTER_KEY): it lives
// in system_setting (DB, hot-reloaded) so it can be toggled from the admin
// console and converge across replicas within the snapshot TTL — a gradual,
// reversible rollout without a redeploy/restart. Default false (backward
// compatible: missing handles are allowed through during the compat window and
// only recorded). See modules/sticker classifyStickerPath and the appconfig
// sticker_handle_required bit.
func (s *SystemSettings) StickerHandleRequired() bool {
	return s.getBool("sticker", "handle_required", false)
}

// DocsEnabled reports whether clients should surface the docs module (backed by
// the new octo-docs-backend service). This is a presentation toggle only: it
// gates client-side display of the docs entry and does not itself grant or
// enforce any server-side authorization. Default false so the module stays
// hidden until octo-docs-backend is live and the admin flips docs.enabled for a
// controlled rollout. Value source: system_setting docs.enabled (DB, hot-reloaded).
func (s *SystemSettings) DocsEnabled() bool {
	return s.getBool("docs", "enabled", false)
}

// DmloopEnabled reports whether the Loop(回路)module entry should be shown to
// clients. Default false — the loop feature (backend service + fleet proxy +
// daemon runtimes) stays hidden until ops flips dmloop.enabled after those deps
// are deployed. Display policy only; /fleet auth lives in the backend.
func (s *SystemSettings) DmloopEnabled() bool {
	return s.getBool("dmloop", "enabled", false)
}

// DmpersonalEnabled reports whether the「我的/运行时」(personal) module entry
// should be shown. Kept separate from dmloop.enabled because 我的 will be
// redesigned to decouple from loop and may roll out on its own schedule.
func (s *SystemSettings) DmpersonalEnabled() bool {
	return s.getBool("dmpersonal", "enabled", false)
}

// ---------------------------------------------------------------------------
// Custom-sticker upload constraints + optional server-side compression
// (sticker-upload-compression task).
//
// These formerly-hard-coded numbers (modules/file/const.go: StickerMaxFileSize
// = 1MB, StickerMaxDimension = 512, stickerUploadExts) become operator-tunable
// through system_setting so a bad configuration can be greyed out / rolled back
// without a redeploy. Every int key has a server-side HARD CAP that read-side
// clamp getters enforce even against a direct DB edit — the admin write path
// already rejects non-positive ints via Positive:true; these clamps are defence
// in depth against the "someone edits the row by hand" case.
//
// stickerUploadRasterAllowlist mirrors modules/file/const.go:stickerUploadExts
// verbatim. Duplicated intentionally to keep modules/common a leaf (modules/file
// already imports modules/common; reversing would cycle). Keep in sync — the
// upload_allowed_formats getter uses this list as the outer bound the config
// may only narrow from.
// ---------------------------------------------------------------------------

const (
	defaultStickerUploadMaxSizeKB = 1024
	stickerUploadMaxSizeKBHardCap = 5 * 1024

	defaultStickerUploadMaxDimension = 512
	stickerUploadMaxDimensionHardCap = 1024

	// StickerUploadMaxDimensionHardCap is the exported alias of the decoded-pixel
	// dimension hard cap (== stickerUploadMaxDimensionHardCap). modules/file
	// references it so the compressible-accept ceiling shares this single source
	// of truth rather than re-declaring a bare 1024 literal (review finding: a
	// hand-synced duplicate could silently drift and re-widen the bomb gate).
	StickerUploadMaxDimensionHardCap = stickerUploadMaxDimensionHardCap

	defaultStickerCompressEnabled = false

	defaultStickerCompressTargetKB = 1024
	stickerCompressTargetKBHardCap = 5 * 1024

	defaultStickerCompressMaxConcurrency = 4
	stickerCompressMaxConcurrencyHardCap = 32

	defaultStickerCompressTimeoutMs = 2000
	stickerCompressTimeoutMsHardCap = 10000

	// defaultStickerCompressMaxDimension is the shrink target the compressor
	// downscales static jpg/png into. 512 makes ">512 shrinks to 512" the built-in
	// behavior once compression is enabled. Its hard cap is
	// stickerUploadMaxDimensionHardCap (1024) — shrink target and compressible-accept
	// ceiling share the decoded-pixel bound.
	defaultStickerCompressMaxDimension = 512
)

// stickerUploadRasterAllowlist 与 modules/file/const.go:stickerUploadExts 保持一致。
// 用于 upload_allowed_formats 配置的读侧交集：管理台只能收窄，不能加入非位图。
// 若 modules/file 侧改动允许扩展名，此列表也需同步。
var stickerUploadRasterAllowlist = []string{".gif", ".png", ".jpg", ".jpeg", ".webp"}

// stickerClampIntUpper clamps an int getter to [1, hardCap]. Any value ≤0 or
// non-numeric (which surface as fallback default from getInt) is served as
// default; values above hardCap are clamped to hardCap; everything else is
// returned verbatim. Shared by every KB/px/ms/count sticker upload setting so
// the clamp policy is single-sourced.
//
// key is the fully qualified setting name (e.g. "sticker.upload_max_size_kb");
// when v exceeds hardCap this method emits a per-(key, v) one-shot Warn so a
// bad admin edit is operator-observable without spamming the read hot path
// (review R6). Admin fixes → new越界 value or in-range value → new Warn or
// silence, matching human-friendly signal semantics.
func (s *SystemSettings) stickerClampIntUpper(key string, v, fallback, hardCap int) int {
	if v <= 0 {
		return fallback
	}
	if v > hardCap {
		dedupKey := fmt.Sprintf("%s=%d>%d", key, v, hardCap)
		if _, loaded := s.stickerClampWarned.LoadOrStore(dedupKey, struct{}{}); !loaded {
			s.Warn("system_setting sticker knob exceeds hard cap; clamped",
				zap.String("key", key),
				zap.Int("configured", v),
				zap.Int("hard_cap", hardCap))
		}
		return hardCap
	}
	return v
}

// StickerUploadMaxSizeKB returns the per-file upload cap in KB. Read-side
// clamped to [1, stickerUploadMaxSizeKBHardCap]; out-of-range falls back to
// the historical 1024 KB default.
func (s *SystemSettings) StickerUploadMaxSizeKB() int {
	return s.stickerClampIntUpper("sticker.upload_max_size_kb",
		s.getInt("sticker", "upload_max_size_kb", defaultStickerUploadMaxSizeKB),
		defaultStickerUploadMaxSizeKB,
		stickerUploadMaxSizeKBHardCap,
	)
}

// StickerUploadMaxDimension returns the decoded-pixel single-edge cap. Read-side
// clamped to [1, stickerUploadMaxDimensionHardCap]; out-of-range falls back to
// the historical 512-px default.
func (s *SystemSettings) StickerUploadMaxDimension() int {
	return s.stickerClampIntUpper("sticker.upload_max_dimension",
		s.getInt("sticker", "upload_max_dimension", defaultStickerUploadMaxDimension),
		defaultStickerUploadMaxDimension,
		stickerUploadMaxDimensionHardCap,
	)
}

// StickerUploadAllowedFormats returns the sanitized set of allowed extensions
// (each including the leading dot, lowercased). It is intersected with the
// built-in raster allowlist (stickerUploadRasterAllowlist) so a mis-config can
// only narrow — never widen to non-raster (mp4/pdf/svg/...). If the config
// exists but the intersection is empty (all tokens illegal), the FULL default
// set is returned instead of an empty slice so a bad config cannot "dark-close"
// the feature; deployments narrow explicitly by writing a valid CSV.
//
// Order of returned slice is deterministic for stability of callers that log
// or index it; tests sort before comparing regardless.
func (s *SystemSettings) StickerUploadAllowedFormats() []string {
	raw, ok := s.lookup("sticker", "upload_allowed_formats")
	if !ok {
		out := make([]string, len(stickerUploadRasterAllowlist))
		copy(out, stickerUploadRasterAllowlist)
		return out
	}
	allowlist := make(map[string]struct{}, len(stickerUploadRasterAllowlist))
	for _, e := range stickerUploadRasterAllowlist {
		allowlist[e] = struct{}{}
	}
	seen := make(map[string]struct{}, len(stickerUploadRasterAllowlist))
	out := make([]string, 0, len(stickerUploadRasterAllowlist))
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		if !strings.HasPrefix(tok, ".") {
			tok = "." + tok
		}
		if _, ok := allowlist[tok]; !ok {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	if len(out) == 0 {
		out = make([]string, len(stickerUploadRasterAllowlist))
		copy(out, stickerUploadRasterAllowlist)
	}
	return out
}

// StickerCompressEnabled reports whether server-side compression of static
// sticker images (jpg/png) is turned on. Default false — the feature is
// opt-in, greyed out until an operator flips this bit.
func (s *SystemSettings) StickerCompressEnabled() bool {
	return s.getBool("sticker", "compress_enabled", defaultStickerCompressEnabled)
}

// StickerCompressTargetKB returns the post-compression target size in KB.
// Read-side clamped to [1, stickerCompressTargetKBHardCap]; out-of-range falls
// back to the 1024 KB default.
func (s *SystemSettings) StickerCompressTargetKB() int {
	return s.stickerClampIntUpper("sticker.compress_target_kb",
		s.getInt("sticker", "compress_target_kb", defaultStickerCompressTargetKB),
		defaultStickerCompressTargetKB,
		stickerCompressTargetKBHardCap,
	)
}

// StickerCompressMaxConcurrency returns the process-wide cap on concurrent
// sticker compressions. Read-side clamped to [1, stickerCompressMaxConcurrencyHardCap];
// out-of-range falls back to 4.
func (s *SystemSettings) StickerCompressMaxConcurrency() int {
	return s.stickerClampIntUpper("sticker.compress_max_concurrency",
		s.getInt("sticker", "compress_max_concurrency", defaultStickerCompressMaxConcurrency),
		defaultStickerCompressMaxConcurrency,
		stickerCompressMaxConcurrencyHardCap,
	)
}

// StickerCompressTimeoutMs returns the per-compression timeout in milliseconds.
// Read-side clamped to [1, stickerCompressTimeoutMsHardCap]; out-of-range falls
// back to 2000ms.
func (s *SystemSettings) StickerCompressTimeoutMs() int {
	return s.stickerClampIntUpper("sticker.compress_timeout_ms",
		s.getInt("sticker", "compress_timeout_ms", defaultStickerCompressTimeoutMs),
		defaultStickerCompressTimeoutMs,
		stickerCompressTimeoutMsHardCap,
	)
}

// StickerCompressMaxDimension returns the target single-edge length the
// compressor downscales static jpg/png INTO (sticker-downscale-store /
// sticker-oversized-default). It is the SHRINK target the compressor's
// imaging.Fit fits into, decoupled from the upload dimension gate.
//
// Read-side clamped to [1, stickerUploadMaxDimensionHardCap] (the 1024
// decoded-pixel hard cap — the compressible-accept ceiling and the shrink target
// share that bound); unset / ≤0 / non-numeric falls back to the 512 default,
// which makes ">512 static jpg/png shrinks to 512" the built-in behavior once
// compression is enabled. NOT tied to upload_max_dimension: the dimension gate
// admits compressible formats up to the hard cap (see modules/file
// effectiveGateDim) and this value only decides how far they are shrunk before
// store.
func (s *SystemSettings) StickerCompressMaxDimension() int {
	return s.stickerClampIntUpper("sticker.compress_max_dimension",
		s.getInt("sticker", "compress_max_dimension", defaultStickerCompressMaxDimension),
		defaultStickerCompressMaxDimension,
		stickerUploadMaxDimensionHardCap,
	)
}

// ---------------------------------------------------------------------------
// Space new-user welcome (onboarding.space_welcome_*) — task
// space-new-user-welcome-message
// ---------------------------------------------------------------------------

const (
	// spaceWelcomeCategory is the system_setting category for the onboarding
	// welcome keys.
	spaceWelcomeCategory = "onboarding"
	// spaceWelcomeMessageMaxRunes bounds the welcome body in Unicode code points
	// (validated on the manager write path and re-validated at runtime).
	spaceWelcomeMessageMaxRunes = 2000
)

// SpaceWelcomeConfig is an atomic, point-in-time view of the four
// onboarding.space_welcome_* settings. All fields are read from the SAME
// SystemSettings snapshot in one access, so a caller can never straddle a
// background Reload() and combine values from two different snapshots. The
// event handler, send worker, reconciler and the manager write path all rely
// on this atomicity — reading the keys individually would risk an inconsistent
// combination.
//
// Message is a single plain-text body sent to every recipient (no per-language
// split); it may contain line breaks (\n preserved verbatim; clients render
// type:1 text with newlines) but no markdown.
type SpaceWelcomeConfig struct {
	Enabled       bool
	SpaceID       string
	ActiveFromRaw string
	Message       string
}

// SpaceWelcomeConfig returns the current tuple, all read from one snapshot.
func (s *SystemSettings) SpaceWelcomeConfig() SpaceWelcomeConfig {
	snapPtr := s.snapshot.Load()
	get := func(key string) string {
		if snapPtr == nil {
			return ""
		}
		return (*snapPtr)[schemaKey(spaceWelcomeCategory, key)]
	}
	return SpaceWelcomeConfig{
		Enabled:       parseSettingBool(get("space_welcome_enabled"), false),
		SpaceID:       get("space_welcome_space_id"),
		ActiveFromRaw: get("space_welcome_active_from"),
		Message:       get("space_welcome_message"),
	}
}

// ParsedActiveFrom parses ActiveFromRaw as RFC3339 and returns it in UTC.
// ok is false when the value is empty or unparseable — callers treat that as an
// invalid combination (fail closed) when the feature is enabled.
func (c SpaceWelcomeConfig) ParsedActiveFrom() (time.Time, bool) {
	if c.ActiveFromRaw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, c.ActiveFromRaw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// validWelcomeMessage reports whether the body is non-empty after trim and
// within the code-point limit. Internal newlines are preserved (TrimSpace only
// strips leading/trailing whitespace), so multi-line plain-text bodies pass.
func validWelcomeMessage(msg string) bool {
	if strings.TrimSpace(msg) == "" {
		return false
	}
	return utf8.RuneCountInString(msg) <= spaceWelcomeMessageMaxRunes
}

// ValidateSpaceWelcomeCombination validates the tuple as a coherent combination.
//
//   - When disabled it always passes: a partial or empty config is fine while
//     the feature is off.
//   - When enabled it requires a non-empty space_id that isActiveSpace confirms
//     exists and is not dissolved, a parseable RFC3339 active_from, and a
//     message non-empty (after trim) within spaceWelcomeMessageMaxRunes.
//
// The (field, err) contract lets the caller distinguish a validation failure
// (err == nil, field != "" naming the first offending key) from an
// infrastructure error (err != nil, e.g. the space lookup DB read failed). A
// nil isActiveSpace skips only the space existence check — used by callers that
// cannot reach the DB or want the pure static checks.
func ValidateSpaceWelcomeCombination(cfg SpaceWelcomeConfig, isActiveSpace func(spaceID string) (bool, error)) (field string, err error) {
	if !cfg.Enabled {
		return "", nil
	}
	if strings.TrimSpace(cfg.SpaceID) == "" {
		return "space_welcome_space_id", nil
	}
	if _, ok := cfg.ParsedActiveFrom(); !ok {
		return "space_welcome_active_from", nil
	}
	if !validWelcomeMessage(cfg.Message) {
		return "space_welcome_message", nil
	}
	if isActiveSpace != nil {
		active, checkErr := isActiveSpace(cfg.SpaceID)
		if checkErr != nil {
			return "space_welcome_space_id", checkErr
		}
		if !active {
			return "space_welcome_space_id", nil
		}
	}
	return "", nil
}
