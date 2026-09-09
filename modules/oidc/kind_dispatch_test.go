package oidc

// kind_dispatch_test.go — 决定"用哪个实现"的那个字段必须和校验看到同一个值。
//
// Kind 走的是未 trim 的 getString,而 oidcboot.ValidateKind / UpstreamBaseURL 内部都
// 归一化。于是 `OCTO_OIDC_PROVIDER_KIND=" oauth2"` 有两条后果,第二条尤其难查:
//
//   A. 无 BASE_URL:ValidateKind 归一化后按 oauth2 要求 base URL → 拒绝启动(全 404),
//      而镜像归一化后也认 oauth2、能取到 issuer 回落 → 报"已配置" → local_off 生效
//      → SSO-only 部署没有任何登录入口。
//
//   B. 有 BASE_URL:**LoadConfig 成功返回**。applyKindConstraints 的 switch 拿未 trim 的
//      值比,落到 default → oauth2 的收窄一条都不生效(scopes 仍是 OIDC 默认);
//      NewAuthProvider 同样落 default → 对一个**没有 Discovery 文档**的 IdP 做
//      标准 OIDC Discovery → NewClient 失败 → provider 为 nil → 全端点 500。
//
// B 不是"拒绝启动",是**静默走错协议**:配置看起来是好的,LoadConfig 没报错。

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func kindDispatchEnv(t *testing.T) {
	t.Helper()
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// 带空白的 kind 必须被归一化,而不是掉进 default 分支。
func TestLoadConfig_KindIsNormalisedBeforeDispatch(t *testing.T) {
	for _, raw := range []string{" oauth2", "oauth2 ", "\toauth2\n"} {
		t.Run(raw, func(t *testing.T) {
			kindDispatchEnv(t)
			t.Setenv("OCTO_OIDC_PROVIDER_KIND", raw)
			t.Setenv("OCTO_OIDC_PROVIDER_BASE_URL", "https://idp.example.com")

			cfg, err := LoadConfig()
			require.NoError(t, err, "with an explicit base URL this config boots")

			if cfg.Provider.Kind != KindOAuth2 {
				t.Fatalf("Kind = %q, want %q. The switch in applyKindConstraints and the one "+
					"in NewAuthProvider both compare against this value, so an untrimmed one "+
					"falls to default: no oauth2 narrowing is applied and provider "+
					"construction runs standard OIDC Discovery against an IdP that has none",
					cfg.Provider.Kind, KindOAuth2)
			}
			// oauth2 的收窄必须真的生效 —— 这是"落进 default 了"最直接的证据。
			require.Equal(t, []string{"read"}, cfg.Provider.Scopes,
				"oauth2 narrowing did not apply, so the switch took default:")
			require.Zero(t, cfg.Provider.SyncInterval,
				"oauth2 narrowing did not apply (sync is not available on this kind)")
			require.False(t, cfg.Provider.RequirePKCE,
				"oauth2 narrowing did not apply (this protocol has no PKCE)")
		})
	}
}

// 无 BASE_URL 时,带空白的 kind 与不带空白的必须得出同一个结论。
func TestLoadConfig_WhitespaceKindAgreesWithTrimmedKind(t *testing.T) {
	run := func(kind string) error {
		kindDispatchEnv(t)
		t.Setenv("OCTO_OIDC_PROVIDER_KIND", kind)
		_, err := LoadConfig()
		return err
	}
	trimmedErr := run("oauth2")
	paddedErr := run(" oauth2")
	if (trimmedErr == nil) != (paddedErr == nil) {
		t.Errorf("a leading space changed the boot verdict: trimmed=%v padded=%v", trimmedErr, paddedErr)
	}
}

// 未知 kind 必须**拒绝**,不能猜成标准 OIDC。
//
// NewAuthProvider 的 default 分支现在会去建标准 OIDC 客户端 —— 对一个拼错 kind 的
// 部署,那意味着"对着一个没有 Discovery 的 IdP 做 Discovery",症状是全端点 500,
// 而运维手上只有一个拼写错误和一堆 500。不猜比猜好。
func TestNewAuthProvider_RefusesUnknownKindInsteadOfGuessingOIDC(t *testing.T) {
	_, err := NewAuthProvider(nil, ProviderConfig{
		ID: "oidc", Kind: ProviderKind("oauth-2"), // 拼错
		Issuer: "https://idp.example.com", ClientID: "cid", ClientSecret: "s",
		RedirectURI: "https://app.example.com/cb",
		HTTPTimeout: time.Second,
	}, func(string, error) {})
	if err == nil {
		t.Fatal("an unknown provider kind was accepted; it silently becomes standard OIDC " +
			"Discovery, which fails against an IdP that has no Discovery document and leaves " +
			"every endpoint answering 500 with only a typo to show for it")
	}
}
