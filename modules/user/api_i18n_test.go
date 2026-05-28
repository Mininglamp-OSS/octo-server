package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestUserAPINoLegacyResponseError pins the post-Phase-2.1 contract that
// modules/user/api.go does not regress to legacy octo-lib error responses.
// Catches both `.ResponseError(...)` and `.ResponseErrorf(...)` — the latter
// is the formatted variant that bypasses the renderer just as completely,
// so the guard must look for it too even though it never matches the
// plain `.ResponseError(` substring (the `f` intervenes).
func TestUserAPINoLegacyResponseError(t *testing.T) {
	data, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	source := string(data)
	// Strip line comments so commented-out legacy snippets don't fail the
	// guard. The Phase 0 inventory left a couple of commented references
	// in wxLogin / uploadAvatar deliberately as TODO breadcrumbs.
	var clean strings.Builder
	for _, line := range strings.Split(source, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	cleaned := clean.String()
	for _, banned := range []string{".ResponseError(", ".ResponseErrorf("} {
		if strings.Contains(cleaned, banned) {
			t.Fatalf("modules/user/api.go must use httperr.ResponseErrorL via respondUser* helpers instead of legacy %s", banned)
		}
	}
}

// helperHarness mounts a single GET /probe route that invokes the supplied
// helper. Tests exercise the helper directly without paying the DB / auth
// setup cost — the contract we care about is what the wkhttp envelope
// looks like once the renderer has run.
func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

// envelope is the partial shape of an httperr.ResponseErrorL response that
// these tests assert on. All fields are present on every error response
// (the renderer emits both legacy {msg,status} and v2 {error.{...}}
// unconditionally — v7.2 contract).
type envelope struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, body)
	}
	return env
}

func TestRespondUserHelpers(t *testing.T) {
	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int    // always 400 for legacy compat (D14)
		wantContains    string // zh-CN substring expected in error.message
		wantNotContains string // forbid leaked DefaultMessage when Internal=true
		wantDetails     map[string]any
	}{
		{
			name:            "ErrUserNotFound surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserNotFound) },
			wantCodeID:      "err.server.user.not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "用户不存在",
		},
		{
			name: "ErrUserStoreFailed (Internal=true) collapses to shared internal copy",
			probe: func(c *wkhttp.Context) {
				respondUserError(c, errcode.ErrUserStoreFailed)
			},
			wantCodeID:      "err.server.user.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "Failed to persist user data",
		},
		{
			name:            "respondUserServiceError defaults to store_failed",
			probe:           func(c *wkhttp.Context) { respondUserServiceError(c) },
			wantCodeID:      "err.server.user.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
		},
		{
			name:            "respondUserNotLoggedIn routes to shared.auth.required",
			probe:           func(c *wkhttp.Context) { respondUserNotLoggedIn(c) },
			wantCodeID:      "err.shared.auth.required",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请先登录",
		},
		{
			name:            "respondUserRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondUserRequestInvalid(c, "phone") },
			wantCodeID:      "err.server.user.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求数据格式有误",
			wantDetails:     map[string]any{"field": "phone"},
		},
		{
			name:            "respondUserRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondUserRequestInvalid(c, "") },
			wantCodeID:      "err.server.user.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求数据格式有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondUserUpdateNotAllowed carries the field detail",
			probe:           func(c *wkhttp.Context) { respondUserUpdateNotAllowed(c, "short_no") },
			wantCodeID:      "err.server.user.update_not_allowed",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不允许修改",
			wantDetails:     map[string]any{"field": "short_no"},
		},
		{
			name:            "respondUserUpdateNotAllowed drops empty field key",
			probe:           func(c *wkhttp.Context) { respondUserUpdateNotAllowed(c, "") },
			wantCodeID:      "err.server.user.update_not_allowed",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不允许修改",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondUserAuthInfoInvalid carries missing_field",
			probe:           func(c *wkhttp.Context) { respondUserAuthInfoInvalid(c, "type") },
			wantCodeID:      "err.server.user.auth_info_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "授权信息格式错误",
			wantDetails:     map[string]any{"missing_field": "type"},
		},
		{
			name:            "respondUserTokenRequired carries the field detail",
			probe:           func(c *wkhttp.Context) { respondUserTokenRequired(c, "bot_token") },
			wantCodeID:      "err.server.user.token_required",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "Token 不能为空",
			wantDetails:     map[string]any{"field": "bot_token"},
		},
		{
			name:            "respondUserLockMinuteOutOfRange surfaces bounds",
			probe:           func(c *wkhttp.Context) { respondUserLockMinuteOutOfRange(c) },
			wantCodeID:      "err.server.user.lock_minute_out_of_range",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "0 到 60 分钟",
			wantDetails: map[string]any{
				"field": "lock_after_minute",
				"min":   float64(0),
				"max":   float64(60),
			},
		},
		{
			name:            "ErrUserLanguageUnsupported surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserLanguageUnsupported) },
			wantCodeID:      "err.server.user.language_unsupported",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不支持的语言",
		},
		{
			name:            "ErrUserAccountBanned surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserAccountBanned) },
			wantCodeID:      "err.server.user.account_banned",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "已被封禁",
		},
		{
			name:            "ErrUserLoginLocked surfaces 429 + user-facing zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserLoginLocked) },
			wantCodeID:      "err.server.user.login_locked",
			wantSemStatus:   http.StatusTooManyRequests,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "登录失败次数过多",
		},
		{
			name:            "ErrUserWeChatExchangeFailed (Internal=true, 502) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserWeChatExchangeFailed) },
			wantCodeID:      "err.server.user.wechat_exchange_failed",
			wantSemStatus:   http.StatusBadGateway,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "WeChat",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := helperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Accept-Language", "zh-CN")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantTransStatus {
				t.Fatalf("HTTP status = %d, want %d; body=%s", rec.Code, tc.wantTransStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Language"); got != "zh-CN" {
				t.Fatalf("Content-Language = %q, want zh-CN", got)
			}
			env := decodeEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
			}
			if env.Status != tc.wantTransStatus {
				t.Fatalf("legacy status = %d, want %d (D14 transport=400 compat)", env.Status, tc.wantTransStatus)
			}
			if env.Msg != env.Error.Message {
				t.Fatalf("legacy msg %q != error.message %q (dual envelope must agree)", env.Msg, env.Error.Message)
			}
			if !strings.Contains(env.Error.Message, tc.wantContains) {
				t.Fatalf("error.message = %q, want substring %q", env.Error.Message, tc.wantContains)
			}
			if tc.wantNotContains != "" && strings.Contains(env.Error.Message, tc.wantNotContains) {
				t.Fatalf("error.message = %q must not contain %q (Internal leak)", env.Error.Message, tc.wantNotContains)
			}
			if tc.wantDetails != nil {
				gotDetails := env.Error.Details
				if gotDetails == nil {
					gotDetails = map[string]any{}
				}
				if len(gotDetails) != len(tc.wantDetails) {
					t.Fatalf("error.details = %#v, want %#v", gotDetails, tc.wantDetails)
				}
				for k, v := range tc.wantDetails {
					if gotDetails[k] != v {
						t.Fatalf("error.details[%q] = %#v, want %#v", k, gotDetails[k], v)
					}
				}
			}
		})
	}
}
