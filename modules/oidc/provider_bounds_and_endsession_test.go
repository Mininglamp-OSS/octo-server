package oidc

// provider_bounds_and_endsession_test.go
//
// 两件事:
//   1. issuer 长度在**构造期**就要拒 —— 运维配错不该等到第一个用户登录才炸;
//   2. "该不该缓存 id_token" 要读 Capabilities,不能对具体类型做断言。
// -----------------------------------------------------------------------------

import (
	"context"
	"strings"
	"testing"
)

// issuer 与 subject 同在 uk_issuer_subject 里,超长的后果一样(截断则两个命名
// 空间塌成一个)。区别在于 issuer 是**运维配置的常量**,所以能在构造期就拒 ——
// 那比等到运行期强:一个配错的部署会在启动日志里立刻暴露,而不是让第一个登录
// 的人拿到 500,再由别人去猜。
func TestNewOAuth2Provider_RefusesOverlongIssuer(t *testing.T) {
	m := newMockOAuth2Provider(t)
	cfg := m.providerConfig()
	cfg.Issuer = "https://idp.example.com/" +
		strings.Repeat("a", issuerMaxLen+1-len("https://idp.example.com/"))

	if _, err := newOAuth2Provider(cfg); err == nil {
		t.Fatalf("newOAuth2Provider accepted a %d-byte issuer; the column is VARCHAR(%d) and "+
			"the value is written to user_oidc_identity.issuer inside uk_issuer_subject",
			len(cfg.Issuer), issuerMaxLen)
	}
}

// 边界:正好等于列宽必须放行。
func TestNewOAuth2Provider_AcceptsIssuerAtExactlyTheColumnWidth(t *testing.T) {
	m := newMockOAuth2Provider(t)
	cfg := m.providerConfig()
	cfg.Issuer = "https://idp.example.com/" +
		strings.Repeat("a", issuerMaxLen-len("https://idp.example.com/"))
	if len(cfg.Issuer) != issuerMaxLen {
		t.Fatalf("fixture is wrong: issuer is %d bytes, want %d", len(cfg.Issuer), issuerMaxLen)
	}
	if _, err := newOAuth2Provider(cfg); err != nil {
		t.Fatalf("an issuer exactly at the column width was refused: %v", err)
	}
}

// wrappedProvider 一个把 AuthProvider **包一层**的实现 —— 装饰器,或者将来接的
// 第三个 IdP。它满足接口,能力声明照抄被包的那个。
//
// 这个 double 存在的理由很具体:原先"要不要缓存 id_token"的判断写成
// `o.provider.(*oidcProvider)`,对**具体类型**断言。任何一层包装都会让断言失败,
// 于是 RP-Initiated Logout 静默消失(和这个 PR 已经栽过一次的那个回归同样的
// 静默方式)。而 provider.go 自己写着"业务分支一律读 Capabilities,不读 Kind",
// Capabilities.IDToken 就是为这个判断定义的。
//
// 这也不是假想:本轮为 subject 上限守卫权衡过一个 AuthProvider 装饰器方案 ——
// 若采用,它当场就会踩掉这个 logout 能力。
type wrappedProvider struct{ AuthProvider }

func TestEndSessionEndpointForLogout_ReadsCapabilitiesNotConcreteType(t *testing.T) {
	oidcFx := oidcConformanceFixture(t)
	oauth2Fx := oauth2ConformanceFixture(t)

	t.Run("standard oidc exposes its endpoint", func(t *testing.T) {
		if got := endSessionEndpointForLogout(oidcFx.provider); got == "" {
			t.Error("the standard OIDC provider reported no end_session endpoint, so the " +
				"id_token cache would never be wired and logout could not carry id_token_hint")
		}
	})

	t.Run("a wrapped id_token-capable provider still exposes it", func(t *testing.T) {
		wrapped := wrappedProvider{AuthProvider: oidcFx.provider}
		if !wrapped.Capabilities().IDToken {
			t.Fatal("fixture is wrong: the wrapper must report IDToken=true")
		}
		if got := endSessionEndpointForLogout(wrapped); got == "" {
			t.Error("one layer of wrapping lost the end_session endpoint. The decision must " +
				"read Capabilities().IDToken plus an interface-level accessor, not assert on " +
				"*oidcProvider — otherwise any decorator or a third id_token-capable provider " +
				"silently loses RP-Initiated Logout, exactly the regression this PR already had")
		}
	})

	t.Run("a provider without id_token reports nothing", func(t *testing.T) {
		if oauth2Fx.provider.Capabilities().IDToken {
			t.Fatal("fixture is wrong: plain OAuth2 has no id_token")
		}
		if got := endSessionEndpointForLogout(oauth2Fx.provider); got != "" {
			t.Errorf("a provider without id_token reported an end_session endpoint %q; "+
				"RP-Initiated Logout needs id_token_hint, so there is nothing to wire", got)
		}
	})
}

// 契约:声明 IDToken=false 的实现必须在 EndSessionEndpoint 上返回空 —— 否则上层
// 会把一个拿不到 id_token_hint 的端点当成可用的 RP-logout。
func TestAuthProviderConformance_EndSessionMatchesIDTokenCapability(t *testing.T) {
	for _, fx := range conformanceFixtures(t) {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			cap := fx.provider.Capabilities()
			ep := fx.provider.EndSessionEndpoint()
			if !cap.IDToken && ep != "" {
				t.Errorf("Capabilities().IDToken=false but EndSessionEndpoint()=%q; without an "+
					"id_token there is no id_token_hint to send", ep)
			}
		})
	}
}

// 契约:issuer 必须落在列宽内 —— 它是身份唯一键的一半。
func TestAuthProviderConformance_IssuerFitsTheIdentityColumn(t *testing.T) {
	for _, fx := range conformanceFixtures(t) {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			if n := len(fx.provider.Issuer()); n > issuerMaxLen {
				t.Errorf("Issuer() is %d bytes, exceeds the %d-byte identity column; the INSERT "+
					"would fail after the user row exists, or truncate and merge namespaces",
					n, issuerMaxLen)
			}
		})
	}
}

// 契约:任何 provider 都必须拒绝"短纯数字"的上游 subject。
//
// 这条规则原先只挂在 plain-OAuth2 的信封解析上。它属于**上游断言的 subject** ——
// 工号由人事系统分配、会在离职/入职之间复用,而 (issuer, subject) 是不可变主键,
// 于是新人会被指到前任的账号上。这个论证对任何上游 IdP 都成立,所以两个实现都要
// 有,而且必须由契约钉住:两处各写一遍的东西迟早只剩一处。
//
// 注意它**不**属于共享入口:我方自己派生的 subject(业务 JWT 的 userId)是数据库
// 主键,不复用,小取值正常 —— 见 checkUpstreamSubjectShape。
func TestAuthProviderConformance_RefusesShortNumericUpstreamSubject(t *testing.T) {
	t.Run("oauth2", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.UserInfoBody = `{"success":true,"code":"200","requestId":"req-empno",
		  "data":{"sub":"7654321","nickname":"n"}}`
		p, err := newOAuth2Provider(m.providerConfig())
		if err != nil {
			t.Fatalf("newOAuth2Provider: %v", err)
		}
		if _, err := p.IdentityFromClientCredential(context.Background(), "any-token"); err == nil {
			t.Error("plain OAuth2 accepted a 7-digit numeric subject")
		}
	})

	t.Run("oidc", func(t *testing.T) {
		mp := NewMockProvider(t)
		const code = "code-empno"
		mp.PrepCode(code, "7654321", "")
		p, err := newOIDCProvider(oidcProviderConfig{
			Client: newTestClient(t, mp),
			Scopes: []string{"openid"},
		})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		tok, err := p.Exchange(context.Background(), code, "")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if _, err := p.Identity(context.Background(), tok); err == nil {
			t.Error("standard OIDC accepted a 7-digit numeric subject; a signature proves the " +
				"IdP asserted it, not that the value is a safe identity key — employee numbers " +
				"are reused, so the mapping would eventually point a new hire at a former " +
				"employee's account")
		}
	})
}
