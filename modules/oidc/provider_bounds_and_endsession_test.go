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
	"time"
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

// 契约:短纯数字 subject 的拒绝**按 provider 声明的能力**决定,不是所有 provider 都做。
//
// 这条测试原先断言"两个 provider 都必须拒绝",而那是错的 —— 我把一个厂商特定的
// 事实当成了协议事实。
//
// 论证的来源是**某一家 IdP** 的文档自相矛盾:它对 sub 到底是内部 id 还是工号说法
// 不一,而工号由人事系统分配、会在离职/入职之间复用。那是这家 IdP 的人事系统的
// 性质,不是 OIDC 的性质。
//
// kind=oidc 是存量部署已经在跑的通用客户端,对着运维自己选的任何 IdP。一个自建
// IdP 把用户表主键当 sub(1001、42)完全正常,它不复用任何东西 —— 而上一版会把
// 这类部署**全量拒登**,且 minNumericSubjectLen 是 const、没有开关,恢复手段是
// 改代码重新发布。
//
// 同一个论证本模块自己在另一根轴上已经写过:identity_bounds.go 把业务 JWT 排除在
// 这条启发式之外,理由是"那是我方业务库主键、不复用、userId=42 完全正常"。自建
// IdP 的主键是同一个情形,我当时选的区分轴(自己派生 vs 上游断言)不是论证需要的
// 那一根(这家 IdP 的 subject 是不是复用的人事标识)。
//
// 现在由 Capabilities().SubjectMayBeReusedPersonnelID 声明,provider 各自表态。
// 长度上限**不**受此影响:那是存储性质,由共享入口对所有路径统一施加。
func TestAuthProviderConformance_ShortNumericSubjectFollowsCapability(t *testing.T) {
	t.Run("oauth2 refuses it", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.UserInfoBody = `{"success":true,"code":"200","requestId":"req-empno",
		  "data":{"sub":"7654321","nickname":"n"}}`
		p, err := newOAuth2Provider(m.providerConfig())
		if err != nil {
			t.Fatalf("newOAuth2Provider: %v", err)
		}
		if !p.Capabilities().SubjectMayBeReusedPersonnelID {
			t.Fatal("this kind is the one whose IdP documentation motivates the guard; " +
				"it must declare the hazard")
		}
		if _, err := p.IdentityFromClientCredential(context.Background(), "any-token"); err == nil {
			t.Error("plain OAuth2 accepted a 7-digit numeric subject; for this IdP that is " +
				"an employee number, and employee numbers are reused between a leaver and " +
				"a joiner while (issuer, subject) is an immutable key")
		}
	})

	t.Run("standard oidc accepts it", func(t *testing.T) {
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
		if p.Capabilities().SubjectMayBeReusedPersonnelID {
			t.Fatal("the generic OIDC client cannot know the operator's IdP allocates " +
				"reusable personnel identifiers; declaring the hazard here refuses every " +
				"login for a deployment whose IdP simply uses small primary keys")
		}
		tok, err := p.Exchange(context.Background(), code, "")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if _, err := p.Identity(context.Background(), tok); err != nil {
			t.Errorf("standard OIDC refused a 7-digit numeric subject: %v. That heuristic "+
				"comes from one vendor's personnel system; applying it to the generic client "+
				"takes login away from every user of an existing deployment whose IdP emits "+
				"short numeric subs, with no operator override and no recovery short of a "+
				"redeploy", err)
		}
	})

	// 上限对两个 kind 都生效 —— 它是列宽,不是启发式。
	t.Run("the storage bound still applies to both", func(t *testing.T) {
		long := strings.Repeat("9", subjectMaxLen+1)
		if err := requireStorableIdentity(&IdentityClaims{
			Issuer: "https://idp.example", Subject: long}); err == nil {
			t.Error("an oversized subject was accepted; the column cannot hold it")
		}
	})
}

// DM_OIDC_PROVIDER_HTTP_TIMEOUT 在 kind=oauth2 下必须真的生效。
//
// 本 PR 自己立的规矩是"没有配置项可以静默无效" —— ValidateKind 拒绝 BASE_URL /
// APP_ID / END_SESSION_URL 就是为了这个。超时被硬编码成 10s 而运维配的值被忽略,
// 是同一条规矩的反例:排障时会得出"我明明调过超时"的错误结论。
func TestOAuth2Provider_HonoursConfiguredHTTPTimeout(t *testing.T) {
	m := newMockOAuth2Provider(t)
	cfg := m.providerConfig()
	cfg.HTTPTimeout = 3 * time.Second
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	if got := p.httpClient().Timeout; got != 3*time.Second {
		t.Errorf("http client timeout = %v, want the configured 3s; a silently ignored "+
			"setting makes an operator conclude they already tuned it", got)
	}
}

// 未配置时回落到默认值,不能变成"无超时"—— 那会让被吊死的上游永久占住 goroutine。
func TestOAuth2Provider_DefaultsHTTPTimeoutWhenUnset(t *testing.T) {
	m := newMockOAuth2Provider(t)
	p, err := newOAuth2Provider(m.providerConfig()) // HTTPTimeout 零值
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	if got := p.httpClient().Timeout; got != oauth2HTTPTimeout {
		t.Errorf("http client timeout = %v, want the %v default", got, oauth2HTTPTimeout)
	}
}
