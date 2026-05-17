package oidc

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// BindConfig 自助绑定相关配置(NFR-4 全部走 env,不硬编码)。
//
// **Enabled=false 时其余字段无效**:LoadConfig 不校验任何依赖关系,
// PR3 仅起骨架作用,callback 接管在 PR4 才打开。PR4 会加 "Enabled && RedirectBase==''"
// 这类硬校验。
//
// keyspace 命名:DM_OIDC_BIND_* 与已有 DM_OIDC_* 并列,语义上 BindConfig
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
}

// 默认值集中定义,与 bind_config_test.go 的 TestLoadConfig_BindDefaults
// 保持单一事实源 —— 改阈值时只需动一处。
const (
	defaultBindTokenTTL       = 5 * time.Minute
	defaultBindVerifyMax      = 5  // SR-2.1
	defaultBindOTPSendMax     = 3  // SR-2.1
	defaultBindConfirmMax     = 3  // SR-2.1
	defaultBindUIDFailPerDay  = 10 // SR-2.2
)

// defaultBindMethods 不导出但被 LoadConfig 与 fallback 路径共享,
// 避免在两处 hardcode 顺序不一致(测试断言依赖顺序)。
var defaultBindMethods = []BindMethod{BindMethodPassword, BindMethodSMSOTP}

func loadBindConfig() BindConfig {
	return BindConfig{
		Enabled:         getBool("DM_OIDC_BIND_ENABLED", false),
		IssuerAllowlist: getStringSlice("DM_OIDC_BIND_ISSUER_ALLOWLIST", nil),
		TokenTTL:        loadBindTokenTTL(),
		VerifyMax:       loadBindCounter("DM_OIDC_BIND_VERIFY_MAX", defaultBindVerifyMax),
		OTPSendMax:      loadBindCounter("DM_OIDC_BIND_OTP_SEND_MAX", defaultBindOTPSendMax),
		ConfirmMax:      loadBindCounter("DM_OIDC_BIND_CONFIRM_MAX", defaultBindConfirmMax),
		UIDFailPerDay:   loadBindCounter("DM_OIDC_BIND_UID_FAIL_PER_DAY", defaultBindUIDFailPerDay),
		Methods:         loadBindMethods(),
		SupportContact:  getString("DM_OIDC_BIND_SUPPORT_CONTACT", ""),
		RedirectBase:    getString("DM_OIDC_BIND_REDIRECT_BASE", ""),
	}
}

// loadBindTokenTTL 秒级整数 -> Duration。非法/0/负数回退默认 5min。
func loadBindTokenTTL() time.Duration {
	v, ok := os.LookupEnv("DM_OIDC_BIND_TOKEN_TTL_SEC")
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

// loadBindMethods 解析 DM_OIDC_BIND_METHODS,逗号分隔。未知值/email_otp
// 静默 drop(后者 SR-3 明确禁用);全部 drop 则回退默认两项,避免"无可用方法"死锁。
func loadBindMethods() []BindMethod {
	v, ok := os.LookupEnv("DM_OIDC_BIND_METHODS")
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
	if len(out) == 0 {
		return defaultBindMethods
	}
	return out
}
