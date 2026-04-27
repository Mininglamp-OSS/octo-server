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
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_AEGIS_REDIRECT_URI", "https://web.imocto.cn/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Fatal("expected Enabled=true")
	}
	if cfg.Aegis.Issuer != "https://accounts.imocto.cn" {
		t.Fatalf("issuer mismatch: %q", cfg.Aegis.Issuer)
	}
	if cfg.Aegis.ClientID != "cid" || cfg.Aegis.ClientSecret != "csecret" {
		t.Fatal("client id/secret mismatch")
	}
	wantScopes := []string{"openid", "profile", "email", "phone", "offline_access"}
	if len(cfg.Aegis.Scopes) != len(wantScopes) {
		t.Fatalf("default scopes mismatch: %v", cfg.Aegis.Scopes)
	}
	if cfg.Aegis.SyncInterval != 15*time.Minute {
		t.Fatalf("default sync_interval: %v", cfg.Aegis.SyncInterval)
	}
	if cfg.Aegis.HTTPTimeout != 10*time.Second {
		t.Fatalf("default http_timeout: %v", cfg.Aegis.HTTPTimeout)
	}
	if cfg.Aegis.ClockSkew != 60*time.Second {
		t.Fatalf("default clock_skew: %v", cfg.Aegis.ClockSkew)
	}
	if !cfg.Aegis.RequirePKCE || !cfg.Aegis.RequireEmailVerified || !cfg.Aegis.AutoLinkByEmail || !cfg.Aegis.AllowNewUser {
		t.Fatal("default safety flags should be true")
	}
	if len(cfg.Aegis.RefreshTokenEncryptionKey) != 32 {
		t.Fatalf("RTEncryptionKey len=%d", len(cfg.Aegis.RefreshTokenEncryptionKey))
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
		name   string
		unset       string
		setKey      string
		setVal      string
		errContains string // 错误消息须包含此关键字,捕获"因预期外原因报错"的回归
	}{
		{"missing issuer", "DM_OIDC_AEGIS_ISSUER", "", "", "DM_OIDC_AEGIS_ISSUER"},
		{"missing client id", "DM_OIDC_AEGIS_CLIENT_ID", "", "", "DM_OIDC_AEGIS_CLIENT_ID"},
		{"missing client secret", "DM_OIDC_AEGIS_CLIENT_SECRET", "", "", "DM_OIDC_AEGIS_CLIENT_SECRET"},
		{"missing redirect uri", "DM_OIDC_AEGIS_REDIRECT_URI", "", "", "DM_OIDC_AEGIS_REDIRECT_URI"},
		{"missing rt enc key", "DM_OIDC_RT_ENC_KEY", "", "", "DM_OIDC_RT_ENC_KEY"},
		{"rt enc key wrong length", "", "DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)), "32 bytes"},
		{"rt enc key not base64", "", "DM_OIDC_RT_ENC_KEY", "!!!not-base64!!!", "base64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOIDCEnv(t)
			t.Setenv("DM_OIDC_ENABLED", "true")
			t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://accounts.imocto.cn")
			t.Setenv("DM_OIDC_AEGIS_CLIENT_ID", "cid")
			t.Setenv("DM_OIDC_AEGIS_CLIENT_SECRET", "csecret")
			t.Setenv("DM_OIDC_AEGIS_REDIRECT_URI", "https://web.imocto.cn/cb")
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
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_AEGIS_REDIRECT_URI", "https://web.imocto.cn/cb")
	t.Setenv("DM_OIDC_AEGIS_SCOPES", "openid,email")
	t.Setenv("DM_OIDC_AEGIS_SYNC_INTERVAL", "5m")
	t.Setenv("DM_OIDC_AEGIS_HTTP_TIMEOUT", "30s")
	t.Setenv("DM_OIDC_AEGIS_CLOCK_SKEW", "30s")
	t.Setenv("DM_OIDC_AEGIS_REQUIRE_EMAIL_VERIFIED", "false")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Aegis.SyncInterval != 5*time.Minute {
		t.Fatalf("sync_interval: %v", cfg.Aegis.SyncInterval)
	}
	if cfg.Aegis.HTTPTimeout != 30*time.Second {
		t.Fatalf("http_timeout: %v", cfg.Aegis.HTTPTimeout)
	}
	if cfg.Aegis.ClockSkew != 30*time.Second {
		t.Fatalf("clock_skew: %v", cfg.Aegis.ClockSkew)
	}
	if len(cfg.Aegis.Scopes) != 2 || cfg.Aegis.Scopes[0] != "openid" || cfg.Aegis.Scopes[1] != "email" {
		t.Fatalf("scopes: %v", cfg.Aegis.Scopes)
	}
	if cfg.Aegis.RequireEmailVerified {
		t.Fatal("RequireEmailVerified should be false")
	}
}

// clearOIDCEnv 用 t.Setenv 把 key 清成 ""(底层等价于 setenv,t.Cleanup 自动复原)。
// 配合 getString 的 "ok && v != \"\"" 语义,效果等于"未设置"。
// 不用 os.Unsetenv:那样需要手动注册 cleanup,违反 t.Setenv 的并行隔离保证。
func clearOIDCEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"DM_OIDC_ENABLED",
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
		"DM_OIDC_AEGIS_ALLOW_NEW_USER",
		"DM_OIDC_RT_ENC_KEY",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}
