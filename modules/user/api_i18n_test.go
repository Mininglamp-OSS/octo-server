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
	for _, banned := range []string{".ResponseError(", ".ResponseErrorf(", "c.Response(\""} {
		if strings.Contains(cleaned, banned) {
			t.Fatalf("modules/user/api.go must use httperr.ResponseErrorL via respondUser* helpers instead of legacy %s", banned)
		}
	}
}

// TestManagerAPINoLegacyResponseError pins the post-Phase-2.1 contract that
// modules/user/api_manager.go does not regress to legacy octo-lib error
// responses (the management console migration). Mirrors the api.go guard
// above; also forbids the `c.Response("<string>")` shape that two handlers
// used to (mis)use as an error path — those returned HTTP 200 with a bare
// string body and are now proper httperr.ResponseErrorL envelopes.
func TestManagerAPINoLegacyResponseError(t *testing.T) {
	data, err := os.ReadFile("api_manager.go")
	if err != nil {
		t.Fatalf("read api_manager.go: %v", err)
	}
	var clean strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	cleaned := clean.String()
	for _, banned := range []string{".ResponseError(", ".ResponseErrorf(", "c.Response(\""} {
		if strings.Contains(cleaned, banned) {
			t.Fatalf("modules/user/api_manager.go must use httperr.ResponseErrorL via respond* helpers instead of legacy %s", banned)
		}
	}
}

// TestMigratedUserFilesNoLegacyResponseError pins the Phase 2.1 contract that
// the remaining migrated modules/user handlers do not regress to legacy
// octo-lib error responses. Comments are stripped first so commented-out
// breadcrumbs do not trip the guard. New handler files that still rely on
// c.ResponseError / c.ResponseErrorf must NOT be added to this list — migrate
// them instead.
func TestMigratedUserFilesNoLegacyResponseError(t *testing.T) {
	files := []string{
		"api_friend.go", "api_online.go", "api_setting.go", "api_maillist.go",
		"api_device.go", "api_destroy.go", "api_pinned.go", "api_gitee.go",
		"api_github.go", "api_emaillogin.go", "api_usernamelogin.go",
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			var clean strings.Builder
			for _, line := range strings.Split(string(data), "\n") {
				if idx := strings.Index(line, "//"); idx >= 0 {
					line = line[:idx]
				}
				clean.WriteString(line)
				clean.WriteByte('\n')
			}
			cleaned := clean.String()
			for _, banned := range []string{".ResponseError(", ".ResponseErrorf(", "c.Response(\""} {
				if strings.Contains(cleaned, banned) {
					t.Fatalf("modules/user/%s must use httperr.ResponseErrorL via respond* helpers instead of legacy %s", f, banned)
				}
			}
		})
	}
}

func TestUploadAvatarPostCommitNotificationFailuresRespondOK(t *testing.T) {
	data, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	source := string(data)
	start := strings.Index(source, "func (u *User) uploadAvatar")
	if start < 0 {
		t.Fatalf("locate uploadAvatar in api.go")
	}
	end := strings.Index(source[start:], "\n// 获取用户的IM连接地址")
	if end < 0 {
		t.Fatalf("locate end of uploadAvatar in api.go")
	}
	body := source[start : start+end]

	for _, marker := range []string{"查询用户好友失败", "发送个人头像更新命令失败"} {
		idx := strings.Index(body, marker)
		if idx < 0 {
			t.Fatalf("uploadAvatar missing post-commit notification branch %q", marker)
		}
		after := body[idx:]
		returnIdx := strings.Index(after, "return")
		if returnIdx < 0 {
			t.Fatalf("uploadAvatar branch %q has no return", marker)
		}
		if !strings.Contains(after[:returnIdx], "c.ResponseOK()") {
			t.Fatalf("uploadAvatar branch %q must respond OK after avatar DB update has committed", marker)
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

		// ---- Phase 2.1 api_manager.go migration helpers / codes ----
		{
			name:            "respondManagerForbidden routes to shared.auth.forbidden",
			probe:           func(c *wkhttp.Context) { respondManagerForbidden(c) },
			wantCodeID:      "err.shared.auth.forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权执行此操作",
		},
		{
			name: "respondUserListFilterConflict carries both filter names",
			probe: func(c *wkhttp.Context) {
				respondUserListFilterConflict(c, "bot_only", "exclude_bot")
			},
			wantCodeID:      "err.server.user.list_filter_conflict",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "互斥",
			wantDetails:     map[string]any{"filter": "bot_only", "conflicts_with": "exclude_bot"},
		},
		{
			name:            "ErrUserManagerPermissionRequired surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserManagerPermissionRequired) },
			wantCodeID:      "err.server.user.manager_permission_required",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "未开通管理权限",
		},
		{
			name:            "ErrUserPasswordTooShort surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserPasswordTooShort) },
			wantCodeID:      "err.server.user.password_too_short",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "密码长度必须大于",
		},
		{
			name:            "ErrUserOldPasswordIncorrect surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserOldPasswordIncorrect) },
			wantCodeID:      "err.server.user.old_password_incorrect",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "原密码错误",
		},
		{
			name:            "ErrUserCannotDeleteSuperAdmin surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserCannotDeleteSuperAdmin) },
			wantCodeID:      "err.server.user.cannot_delete_super_admin",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "超级管理员账号不能删除",
		},
		{
			name:            "ErrUserTokenCacheFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserTokenCacheFailed) },
			wantCodeID:      "err.server.user.token_cache_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "session token",
		},
		{
			name:            "ErrUserShortNoGenFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserShortNoGenFailed) },
			wantCodeID:      "err.server.user.short_no_gen_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "short ID",
		},

		// ---- Phase 2.1 pinned / oauth / web3 / email codes ----
		{
			name: "respondUserPinnedLimitExceeded carries the max detail",
			probe: func(c *wkhttp.Context) {
				respondUserPinnedLimitExceeded(c, 7)
			},
			wantCodeID:      "err.server.user.pinned_limit_exceeded",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "置顶频道数量已达上限",
			wantDetails:     map[string]any{"max": float64(7)},
		},
		{
			name:            "ErrUserChannelAccessDenied surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserChannelAccessDenied) },
			wantCodeID:      "err.server.user.channel_access_denied",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权访问该频道",
		},
		{
			name:            "ErrUserOAuthStateExpired surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserOAuthStateExpired) },
			wantCodeID:      "err.server.user.oauth_state_expired",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "登录状态已过期",
		},
		{
			name:            "ErrUserOAuthExchangeFailed (Internal=true, 502) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserOAuthExchangeFailed) },
			wantCodeID:      "err.server.user.oauth_exchange_failed",
			wantSemStatus:   http.StatusBadGateway,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "OAuth",
		},
		{
			name:            "ErrUserUsernameFormatInvalid surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserUsernameFormatInvalid) },
			wantCodeID:      "err.server.user.username_format_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "用户名必须为",
		},
		{
			name:            "ErrUserPublicKeyNotFound surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserPublicKeyNotFound) },
			wantCodeID:      "err.server.user.public_key_not_found",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "未上传公钥",
		},
		{
			name:            "ErrUserSignatureInvalid surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserSignatureInvalid) },
			wantCodeID:      "err.server.user.signature_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "签名校验失败",
		},
		{
			name:            "ErrUserEmailInvalid surfaces zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserEmailInvalid) },
			wantCodeID:      "err.server.user.email_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "邮箱格式不正确",
		},
		{
			name:            "ErrUserAccountUnavailable surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserAccountUnavailable) },
			wantCodeID:      "err.server.user.account_unavailable",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "已注销或被禁用",
		},
		{
			name:            "ErrUserEmailRateLimited surfaces 429 client-actionable copy (not Internal)",
			probe:           func(c *wkhttp.Context) { respondUserError(c, errcode.ErrUserEmailRateLimited) },
			wantCodeID:      "err.server.user.email_rate_limited",
			wantSemStatus:   http.StatusTooManyRequests,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "发送过于频繁",
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
