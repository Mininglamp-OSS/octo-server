package oidc

import (
	"encoding/base64"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/oidcboot"
)

// LoadConfig 必须拒绝 oidcboot 声明为不可启动的每一种配置。
//
// 这条测试与 modules/common 那侧的同名钉合测试遍历**同一张表**
// (oidcboot.RefusedScenarios)。两边因此不可能对某个场景给出不同判断 ——
// 而"给出不同判断"正是上一版的缺陷:本模块新增了 5 个致命条件,
// common 的镜像一个都没跟上,于是一个 KIND 拼写错误会让 OIDC 端点全部 404、
// 同时 login.local_off 仍被当作"第三方登录已配置"而生效,SSO-only 部署
// 因此没有任何可用登录方式,只能重新发版恢复。
func TestLoadConfig_RefusesEveryUnbootableScenario(t *testing.T) {
	for _, sc := range oidcboot.RefusedScenarios {
		t.Run(sc.Name, func(t *testing.T) {
			setBaseOIDCEnv(t)
			for k, v := range sc.Env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); err == nil {
				t.Errorf("LoadConfig accepted a configuration oidcboot declares unbootable; "+
					"modules/common would then also report it as configured, and the two "+
					"together are what produce a total login lockout (env=%v)", sc.Env)
			}
		})
	}
}

// 反面:oidcboot 认为可启动的配置,LoadConfig 也必须接受。
//
// 只测"该拒的都拒了"会让一个把所有配置都拒掉的实现通过。
func TestLoadConfig_AcceptsBootableKinds(t *testing.T) {
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
		"oauth2 with app id and post-logout redirect": {
			"OCTO_OIDC_PROVIDER_KIND":            "oauth2",
			"OCTO_OIDC_PROVIDER_APP_ID":          "app1",
			"OCTO_OIDC_POST_LOGOUT_REDIRECT_URI": "https://app.example.com/login",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			setBaseOIDCEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); err != nil {
				t.Errorf("LoadConfig refused a bootable configuration: %v (env=%v)", err, env)
			}
		})
	}
}

// 确保基线 env 本身是可启动的 —— 否则上面的反面用例在测一个假前提。
func TestSetBaseOIDCEnv_IsItselfBootable(t *testing.T) {
	setBaseOIDCEnv(t)
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("the shared base env is not bootable: %v", err)
	}
	// 顺带钉住 RT key 的形状约束,common 那侧也复刻了它。
	setBaseOIDCEnv(t)
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := LoadConfig(); err == nil {
		t.Error("a 16-byte RT key was accepted; it must be exactly 32")
	}
}
