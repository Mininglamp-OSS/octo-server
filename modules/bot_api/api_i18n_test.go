package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// httperrL / httperrLWS are terse shims for the two ResponseErrorL call shapes
// (D14 fixed-400 vs status-preserving) exercised by the direct-code cases.
func httperrL(c *wkhttp.Context, code codes.Code)   { httperr.ResponseErrorL(c, code, nil, nil) }
func httperrLWS(c *wkhttp.Context, code codes.Code) { httperr.ResponseErrorLWithStatus(c, code, nil, nil) }

// TestBotAPINoLegacyResponseError pins the Phase 2.1 contract that every
// migrated modules/bot_api handler renders through the i18n envelope and never
// regresses to a legacy octo-lib error response or a raw non-OK gin write.
// Comments are stripped first so commented-out breadcrumbs do not trip the
// guard. ba.Error(...)/ba.Warn(...) zap LOG calls are not responses and are
// intentionally allowed. Success writes (c.JSON(http.StatusOK,...), c.Response,
// c.Redirect, c.DataFromReader) are allowed; only NON-OK c.JSON(...) is banned.
//
// Known exemption (tracked as a follow-up, outside D23): getEvents still emits
// an HTTP-200 in-band error via c.Response(gin.H{"status":0,"msg":err.Error()}),
// which is not an error *response* (wire 200) and so is not bannable here.
func TestBotAPINoLegacyResponseError(t *testing.T) {
	files := []string{
		"auth.go", "register.go", "commands.go", "events.go", "mention_pref.go",
		"sync.go", "typing.go", "send.go", "threads.go", "file.go", "groups.go",
		"voice_adapter.go", "obo_api.go", "resolve_targets.go",
	}
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
		".AbortWithStatusJSON(",
		".AbortWithStatus(",
		"c.Response(\"",
		// raw non-OK c.JSON — every error status must go through the envelope
		"c.JSON(http.StatusBadRequest",
		"c.JSON(http.StatusUnauthorized",
		"c.JSON(http.StatusForbidden",
		"c.JSON(http.StatusNotFound",
		"c.JSON(http.StatusConflict",
		"c.JSON(http.StatusInternalServerError",
		"c.JSON(http.StatusBadGateway",
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
			for _, b := range banned {
				if strings.Contains(cleaned, b) {
					t.Fatalf("modules/bot_api/%s must render via httperr.ResponseErrorL / respondBotAPI* helpers / errcode.ErrBotAPI* instead of legacy %s", f, b)
				}
			}
		})
	}
}

// errEnvelope is the partial shape of an httperr.ResponseErrorL response. The
// renderer emits both the legacy {msg,status} and the v2 {error.{...}} blocks
// unconditionally (v7.2 dual-envelope contract).
type errEnvelope struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

// helperHarness mounts a single GET /probe route with the i18n renderer wired,
// so tests can assert the rendered envelope without DB / auth setup.
func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondBotAPIHelpers(t *testing.T) {
	// ba carries only a logger — enough for the helpers that log (identity
	// guard) without standing up DB / redis / IM.
	ba := &BotAPI{Log: log.NewTLog("BotAPI-i18n-test")}

	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int // 400 for D14 ResponseErrorL; real status for WithStatus
		wantContains    string
		wantNotContains string // forbid leaked English DefaultMessage when Internal=true
		wantDetails     map[string]any
	}{
		// ---- detail-carrying validation helpers (400, D14) -------------------
		{
			name:            "respondBotAPIRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondBotAPIRequestInvalid(c, "channel_id") },
			wantCodeID:      "err.server.bot_api.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "channel_id"},
		},
		{
			name:            "respondBotAPIRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondBotAPIRequestInvalid(c, "") },
			wantCodeID:      "err.server.bot_api.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondBotAPILimitExceeded surfaces field + cap",
			probe:           func(c *wkhttp.Context) { respondBotAPILimitExceeded(c, "members", 200) },
			wantCodeID:      "err.server.bot_api.limit_exceeded",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求数量超过上限",
			wantDetails:     map[string]any{"field": "members", "max": float64(200)},
		},
		{
			name:            "respondBotAPIContentTooLarge surfaces field + max_size",
			probe:           func(c *wkhttp.Context) { respondBotAPIContentTooLarge(c, "content", 4096) },
			wantCodeID:      "err.server.bot_api.content_too_large",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "内容大小超过限制",
			wantDetails:     map[string]any{"field": "content", "max_size": float64(4096)},
		},
		{
			name:            "respondBotAPIFileTooLarge surfaces the MB cap",
			probe:           func(c *wkhttp.Context) { respondBotAPIFileTooLarge(c, 100) },
			wantCodeID:      "err.server.bot_api.file_too_large",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "文件大小超过限制",
			wantDetails:     map[string]any{"max_mb": float64(100)},
		},
		{
			name:            "respondBotAPIPayloadTooLarge preserves 413 + byte cap",
			probe:           func(c *wkhttp.Context) { respondBotAPIPayloadTooLarge(c, 1048576) },
			wantCodeID:      "err.server.bot_api.payload_too_large",
			wantSemStatus:   http.StatusRequestEntityTooLarge,
			wantTransStatus: http.StatusRequestEntityTooLarge,
			wantContains:    "上传文件大小超过限制",
			wantDetails:     map[string]any{"max_bytes": float64(1048576)},
		},
		// ---- direct business codes: 400 / 403 / 404 / 409 (D14, wire 400) ----
		{
			name:            "ErrBotAPINotGroupMember 403 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPINotGroupMember) },
			wantCodeID:      "err.server.bot_api.not_group_member",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "机器人不是该群成员",
		},
		{
			name:            "ErrBotAPIAppBotUnsupported 403",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIAppBotUnsupported) },
			wantCodeID:      "err.server.bot_api.app_bot_unsupported",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "应用机器人不支持该操作",
		},
		{
			name:            "ErrBotAPIOBONotAuthorized 403",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIOBONotAuthorized) },
			wantCodeID:      "err.server.bot_api.obo_not_authorized",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权代表该用户操作",
		},
		{
			name:            "ErrBotAPIMemberNotHuman 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIMemberNotHuman) },
			wantCodeID:      "err.server.bot_api.member_not_human",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "仅可通过机器人 API 添加真人成员",
		},
		{
			name:            "ErrBotAPIGroupNotFound 404 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIGroupNotFound) },
			wantCodeID:      "err.server.bot_api.group_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "群组不存在",
		},
		{
			name:            "ErrBotAPIMessageNotDelivered 409 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIMessageNotDelivered) },
			wantCodeID:      "err.server.bot_api.message_not_delivered",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "消息尚未投递完成",
		},
		// ---- internal codes (500, Internal=true): collapse + no English leak --
		{
			name:            "ErrBotAPIQueryFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIQueryFailed) },
			wantCodeID:      "err.server.bot_api.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query data",
		},
		{
			name:            "ErrBotAPIStoreFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIStoreFailed) },
			wantCodeID:      "err.server.bot_api.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "update data",
		},
		{
			name:            "ErrBotAPISendFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPISendFailed) },
			wantCodeID:      "err.server.bot_api.send_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "send the message",
		},
		{
			name:            "ErrBotAPIUploadFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIUploadFailed) },
			wantCodeID:      "err.server.bot_api.upload_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "process the file",
		},
		{
			name:            "ErrBotAPIOBOInternal collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIOBOInternal) },
			wantCodeID:      "err.server.bot_api.obo_internal",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "OBO operation",
		},
		{
			name:            "ErrBotAPIIMTokenFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotAPIIMTokenFailed) },
			wantCodeID:      "err.server.bot_api.im_token_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "IM token",
		},
		// ---- status-preserving middleware helpers (real wire status) ---------
		{
			name:            "respondBotAPIAuthFailed preserves 401 (external adapters)",
			probe:           respondBotAPIAuthFailed,
			wantCodeID:      "err.server.bot_api.auth_failed",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusUnauthorized,
			wantContains:    "机器人认证失败",
		},
		{
			name:            "respondBotAPIBotUnavailable preserves 403",
			probe:           respondBotAPIBotUnavailable,
			wantCodeID:      "err.server.bot_api.bot_unavailable",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusForbidden,
			wantContains:    "机器人不可用",
		},
		{
			name:            "respondBotAPIAuthCheckFailed preserves 500 + hides internal copy",
			probe:           respondBotAPIAuthCheckFailed,
			wantCodeID:      "err.server.bot_api.auth_check_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusInternalServerError,
			wantContains:    "服务器内部错误",
			wantNotContains: "authentication check",
		},
		// ---- status-preserving direct codes (OBO management + voice) ---------
		{
			name:            "ErrBotAPIOBOGrantNotFound preserves 404",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotAPIOBOGrantNotFound) },
			wantCodeID:      "err.server.bot_api.obo_grant_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusNotFound,
			wantContains:    "OBO 授权不存在",
		},
		{
			name:            "ErrBotAPIOBOGrantExists preserves 409",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotAPIOBOGrantExists) },
			wantCodeID:      "err.server.bot_api.obo_grant_exists",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusConflict,
			wantContains:    "OBO 授权已存在",
		},
		{
			name:            "ErrBotAPISharedAuthRequired preserves 401",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotAPISharedAuthRequired) },
			wantCodeID:      "err.shared.auth.required",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusUnauthorized,
			wantContains:    "请先登录",
		},
		{
			name:            "ErrBotAPIUpstreamFailed preserves 502 + hides internal copy",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotAPIUpstreamFailed) },
			wantCodeID:      "err.server.bot_api.upstream_failed",
			wantSemStatus:   http.StatusBadGateway,
			wantTransStatus: http.StatusBadGateway,
			wantContains:    "服务器内部错误",
			wantNotContains: "upstream service",
		},
		// ---- checkSendPermission classifier ----------------------------------
		{
			name:            "respondSendPermissionError maps not-friend → 403 not_friend",
			probe:           func(c *wkhttp.Context) { respondSendPermissionError(c, errBotSendPermNotFriend) },
			wantCodeID:      "err.server.bot_api.not_friend",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "机器人不是该用户的好友",
		},
		{
			name:            "respondSendPermissionError maps conv-not-started → 403",
			probe:           func(c *wkhttp.Context) { respondSendPermissionError(c, errBotSendPermConvNotStarted) },
			wantCodeID:      "err.server.bot_api.conversation_not_started",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "用户尚未与该机器人发起会话",
		},
		{
			name:            "respondSendPermissionError maps bad-thread-channel → 400 field",
			probe:           func(c *wkhttp.Context) { respondSendPermissionError(c, errBotSendPermBadThreadChan) },
			wantCodeID:      "err.server.bot_api.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "channel_id"},
		},
		{
			name:            "respondSendPermissionError maps infra failure → 500 query_failed no leak",
			probe:           func(c *wkhttp.Context) { respondSendPermissionError(c, errBotSendPermCheckFailed) },
			wantCodeID:      "err.server.bot_api.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query data",
		},
		// ---- defensive identity guard ----------------------------------------
		{
			name:            "respondBotAPIIdentityMissing renders generic internal 500",
			probe:           func(c *wkhttp.Context) { ba.respondBotAPIIdentityMissing(c) },
			wantCodeID:      "err.shared.internal",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
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
			var env errEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v\nbody: %s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
			}
			if env.Status != tc.wantTransStatus {
				t.Fatalf("legacy status = %d, want %d", env.Status, tc.wantTransStatus)
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
				got := env.Error.Details
				if got == nil {
					got = map[string]any{}
				}
				if len(got) != len(tc.wantDetails) {
					t.Fatalf("error.details = %#v, want %#v", got, tc.wantDetails)
				}
				for k, v := range tc.wantDetails {
					if got[k] != v {
						t.Fatalf("error.details[%q] = %#v, want %#v", k, got[k], v)
					}
				}
			}
		})
	}
}
