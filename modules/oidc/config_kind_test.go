package oidc

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// setBaseOIDCEnv 铺一份最小可用的必填 env。
//
// 刻意与 config_test.go 用同一组必填项 —— 本次改动**不新增任何必填 env**。
// 原因:modules/common/system_settings.go 的 isOIDCFullyConfigured() 保有一份
// 该列表的镜像副本(为避开 common→oidc→user→common 的 import 循环)。两处一旦
// 漂移,isOIDCFullyConfigured 会误答,anyThirdPartyLoginConfigured 跟着误判,
// login.local_off 的安全兜底可能静默翻回 false —— 全站恢复密码登录。
func setBaseOIDCEnv(t *testing.T) {
	t.Helper()
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// 不配 KIND 时必须落 oidc —— 存量部署没有这个 env,不能因为本次改动改变行为。
func TestLoadConfig_KindDefaultsToOIDC(t *testing.T) {
	setBaseOIDCEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Provider.Kind != KindOIDC {
		t.Errorf("Kind = %q, want %q (existing deployments have no KIND env set)", cfg.Provider.Kind, KindOIDC)
	}
}

// kind=oidc 显式配置时,所有字段必须与"不配 KIND"完全一致。
// 这是重构的回归保护:标准 OIDC 部署的配置解析不能有任何行为变化。
func TestLoadConfig_ExplicitOIDCKindMatchesDefault(t *testing.T) {
	setBaseOIDCEnv(t)
	t.Setenv("DM_OIDC_PROVIDER_SCOPES", "openid,profile,email")
	t.Setenv("DM_OIDC_PROVIDER_SYNC_INTERVAL", "20m")
	implicit, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig(implicit): %v", err)
	}

	t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oidc")
	explicit, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig(explicit): %v", err)
	}

	if explicit.Provider.SyncInterval != implicit.Provider.SyncInterval {
		t.Errorf("SyncInterval diverged: %v vs %v", explicit.Provider.SyncInterval, implicit.Provider.SyncInterval)
	}
	if explicit.Provider.RequirePKCE != implicit.Provider.RequirePKCE {
		t.Errorf("RequirePKCE diverged: %v vs %v", explicit.Provider.RequirePKCE, implicit.Provider.RequirePKCE)
	}
	if strings.Join(explicit.Provider.Scopes, ",") != strings.Join(implicit.Provider.Scopes, ",") {
		t.Errorf("Scopes diverged: %v vs %v", explicit.Provider.Scopes, implicit.Provider.Scopes)
	}
	if explicit.Provider.SyncInterval != 20*time.Minute {
		t.Errorf("SyncInterval = %v, want 20m (must not be clamped for the oidc kind)", explicit.Provider.SyncInterval)
	}
}

func TestLoadConfig_UnknownKindRejected(t *testing.T) {
	setBaseOIDCEnv(t)
	t.Setenv("OCTO_OIDC_PROVIDER_KIND", "saml")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("want error for an unknown provider kind; a typo must not silently fall back to oidc")
	}
}

func TestLoadConfig_OAuth2Kind(t *testing.T) {
	t.Run("base_url_defaults_to_issuer", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		// ISSUER 在这个 kind 下语义变为"身份命名空间",同时兼作站点根的缺省值,
		// 这样就不必新增必填 env(见 setBaseOIDCEnv 注释)。
		if cfg.Provider.BaseURL != "https://idp.example.com" {
			t.Errorf("BaseURL = %q, want it to default to the issuer value", cfg.Provider.BaseURL)
		}
	})

	t.Run("base_url_explicit_override", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("OCTO_OIDC_PROVIDER_BASE_URL", "https://gateway.example.com/auth")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Provider.BaseURL != "https://gateway.example.com/auth" {
			t.Errorf("BaseURL = %q, want the explicit override", cfg.Provider.BaseURL)
		}
		// issuer 不受影响:它是身份命名空间,与站点根解耦。
		if cfg.Provider.Issuer != "https://idp.example.com" {
			t.Errorf("Issuer = %q, want it untouched by the base URL override", cfg.Provider.Issuer)
		}
	})

	t.Run("scopes_forced_to_read", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		// 运维照抄 OIDC 配置的常见情形。该 IdP 的 authorize 端点只认 scope=read,
		// 发别的值只会被上游拒,所以这里收窄而不是原样透传。
		t.Setenv("DM_OIDC_PROVIDER_SCOPES", "openid,profile,email,identity_verification")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if len(cfg.Provider.Scopes) != 1 || cfg.Provider.Scopes[0] != "read" {
			t.Errorf("Scopes = %v, want [read]", cfg.Provider.Scopes)
		}
	})

	t.Run("pkce_forced_off", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("DM_OIDC_PROVIDER_REQUIRE_PKCE", "true")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Provider.RequirePKCE {
			t.Error("RequirePKCE = true; the upstream authorize endpoint has no code_challenge parameter")
		}
	})

	t.Run("sync_forced_disabled", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("DM_OIDC_PROVIDER_SYNC_INTERVAL", "15m")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		// 该 IdP 返回 refresh_token 但从未给出刷新端点。留着非零间隔只会让
		// sync worker 每 15 分钟空转一次,并让运维误以为账号状态回传在工作。
		if cfg.Provider.SyncInterval != 0 {
			t.Errorf("SyncInterval = %v, want 0 (no documented refresh endpoint exists)", cfg.Provider.SyncInterval)
		}
	})

	t.Run("app_id_is_read", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("OCTO_OIDC_PROVIDER_APP_ID", "app1")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Provider.AppID != "app1" {
			t.Errorf("AppID = %q, want app1", cfg.Provider.AppID)
		}
	})

	t.Run("rejects_end_session_url", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("OCTO_OIDC_PROVIDER_END_SESSION_URL", "https://idp.example.com/end-session")
		// 该 kind 的登出端点由 app id 拼路径得出,这个 override 语义不成立。
		// 静默忽略会让运维以为登出配好了,所以启动期直接拒。
		if _, err := LoadConfig(); err == nil {
			t.Fatal("want error: END_SESSION_URL is not applicable to this kind and must not be silently ignored")
		}
	})

	t.Run("logout_redirect_without_app_id_rejected", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "https://app.example.com/login")
		// 配了回跳地址却没有 app id,登出 URL 拼不出来 —— 会静默降级成
		// "仅清本地",而运维完全不知道。启动期报出来。
		if _, err := LoadConfig(); err == nil {
			t.Fatal("want error: a post-logout redirect without an app id yields no usable logout URL")
		}
	})

	t.Run("logout_fully_configured_is_accepted", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "https://app.example.com/login")
		t.Setenv("OCTO_OIDC_PROVIDER_APP_ID", "app1")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Provider.AppID != "app1" || cfg.Provider.PostLogoutRedirectURI == "" {
			t.Errorf("logout config not loaded: appID=%q redirect=%q",
				cfg.Provider.AppID, cfg.Provider.PostLogoutRedirectURI)
		}
	})

	t.Run("app_id_must_be_url_safe", func(t *testing.T) {
		setBaseOIDCEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
		t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "https://app.example.com/login")
		// app id 直接拼进 URL 路径段。误配 ../ 会让请求落到别的端点上,
		// 必须在启动期就挡住,而不是等运行期拼 URL 时才发现。
		t.Setenv("OCTO_OIDC_PROVIDER_APP_ID", "../../evil")
		if _, err := LoadConfig(); err == nil {
			t.Fatal("want error for a non-URL-safe app id")
		}
	})

	t.Run("required_env_list_is_unchanged", func(t *testing.T) {
		// 这个 kind 不减少任何必填项 —— 见 setBaseOIDCEnv 的注释。
		for _, missing := range []string{
			"DM_OIDC_PROVIDER_ISSUER",
			"DM_OIDC_PROVIDER_CLIENT_ID",
			"DM_OIDC_PROVIDER_CLIENT_SECRET",
			"DM_OIDC_PROVIDER_REDIRECT_URI",
			"DM_OIDC_RT_ENC_KEY",
		} {
			t.Run(missing, func(t *testing.T) {
				setBaseOIDCEnv(t)
				t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
				t.Setenv(missing, "")
				if _, err := LoadConfig(); err == nil {
					t.Fatalf("%s is still required for the oauth2 kind, but LoadConfig succeeded without it", missing)
				}
			})
		}
	})
}
