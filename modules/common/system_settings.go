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
	commonbase "github.com/Mininglamp-OSS/octo-server/modules/base/common"
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
// Failed initial Load is non-fatal: the background auto-reload (started here)
// retries every reloadTTL. Ordinary getters fall back to yaml while the
// snapshot is nil; security gates such as ScanLoginEnabled fail closed until a
// successful load publishes the first snapshot. A subsequent reload self-heals.
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

// ManagerEmailMFAState is deliberately tri-state. A nil snapshot means the
// database settings have never loaded successfully and must not be treated as
// the safe-looking "off" default by a manager login gate.
type ManagerEmailMFAState uint8

const (
	ManagerEmailMFAUnavailable ManagerEmailMFAState = iota
	ManagerEmailMFAOff
	ManagerEmailMFAOn
)

// SystemSettings is the read path for admin-tunable global config.
//
// Lookup model:
//   - Snapshot is an immutable map[string]string ("category.key" → value),
//     swapped atomically by Load / Reload. Generic readers go through the
//     atomic.Pointer; the MFA readiness gate additionally takes a small
//     publication lock so its snapshot and probe result stay paired.
//   - Empty DB value means "not configured" and falls back to the matching
//     yaml field on *config.Config.
//   - Encrypted values are decrypted at snapshot-build time and cached in
//     plaintext form in the map; the high-frequency read path never calls
//     the cipher. Decryption failure logs an error and skips the entry, so
//     the getter falls back to yaml rather than serving a corrupt value.
type SystemSettings struct {
	ctx      *config.Context
	db       *systemSettingDB
	snapshot atomic.Pointer[map[string]string]
	// managerMFAProbe* records the last real SMTP preflight for the effective
	// manager-console MFA configuration.  A syntactically valid configuration
	// is not enough for the login gate: startup may have observed a real SMTP
	// failure and must keep management login fail-closed until a later probe
	// succeeds.  The pair is reset only when one of the relevant snapshot
	// values changes, so the 60s ordinary settings reload does not create a
	// needless login outage on every tick.
	managerMFAProbeMu    sync.Mutex
	managerMFAProbeKnown bool
	managerMFAProbeReady bool
	// managerMFAProbeGeneration changes whenever the effective MFA/SMTP
	// snapshot changes. Async probes carry this generation so a result from an
	// older snapshot cannot publish readiness for newer settings.
	managerMFAProbeGeneration uint64
	managerMFAProbeInFlight   atomic.Bool
	reloadTTL                 time.Duration
	// stickerClampWarned 去重 clamp getter 的越界 Warn(review R6)。key 形如
	// "sticker.upload_max_size_kb=99999>5120",同一 (key, 越界值) 在进程周期
	// 内只 log 一次;admin 改到别的越界值会重新 log 一条。避免读侧热路径
	// 刷屏,同时保留 operator 可观测性。
	stickerClampWarned sync.Map
	log.Log
}

// NewSystemSettings builds a helper with an uninitialized snapshot. A nil
// snapshot distinguishes "the DB successfully returned no rows" from "the DB
// has never been read successfully". Callers must invoke Load() once at startup
// before serving traffic; Reload() is safe at any time.
func NewSystemSettings(ctx *config.Context, db *systemSettingDB) *SystemSettings {
	return &SystemSettings{
		ctx:       ctx,
		db:        db,
		reloadTTL: defaultReloadTTL,
		Log:       log.NewTLog("SystemSettings"),
	}
}

// Load reads every row from system_setting and atomically replaces the
// snapshot. It is the no-probe load used during startup and by write paths
// that already validated the prospective configuration synchronously.
func (s *SystemSettings) Load() error {
	_, err := s.loadWithGeneration(false)
	return err
}

// loadWithGeneration replaces the snapshot and returns the generation that
// was published with it. The manager-settings write path uses that generation
// to bind its prospective SMTP probe to the loaded snapshot.
func (s *SystemSettings) loadWithGeneration(reprobe bool) (uint64, error) {
	rows, err := s.db.listAll()
	if err != nil {
		return 0, err
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
	s.managerMFAProbeMu.Lock()
	previous := s.snapshot.Load()
	changed := previous == nil || managerMFASettingsChanged(previous, &next)
	if changed {
		s.managerMFAProbeGeneration++
		s.managerMFAProbeKnown = false
		s.managerMFAProbeReady = false
	}
	// Clear readiness before publishing a changed snapshot. This keeps the
	// login gate fail-closed during the small publication window instead of
	// allowing a probe for the previous settings to authorize the new ones.
	s.snapshot.Store(&next)
	generation := s.managerMFAProbeGeneration
	s.managerMFAProbeMu.Unlock()
	if reprobe && previous != nil && changed && s.ManagerEmailMFAState() == ManagerEmailMFAOn {
		s.scheduleManagerEmailMFAPreflight()
	}
	return generation, nil
}

func managerMFASettingsChanged(previous, next *map[string]string) bool {
	if previous == nil || next == nil {
		return true
	}
	for _, key := range []string{
		"login.manager_email_mfa_on",
		"support.email",
		"support.email_smtp",
		"support.email_pwd",
	} {
		if (*previous)[key] != (*next)[key] {
			return true
		}
	}
	return false
}

// Reload refreshes the snapshot and, when manager MFA/SMTP values changed,
// schedules one generation-bound SMTP preflight. This covers direct DB
// changes followed by Reload as well as instances that observe a peer change
// through the automatic reload loop. The manager settings write path uses
// Load instead because it already probes the merged prospective values before
// committing the transaction.
func (s *SystemSettings) Reload() error {
	_, err := s.loadWithGeneration(true)
	return err
}

// StartAutoReload kicks off a goroutine that re-loads the snapshot every
// reloadTTL until ctx is canceled. 当前系统每 60 秒会执行一次自动 reload，
// 发现配置变化后更新本地快照。Intended to be called once at startup
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
				if _, err := s.loadWithGeneration(true); err != nil {
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

// SettingBoolOK reports the configured bool for (category, key) and whether the
// key is configured at all.
//
// getBool collapses "unset" and "explicitly false" into the same `false`, which
// is fine for a two-tier (DB → yaml) key but wrong for a caller that layers its
// own override on top of this one: a per-bot resolver must be able to tell
// "the deployment default says false" from "the deployment has no opinion, fall
// through to the code default". Hence the second return.
//
// An unparseable literal reports configured=false so the caller falls through
// rather than inheriting a silent false — same fail-forward posture getBool
// takes with its fallback.
//
// The literal is trimmed and matched case-insensitively, which getBool's
// parseSettingBool is not (round-3 evaluation P2-3). The reason is the fallback
// direction, not taste: this tier's callers layer their own code default on top,
// and for every key that uses it today (the bot card switches) that default is
// `true`. So an operator typing `False`, `FALSE ` or `\tfalse` into a
// system_setting row does not get "no opinion, keep the code default" in some
// harmless sense — they get **the capability they meant to disable left on**,
// with no error anywhere. Tolerant lexing removes the class.
//
// Deliberately the same *vocabulary* as parseSettingBool (1/0/true/false), only
// lexed more forgivingly. Accepting on/off/yes/no here would make one column
// mean different things to two readers of the same table — the divergence
// pattern this module is meant to avoid, not a convenience.
//
// **This tier is best-effort, not enforcement.** EnsureSystemSettings tolerates
// a failed startup Load() and leaves an empty snapshot for the auto-reload to
// repair, and an empty snapshot is indistinguishable here from "key not
// configured" — so a replica whose startup load blipped reports configured=false
// and its caller falls through to the code default for up to one reload TTL.
// A caller that layers a fail-closed control on top (see modules/robot's
// bot_setting resolver) fails closed only in ITS own tier; do not read that
// posture as covering this one. An operator who needs a capability genuinely
// disabled should set it at the layer that enforces, not only here.
func (s *SystemSettings) SettingBoolOK(category, key string) (value bool, configured bool) {
	v, ok := s.lookup(category, key)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	default:
		return false, false
	}
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

// ScanLoginEnabled returns whether QR-code login is enabled deployment-wide.
//
// Default false permits a server-first rollout without breaking clients that do
// not yet send poll_secret on redemption. Operators must explicitly enable scan
// login only after every client in the flow supports the hardened contract.
// Before the first successful settings load the snapshot is nil and this gate
// also fails closed; later reload failures retain the last successful snapshot.
func (s *SystemSettings) ScanLoginEnabled() bool {
	if s.snapshot.Load() == nil {
		return false
	}
	return s.getBool("login", "scan_enabled", false)
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

// ----- thread auto-archive policy (task inactive-hiding-user-control / P1) -----

// ThreadAutoArchiveEnabled reports whether the 子区 auto-archive worker should
// sweep. Resolution chain: system_setting row → env
// DM_THREAD_AUTO_ARCHIVE_ENABLED → code default (false).
//
// The env layer is what makes this migration a no-op on rollout: no row is
// written to system_setting, so every deployment keeps resolving to exactly the
// value its env already produces. An admin write later overrides it, and the
// worker picks that up within one tick.
func (s *SystemSettings) ThreadAutoArchiveEnabled() bool {
	return s.getBool("thread", "auto_archive_enabled", threadAutoArchiveEnabledFromEnv())
}

// ThreadAutoArchiveDays returns the inactivity threshold, in days, after which
// a quiet 子区 is archived. 0 disables the time threshold while leaving the
// worker enabled — the env semantics this key inherits (RunOnce short-circuits
// on Threshold<=0), preserved deliberately so existing
// DM_THREAD_AUTO_ARCHIVE_DAYS=0 deployments keep their meaning.
func (s *SystemSettings) ThreadAutoArchiveDays() int {
	return s.getIntClamped("thread", "auto_archive_days", threadAutoArchiveDaysFromEnv())
}

// ThreadArchiveOrdering is the merged view the two-stage-decay guard validates:
// the global archive window versus the recent-tab thread window.
type ThreadArchiveOrdering struct {
	ArchiveEnabled bool
	ArchiveDays    int
	RecentDays     int
}

// ThreadArchiveOrdering snapshots the currently-effective ordering inputs so a
// partial admin write can be validated against merge(current, incoming).
func (s *SystemSettings) ThreadArchiveOrdering() ThreadArchiveOrdering {
	return ThreadArchiveOrdering{
		ArchiveEnabled: s.ThreadAutoArchiveEnabled(),
		ArchiveDays:    s.ThreadAutoArchiveDays(),
		RecentDays:     s.SidebarRecentFilterThreadDays(),
	}
}

// ApplyThreadArchiveOrderingOverlay merges an incoming admin batch onto the
// current snapshot, producing the state that will be in effect once the batch
// commits. Keys absent from `incoming` keep their current value.
//
// The subtle part is the empty payload. For settingTypeInt an empty value is
// the documented "reset to default" (the getter treats an empty snapshot entry
// as not-configured), and normaliseBool accepts "" for bools with the same
// meaning. Carrying the CURRENT value forward for those would under-detect:
// clearing thread.auto_archive_days while it sits above the recent window
// resets it to env/code default and can land below the window — precisely the
// state the guard exists to reject. So "" resolves through the same
// env → code-default chain the getters will use after the write.
//
// Exported and pure so the merge can be unit-tested without an HTTP round-trip.
func ApplyThreadArchiveOrderingOverlay(cur ThreadArchiveOrdering, incoming map[string]string) ThreadArchiveOrdering {
	if v, ok := incoming["thread.auto_archive_enabled"]; ok {
		if v == "" {
			cur.ArchiveEnabled = threadAutoArchiveEnabledFromEnv()
		} else {
			cur.ArchiveEnabled = v == "1"
		}
	}
	if v, ok := incoming["thread.auto_archive_days"]; ok {
		if v == "" {
			cur.ArchiveDays = threadAutoArchiveDaysFromEnv()
		} else if n, err := strconv.Atoi(v); err == nil {
			cur.ArchiveDays = n
		}
	}
	if v, ok := incoming["sidebar.recent_filter_thread_days"]; ok {
		if v == "" {
			// 该键无 env 层，重置即回到代码默认。
			cur.RecentDays = defaultSidebarRecentFilterThreadDays
		} else if n, err := strconv.Atoi(v); err == nil {
			cur.RecentDays = n
		}
	}
	return cur
}

// ViolatesThreadArchiveOrdering reports whether a configuration would make the
// recent-tab thread window unobservable.
//
// The invariant is ArchiveDays >= RecentDays, but only where both windows
// actually bite:
//   - archiving disabled, or ArchiveDays == 0 (threshold disabled): nothing is
//     ever archived on a timer, so no window can be shadowed — always valid.
//   - RecentDays == 0: the recent-tab window is off; there is nothing to
//     shadow — always valid.
//
// Exported and pure so the guard can be unit-tested without an HTTP round-trip.
func ViolatesThreadArchiveOrdering(o ThreadArchiveOrdering) bool {
	if !o.ArchiveEnabled || o.ArchiveDays <= 0 || o.RecentDays <= 0 {
		return false
	}
	return o.ArchiveDays < o.RecentDays
}

// threadAutoArchiveEnabledFromEnv mirrors the literal rules the thread module's
// LoadArchiveConfig used before this key moved into system_setting: only
// "true"/"1" (case-insensitive, trimmed) enable; empty or unparseable stays
// disabled.
func threadAutoArchiveEnabledFromEnv() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(envThreadAutoArchiveEnabled)))
	if raw == "" {
		return defaultThreadAutoArchiveEnabled
	}
	return raw == "true" || raw == "1"
}

// threadAutoArchiveDaysFromEnv mirrors the legacy thread parseDays **exactly**:
// empty / unparseable / negative fall back to the code default, 0 is a
// legitimate "disable the threshold" value, and any other non-negative integer
// is honoured verbatim — including values above settingIntMax.
//
// The upper bound deliberately does NOT apply here (PR #679 review, Jerry-Xin).
// An earlier revision clamped the env layer too, on a defence-in-depth
// argument, but that silently reinterprets an existing deployment's config:
// DM_THREAD_AUTO_ARCHIVE_DAYS=9999 means "effectively never archive", and
// folding it to the 3-day default on rollout would mass-archive long-lived
// threads — the exact opposite of this migration's byte-identical-rollout
// guarantee. The env layer is a compatibility shim for config that already
// exists in production, so it must not change meaning.
//
// The [settingIntMin, settingIntMax] bound still applies to admin/DB-supplied
// values, where an operator gets an explicit rejection (the manager write path)
// or a documented fallback (getIntClamped) rather than a silent
// reinterpretation.
func threadAutoArchiveDaysFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(envThreadAutoArchiveDays))
	if raw == "" {
		return defaultThreadAutoArchiveDays
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultThreadAutoArchiveDays
	}
	return n
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

// ManagerEmailMFAState returns the security state used only by management
// console login. A successfully loaded snapshot with no row means the
// feature's documented default-off state; an uninitialized snapshot is
// unavailable and therefore fail-closed.
func (s *SystemSettings) ManagerEmailMFAState() ManagerEmailMFAState {
	if s.snapshot.Load() == nil {
		return ManagerEmailMFAUnavailable
	}
	v, configured := s.lookup("login", "manager_email_mfa_on")
	if !configured {
		return ManagerEmailMFAOff
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return ManagerEmailMFAOn
	case "0", "false", "":
		return ManagerEmailMFAOff
	default:
		return ManagerEmailMFAUnavailable
	}
}

// ManagerEmailMFAOn is the convenient boolean view for schema/appconfig
// rendering. Security-sensitive handlers must use ManagerEmailMFAState so an
// unavailable snapshot cannot collapse into false.
func (s *SystemSettings) ManagerEmailMFAOn() bool {
	return s.ManagerEmailMFAState() == ManagerEmailMFAOn
}

// ValidateManagerEmailMFASMTP checks the effective SMTP values without doing
// network I/O. Callers that are about to enable the policy must follow it with
// PreflightManagerEmailMFA so the actual delivery path is exercised too.
func (s *SystemSettings) ValidateManagerEmailMFASMTP() error {
	return commonbase.ValidateSMTPConfiguration(
		s.SupportEmailSmtp(), s.SupportEmail(), s.SupportEmailPwd(),
	)
}

// PreflightManagerEmailMFA sends a real probe through the same SMTP path used
// for OTP mail. It never changes the policy value and never panics; startup
// callers log the returned error and leave the login gate fail-closed.
func (s *SystemSettings) PreflightManagerEmailMFA(ctx context.Context) error {
	generation := s.ManagerEmailMFAProbeGeneration()
	err := s.preflightManagerEmailMFA(ctx)
	if s.ManagerEmailMFAState() != ManagerEmailMFAOn {
		s.publishManagerEmailMFAPreflight(generation, false, false)
		return err
	}
	if !s.publishManagerEmailMFAPreflight(generation, true, err == nil) {
		s.Warn("丢弃过期的管理端 MFA SMTP 预检结果")
	}
	return err
}

func (s *SystemSettings) preflightManagerEmailMFA(ctx context.Context) error {
	if s.ManagerEmailMFAState() != ManagerEmailMFAOn {
		return nil
	}
	if err := s.ValidateManagerEmailMFASMTP(); err != nil {
		return err
	}
	return commonbase.NewEmailService(s.ctx, s).PreflightSMTP(ctx)
}

// scheduleManagerEmailMFAPreflight re-probes a peer after its auto-reload
// observes a changed MFA/SMTP snapshot. It runs at most one probe at a time;
// a later snapshot change causes a follow-up probe after the current one
// finishes. This is deliberately event-driven, not periodic, so the 60-second
// settings poll does not send an email on every tick.
func (s *SystemSettings) scheduleManagerEmailMFAPreflight() {
	if s.ManagerEmailMFAState() != ManagerEmailMFAOn {
		return
	}
	generation := s.ManagerEmailMFAProbeGeneration()
	if !s.managerMFAProbeInFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer func() {
			s.managerMFAProbeInFlight.Store(false)
			if generation != s.ManagerEmailMFAProbeGeneration() && s.ManagerEmailMFAState() == ManagerEmailMFAOn {
				s.scheduleManagerEmailMFAPreflight()
			}
		}()

		probeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := s.preflightManagerEmailMFA(probeCtx)
		if s.ManagerEmailMFAState() != ManagerEmailMFAOn {
			return
		}
		if !s.publishManagerEmailMFAPreflight(generation, true, err == nil) {
			return
		}
		if err != nil {
			s.Warn("manager-console MFA SMTP auto-reload preflight failed; management login remains fail-closed", zap.Error(err))
		}
	}()
}

// ManagerEmailMFAProbeGeneration returns the generation of the effective
// MFA/SMTP snapshot. Callers that perform a probe outside this type must pass
// the captured value back to RecordManagerEmailMFAPreflight.
func (s *SystemSettings) ManagerEmailMFAProbeGeneration() uint64 {
	s.managerMFAProbeMu.Lock()
	defer s.managerMFAProbeMu.Unlock()
	return s.managerMFAProbeGeneration
}

func (s *SystemSettings) publishManagerEmailMFAPreflight(generation uint64, known, ready bool) bool {
	s.managerMFAProbeMu.Lock()
	defer s.managerMFAProbeMu.Unlock()
	if generation != s.managerMFAProbeGeneration {
		return false
	}
	s.managerMFAProbeKnown = known
	s.managerMFAProbeReady = ready
	return true
}

// RecordManagerEmailMFAPreflight lets a system-setting write path publish the
// result of a probe performed against the prospective (not-yet-reloaded)
// SMTP values. It refuses to publish a result captured for an older snapshot.
// It is intentionally tiny: callers still own the actual probe and this
// method never changes the MFA policy or snapshot.
func (s *SystemSettings) RecordManagerEmailMFAPreflight(generation uint64, ok bool) bool {
	return s.publishManagerEmailMFAPreflight(generation, true, ok)
}

// ManagerEmailMFAReady is the login gate's fail-closed view.  It requires an
// enabled policy, a complete effective configuration, and a successful real
// SMTP preflight for that exact snapshot.
func (s *SystemSettings) ManagerEmailMFAReady() bool {
	s.managerMFAProbeMu.Lock()
	defer s.managerMFAProbeMu.Unlock()
	return s.ManagerEmailMFAState() == ManagerEmailMFAOn &&
		s.managerMFAProbeKnown &&
		s.managerMFAProbeReady &&
		s.ValidateManagerEmailMFASMTP() == nil
}

// ----- incomingwebhook settings (总开关 + 核心阈值) -----
//
// 这些 env 名 / 默认值是 modules/incomingwebhook 的「单一真源」：incomingwebhook 侧
// 通过下面的 getter 读取（不再各自读 env），从而让 system_setting 的 effective_value
// 能反映完整的 DB → env → code-default 回退链。修改 env 名或默认值时，需同步
// systemSettingSchema 的 incomingwebhook 行；reciprocal sync 注释见
// modules/incomingwebhook/api.go 的 New / allowPerWebhook / create。
const (
	envIncomingWebhookEnabled          = "DM_INCOMINGWEBHOOK_ENABLED"
	envIncomingWebhookPerWebhookRPS    = "DM_INCOMINGWEBHOOK_RPS"
	envIncomingWebhookPerWebhookBurst  = "DM_INCOMINGWEBHOOK_BURST"
	envIncomingWebhookMaxPerGroup      = "DM_INCOMINGWEBHOOK_MAX_PER_GROUP"
	envIncomingWebhookMaxPerThread     = "DM_INCOMINGWEBHOOK_MAX_PER_THREAD"
	envIncomingWebhookMaxPerCreator    = "DM_INCOMINGWEBHOOK_MAX_PER_CREATOR"
	envIncomingWebhookMaxTotalPerGroup = "DM_INCOMINGWEBHOOK_MAX_TOTAL_PER_GROUP"
	// 控制非管理员成员创建的 webhook 是否可用广播型 @（@所有人 / @所有 AI）。
	envIncomingWebhookMemberCanBroadcast = "OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST"

	defaultIncomingWebhookEnabled         = true
	defaultIncomingWebhookPerWebhookRPS   = 5.0
	defaultIncomingWebhookPerWebhookBurst = 10
	defaultIncomingWebhookMaxPerGroup     = 10
	defaultIncomingWebhookMaxPerCreator   = 5
	// max_total_per_group 默认 0 = 不启用群级聚合天花板（只受各作用域的
	// max_per_group / max_per_thread 约束）。设为正数才封顶单群 webhook 总量。
	defaultIncomingWebhookMaxTotalPerGroup   = 0
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

// IncomingWebhookMaxPerGroup 群本体作用域（thread_short_id=”)可创建的 webhook 数量上限。
// 子区作用域另用 IncomingWebhookMaxPerThread（未配置时回退到本值），两者独立计数、不共享
// 名额。DB → env → 默认 10。
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

// IncomingWebhookMaxPerThread 单个【子区作用域】可创建的 webhook 数量上限，与群本体的
// max_per_group 解耦，用于「群和子区分别精确设数量」。
// DB(incomingwebhook.max_per_thread) → env(DM_INCOMINGWEBHOOK_MAX_PER_THREAD) → 回退到
// IncomingWebhookMaxPerGroup()。即【未单独配置子区上限时，子区与群本体同额度】——与本
// 特性上线时「群/子区共用 max_per_group」的行为逐字节一致，配置向后兼容。
// 读侧防御：≤0 回退默认（同 max_per_group，避免误配成子区一个都建不了的「暗关」）。
func (s *SystemSettings) IncomingWebhookMaxPerThread() int {
	def := s.incomingWebhookMaxPerThreadEnvDefault()
	v := s.getInt("incomingwebhook", "max_per_thread", def)
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

// incomingWebhookMaxPerThreadEnvDefault 解析 DM_INCOMINGWEBHOOK_MAX_PER_THREAD；仅接受
// 正整数，否则【回退到 IncomingWebhookMaxPerGroup()】——未单独配置子区上限时子区与群本体
// 同额度（本方法带 s 接收者正是为了拿到这个动态回退，而非一个静态 code default）。
func (s *SystemSettings) incomingWebhookMaxPerThreadEnvDefault() int {
	if v := os.Getenv(envIncomingWebhookMaxPerThread); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return s.IncomingWebhookMaxPerGroup()
}

// IncomingWebhookMaxPerCreator 单个普通成员/bot 在一个【投递作用域】（群本体或每个
// 子区）内可创建的 webhook 数量上限（群主/管理员豁免，仅受作用域级 max_per_group
// 约束）。DB → env → 默认 5。
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

// IncomingWebhookMaxTotalPerGroup 单个群【跨群本体 + 所有子区】的 webhook 聚合总数上限，
// 用于挡住「子区可无限创建 → 单群 webhook 总量 = 作用域上限 ×(子区数+1) 失控」。
// DB(incomingwebhook.max_total_per_group) → env(DM_INCOMINGWEBHOOK_MAX_TOTAL_PER_GROUP) →
// 默认 0。
// 语义与作用域上限【不同】：0（或负）表示【不启用】聚合天花板——只受 max_per_group /
// max_per_thread 约束；仅当 > 0 时 insertWithQuota 才多做一次全群计数校验。因此读侧把
// 负值夹到 0（关闭）而非回退某默认值：0 是有意义的「关闭」哨兵，不是误配。
func (s *SystemSettings) IncomingWebhookMaxTotalPerGroup() int {
	v := s.getInt("incomingwebhook", "max_total_per_group", incomingWebhookMaxTotalPerGroupEnvDefault())
	if v < 0 {
		return 0
	}
	return v
}

// incomingWebhookMaxTotalPerGroupEnvDefault 解析 DM_INCOMINGWEBHOOK_MAX_TOTAL_PER_GROUP；
// 接受【非负】整数（0 = 关闭），否则回退默认 0。
func incomingWebhookMaxTotalPerGroupEnvDefault() int {
	if v := os.Getenv(envIncomingWebhookMaxTotalPerGroup); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultIncomingWebhookMaxTotalPerGroup
}

// ----- bot API rate limit settings (issue #696) -----
//
// 三条限流通道（business / heartbeat / register）各自独立的开关、影子开关与配额。
// 与 incomingwebhook 同源的约定：这些 env 名与默认值是**单一真源**，
// bot_api 侧只经这些 getter 读取，改动需同步 systemSettingSchema 的 botratelimit 行。
//
// 为什么每层都要 enabled 且默认关闭：register 层误配的后果是「全部 bot 无法注册」，
// 而翻开关比回滚发版快一个数量级。
//
// 为什么默认 dry_run=true：初始配额没有数据支撑——在此之前限流拒绝路径完全不写日志、
// access log 也无 robot_id，集群还没有日志采集，因此「设成 X 会拒谁」无从回答。
// 影子模式先跑真实判定但不拦截，确认只命中异常 bot 后再关掉 dry_run。
//
// 初始配额是按端点语义推的**保守偏宽**值，不是实测结论：正常 bot 的 heartbeat 受
// bot:heartbeat TTL(60s) 约束在 0.1 rps 量级，register 只在重连时调用，业务流量峰值
// 参考一次推理连发 7-8 条消息。上线后按影子数据收敛。
const (
	envBotRateLimitBusinessEnabled = "OCTO_BOT_RATELIMIT_BUSINESS_ENABLED"
	envBotRateLimitBusinessDryRun  = "OCTO_BOT_RATELIMIT_BUSINESS_DRY_RUN"
	envBotRateLimitBusinessRPS     = "OCTO_BOT_RATELIMIT_BUSINESS_RPS"
	envBotRateLimitBusinessBurst   = "OCTO_BOT_RATELIMIT_BUSINESS_BURST"

	envBotRateLimitHeartbeatEnabled = "OCTO_BOT_RATELIMIT_HEARTBEAT_ENABLED"
	envBotRateLimitHeartbeatDryRun  = "OCTO_BOT_RATELIMIT_HEARTBEAT_DRY_RUN"
	envBotRateLimitHeartbeatRPS     = "OCTO_BOT_RATELIMIT_HEARTBEAT_RPS"
	envBotRateLimitHeartbeatBurst   = "OCTO_BOT_RATELIMIT_HEARTBEAT_BURST"

	envBotRateLimitRegisterEnabled = "OCTO_BOT_RATELIMIT_REGISTER_ENABLED"
	envBotRateLimitRegisterDryRun  = "OCTO_BOT_RATELIMIT_REGISTER_DRY_RUN"
	envBotRateLimitRegisterRPS     = "OCTO_BOT_RATELIMIT_REGISTER_RPS"
	envBotRateLimitRegisterBurst   = "OCTO_BOT_RATELIMIT_REGISTER_BURST"

	defaultBotRateLimitEnabled = false
	defaultBotRateLimitDryRun  = true

	defaultBotRateLimitBusinessRPS   = 20.0
	defaultBotRateLimitBusinessBurst = 200

	// heartbeat 的速率下界是硬约束：bot:heartbeat key 的 TTL 是 60s
	// （modules/bot_api/bot_api.go heartbeatTTL），配额若低到让心跳「一次被限流就
	// 让 key 过期」，这条保命通道就变成了断联的成因。1 rps 远高于 1/60，留足余量。
	defaultBotRateLimitHeartbeatRPS   = 1.0
	defaultBotRateLimitHeartbeatBurst = 10

	// register 是自愈链路的最后一环（心跳失效 → 重连 → 刷 token），正常极低频；
	// 但它同时是**未鉴权可达**的写入口（会触发 UpdateIMToken），所以不能不设上限。
	defaultBotRateLimitRegisterRPS   = 0.5
	defaultBotRateLimitRegisterBurst = 10

	// botRateLimitMinHeartbeatRPS 是 heartbeat 配额的**结构性下界**。低于它意味着
	// 一次限流就可能让心跳 key 过期，即这条通道自己制造了它本该防止的断联。
	// 取 1/60 的 6 倍余量。
	botRateLimitMinHeartbeatRPS = 0.1

	// botRateLimitMaxRegisterFillRatio 是 register 通道 `burst/rps` 的**结构性上界**。
	//
	// 为什么只有 register 需要这道夹紧（第三轮 review P2-5）：令牌桶 key 的 TTL 由参数
	// 推导而来，不是常数——`pkg/ratelimit/bucket.go` 的 Lua 是
	// `ttl = ceil(burst/rate * 2)`。于是**每 IP 的 live key 上界 = 建 key 速率 × TTL**。
	// business 与 heartbeat 的 key 是 robotID，基数被真实 bot 数量（约 2903）封住，
	// TTL 多长都不会让 key 数发散。而 register 跑在鉴权之前、key 是
	// `SHA-256(客户端提供的 token)`，轮换 token 就是一请求一 key——基数只受
	// `bot_register` IP 底线（100 rps）约束，所以 live key 上界随 TTL 线性增长。
	//
	// 陷阱在于**方向**：TTL ∝ burst/rps，而上线流程是"按影子数据收紧 register_rps"。
	// 收紧配额会**放大** keyspace 上界，与运维的直觉相反且单调。举例（burst=10）：
	// rps 0.5 → TTL 40s → 约 4000 key；rps 0.05 → TTL 400s → 4 万；
	// rps 0.01 + burst 100 → TTL 20000s → 每 IP 约 200 万。生产 Redis 无 maxmemory，
	// 结局是 OOM-kill 而不是淘汰。
	//
	// 取 20 使**出厂默认恰好落在界上**（burst 10 / rps 0.5 = 20 ⇒ TTL 40s ⇒
	// 100 rps × 40s ≈ 4000 key），也就是让 register 底线注释里那个 "≈4000" 从
	// "在当前默认值下碰巧成立"变成**由代码保证**。
	//
	// 夹的是 burst 而不是抬 rps：运维调的是 rps（他要的是速率），抬 rps 会把
	// "我要收紧"静默变成"放宽了"。降 burst 只削突发额度，保留他表达的稳态速率。
	botRateLimitMaxRegisterFillRatio = 20.0

	// botRateLimitMinRegisterRPS 是 register 速率的**结构性下界**，与上面的比值上界
	// 成对存在——单独夹比值是守不住的。
	//
	// burst 必须 >= 1（否则桶永远填不进 token，等于 100% 拒绝），所以当 rps 低到
	// `rps * 比值 < 1` 时，比值约束与 burst>=1 直接冲突，夹紧只能让位给后者，
	// TTL 随即挣脱上界：rps 0.01 + burst 1 ⇒ TTL 200s ⇒ 每 IP 2 万 key。
	// 这个洞是本轮那条 keyspace 上界用例抓出来的，不是推导出来的。
	//
	// 取 `1/比值` 恰好是"burst=1 时比值仍然成立"的临界点，于是两道夹紧合起来把
	// TTL **钉成常数 40s**（任意合法 rps 下 burst/rps 都被压到 <= 20），
	// live key 上界因此恒为 100 rps × 40s ≈ 4000。
	botRateLimitMinRegisterRPS = 1.0 / botRateLimitMaxRegisterFillRatio
)

// BotCardDisplayEnabledDefault 等三个 getter 是 Bot 卡片能力的**服务端全局默认**
// （task bot-setting-store）。它们只回答「某个 Bot 没写过覆盖时该取什么」，不含
// per-Bot 覆盖，也不含卡片总闸 cardmsg.BotEnabled() —— 完整解析链由
// modules/robot 的 bot_setting 解析器组合，管理端的 effective_value 展示的也正是
// 本层的值（全局默认），而非某个具体 Bot 的有效值。
//
// 代码默认一律 true：总闸本身 fail-closed（OCTO_CARD_MESSAGE_ENABLED 未设即关），
// 安全默认由总闸承担；若这三项再各自默认 false，运维开了总闸还要逐 Bot 补开，
// 徒增困惑。
//
// 三者都委托给 SettingBoolOK 而不是 getBool，是因为 modules/robot 的解析器读的正是
// SettingBoolOK：走两条路就等于同一列有两套词法。上一轮把 SettingBoolOK 放宽成
// trim + 折叠大小写却没动这里，`botcard` 里一行 `False` 就会让管理台显示「开」而每个
// Bot 解析成「关」—— 同一列对两个读者含义不同，正是本层要消灭的东西。委托而非复制：
// 词法只有一份，下次再改也不会只改一边。
func (s *SystemSettings) BotCardDisplayEnabledDefault() bool {
	return s.botCardSwitchDefault("display_enabled")
}

func (s *SystemSettings) BotCardInteractionEnabledDefault() bool {
	return s.botCardSwitchDefault("interaction_enabled")
}

func (s *SystemSettings) BotCardReasoningEnabledDefault() bool {
	return s.botCardSwitchDefault("reasoning_enabled")
}

func (s *SystemSettings) botCardSwitchDefault(key string) bool {
	if v, configured := s.SettingBoolOK("botcard", key); configured {
		return v
	}
	return defaultBotCardSwitchEnabled
}

// BotRateLimitBusinessEnabled 等三组 getter 的回退链均为 DB → env → code default。
func (s *SystemSettings) BotRateLimitBusinessEnabled() bool {
	return s.getBool("botratelimit", "business_enabled", boolEnvDefault(envBotRateLimitBusinessEnabled, defaultBotRateLimitEnabled))
}

func (s *SystemSettings) BotRateLimitBusinessDryRun() bool {
	return s.getBool("botratelimit", "business_dry_run", boolEnvDefault(envBotRateLimitBusinessDryRun, defaultBotRateLimitDryRun))
}

func (s *SystemSettings) BotRateLimitBusinessRPS() float64 {
	return s.getRateLimitFloat("botratelimit", "business_rps", envBotRateLimitBusinessRPS, defaultBotRateLimitBusinessRPS)
}

func (s *SystemSettings) BotRateLimitBusinessBurst() int {
	return s.getRateLimitInt("botratelimit", "business_burst", envBotRateLimitBusinessBurst, defaultBotRateLimitBusinessBurst)
}

func (s *SystemSettings) BotRateLimitHeartbeatEnabled() bool {
	return s.getBool("botratelimit", "heartbeat_enabled", boolEnvDefault(envBotRateLimitHeartbeatEnabled, defaultBotRateLimitEnabled))
}

func (s *SystemSettings) BotRateLimitHeartbeatDryRun() bool {
	return s.getBool("botratelimit", "heartbeat_dry_run", boolEnvDefault(envBotRateLimitHeartbeatDryRun, defaultBotRateLimitDryRun))
}

// BotRateLimitHeartbeatRPS 在通用消毒之外再夹一道下界：见 botRateLimitMinHeartbeatRPS。
// 这条不是防手滑，是防「把保命通道配成断联成因」这个具体的失败模式。
func (s *SystemSettings) BotRateLimitHeartbeatRPS() float64 {
	v := s.getRateLimitFloat("botratelimit", "heartbeat_rps", envBotRateLimitHeartbeatRPS, defaultBotRateLimitHeartbeatRPS)
	if v < botRateLimitMinHeartbeatRPS {
		return botRateLimitMinHeartbeatRPS
	}
	return v
}

func (s *SystemSettings) BotRateLimitHeartbeatBurst() int {
	return s.getRateLimitInt("botratelimit", "heartbeat_burst", envBotRateLimitHeartbeatBurst, defaultBotRateLimitHeartbeatBurst)
}

func (s *SystemSettings) BotRateLimitRegisterEnabled() bool {
	return s.getBool("botratelimit", "register_enabled", boolEnvDefault(envBotRateLimitRegisterEnabled, defaultBotRateLimitEnabled))
}

func (s *SystemSettings) BotRateLimitRegisterDryRun() bool {
	return s.getBool("botratelimit", "register_dry_run", boolEnvDefault(envBotRateLimitRegisterDryRun, defaultBotRateLimitDryRun))
}

// BotRateLimitRegisterRPS 在通用消毒之外再夹一道下界：见 botRateLimitMinRegisterRPS。
// 与 BotRateLimitRegisterBurst 的比值上界成对，共同把令牌桶 key 的 TTL 钉成常数。
func (s *SystemSettings) BotRateLimitRegisterRPS() float64 {
	v := s.getRateLimitFloat("botratelimit", "register_rps", envBotRateLimitRegisterRPS, defaultBotRateLimitRegisterRPS)
	if v < botRateLimitMinRegisterRPS {
		return botRateLimitMinRegisterRPS
	}
	return v
}

// BotRateLimitRegisterBurst 在通用消毒之外再夹一道上界：`burst <= rps * 上限比值`
// （见 botRateLimitMaxRegisterFillRatio）。
//
// 这条不是防手滑，是防「收紧配额反而放大 keyspace」这个方向相反的失败模式：
// 令牌桶 key 的 TTL 是 `ceil(burst/rps * 2)`，而 register 的 key 由客户端提供的
// token 决定基数，所以 live key 上界正比于 burst/rps。夹住比值即夹住上界。
//
// 夹紧后 rps 与 burst **不再独立**：调低 register_rps 会同时压低有效 burst。
// 这是刻意的——两者的比值是一个结构性量，不是两个自由参数。
// 有效值可在管理台的 Effective 列读到，不必靠猜。
func (s *SystemSettings) BotRateLimitRegisterBurst() int {
	burst := s.getRateLimitInt("botratelimit", "register_burst", envBotRateLimitRegisterBurst, defaultBotRateLimitRegisterBurst)
	maxBurst := int(math.Floor(s.BotRateLimitRegisterRPS() * botRateLimitMaxRegisterFillRatio))
	if maxBurst < 1 {
		// 有 botRateLimitMinRegisterRPS 在，这里**不可达**（rps >= 1/比值 ⇒ maxBurst >= 1）。
		// 保留是因为两道夹紧各自独立成立比互相依赖更结实：若将来有人调了比值或下界
		// 而忘了另一边，这里兜住的是"100% 拒绝"，比 keyspace 放大更急。
		maxBurst = 1
	}
	if burst > maxBurst {
		return maxBurst
	}
	return burst
}

// getRateLimitFloat 是限流配额专用的读侧防御，语义与 IncomingWebhookPerWebhookRPS
// 那套逐条对应（D-289 同型，覆盖直接改库的旁路）：
//
//   - rps <= 0 会让令牌桶 Lua 走 `rate <= 0` 短路，即**整条路由 100% 拒绝**；
//   - NaN 会让 Lua 的算术全部变 NaN、比较失效，行为不可预测；
//   - env 兜底自身也可能非有限——wkhttp.ParseRPSFromEnv 底层是 strconv.ParseFloat，
//     会原样接受 "NaN" / "+Inf"，所以 def 必须先消毒再参与回退。
//
// 任何非法值都回退到**永远合法的 code default**，而不是「关闭限流」：
// 静默 fail-open 会让一个配置错误伪装成「限流正常但没人超限」。
func (s *SystemSettings) getRateLimitFloat(category, key, envKey string, codeDefault float64) float64 {
	def := wkhttp.ParseRPSFromEnv(envKey, codeDefault)
	if math.IsNaN(def) || math.IsInf(def, 0) || def <= 0 {
		def = codeDefault
	}
	v := s.getFloat(category, key, def)
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return def
	}
	return v
}

// getRateLimitInt 同上。burst <= 0 会让桶永远填不进 1 个 token，同样等于 100% 拒绝。
func (s *SystemSettings) getRateLimitInt(category, key, envKey string, codeDefault int) int {
	def := wkhttp.ParseBurstFromEnv(envKey, codeDefault)
	if def <= 0 {
		def = codeDefault
	}
	v := s.getInt(category, key, def)
	if v <= 0 {
		return def
	}
	return v
}

// boolEnvDefault 解析 bool 型 env 兜底，语义与 system_setting 的 bool 字面量一致
// （1/true/TRUE → true，0/false/FALSE → false，其余 → fallback）。
func boolEnvDefault(envKey string, fallback bool) bool {
	if v := os.Getenv(envKey); v != "" {
		return parseSettingBool(v, fallback)
	}
	return fallback
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

// MessageReactionReadEnabled reports whether ordinary Web/iOS/Android clients
// should display message reactions. Read defaults on so existing reactions stay
// visible unless an operator explicitly disables message_reaction.read.
func (s *SystemSettings) MessageReactionReadEnabled() bool {
	return s.getBool("message_reaction", "read", true)
}

// MessageReactionWriteEnabled reports whether ordinary Web/iOS/Android clients
// should expose add/cancel controls. Write defaults off for a staged rollout and
// can be enabled independently through message_reaction.write. These capability
// getters are presentation policy; server-side authorization stays independent.
func (s *SystemSettings) MessageReactionWriteEnabled() bool {
	return s.getBool("message_reaction", "write", false)
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

// MailEnabled reports whether clients should surface the Agent Mail module.
// This is a presentation toggle only: Agent Mail authorization and access
// control remain enforced by octo-server and octo-mail. Default false so the
// entry is exposed only after an operator enables system_setting mail.enabled.
func (s *SystemSettings) MailEnabled() bool {
	return s.getBool("mail", "enabled", false)
}

// DocsSearchEnabled reports whether clients should surface cloud-doc full-text
// search. Decoupled from DocsEnabled: the search endpoint is provided
// independently by octo-docs-backend and may land later than the docs module
// itself. Display policy only — it neither grants nor enforces any server-side
// authorization, which lives in octo-docs-backend. Default false so the tab
// stays hidden until search is deployed and the index is populated and the
// admin flips docs.search_enabled for a controlled rollout. Value source:
// system_setting docs.search_enabled (DB, hot-reloaded).
func (s *SystemSettings) DocsSearchEnabled() bool {
	return s.getBool("docs", "search_enabled", false)
}

// DriveEnabled reports whether clients should surface the drive (网盘) module
// entry (backed by the standalone octo-drive service). Display policy only —
// it neither grants nor enforces any server-side authorization, which lives in
// octo-drive itself. Default false so the entry stays hidden until octo-drive
// is deployed and the admin flips drive.enabled for a controlled rollout.
// Value source: system_setting drive.enabled (DB, hot-reloaded).
func (s *SystemSettings) DriveEnabled() bool {
	return s.getBool("drive", "enabled", false)
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

// TrackingEnabled reports whether the frontend event-tracking layer (octo-dap)
// should collect. Default false — the tracker ships dark (fail-closed): octo-web
// only starts collecting when appconfig delivers tracking_enabled truthy, so ops
// keeps this off until the octo-dap collector is deployed and the TRACK_API_URL
// egress is verified in-cluster. Display/behaviour policy only; the collector
// enforces its own auth. Value source: system_setting tracking.enabled
// (DB, hot-reloaded — flip in the admin console, 60s multi-instance converge,
// no restart). Delivered via GET /v1/common/appconfig as tracking_enabled.
func (s *SystemSettings) TrackingEnabled() bool {
	return s.getBool("tracking", "enabled", false)
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

// ---------------------------------------------------------------------------
// Group new-member welcome master switch (onboarding.group_welcome_enabled) —
// task group-welcome-message
// ---------------------------------------------------------------------------

// GroupWelcomeEnabled is the platform master switch for the group new-member
// welcome (群入群欢迎语). Default false (dark launch): the feature ships off, and an
// operator flips onboarding.group_welcome_enabled to turn it on across every
// group. Flipping it back is an instant kill switch — the event path stops
// enqueuing and the send worker stops posting within the snapshot TTL — without
// touching any per-group config row. Read from the SAME snapshot as every other
// setting, so it hot-reloads and converges across replicas within reloadTTL.
//
// Unlike the Space welcome's onboarding keys this is enablement ONLY: there is no
// platform-global content fallback, so a group's body always comes from its own
// row (see brief: no global fallback). The per-group config.Enabled still gates
// each group individually; this switch is the outer AND.
func (s *SystemSettings) GroupWelcomeEnabled() bool {
	return s.getBool(spaceWelcomeCategory, "group_welcome_enabled", false)
}
