package space

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

// httperrL is a terse shim for the no-params/no-details ResponseErrorL shape.
func httperrL(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorL(c, code, nil, nil)
}

// TestSpaceNoLegacyResponseError pins the Phase 2.1 contract that the migrated
// modules/space handlers do not regress to legacy octo-lib error responses.
// Comments are stripped first so commented-out breadcrumbs do not trip the guard.
func TestSpaceNoLegacyResponseError(t *testing.T) {
	files := []string{
		"api.go",
		"api_manager.go",
		"api_email_invite.go",
		"api_email_invite_public.go",
		"api_manager_email_invite.go",
	}
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
		".ResponseWithStatus(",
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
			if cleaned := clean.String(); func() bool {
				for _, b := range banned {
					if strings.Contains(cleaned, b) {
						t.Errorf("modules/space/%s must use httperr.ResponseErrorL via respondSpace* helpers / errcode.ErrSpace* instead of legacy %s", f, b)
						return true
					}
				}
				return false
			}() {
				return
			}
		})
	}
}

type spaceErrEnvelope struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

func decodeSpaceEnvelope(t *testing.T, body []byte) spaceErrEnvelope {
	t.Helper()
	var env spaceErrEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, body)
	}
	return env
}

// assertSpaceErrorCode asserts the migrated dual envelope carries the expected
// error.code, replacing brittle zh-CN body-substring matching in the integration
// tests so future copy edits do not break them.
func assertSpaceErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	env := decodeSpaceEnvelope(t, w.Body.Bytes())
	if env.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q\nbody: %s", env.Error.Code, wantCode, w.Body.String())
	}
}

// spaceHelperHarness mounts a single GET /probe route with the i18n renderer
// wired, so helper assertions need no DB / auth setup.
func spaceHelperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondSpaceHelpers(t *testing.T) {
	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantContains    string
		wantNotContains string
		wantDetails     map[string]any
	}{
		{
			name:          "respondSpaceRequestInvalid carries the field detail",
			probe:         func(c *wkhttp.Context) { respondSpaceRequestInvalid(c, "name") },
			wantCodeID:    "err.server.space.request_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "请求参数有误",
			wantDetails:   map[string]any{"field": "name"},
		},
		{
			name:          "respondSpaceRequestInvalid drops empty field key",
			probe:         func(c *wkhttp.Context) { respondSpaceRequestInvalid(c, "") },
			wantCodeID:    "err.server.space.request_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "请求参数有误",
			wantDetails:   map[string]any{},
		},
		{
			name:          "respondSpaceFieldTooLong surfaces the length cap",
			probe:         func(c *wkhttp.Context) { respondSpaceFieldTooLong(c, "description", 200) },
			wantCodeID:    "err.server.space.field_too_long",
			wantSemStatus: http.StatusBadRequest,
			wantDetails:   map[string]any{"field": "description", "max_chars": float64(200)},
		},
		{
			name:          "respondSpaceBatchTooLarge surfaces the batch cap",
			probe:         func(c *wkhttp.Context) { respondSpaceBatchTooLarge(c, 50) },
			wantCodeID:    "err.server.space.batch_too_large",
			wantSemStatus: http.StatusBadRequest,
			wantDetails:   map[string]any{"max": float64(50)},
		},
		{
			name:          "ErrSpacePermissionDenied 403",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpacePermissionDenied) },
			wantCodeID:    "err.server.space.permission_denied",
			wantSemStatus: http.StatusForbidden,
			wantContains:  "没有权限",
		},
		{
			name:          "ErrSpaceCreationDisabled 403",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceCreationDisabled) },
			wantCodeID:    "err.server.space.creation_disabled",
			wantSemStatus: http.StatusForbidden,
			wantContains:  "关闭空间创建",
		},
		{
			name:          "ErrSpaceNotFound 404",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceNotFound) },
			wantCodeID:    "err.server.space.not_found",
			wantSemStatus: http.StatusNotFound,
			wantContains:  "空间不存在",
		},
		{
			name:          "ErrSpaceFull 409",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceFull) },
			wantCodeID:    "err.server.space.full",
			wantSemStatus: http.StatusConflict,
			wantContains:  "空间已满",
		},
		{
			name:          "ErrSpaceInviteCodeInvalid 400 (public anti-enum)",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceInviteCodeInvalid) },
			wantCodeID:    "err.server.space.invite_code_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "邀请码无效",
		},
		{
			name:          "ErrSpaceEmailInviteEmailMismatch 403",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceEmailInviteEmailMismatch) },
			wantCodeID:    "err.server.space.email_invite_email_mismatch",
			wantSemStatus: http.StatusForbidden,
			wantContains:  "邮箱与邀请目标不一致",
		},
		{
			name:            "ErrSpaceQueryFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceQueryFailed) },
			wantCodeID:      "err.server.space.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantContains:    "服务器内部错误",
			wantNotContains: "query space data",
		},
		{
			name:            "ErrSpaceStoreFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrSpaceStoreFailed) },
			wantCodeID:      "err.server.space.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantContains:    "服务器内部错误",
			wantNotContains: "update space data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := spaceHelperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Accept-Language", "zh-CN")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			// All helpers use the D14 ResponseErrorL path → wire 400.
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("HTTP status = %d, want 400 (D14); body=%s", rec.Code, rec.Body.String())
			}
			env := decodeSpaceEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
			}
			if env.Status != http.StatusBadRequest {
				t.Fatalf("legacy status = %d, want 400 (D14)", env.Status)
			}
			if env.Msg != env.Error.Message {
				t.Fatalf("legacy msg %q != error.message %q", env.Msg, env.Error.Message)
			}
			if tc.wantContains != "" && !strings.Contains(env.Error.Message, tc.wantContains) {
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
