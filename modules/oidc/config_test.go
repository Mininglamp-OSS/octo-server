package oidc

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web.imocto.cn/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected Enabled=true")
	}
	if cfg.Provider.Issuer != "https://accounts.imocto.cn" {
		t.Fatalf("issuer mismatch: %q", cfg.Provider.Issuer)
	}
	if cfg.Provider.ClientID != "cid" || cfg.Provider.ClientSecret != "csecret" {
		t.Fatal("client id/secret mismatch")
	}
	if cfg.Provider.ID != "oidc" {
		t.Fatalf("default ID: %q (want \"oidc\")", cfg.Provider.ID)
	}
	if cfg.Provider.Name != "SSO" {
		t.Fatalf("default Name: %q (want \"SSO\")", cfg.Provider.Name)
	}
	wantScopes := []string{"openid", "profile", "email", "phone", "offline_access"}
	if len(cfg.Provider.Scopes) != len(wantScopes) {
		t.Fatalf("default scopes mismatch: got=%v want=%v", cfg.Provider.Scopes, wantScopes)
	}
	for i, s := range wantScopes {
		if cfg.Provider.Scopes[i] != s {
			t.Fatalf("default scopes[%d]=%q want=%q (full=%v)", i, cfg.Provider.Scopes[i], s, cfg.Provider.Scopes)
		}
	}
	if cfg.Provider.SyncInterval != 15*time.Minute {
		t.Fatalf("default sync_interval: %v", cfg.Provider.SyncInterval)
	}
	if cfg.Provider.HTTPTimeout != 10*time.Second {
		t.Fatalf("default http_timeout: %v", cfg.Provider.HTTPTimeout)
	}
	if cfg.Provider.ClockSkew != 60*time.Second {
		t.Fatalf("default clock_skew: %v", cfg.Provider.ClockSkew)
	}
	if !cfg.Provider.RequirePKCE || !cfg.Provider.RequireEmailVerified || !cfg.Provider.AutoLinkByEmail || !cfg.Provider.AllowNewUser {
		t.Fatal("default safety flags should be true")
	}
	if len(cfg.Provider.RefreshTokenEncryptionKey) != 32 {
		t.Fatalf("RTEncryptionKey len=%d", len(cfg.Provider.RefreshTokenEncryptionKey))
	}
}

// 自定义 Provider ID/Name 用于多 IdP 接入(本期单 provider, 字段由前端展示驱动)。
func TestLoadConfigFromEnv_OverrideProviderIDAndName(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ID", "google")
	t.Setenv("DM_OIDC_PROVIDER_NAME", "Google")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://accounts.google.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider.ID != "google" {
		t.Fatalf("ID: %q", cfg.Provider.ID)
	}
	if cfg.Provider.Name != "Google" {
		t.Fatalf("Name: %q", cfg.Provider.Name)
	}
}

func TestLoadConfigFromEnv_DisabledSkipsValidation(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "false")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("disabled config should load without required fields, got %v", err)
	}
	if cfg.Enabled {
		t.Fatal("expected disabled")
	}
}

func TestLoadConfigFromEnv_MissingRequired(t *testing.T) {
	tests := []struct {
		name        string
		unset       string
		setKey      string
		setVal      string
		errContains string // 错误消息须包含此关键字,捕获"因预期外原因报错"的回归
	}{
		{"missing issuer", "DM_OIDC_PROVIDER_ISSUER", "", "", "DM_OIDC_PROVIDER_ISSUER"},
		{"missing client id", "DM_OIDC_PROVIDER_CLIENT_ID", "", "", "DM_OIDC_PROVIDER_CLIENT_ID"},
		{"missing client secret", "DM_OIDC_PROVIDER_CLIENT_SECRET", "", "", "DM_OIDC_PROVIDER_CLIENT_SECRET"},
		{"missing redirect uri", "DM_OIDC_PROVIDER_REDIRECT_URI", "", "", "DM_OIDC_PROVIDER_REDIRECT_URI"},
		{"missing rt enc key", "DM_OIDC_RT_ENC_KEY", "", "", "DM_OIDC_RT_ENC_KEY"},
		{"rt enc key wrong length", "", "DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)), "32 bytes"},
		{"rt enc key not base64", "", "DM_OIDC_RT_ENC_KEY", "!!!not-base64!!!", "base64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOIDCEnv(t)
			t.Setenv("DM_OIDC_ENABLED", "true")
			t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://accounts.imocto.cn")
			t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
			t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
			t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web.imocto.cn/cb")
			t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

			if tc.unset != "" {
				t.Setenv(tc.unset, "")
			}
			if tc.setKey != "" {
				t.Setenv(tc.setKey, tc.setVal)
			}

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.errContains)
			}
		})
	}
}

func TestLoadConfigFromEnv_OverrideDurationsAndScopes(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web.imocto.cn/cb")
	t.Setenv("DM_OIDC_PROVIDER_SCOPES", "openid,email")
	t.Setenv("DM_OIDC_PROVIDER_SYNC_INTERVAL", "5m")
	t.Setenv("DM_OIDC_PROVIDER_HTTP_TIMEOUT", "30s")
	t.Setenv("DM_OIDC_PROVIDER_CLOCK_SKEW", "30s")
	t.Setenv("DM_OIDC_PROVIDER_REQUIRE_EMAIL_VERIFIED", "false")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider.SyncInterval != 5*time.Minute {
		t.Fatalf("sync_interval: %v", cfg.Provider.SyncInterval)
	}
	if cfg.Provider.HTTPTimeout != 30*time.Second {
		t.Fatalf("http_timeout: %v", cfg.Provider.HTTPTimeout)
	}
	if cfg.Provider.ClockSkew != 30*time.Second {
		t.Fatalf("clock_skew: %v", cfg.Provider.ClockSkew)
	}
	if len(cfg.Provider.Scopes) != 2 || cfg.Provider.Scopes[0] != "openid" || cfg.Provider.Scopes[1] != "email" {
		t.Fatalf("scopes: %v", cfg.Provider.Scopes)
	}
	if cfg.Provider.RequireEmailVerified {
		t.Fatal("RequireEmailVerified should be false")
	}
}

// 老的 DM_OIDC_AEGIS_* 环境变量在过渡期作为 alias 仍然可用,
// 让运维不必在重命名 PR 当天同步改部署配置。一个迭代后删除。
func TestLoadConfigFromEnv_AegisAliasBackwardsCompat(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	// 只设老 AEGIS_* 变量,不设新 PROVIDER_* 变量
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://legacy.example.com")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_ID", "legacy-cid")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_SECRET", "legacy-csecret")
	t.Setenv("DM_OIDC_AEGIS_REDIRECT_URI", "https://legacy.example.com/cb")
	t.Setenv("DM_OIDC_AEGIS_SYNC_INTERVAL", "7m")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with AEGIS aliases: %v", err)
	}
	if cfg.Provider.Issuer != "https://legacy.example.com" {
		t.Fatalf("AEGIS_ISSUER alias not applied: %q", cfg.Provider.Issuer)
	}
	if cfg.Provider.ClientID != "legacy-cid" {
		t.Fatalf("AEGIS_CLIENT_ID alias not applied: %q", cfg.Provider.ClientID)
	}
	if cfg.Provider.SyncInterval != 7*time.Minute {
		t.Fatalf("AEGIS_SYNC_INTERVAL alias not applied: %v", cfg.Provider.SyncInterval)
	}
}

// 非法 ID 必须在 LoadConfig 阶段 fail-fast,避免畸形值进路由路径。
func TestLoadConfigFromEnv_InvalidProviderIDRejected(t *testing.T) {
	for _, bad := range []string{"foo/bar", "foo bar", "FOO", "-leading", "with.dot"} {
		t.Run(bad, func(t *testing.T) {
			clearOIDCEnv(t)
			t.Setenv("DM_OIDC_ENABLED", "true")
			t.Setenv("DM_OIDC_PROVIDER_ID", bad)
			t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://x")
			t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
			t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "cs")
			t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://x/cb")
			t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("expected error for invalid ID %q", bad)
			}
			if !strings.Contains(err.Error(), "DM_OIDC_PROVIDER_ID") {
				t.Fatalf("error %q should mention DM_OIDC_PROVIDER_ID", err)
			}
		})
	}
}

// 新 key 解析失败时回退到 alias 而非默认,保护迁移期 ops 改错新值导致老配置被吞。
func TestLoadConfigFromEnv_PrimaryParseErrorFallsBackToAlias(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://x")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "cs")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://x/cb")
	// 新 key 写错(空格), 老 key 仍有效
	t.Setenv("DM_OIDC_PROVIDER_SYNC_INTERVAL", "30 minutes")
	t.Setenv("DM_OIDC_AEGIS_SYNC_INTERVAL", "7m")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider.SyncInterval != 7*time.Minute {
		t.Fatalf("expected fall-through to AEGIS alias on parse error, got %v", cfg.Provider.SyncInterval)
	}
}

// PROVIDER_* 同时与 AEGIS_* 设置时,以新名 PROVIDER_* 优先。
func TestLoadConfigFromEnv_ProviderTakesPrecedenceOverAegis(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://old.example.com")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://new.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider.Issuer != "https://new.example.com" {
		t.Fatalf("PROVIDER should override AEGIS, got %q", cfg.Provider.Issuer)
	}
}

// clearOIDCEnv 用 t.Setenv 把 key 清成 ""(底层等价于 setenv,t.Cleanup 自动复原)。
// 配合 getString 的 "ok && v != \"\"" 语义,效果等于"未设置"。
// 不用 os.Unsetenv:那样需要手动注册 cleanup,违反 t.Setenv 的并行隔离保证。
func clearOIDCEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"DM_OIDC_ENABLED",
		// 新名
		"DM_OIDC_PROVIDER_ID",
		"DM_OIDC_PROVIDER_NAME",
		"DM_OIDC_PROVIDER_ISSUER",
		"DM_OIDC_PROVIDER_CLIENT_ID",
		"DM_OIDC_PROVIDER_CLIENT_SECRET",
		"DM_OIDC_PROVIDER_REDIRECT_URI",
		"DM_OIDC_PROVIDER_SCOPES",
		"DM_OIDC_PROVIDER_SYNC_INTERVAL",
		"DM_OIDC_PROVIDER_SYNC_CONCURRENCY",
		"DM_OIDC_PROVIDER_HTTP_TIMEOUT",
		"DM_OIDC_PROVIDER_CLOCK_SKEW",
		"DM_OIDC_PROVIDER_REQUIRE_EMAIL_VERIFIED",
		"DM_OIDC_PROVIDER_REQUIRE_PKCE",
		"DM_OIDC_PROVIDER_AUTO_LINK_BY_EMAIL",
		"DM_OIDC_PROVIDER_AUTO_LINK_BY_PHONE",
		"DM_OIDC_PROVIDER_ALLOW_NEW_USER",
		// 老 alias
		"DM_OIDC_AEGIS_ISSUER",
		"DM_OIDC_AEGIS_CLIENT_ID",
		"DM_OIDC_AEGIS_CLIENT_SECRET",
		"DM_OIDC_AEGIS_REDIRECT_URI",
		"DM_OIDC_AEGIS_SCOPES",
		"DM_OIDC_AEGIS_SYNC_INTERVAL",
		"DM_OIDC_AEGIS_SYNC_CONCURRENCY",
		"DM_OIDC_AEGIS_HTTP_TIMEOUT",
		"DM_OIDC_AEGIS_CLOCK_SKEW",
		"DM_OIDC_AEGIS_REQUIRE_EMAIL_VERIFIED",
		"DM_OIDC_AEGIS_REQUIRE_PKCE",
		"DM_OIDC_AEGIS_AUTO_LINK_BY_EMAIL",
		"DM_OIDC_AEGIS_AUTO_LINK_BY_PHONE",
		"DM_OIDC_AEGIS_ALLOW_NEW_USER",
		"DM_OIDC_RT_ENC_KEY",
		// RP-Initiated Logout(可选)
		"OCTO_OIDC_POST_LOGOUT_REDIRECT_URI",
		"OCTO_OIDC_PROVIDER_END_SESSION_URL",
		"OCTO_OIDC_PROVIDER_ID_TOKEN_TTL",
		"OCTO_OIDC_LOGOUT_ALLOW_INSECURE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

// setRequiredOIDCEnv 设齐 enabled + 必填项,供下面只想验单个可选项的用例复用。
func setRequiredOIDCEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://web.imocto.cn/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// TTL:默认 7d=168h;合法 override 生效;"7d"(ParseDuration 不认 d)静默回落默认;
// 0 / 负值被钳回默认 —— 杜绝 go-redis "永不过期" footgun。
func TestLoadConfig_IDTokenTTL(t *testing.T) {
	const def = 7 * 24 * time.Hour
	cases := []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{"default", false, "", def},
		{"override 24h", true, "24h", 24 * time.Hour},
		{"invalid 7d falls back", true, "7d", def},
		{"zero clamped", true, "0", def},
		{"negative clamped", true, "-5h", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOIDCEnv(t)
			setRequiredOIDCEnv(t)
			if tc.set {
				t.Setenv("OCTO_OIDC_PROVIDER_ID_TOKEN_TTL", tc.val)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Provider.IDTokenTTL != tc.want {
				t.Errorf("IDTokenTTL = %v, want %v", cfg.Provider.IDTokenTTL, tc.want)
			}
		})
	}
}

// validateLogoutURL:空=放行(可选);绝对 https 放行;相对/javascript:/非 http(s) 拒绝;
// http 仅在 OCTO_OIDC_LOGOUT_ALLOW_INSECURE=1 时放行。
func TestValidateLogoutURL(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		insecure bool
		wantErr  bool
	}{
		{"empty ok", "", false, false},
		{"https ok", "https://app.example.com/login", false, false},
		{"relative rejected", "/login", false, true},
		{"javascript rejected", "javascript:alert(1)", false, true},
		{"ftp rejected", "ftp://host/x", false, true},
		{"http rejected by default", "http://app.example.com/login", false, true},
		{"http allowed with dev flag", "http://app.example.com/login", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.insecure {
				t.Setenv("OCTO_OIDC_LOGOUT_ALLOW_INSECURE", "1")
			}
			err := validateLogoutURL("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", tc.raw)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.raw, err)
			}
		})
	}
}

// LoadConfig 对非法 logout URL 启动期 fail-loud(整模块拒绝加载)。
func TestLoadConfig_RejectsInsecureLogoutURL(t *testing.T) {
	clearOIDCEnv(t)
	setRequiredOIDCEnv(t)
	t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "http://insecure.example.com/login")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected LoadConfig to reject non-https post_logout_redirect_uri")
	} else if !strings.Contains(err.Error(), "OCTO_OIDC_POST_LOGOUT_REDIRECT_URI") {
		t.Errorf("error should name the offending env, got: %v", err)
	}
}
