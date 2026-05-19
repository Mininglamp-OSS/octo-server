package oidc

import (
	"testing"
	"time"
)

// TestLoadConfig_BindDefaults 锁定 BindConfig 默认值:
//   - Enabled=false (灰度未开,callback 行为退回旧版,需求 NFR-5)
//   - IssuerAllowlist 空 (P0 上线初期所有 issuer 都拒绝,运维显式加白)
//   - TokenTTL=5min (NFR-2)
//   - 三个 counter 阈值与 SR-2.1 对齐
//   - UIDFailPerDay=10 (SR-2.2)
//   - Methods 默认 ["password","sms_otp"] (FR-3.1)
//   - SupportContact 空 (FR-7.1 由 ops 显式配)
//   - RedirectBase 空 (PR4 才会真用上,但 LoadConfig 不应因此报错)
//
// 关键不变式:Bind.Enabled=false 时其他字段值不影响生产路径,LoadConfig 不
// 应因为 OIDC 主开关未开就拒绝加载 (即 Bind 字段不参与 required 校验)。
func TestLoadConfig_BindDefaults(t *testing.T) {
	clearOIDCBindEnv(t)
	clearOIDCEnv(t)
	mustSetMinimalOIDCEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	b := cfg.Bind
	if b.Enabled {
		t.Fatal("Bind.Enabled default must be false (gradual rollout safety)")
	}
	if len(b.IssuerAllowlist) != 0 {
		t.Fatalf("Bind.IssuerAllowlist default must be empty, got %v", b.IssuerAllowlist)
	}
	if b.TokenTTL != 5*time.Minute {
		t.Fatalf("Bind.TokenTTL default=%v, want 5m", b.TokenTTL)
	}
	if b.VerifyMax != 5 {
		t.Fatalf("Bind.VerifyMax default=%d, want 5 (SR-2.1)", b.VerifyMax)
	}
	if b.OTPSendMax != 3 {
		t.Fatalf("Bind.OTPSendMax default=%d, want 3 (SR-2.1)", b.OTPSendMax)
	}
	if b.ConfirmMax != 3 {
		t.Fatalf("Bind.ConfirmMax default=%d, want 3 (SR-2.1)", b.ConfirmMax)
	}
	if b.UIDFailPerDay != 10 {
		t.Fatalf("Bind.UIDFailPerDay default=%d, want 10 (SR-2.2)", b.UIDFailPerDay)
	}
	if len(b.Methods) != 2 || b.Methods[0] != BindMethodPassword || b.Methods[1] != BindMethodSMSOTP {
		t.Fatalf("Bind.Methods default=%v, want [password sms_otp]", b.Methods)
	}
	if b.SupportContact != "" {
		t.Fatalf("Bind.SupportContact default must be empty, got %q", b.SupportContact)
	}
	if b.RedirectBase != "" {
		t.Fatalf("Bind.RedirectBase default must be empty, got %q", b.RedirectBase)
	}
}

// TestLoadConfig_BindOverrides 覆盖每个 OCTO_OIDC_BIND_* env 的解析。
func TestLoadConfig_BindOverrides(t *testing.T) {
	clearOIDCBindEnv(t)
	clearOIDCEnv(t)
	mustSetMinimalOIDCEnv(t)

	t.Setenv("OCTO_OIDC_BIND_ENABLED", "true")
	t.Setenv("OCTO_OIDC_BIND_ISSUER_ALLOWLIST", "https://aegis,https://google")
	t.Setenv("OCTO_OIDC_BIND_TOKEN_TTL_SEC", "120")
	t.Setenv("OCTO_OIDC_BIND_VERIFY_MAX", "8")
	t.Setenv("OCTO_OIDC_BIND_OTP_SEND_MAX", "2")
	t.Setenv("OCTO_OIDC_BIND_CONFIRM_MAX", "1")
	t.Setenv("OCTO_OIDC_BIND_UID_FAIL_PER_DAY", "20")
	t.Setenv("OCTO_OIDC_BIND_METHODS", "password")
	t.Setenv("OCTO_OIDC_BIND_SUPPORT_CONTACT", "ops@example.com")
	t.Setenv("OCTO_OIDC_BIND_REDIRECT_BASE", "https://im.example.com/oidc/bind")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	b := cfg.Bind
	if !b.Enabled {
		t.Fatal("Bind.Enabled=true expected")
	}
	if len(b.IssuerAllowlist) != 2 || b.IssuerAllowlist[0] != "https://aegis" || b.IssuerAllowlist[1] != "https://google" {
		t.Fatalf("Bind.IssuerAllowlist=%v", b.IssuerAllowlist)
	}
	if b.TokenTTL != 120*time.Second {
		t.Fatalf("Bind.TokenTTL=%v", b.TokenTTL)
	}
	if b.VerifyMax != 8 || b.OTPSendMax != 2 || b.ConfirmMax != 1 {
		t.Fatalf("counters=%d/%d/%d", b.VerifyMax, b.OTPSendMax, b.ConfirmMax)
	}
	if b.UIDFailPerDay != 20 {
		t.Fatalf("UIDFailPerDay=%d", b.UIDFailPerDay)
	}
	if len(b.Methods) != 1 || b.Methods[0] != BindMethodPassword {
		t.Fatalf("Methods=%v", b.Methods)
	}
	if b.SupportContact != "ops@example.com" {
		t.Fatalf("SupportContact=%q", b.SupportContact)
	}
	if b.RedirectBase != "https://im.example.com/oidc/bind" {
		t.Fatalf("RedirectBase=%q", b.RedirectBase)
	}
}

// TestLoadConfig_BindInvalidValuesFallback 锁定容错语义:非法/0/负数应当回退到默认,
// 不让一个写错的 env 把生产服务卡住启动。
func TestLoadConfig_BindInvalidValuesFallback(t *testing.T) {
	clearOIDCBindEnv(t)
	clearOIDCEnv(t)
	mustSetMinimalOIDCEnv(t)

	t.Setenv("OCTO_OIDC_BIND_TOKEN_TTL_SEC", "not-a-number")
	t.Setenv("OCTO_OIDC_BIND_VERIFY_MAX", "0")
	t.Setenv("OCTO_OIDC_BIND_OTP_SEND_MAX", "-1")
	t.Setenv("OCTO_OIDC_BIND_CONFIRM_MAX", "abc")
	t.Setenv("OCTO_OIDC_BIND_UID_FAIL_PER_DAY", "")
	// 注意:此处不再设 METHODS,留默认行为。METHODS env 显式全非法的语义
	// 已迁移到 TestLoadConfig_BindMethodsParsing + TestValidateBindConfigAgainstProvider:
	// 显式配但全 invalid → 空切片 → validate fail Init(不再静默回退默认值)。

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	b := cfg.Bind
	if b.TokenTTL != 5*time.Minute {
		t.Fatalf("non-numeric TTL must fall back to 5m, got %v", b.TokenTTL)
	}
	if b.VerifyMax != 5 || b.OTPSendMax != 3 || b.ConfirmMax != 3 {
		t.Fatalf("zero/negative/non-numeric counters must fall back to defaults, got %d/%d/%d",
			b.VerifyMax, b.OTPSendMax, b.ConfirmMax)
	}
	if b.UIDFailPerDay != 10 {
		t.Fatalf("empty UIDFailPerDay must fall back, got %d", b.UIDFailPerDay)
	}
}

// TestLoadConfig_BindMethodsParsing 覆盖 Methods 字段几种边界:
//   - 仅 password / 仅 sms_otp / 两者 / 含未知值
//   - 未知方法静默丢弃(不报错,避免运维迁移 typo 整条 env 失效)
//   - email_otp 必须被过滤(SR-3 禁用)
//   - **全部非法 → 返空切片**(让 validateBindConfigAgainstProvider fail Init,
//     防 fail-open 回退到 [password, sms_otp])
func TestLoadConfig_BindMethodsParsing(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []BindMethod
	}{
		{"only password", "password", []BindMethod{BindMethodPassword}},
		{"only sms", "sms_otp", []BindMethod{BindMethodSMSOTP}},
		{"both", "password,sms_otp", []BindMethod{BindMethodPassword, BindMethodSMSOTP}},
		{"with whitespace", "  password ,  sms_otp ", []BindMethod{BindMethodPassword, BindMethodSMSOTP}},
		{"email_otp dropped (SR-3)", "password,email_otp,sms_otp", []BindMethod{BindMethodPassword, BindMethodSMSOTP}},
		{"unknown dropped", "password,bogus", []BindMethod{BindMethodPassword}},
		{"all invalid returns empty (no fail-open default)", "email_otp,bogus", []BindMethod{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOIDCBindEnv(t)
			clearOIDCEnv(t)
			mustSetMinimalOIDCEnv(t)
			t.Setenv("OCTO_OIDC_BIND_METHODS", tc.in)

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if len(cfg.Bind.Methods) != len(tc.want) {
				t.Fatalf("Methods len mismatch: got=%v want=%v", cfg.Bind.Methods, tc.want)
			}
			for i, m := range tc.want {
				if cfg.Bind.Methods[i] != m {
					t.Fatalf("Methods[%d]=%v want=%v (full=%v)", i, cfg.Bind.Methods[i], m, cfg.Bind.Methods)
				}
			}
		})
	}
}

// TestLoadConfig_BindEnabledDoesNotForceRedirectBase 锁定关键不变式:
// PR3 阶段 Bind.Enabled=true 仅意味着模块骨架就位、handler 路由挂载,
// callback 真接管在 PR4 才发生。所以 RedirectBase 没配置时 LoadConfig
// 不应当报错,以便 PR3 灰度阶段能先把 flag 打开做 handler smoke test。
//
// PR4 引入 callback 接管后再加 "Bind.Enabled && RedirectBase == \"\" → fail"
// 的硬校验。
func TestLoadConfig_BindEnabledDoesNotForceRedirectBase(t *testing.T) {
	clearOIDCBindEnv(t)
	clearOIDCEnv(t)
	mustSetMinimalOIDCEnv(t)
	t.Setenv("OCTO_OIDC_BIND_ENABLED", "true")
	// 故意不设 OCTO_OIDC_BIND_REDIRECT_BASE

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig must not require RedirectBase at PR3 stage, got err=%v", err)
	}
	if !cfg.Bind.Enabled {
		t.Fatal("Bind.Enabled=true expected")
	}
	if cfg.Bind.RedirectBase != "" {
		t.Fatalf("RedirectBase=%q (expected empty)", cfg.Bind.RedirectBase)
	}
}

// mustSetMinimalOIDCEnv 给非 BindConfig 测试塞一组最小可通过 LoadConfig
// required 校验的 env(主 Provider 字段必填,Bind 字段无依赖)。
func mustSetMinimalOIDCEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web.example/cb")
	// 32 字节零作 base64,LoadConfig 只校长度不校熵 —— 测试可用,不会泄漏。
	t.Setenv("DM_OIDC_RT_ENC_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

// clearOIDCBindEnv 配合 clearOIDCEnv 在 BindConfig 测试间隔离 env,
// 单独抽出来避免污染主 clearOIDCEnv 的迁移期老 alias 列表。
func clearOIDCBindEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"OCTO_OIDC_BIND_ENABLED",
		"OCTO_OIDC_BIND_ISSUER_ALLOWLIST",
		"OCTO_OIDC_BIND_TOKEN_TTL_SEC",
		"OCTO_OIDC_BIND_VERIFY_MAX",
		"OCTO_OIDC_BIND_OTP_SEND_MAX",
		"OCTO_OIDC_BIND_CONFIRM_MAX",
		"OCTO_OIDC_BIND_UID_FAIL_PER_DAY",
		"OCTO_OIDC_BIND_METHODS",
		"OCTO_OIDC_BIND_SUPPORT_CONTACT",
		"OCTO_OIDC_BIND_REDIRECT_BASE",
		"OCTO_OIDC_BIND_ALLOW_CREATE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

// T1: 默认 AllowCreate=true
func TestLoadConfig_BindCreate_Defaults(t *testing.T) {
	clearOIDCBindEnv(t)
	clearOIDCEnv(t)
	mustSetMinimalOIDCEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Bind.AllowCreate {
		t.Fatal("AllowCreate default must be true (D1)")
	}
}

// T2: 显式关 AllowCreate
func TestLoadConfig_BindCreate_Disabled(t *testing.T) {
	clearOIDCBindEnv(t)
	clearOIDCEnv(t)
	mustSetMinimalOIDCEnv(t)
	t.Setenv("OCTO_OIDC_BIND_ALLOW_CREATE", "false")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Bind.AllowCreate {
		t.Fatal("AllowCreate must be false when env=false")
	}
}
