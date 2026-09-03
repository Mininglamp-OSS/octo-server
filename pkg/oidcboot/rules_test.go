package oidcboot

import (
	"strings"
	"testing"
)

// 这些规则决定"这份 OIDC 环境配置能不能起得来"。
//
// 它们之所以必须在一个叶子包里、而不是在 modules/oidc 和 modules/common 各写
// 一份,是因为两份会漂移,而漂移的后果是**全站登录锁死**:
//
//	KIND 打错一个字
//	  → oidc.LoadConfig 报错 → New 返回时 cfg 为 nil → 五个端点全注册成 404
//	  → 但 common 的 isOIDCFullyConfigured() 若仍答 "已配置"
//	  → anyThirdPartyLoginConfigured 为真 → login.local_off=1 被采信
//	  → 密码登录也是关的 → SSO-only 部署没有任何可用登录方式,只能重新发版
//
// modules/oidc/config.go 里那段注释早就警告过这个漂移,然后 PR 加了 5 个新的
// 致命条件、一个都没镜像过去 —— 写警告不等于遵守警告。所以规则下沉到这里,
// 并由 RefusedScenarios 把两边的测试钉在同一张表上。
func TestValidateKind_AcceptsBootableConfigurations(t *testing.T) {
	cases := map[string]KindInput{
		"oidc kind with nothing kind-specific set": {
			Kind: "oidc",
		},
		"oidc kind with an explicit issuer only": {
			Kind: "oidc", RequireEmailVerified: true,
		},
		"oauth2 kind with https base url and no logout": {
			Kind: "oauth2", BaseURL: "https://idp.example.com",
			RequireEmailVerified: true,
		},
		"oauth2 kind with app id and redirect": {
			Kind: "oauth2", BaseURL: "https://idp.example.com",
			AppID: "app1", PostLogoutRedirectURI: "https://app.example.com/login",
			RequireEmailVerified: true,
		},
		"oauth2 kind with autolink off can skip verified-email requirement": {
			Kind: "oauth2", BaseURL: "https://idp.example.com",
			AutoLinkByEmail: false, RequireEmailVerified: false,
		},
		"http base url with the explicit escape hatch": {
			Kind: "oauth2", BaseURL: "http://idp-pre.example.com",
			AllowInsecureUpstream: true, RequireEmailVerified: true,
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateKind(in); err != nil {
				t.Errorf("ValidateKind refused a bootable configuration: %v", err)
			}
		})
	}
}

// RefusedScenarios 是本包声明的"必须拒绝"清单。modules/oidc 与 modules/common
// 的测试都遍历它,所以两边不可能对某个场景有不同判断。
func TestValidateKind_RefusesEveryDeclaredScenario(t *testing.T) {
	if len(RefusedScenarios) == 0 {
		t.Fatal("RefusedScenarios is empty; the pinning tests in modules/oidc and " +
			"modules/common would then assert nothing")
	}
	for _, sc := range RefusedScenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.SkipRuleCheck {
				t.Skip("refusal comes from the caller's required-env handling, not from the rules")
			}
			if err := ValidateKind(sc.Input); err == nil {
				t.Errorf("scenario %q was accepted but is declared as refused", sc.Name)
			}
		})
	}
}

// 拒绝信息必须点出是哪个 env 键出了问题 —— 运维拿到的就是这行日志。
func TestValidateKind_ErrorNamesTheOffendingKey(t *testing.T) {
	for _, sc := range RefusedScenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.SkipRuleCheck {
				t.Skip("refusal comes from the caller's required-env handling, not from the rules")
			}
			err := ValidateKind(sc.Input)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if sc.ExpectKeyInError == "" {
				return
			}
			if !contains(err.Error(), sc.ExpectKeyInError) {
				t.Errorf("error %q does not mention %q; an operator cannot act on it",
					err.Error(), sc.ExpectKeyInError)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 启动期与运行期必须用**同一个** app id 判据。
//
// 曾经是两套:oidcboot 用 `^[A-Za-z0-9_-]+$`,provider 运行期用
// `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`。于是 `_tenant` 或超过 64 字符的 app id 能过
// 启动校验,却在运行期被拒 —— 而运行期那里 LogoutURL 吞掉错误、返回 ("", false),
// 登出静默降级成"只清本地"。这正是启动期 fail-loud 规则想避免的失效,只是从另一条
// 路走到了同一个结果。
func TestValidAppID_RefusesWhatTheProviderWouldAlsoRefuse(t *testing.T) {
	cases := map[string]bool{ // value = 应当被接受
		"app1":       true,
		"a":          true,
		"app-1_x":    true,
		"2363":       true,
		"_tenant":    false, // 不能以 _ 开头(运行期拒,启动期曾放过)
		"-tenant":    false,
		"":           false,
		"../../evil": false,
		"app/1":      false,
		"app 1":      false,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := ValidAppID(in)
			if got != want {
				t.Errorf("ValidAppID(%q) = %v, want %v", in, got, want)
			}
		})
	}
	// 长度上限也要一致 —— 超长 app id 拼进 URL 路径段同样是运行期才炸。
	if ValidAppID(strings.Repeat("a", 65)) {
		t.Error("a 65-character app id was accepted; the provider refuses it at runtime, " +
			"where the error is swallowed and logout silently degrades to local-only")
	}
	if !ValidAppID(strings.Repeat("a", 64)) {
		t.Error("a 64-character app id must be accepted")
	}
}
