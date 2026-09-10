package wkhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 背景:octo-lib 的 AuthMiddleware 只读自定义的 `token` 头
// (pkg/wkhttp/http.go 内 c.GetHeader("token")),不认 Authorization。
// 接入外部 IdP 之后,按标准 OAuth2 习惯开发的客户端会发
// `Authorization: Bearer <token>` —— 登录能过,之后每个 API 调用都 401。
//
// 修法刻意不动 AuthMiddleware:它在共享库里,改它要升版本并影响所有依赖方。
// 改为在本仓加一个全局前置中间件把 Bearer 值回填成 `token` 头。
//
// 三条约束(已确认):token 头优先、只认 Bearer、不接受 query 参数兜底。
func TestBearerTokenCompat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		// 入站请求
		tokenHeader string
		authHeader  string
		query       string
		// 期望 AuthMiddleware 最终看到的 token 头
		wantToken string
		// Authorization 头必须原样保留(bot / integration 类路由直接读它)
		wantAuthPreserved string
	}{
		{
			name:              "token_header_wins_and_auth_is_ignored",
			tokenHeader:       "user-token-1",
			authHeader:        "Bearer some-other-value",
			wantToken:         "user-token-1",
			wantAuthPreserved: "Bearer some-other-value",
		},
		{
			name:              "bearer_is_backfilled_when_token_absent",
			authHeader:        "Bearer user-token-2",
			wantToken:         "user-token-2",
			wantAuthPreserved: "Bearer user-token-2",
		},
		{
			// RFC 6750 规定 scheme 大小写不敏感。
			name:              "scheme_lowercase",
			authHeader:        "bearer user-token-3",
			wantToken:         "user-token-3",
			wantAuthPreserved: "bearer user-token-3",
		},
		{
			name:              "scheme_uppercase",
			authHeader:        "BEARER user-token-4",
			wantToken:         "user-token-4",
			wantAuthPreserved: "BEARER user-token-4",
		},
		{
			name:              "scheme_mixed_case",
			authHeader:        "BeArEr user-token-5",
			wantToken:         "user-token-5",
			wantAuthPreserved: "BeArEr user-token-5",
		},
		{
			// 多余空白由 scheme 与 token 之间的分词处理。
			name:              "extra_whitespace",
			authHeader:        "Bearer    user-token-6   ",
			wantToken:         "user-token-6",
			wantAuthPreserved: "Bearer    user-token-6   ",
		},
		{
			// 只认 Bearer。Basic 仍有 webhook 在用,不能碰。
			name:              "basic_scheme_ignored",
			authHeader:        "Basic dXNlcjpwYXNz",
			wantToken:         "",
			wantAuthPreserved: "Basic dXNlcjpwYXNz",
		},
		{
			name:              "unknown_scheme_ignored",
			authHeader:        "Token user-token-7",
			wantToken:         "",
			wantAuthPreserved: "Token user-token-7",
		},
		{
			// 没有凭据部分,不能回填一个空 token 头 ——
			// 那会把 AuthMiddleware 的 "token 为空" 分支变成 "token 无效"。
			name:              "bearer_without_credentials",
			authHeader:        "Bearer",
			wantToken:         "",
			wantAuthPreserved: "Bearer",
		},
		{
			name:              "bearer_with_only_spaces",
			authHeader:        "Bearer    ",
			wantToken:         "",
			wantAuthPreserved: "Bearer    ",
		},
		{
			// 形态不明确(多出一段)时保守拒绝,不猜哪一段是凭据。
			name:              "bearer_with_extra_segment",
			authHeader:        "Bearer a b",
			wantToken:         "",
			wantAuthPreserved: "Bearer a b",
		},
		{
			// 明确不支持:凭据放 URL 会进浏览器历史与 Referer,
			// accesslog 脱敏只能挡日志这一层。
			name:      "query_param_is_not_accepted",
			query:     "?token=from-query",
			wantToken: "",
		},
		{
			name:      "nothing_provided_stays_empty",
			wantToken: "",
		},
		{
			// 两个都在但值不同:token 头优先,且不报错 ——
			// 报错会让既有客户端在中间件上线后突然失败。
			name:              "conflicting_values_token_wins_without_error",
			tokenHeader:       "the-real-one",
			authHeader:        "Bearer a-different-one",
			wantToken:         "the-real-one",
			wantAuthPreserved: "Bearer a-different-one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotToken, gotAuth string
			r := gin.New()
			r.Use(BearerTokenCompat())
			r.GET("/probe", func(c *gin.Context) {
				gotToken = c.GetHeader("token")
				gotAuth = c.GetHeader("Authorization")
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/probe"+tc.query, nil)
			if tc.tokenHeader != "" {
				req.Header.Set("token", tc.tokenHeader)
			}
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if gotToken != tc.wantToken {
				t.Errorf("token header = %q, want %q", gotToken, tc.wantToken)
			}
			if gotAuth != tc.wantAuthPreserved {
				t.Errorf("Authorization header = %q, want it preserved as %q "+
					"(bot / integration routes read it directly)", gotAuth, tc.wantAuthPreserved)
			}
		})
	}
}

// 中间件必须只碰 token 头,不得吞请求或改状态码 —— 它是全局前置件,
// 任何副作用都会打到每一条路由上。
func TestBearerTokenCompat_IsTransparent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BearerTokenCompat())
	r.GET("/probe", func(c *gin.Context) { c.String(http.StatusTeapot, "handler-ran") })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (middleware must not intercept)", w.Code, http.StatusTeapot)
	}
	if w.Body.String() != "handler-ran" {
		t.Errorf("body = %q, want the handler's own output", w.Body.String())
	}
}
