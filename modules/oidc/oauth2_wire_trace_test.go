package oidc

import (
	"context"
	"strings"
	"testing"
)

// TestOAuth2Provider_WireTrace 是一个**诊断**测试:它不追求断言覆盖,而是把
// 我方实际发出的 HTTP 交互原样打印出来,供联调前自查"我们发的和对方文档要求的
// 是否一致"。
//
// 跑法:go test ./modules/oidc/ -run TestOAuth2Provider_WireTrace -v
//
// 之所以需要它:此前所有验证都是单测断言,看不到线上格式。而这个 IdP 对请求形态
// 敏感 —— 它的错误码表里列了 415 UnsupportedMediaType,官方参考实现是
// "POST + 参数全在 query + 空 body + form content type"。这几项一旦不一致,
// 联调时只会得到一个语焉不详的 4xx。
func TestOAuth2Provider_WireTrace(t *testing.T) {
	m := newMockOAuth2Provider(t)
	p, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}

	t.Log("=================== 1. authorize URL(浏览器可见) ===================")
	authURL, err := p.AuthCodeURL(AuthCodeParams{State: "st-trace-1"})
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	t.Logf("GET %s", authURL)
	if strings.Contains(authURL, "client_secret") {
		t.Error("!! authorize URL 泄漏 client_secret")
	}

	t.Log("=================== 2. token 交换(我方 → IdP) ===================")
	tok, err := p.Exchange(context.Background(), "trace-code", "")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if rec := m.LastTokenRequest; rec != nil {
		t.Logf("Method       : %s", rec.Method)
		t.Logf("Content-Type : %q   <- 对方错误码表有 415,空值需联调确认", rec.ContentType)
		t.Logf("Body         : %q   <- 官方 Demo 是空 body", rec.RawBody)
		t.Log("Query 参数:")
		for k, v := range rec.Query {
			shown := strings.Join(v, ",")
			// 打印时对凭据做遮挡,避免这份 trace 被贴到工单里泄漏。
			if k == "client_secret" {
				shown = maskForTrace(shown)
			}
			t.Logf("  %-16s = %s", k, shown)
		}
	}
	t.Logf("拿到 access_token = %s (token_type=%q, id_token 是否为空=%v)",
		maskForTrace(tok.AccessToken), tok.TokenType, tok.RawIDToken == "")

	t.Log("=================== 3. userinfo(我方 → IdP) ===================")
	claims, err := p.Identity(context.Background(), tok)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if rec := m.LastUserInfoRequest; rec != nil {
		t.Logf("Method        : %s", rec.Method)
		t.Logf("Authorization : %q   <- 该 IdP 不看这个头,token 在 query", rec.AuthHeader)
		for k, v := range rec.Query {
			shown := strings.Join(v, ",")
			if k == "access_token" {
				shown = maskForTrace(shown)
			}
			t.Logf("  %-16s = %s", k, shown)
		}
	}

	t.Log("=================== 4. 归一化后的身份 ===================")
	t.Logf("Issuer        = %q  <- 我方注入,不从响应取", claims.Issuer)
	t.Logf("Subject       = %q", claims.Subject)
	t.Logf("Name          = %q", claims.Name)
	t.Logf("Email         = %q", claims.Email)
	t.Logf("PhoneNumber   = %q  <- 注意:无 +86 前缀时会被 extractPhone 丢弃", claims.PhoneNumber)
	t.Logf("EmailVerified = %v  <- 本协议无 verified 语义,必须恒 false", claims.EmailVerified)
	t.Logf("PhoneVerified = %v", claims.PhoneVerified)

	t.Log("=================== 5. 上游登出 URL ===================")
	if raw, ok := p.LogoutURL(context.Background(), LogoutHint{UID: "u1"}); ok {
		t.Logf("%s", raw)
	} else {
		t.Log("(未配置 AppID / PostLogoutRedirectURI,LogoutURL 返回 false —— 与预期一致)")
	}

	t.Log("=================== 6. 传输层失败时的 error(检查凭据泄漏) ===================")
	dead, err := newOAuth2Provider(oauth2ProviderConfig{
		Issuer: "trace-idp", BaseURL: "http://127.0.0.1:1",
		ClientID: "cid", ClientSecret: "SECRET-MUST-NOT-APPEAR",
		RedirectURI: "https://app.example.com/cb",
	})
	if err != nil {
		t.Fatalf("newOAuth2Provider(dead): %v", err)
	}
	_, derr := dead.Exchange(context.Background(), "c", "")
	t.Logf("err = %v", derr)
	if derr != nil && strings.Contains(derr.Error(), "SECRET-MUST-NOT-APPEAR") {
		t.Error("!! 传输层 error 泄漏了 client_secret")
	}
}

// maskForTrace 诊断输出用的遮挡:保留前 4 位便于比对,其余打码。
func maskForTrace(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:4] + "****"
}
