package oidc

import (
	"net/url"
	"strings"
	"testing"
)

// 该 IdP 的单点登出与 OIDC RP-Initiated Logout 在每个维度上都不同:
// 应用标识在**路径段**而非 query,回跳参数名不是 post_logout_redirect_uri,
// 且没有 id_token_hint 可送。所以 buildEndSessionURL 无一处可复用。
func TestBuildUpstreamLogoutURL(t *testing.T) {
	cases := []struct {
		name         string
		base         string
		appID        string
		redirect     string
		wantErr      bool
		wantPath     string
		wantRedirect string
	}{
		{
			name:         "ok",
			base:         "https://idp.example.com",
			appID:        "app1",
			redirect:     "https://app.example.com/login",
			wantPath:     "/public/sp/slo/app1",
			wantRedirect: "https://app.example.com/login",
		},
		{
			// 对方文档专门提醒过尾斜杠敏感(注册的 redirect URI 与应用首页
			// URL 之间),base 两种写法都必须产出同一个路径,不能出现 //。
			name:         "base_with_trailing_slash",
			base:         "https://idp.example.com/",
			appID:        "app1",
			redirect:     "https://app.example.com/login",
			wantPath:     "/public/sp/slo/app1",
			wantRedirect: "https://app.example.com/login",
		},
		{
			name:         "base_with_subpath",
			base:         "https://idp.example.com/auth/",
			appID:        "app1",
			redirect:     "https://app.example.com/login",
			wantPath:     "/auth/public/sp/slo/app1",
			wantRedirect: "https://app.example.com/login",
		},
		{
			// 回跳地址带 query/fragment 时必须整体 percent-encode,
			// 否则它的 & 会被 IdP 当成自己的参数分隔符。
			name:         "redirect_with_query_is_encoded",
			base:         "https://idp.example.com",
			appID:        "app1",
			redirect:     "https://app.example.com/login?next=/inbox&lang=en",
			wantPath:     "/public/sp/slo/app1",
			wantRedirect: "https://app.example.com/login?next=/inbox&lang=en",
		},
		{name: "reject_empty_base", base: "", appID: "app1", redirect: "https://app.example.com/", wantErr: true},
		{name: "reject_empty_appid", base: "https://idp.example.com", appID: "", redirect: "https://app.example.com/", wantErr: true},
		{
			// 没有回跳地址,用户会停在 IdP 页面回不来 —— 视为配置错误而非可降级项。
			name:     "reject_empty_redirect",
			base:     "https://idp.example.com",
			appID:    "app1",
			redirect: "",
			wantErr:  true,
		},
		{
			// appId 直接拼进路径。运维误配 ../ 时,路径拼接会规范化掉 .. 段,
			// 让请求落到完全不同的端点上(本仓在 object-key → URL 的场景踩过
			// 同一个坑)。必须在拼接前挡住,不能依赖拼接函数的清洗行为。
			name:     "reject_appid_path_traversal",
			base:     "https://idp.example.com",
			appID:    "../../evil",
			redirect: "https://app.example.com/",
			wantErr:  true,
		},
		{name: "reject_appid_with_slash", base: "https://idp.example.com", appID: "a/b", redirect: "https://app.example.com/", wantErr: true},
		{name: "reject_appid_with_space", base: "https://idp.example.com", appID: "app 1", redirect: "https://app.example.com/", wantErr: true},
		{name: "reject_appid_with_query", base: "https://idp.example.com", appID: "app?x=1", redirect: "https://app.example.com/", wantErr: true},
		{name: "reject_non_absolute_base", base: "/relative/path", appID: "app1", redirect: "https://app.example.com/", wantErr: true},
		{name: "reject_non_http_scheme", base: "javascript:alert(1)", appID: "app1", redirect: "https://app.example.com/", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildUpstreamLogoutURL(tc.base, tc.appID, tc.redirect)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			u, perr := url.Parse(got)
			if perr != nil {
				t.Fatalf("produced an unparseable URL %q: %v", got, perr)
			}
			if u.Path != tc.wantPath {
				t.Errorf("path = %q, want %q (full=%q)", u.Path, tc.wantPath, got)
			}
			if strings.Contains(u.Path, "//") {
				t.Errorf("path contains a doubled slash: %q", u.Path)
			}
			// 参数名是 redirect_url,不是 OIDC 的 post_logout_redirect_uri。
			if got := u.Query().Get("redirect_url"); got != tc.wantRedirect {
				t.Errorf("redirect_url = %q, want %q", got, tc.wantRedirect)
			}
			if u.Query().Has("post_logout_redirect_uri") {
				t.Error("emitted the OIDC parameter name; this IdP expects redirect_url")
			}
			if u.Query().Has("id_token_hint") {
				t.Error("emitted id_token_hint; this protocol has no ID token")
			}
		})
	}
}

// 与 RP-Initiated Logout 相反,这个 URL 不含任何凭据(只有应用标识和一个
// 运维写死的回跳地址),因此可以安全地打日志 —— 排查登出问题时这很有用。
// 本测试把该属性钉住,防止后续有人往里塞 token。
func TestBuildUpstreamLogoutURL_CarriesNoCredential(t *testing.T) {
	got, err := buildUpstreamLogoutURL("https://idp.example.com", "app1", "https://app.example.com/login")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, forbidden := range []string{"id_token", "access_token", "client_secret", "refresh_token", "code="} {
		if strings.Contains(got, forbidden) {
			t.Errorf("logout URL contains %q, it must stay credential-free: %s", forbidden, got)
		}
	}
}
