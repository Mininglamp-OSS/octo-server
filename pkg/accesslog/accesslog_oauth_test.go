package accesslog

import (
	"strings"
	"testing"
)

// OIDC/OAuth2 的 callback 会以 query 参数形式带上 code 与 state。
//
// 两者都是可兑换的登录凭据材料:code 能换 token(在没有 PKCE 的 IdP 上,
// 它的唯一保护就是 client_secret),state 是一次性 CSRF 绑定。gin 的 logger
// 打的是 path + "?" + RawQuery,所以不脱敏它们就明文落进 access log。
//
// 之所以扩这个共享正则而不是在 OIDC 模块里单独处理:同一个 pattern 同时供
// access-log 与 panic-dump 两个 sink 使用(见 scrubSecretPatterns 的注释,
// 两个 sink 漂移过一次)。
func TestScrubPath_MasksOAuthCallbackCredentials(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustKeep []string // 参数名要保留,便于日志关联
		mustHide []string
	}{
		{
			name:     "callback_code_and_state",
			in:       "/v1/auth/oidc/sso/callback?code=4/0AVMBsJhRealAuthCode&state=st_abc123",
			mustKeep: []string{"code=", "state=", "/v1/auth/oidc/sso/callback"},
			mustHide: []string{"4/0AVMBsJhRealAuthCode", "st_abc123"},
		},
		{
			name:     "access_token_in_query",
			in:       "/api/oauth2/userinfo?access_token=aaaa-bbbb-cccc-dddd",
			mustKeep: []string{"access_token="},
			mustHide: []string{"aaaa-bbbb-cccc-dddd"},
		},
		{
			name:     "client_secret_in_query",
			in:       "/oauth/token?grant_type=authorization_code&client_secret=s3cr3t&code=xyz",
			mustKeep: []string{"client_secret=", "code="},
			mustHide: []string{"s3cr3t", "xyz"},
		},
		{
			name:     "id_token_in_query",
			in:       "/callback?id_token=eyJhbGciOiJSUzI1NiJ9.payload.sig",
			mustKeep: []string{"id_token="},
			mustHide: []string{"eyJhbGciOiJSUzI1NiJ9.payload.sig"},
		},
		{
			name:     "refresh_token_in_query",
			in:       "/oauth/token?refresh_token=rt-secret-value",
			mustKeep: []string{"refresh_token="},
			mustHide: []string{"rt-secret-value"},
		},
		{
			// 大小写变体:路由会 404,但 logger 照样打印,所以脱敏必须大小写不敏感
			// (与 ScrubPath 既有约定一致)。
			name:     "case_insensitive",
			in:       "/cb?CODE=MixedCaseValue&State=AnotherValue",
			mustHide: []string{"MixedCaseValue", "AnotherValue"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubPath(tc.in)
			for _, keep := range tc.mustKeep {
				if !strings.Contains(got, keep) {
					t.Errorf("ScrubPath dropped %q, log line loses correlation value:\n  in=%s\n  got=%s", keep, tc.in, got)
				}
			}
			for _, hide := range tc.mustHide {
				if strings.Contains(got, hide) {
					t.Errorf("ScrubPath leaked %q:\n  in=%s\n  got=%s", hide, tc.in, got)
				}
			}
		})
	}
}

// panic-dump sink 必须与 access-log sink 脱敏一致。两者共用
// scrubSecretPatterns 正是为了防止再次漂移。
func TestScrubbingErrorWriter_MasksOAuthCredentials(t *testing.T) {
	var sink strings.Builder
	w := &scrubbingErrorWriter{w: &sink}
	line := "panic while handling GET /v1/auth/oidc/sso/callback?code=RealCode123&state=RealState456\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := sink.String()
	for _, leak := range []string{"RealCode123", "RealState456"} {
		if strings.Contains(out, leak) {
			t.Errorf("panic dump leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "code=") || !strings.Contains(out, "state=") {
		t.Errorf("panic dump lost the parameter names: %s", out)
	}
}

// 回归:已有的三个被脱敏参数不能因为扩正则而失效。
func TestScrubPath_ExistingMaskersStillApply(t *testing.T) {
	cases := map[string]string{
		"/v1/x?poll_secret=abc123":   "abc123",
		"/v1/x?auth_code=code789":    "code789",
		"/v1/x?encrypt=enc555":       "enc555",
		"/v1/user/login_authcode/c1": "c1",
	}
	for in, secret := range cases {
		got := ScrubPath(in)
		if strings.Contains(got, secret) {
			t.Errorf("ScrubPath(%q) regressed, still leaks %q: %s", in, secret, got)
		}
	}
}
