package oidc

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// validateBindConfigAgainstProvider 启动期硬校验:Bind.Enabled=true 时
// Provider.AllowNewUser 必须为 false。
//
// FR-1.1 触发条件要求:autolink 三种(issuer+sub / email / phone)全部失败 +
// AllowNewUser=false 时,绑定流程才会启动。如果 AllowNewUser=true,callback
// 会直接走"新建空账号"分支,绑定 handler 根本不可达。两个 flag 同开 →
// 用户体验上等价于"绑定功能完全没开",但运维/前端会以为开了 —— 是危险的
// 静默不一致。
//
// 在 OIDC.Init() 内部调用,返 error 让 module.Setup 直接 panic,ops 必须
// 显式取舍才能启动。
func validateBindConfigAgainstProvider(cfg *Config) error {
	if cfg == nil || !cfg.Bind.Enabled {
		return nil
	}
	if cfg.Provider.AllowNewUser {
		return fmt.Errorf(
			"oidc: Bind.Enabled=true conflicts with Provider.AllowNewUser=true: " +
				"set DM_OIDC_PROVIDER_ALLOW_NEW_USER=false to enable self-service binding (FR-1.1)",
		)
	}
	if err := validateBindRedirectBase(cfg.Bind.RedirectBase); err != nil {
		return err
	}
	if len(cfg.Bind.Methods) == 0 {
		// 唯一进入 0 的路径是 loadBindMethods 在 env 全部非法时返空:运维
		// 显式配 OCTO_OIDC_BIND_METHODS 但拼写错或全是 disallowed (email_otp) 值。
		// 起服务直接拒,避免静默回退到默认两项让"显式想关 password"的配置
		// fail-open 激活默认 [password, sms_otp]。
		return fmt.Errorf(
			"oidc: Bind.Enabled=true requires at least one valid method in " +
				"OCTO_OIDC_BIND_METHODS (allowed: password, sms_otp); " +
				"got an env value that parsed to zero valid methods",
		)
	}
	return nil
}

// validateBindRedirectBase 启动期 fail-fast:Bind.Enabled=true 时 RedirectBase
// 必须是合法 https URL(开发可用 OCTO_OIDC_BIND_REDIRECT_ALLOW_INSECURE=1 放宽到 http)。
//
// 理由:跑到 callback 才发现 RedirectBase 漏配是糟糕的运维体验 —— bind_token
// 已发,审计/metric 已计,前端轮询要等 5min TTL 才知道失败。启动 panic 比
// runtime 退化好得多。同时拦 javascript:/data: scheme 防 misconfig 引入 XSS。
func validateBindRedirectBase(base string) error {
	if base == "" {
		return fmt.Errorf(
			"oidc: Bind.Enabled=true requires OCTO_OIDC_BIND_REDIRECT_BASE to be set " +
				"(non-empty https URL pointing at the front-end bind page)",
		)
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("oidc: invalid OCTO_OIDC_BIND_REDIRECT_BASE %q: %w", base, err)
	}
	if u.Host == "" {
		return fmt.Errorf(
			"oidc: OCTO_OIDC_BIND_REDIRECT_BASE %q must be absolute (scheme://host/path)", base)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && getBool("OCTO_OIDC_BIND_REDIRECT_ALLOW_INSECURE", false) {
		return nil
	}
	return fmt.Errorf(
		"oidc: OCTO_OIDC_BIND_REDIRECT_BASE %q must use https scheme "+
			"(set OCTO_OIDC_BIND_REDIRECT_ALLOW_INSECURE=1 to allow http for dev)", base)
}

// BindConfig 自助绑定相关配置(NFR-4 全部走 env,不硬编码)。
//
// **Enabled=false 时其余字段无效**:LoadConfig 不校验任何依赖关系,
// PR3 仅起骨架作用,callback 接管在 PR4 才打开。PR4 会加 "Enabled && RedirectBase==”"
// 这类硬校验。
//
// keyspace 命名:OCTO_OIDC_BIND_* 与已有 DM_OIDC_* 并列,语义上 BindConfig
// 是 OIDC 模块的子配置块,但运行期可独立灰度(NFR-5)。
type BindConfig struct {
	Enabled         bool
	IssuerAllowlist []string
	TokenTTL        time.Duration
	VerifyMax       int64
	OTPSendMax      int64
	ConfirmMax      int64
	UIDFailPerDay   int64
	Methods         []BindMethod
	SupportContact  string
	RedirectBase    string
	AllowCreate     bool
}

// 默认值集中定义,与 bind_config_test.go 的 TestLoadConfig_BindDefaults
// 保持单一事实源 —— 改阈值时只需动一处。
const (
	defaultBindTokenTTL      = 5 * time.Minute
	defaultBindVerifyMax     = 5  // SR-2.1
	defaultBindOTPSendMax    = 3  // SR-2.1
	defaultBindConfirmMax    = 3  // SR-2.1
	defaultBindUIDFailPerDay = 10 // SR-2.2
)

// bindCreateMax is fixed at 1 per bind_token: /bind/create is a terminal
// one-shot operation; failure modes (claims incomplete, status conflict,
// already bound) are deterministic and retrying cannot make them succeed.
// Transient errors leave token state ambiguous and must not be retried
// blindly. Operators who want stricter gating should disable AllowCreate
// entirely and rely on /bind/verify/*.
const bindCreateMax int64 = 1

// defaultBindMethods 不导出但被 LoadConfig 与 fallback 路径共享,
// 避免在两处 hardcode 顺序不一致(测试断言依赖顺序)。
var defaultBindMethods = []BindMethod{BindMethodPassword, BindMethodSMSOTP}

func loadBindConfig() BindConfig {
	return BindConfig{
		Enabled:         getBool("OCTO_OIDC_BIND_ENABLED", false),
		IssuerAllowlist: getStringSlice("OCTO_OIDC_BIND_ISSUER_ALLOWLIST", nil),
		TokenTTL:        loadBindTokenTTL(),
		VerifyMax:       loadBindCounter("OCTO_OIDC_BIND_VERIFY_MAX", defaultBindVerifyMax),
		OTPSendMax:      loadBindCounter("OCTO_OIDC_BIND_OTP_SEND_MAX", defaultBindOTPSendMax),
		ConfirmMax:      loadBindCounter("OCTO_OIDC_BIND_CONFIRM_MAX", defaultBindConfirmMax),
		UIDFailPerDay:   loadBindCounter("OCTO_OIDC_BIND_UID_FAIL_PER_DAY", defaultBindUIDFailPerDay),
		Methods:         loadBindMethods(),
		SupportContact:  getString("OCTO_OIDC_BIND_SUPPORT_CONTACT", ""),
		RedirectBase:    getString("OCTO_OIDC_BIND_REDIRECT_BASE", ""),
		AllowCreate:     getBool("OCTO_OIDC_BIND_ALLOW_CREATE", true),
	}
}

// loadBindTokenTTL 秒级整数 -> Duration。非法/0/负数回退默认 5min。
func loadBindTokenTTL() time.Duration {
	v, ok := os.LookupEnv("OCTO_OIDC_BIND_TOKEN_TTL_SEC")
	if !ok || v == "" {
		return defaultBindTokenTTL
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return defaultBindTokenTTL
	}
	return time.Duration(n) * time.Second
}

// loadBindCounter 0/负数/非数字都回退到 def —— "运维误填导致服务起不来"
// 比 "用了默认阈值" 严重得多,所以选 fail-open。
func loadBindCounter(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// loadBindMethods 解析 OCTO_OIDC_BIND_METHODS,逗号分隔。
//
// 语义:
//   - env 未设置/为空 → 返默认两项(password, sms_otp),便于零配置启动
//   - env 设置但解析后**所有**项都被 drop(未知值 / email_otp / 拼写错)
//     → 返**空切片**,让 validateBindConfigAgainstProvider 在
//     Bind.Enabled=true 时拒绝启动。**绝不**回退到默认值 —— 那会让
//     运维"显式想关 password"的 OCTO_OIDC_BIND_METHODS=email_otp
//     之类配置静默激活默认 [password, sms_otp],auth 策略 fail-open。
//   - 至少有一项合法 → 返过滤后的列表
//
// email_otp 在 validBindMethods 里被显式排除(SR-3),所以 "email_otp" 单值
// 输入会落到第二条 → 空列表 → 启动拒绝,正是预期。
func loadBindMethods() []BindMethod {
	v, ok := os.LookupEnv("OCTO_OIDC_BIND_METHODS")
	if !ok || v == "" {
		return defaultBindMethods
	}
	parts := strings.Split(v, ",")
	out := make([]BindMethod, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		m := BindMethod(t)
		if _, valid := validBindMethods[m]; !valid {
			continue
		}
		out = append(out, m)
	}
	return out
}
