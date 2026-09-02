package common

import (
	"encoding/base64"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/oidcboot"
)

// isOIDCFullyConfigured 必须对 oidcboot 声明为不可启动的每一种配置答 false。
//
// 这条测试与 modules/oidc 那侧的 TestLoadConfig_RefusesEveryUnbootableScenario
// 遍历**同一张表**。为什么必须钉在一起:
//
//	OIDC 配置有误
//	  → modules/oidc.LoadConfig 报错 → 五个端点全注册成 404 → SSO 不可用
//	  → 若本函数仍答 true → anyThirdPartyLoginConfigured 为真
//	  → login.local_off=1 被采信 → 密码登录也关着
//	  → SSO-only 部署没有任何可用登录方式,只能重新发版
//
// 本函数曾是 modules/oidc 那份校验的手工镜像。oidc 侧新增 5 个致命条件时镜像
// 没有跟上,上面那条链路因此是活的。规则现在只有一份(pkg/oidcboot),这张
// 共享表保证将来新增 provider kind 时两边不会再次分叉。
func TestIsOIDCFullyConfigured_FalseForEveryUnbootableScenario(t *testing.T) {
	for _, sc := range oidcboot.RefusedScenarios {
		t.Run(sc.Name, func(t *testing.T) {
			setBaseOIDCEnvForMirror(t)
			for k, v := range sc.Env {
				t.Setenv(k, v)
			}
			if isOIDCFullyConfigured() {
				t.Errorf("reported as fully configured, but modules/oidc refuses to boot "+
					"with it; login.local_off would then be honoured while every OIDC "+
					"endpoint answers 404 (env=%v)", sc.Env)
			}
		})
	}
}

// 反面:可启动的配置必须被认作已配置,否则 local_off 的安全兜底会在正常部署上
// 误触发,把密码登录意外打开。
func TestIsOIDCFullyConfigured_TrueForBootableKinds(t *testing.T) {
	cases := map[string]map[string]string{
		"default kind": {},
		"explicit oidc": {
			"OCTO_OIDC_PROVIDER_KIND": "oidc",
		},
		"oauth2 with base url": {
			"OCTO_OIDC_PROVIDER_KIND":     "oauth2",
			"OCTO_OIDC_PROVIDER_BASE_URL": "https://idp.example.com",
		},
		"oauth2 falling back to the issuer as base url": {
			"OCTO_OIDC_PROVIDER_KIND": "oauth2",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			setBaseOIDCEnvForMirror(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if !isOIDCFullyConfigured() {
				t.Errorf("a bootable configuration was reported as not configured; "+
					"local_off's safety fallback would flip password login back on (env=%v)", env)
			}
		})
	}
}

// setBaseOIDCEnvForMirror 铺与 modules/oidc 测试同一组必填 env。
//
// 两份 setup 里的键必须一致 —— 它们代表同一个"最小可用配置"。RT key 的 32 字节
// 约束在这里也复刻,理由与本函数存在的理由相同:一份起不来的配置不能被标记为
// 已配置。
func setBaseOIDCEnvForMirror(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OCTO_OIDC_PROVIDER_KIND", "OCTO_OIDC_PROVIDER_BASE_URL", "OCTO_OIDC_PROVIDER_APP_ID",
		"OCTO_OIDC_PROVIDER_END_SESSION_URL", "OCTO_OIDC_POST_LOGOUT_REDIRECT_URI",
		"OCTO_OIDC_ALLOW_INSECURE_UPSTREAM",
		"DM_OIDC_PROVIDER_AUTO_LINK_BY_EMAIL", "DM_OIDC_AEGIS_AUTO_LINK_BY_EMAIL",
		"DM_OIDC_PROVIDER_REQUIRE_EMAIL_VERIFIED", "DM_OIDC_AEGIS_REQUIRE_EMAIL_VERIFIED",
		"DM_OIDC_PROVIDER_ID",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}
