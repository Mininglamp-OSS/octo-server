package robot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// httperrL is a terse test shim for the no-params/no-details ResponseErrorL
// call shape exercised by the direct-code cases below.
func httperrL(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorL(c, code, nil, nil)
}

// TestRobotNoLegacyResponseError pins the Phase 2.1 contract that the migrated
// modules/robot handlers do not regress to legacy octo-lib error responses.
// Comments are stripped first so commented-out breadcrumbs do not trip the
// guard. The rb.Error(...)/m.Error(...) zap LOG calls are not responses and are
// intentionally allowed (they match none of the banned tokens). AbortWithStatus*
// is banned too: the robot-webhook auth middleware now renders via the i18n
// envelope (respondRobotAuth* helpers) and the inline-query timeout via
// respondRobotInlineQueryTimeout, so no raw gin abort should remain.
func TestRobotNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_manager.go", "mention_pref.go"}
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
		".AbortWithStatusJSON(",
		".AbortWithStatus(",
		"c.Response(\"",
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
					t.Fatalf("modules/robot/%s must use httperr.ResponseErrorL via respondRobot* helpers / errcode.ErrRobot* instead of legacy %s", f, b)
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

func decodeRobotEnvelope(t *testing.T, body []byte) errEnvelope {
	t.Helper()
	var env errEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, body)
	}
	return env
}

// helperHarness mounts a single GET /probe route that invokes the supplied
// helper with the i18n renderer wired, so tests can assert the rendered envelope
// without paying the DB / auth setup cost.
func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondRobotHelpers(t *testing.T) {
	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int    // 400 for D14 ResponseErrorL; real status for ResponseErrorLWithStatus
		wantContains    string // zh-CN substring expected in error.message
		wantNotContains string // forbid leaked English DefaultMessage when Internal=true
		wantDetails     map[string]any
	}{
		// ---- validation helpers (400, D14) -----------------------------------
		{
			name:            "respondRobotRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondRobotRequestInvalid(c, "channel_id") },
			wantCodeID:      "err.server.robot.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "channel_id"},
		},
		{
			name:            "respondRobotRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondRobotRequestInvalid(c, "") },
			wantCodeID:      "err.server.robot.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondRobotContentInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondRobotContentInvalid(c, "payload") },
			wantCodeID:      "err.server.robot.content_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "消息内容无效",
			wantDetails:     map[string]any{"field": "payload"},
		},
		{
			name:            "respondRobotContentTypeUnsupported surfaces the rejected type",
			probe:           func(c *wkhttp.Context) { respondRobotContentTypeUnsupported(c, 99) },
			wantCodeID:      "err.server.robot.content_type_unsupported",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不支持的消息类型",
			wantDetails:     map[string]any{"type": float64(99)},
		},
		{
			name:            "respondRobotFileTooLarge surfaces the size cap",
			probe:           func(c *wkhttp.Context) { respondRobotFileTooLarge(c, 100) },
			wantCodeID:      "err.server.robot.file_too_large",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "文件大小超过限制",
			wantDetails:     map[string]any{"max_mb": float64(100)},
		},
		// ---- direct codes: 400 / 403 / 404 (D14) -----------------------------
		{
			name:            "ErrRobotFileTypeUnsupported surfaces 400 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotFileTypeUnsupported) },
			wantCodeID:      "err.server.robot.file_type_unsupported",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不支持的文件类型",
		},
		{
			name:            "ErrRobotNoFieldsToUpdate surfaces 400 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotNoFieldsToUpdate) },
			wantCodeID:      "err.server.robot.no_fields_to_update",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "没有需要更新的字段",
		},
		{
			name:            "ErrRobotCreatorOnly surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotCreatorOnly) },
			wantCodeID:      "err.server.robot.creator_only",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "仅机器人创建者",
		},
		{
			name:            "ErrRobotMessageEditForbidden surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotMessageEditForbidden) },
			wantCodeID:      "err.server.robot.message_edit_forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "只能编辑自己发送的消息",
		},
		{
			name:            "ErrRobotChannelSendForbidden surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotChannelSendForbidden) },
			wantCodeID:      "err.server.robot.channel_send_forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不允许向该频道发送消息",
		},
		{
			name:            "respondManagerForbidden collapses to shared 403",
			probe:           respondManagerForbidden,
			wantCodeID:      "err.shared.auth.forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权执行此操作",
		},
		{
			name:            "ErrRobotNotFound surfaces 404 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotNotFound) },
			wantCodeID:      "err.server.robot.not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "机器人不存在",
		},
		{
			name:            "ErrRobotMessageNotFound surfaces 404 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotMessageNotFound) },
			wantCodeID:      "err.server.robot.message_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "消息不存在",
		},
		// ---- internal codes (500, Internal=true) collapse + no English leak ---
		{
			name:            "ErrRobotQueryFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotQueryFailed) },
			wantCodeID:      "err.server.robot.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query robot data",
		},
		{
			name:            "ErrRobotStoreFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotStoreFailed) },
			wantCodeID:      "err.server.robot.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "update robot data",
		},
		{
			name:            "ErrRobotSendFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotSendFailed) },
			wantCodeID:      "err.server.robot.send_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "send the message",
		},
		{
			name:            "ErrRobotUploadFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotUploadFailed) },
			wantCodeID:      "err.server.robot.upload_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "process the file",
		},
		{
			name:            "ErrRobotTokenGenFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrRobotTokenGenFailed) },
			wantCodeID:      "err.server.robot.token_gen_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "robot token",
		},
		// ---- ResponseErrorLWithStatus: real wire status preserved -------------
		{
			name:            "respondRobotAuthFailed preserves 401 (external adapters)",
			probe:           respondRobotAuthFailed,
			wantCodeID:      "err.server.robot.auth_failed",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusUnauthorized,
			wantContains:    "机器人鉴权失败",
		},
		{
			name:            "respondRobotAuthCheckFailed preserves 500 + hides internal copy",
			probe:           respondRobotAuthCheckFailed,
			wantCodeID:      "err.server.robot.auth_check_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusInternalServerError,
			wantContains:    "服务器内部错误",
			wantNotContains: "authentication check",
		},
		{
			name:            "respondRobotInlineQueryTimeout preserves 408",
			probe:           respondRobotInlineQueryTimeout,
			wantCodeID:      "err.server.robot.inline_query_timeout",
			wantSemStatus:   http.StatusRequestTimeout,
			wantTransStatus: http.StatusRequestTimeout,
			wantContains:    "行内查询超时",
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
			env := decodeRobotEnvelope(t, rec.Body.Bytes())
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
