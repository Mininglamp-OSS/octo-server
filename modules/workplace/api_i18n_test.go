package workplace

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

// TestWorkplaceNoLegacyResponseError pins the Phase 2.1 contract that the
// migrated modules/workplace handlers do not regress to legacy octo-lib error
// responses. Comments are stripped first so commented-out breadcrumbs do not
// trip the guard. The m.Error(common.ErrData.Error(), ...) zap LOG calls are not
// responses and are intentionally allowed (they match neither banned token).
func TestWorkplaceNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_manager.go"}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", "c.Response(\""}
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
					t.Fatalf("modules/workplace/%s must use httperr.ResponseErrorL via respondWorkplace* helpers / errcode.ErrWorkplace* instead of legacy %s", f, b)
				}
			}
		})
	}
}

// envelope is the partial shape of an httperr.ResponseErrorL response. The
// renderer emits both the legacy {msg,status} and the v2 {error.{...}} blocks
// unconditionally (v7.2 dual-envelope contract).
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

// helperHarness mounts a single GET /probe route that invokes the supplied
// helper with the i18n renderer wired, so tests can assert the rendered
// envelope without paying the DB / auth setup cost.
func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondWorkplaceHelpers(t *testing.T) {
	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int    // always 400 for legacy compat (D14)
		wantContains    string // zh-CN substring expected in error.message
		wantNotContains string // forbid leaked English DefaultMessage when Internal=true
		wantDetails     map[string]any
	}{
		{
			name:            "respondWorkplaceRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondWorkplaceRequestInvalid(c, "app_id") },
			wantCodeID:      "err.server.workplace.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "app_id"},
		},
		{
			name:            "respondWorkplaceRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondWorkplaceRequestInvalid(c, "") },
			wantCodeID:      "err.server.workplace.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondWorkplaceForbidden routes to shared.auth.forbidden",
			probe:           func(c *wkhttp.Context) { respondWorkplaceForbidden(c) },
			wantCodeID:      "err.shared.auth.forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权执行此操作",
		},
		{
			name:            "respondWorkplaceInternal routes to shared.internal",
			probe:           func(c *wkhttp.Context) { respondWorkplaceInternal(c) },
			wantCodeID:      "err.shared.internal",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
		},
		{
			name:            "ErrWorkplaceAppNotFound surfaces 404 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrWorkplaceAppNotFound) },
			wantCodeID:      "err.server.workplace.app_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "该应用不存在或不可用",
		},
		{
			name:            "ErrWorkplaceCategoryNotFound surfaces 404 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrWorkplaceCategoryNotFound) },
			wantCodeID:      "err.server.workplace.category_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "该分类不存在",
		},
		{
			name:            "ErrWorkplaceAppNameExists surfaces 409 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrWorkplaceAppNameExists) },
			wantCodeID:      "err.server.workplace.app_name_exists",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "该应用名称已存在",
		},
		{
			name:            "ErrWorkplaceCategoryNameExists surfaces 409 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrWorkplaceCategoryNameExists) },
			wantCodeID:      "err.server.workplace.category_name_exists",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "该分类名称已存在",
		},
		{
			name:            "ErrWorkplaceQueryFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrWorkplaceQueryFailed) },
			wantCodeID:      "err.server.workplace.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query workplace data",
		},
		{
			name:            "ErrWorkplaceStoreFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrWorkplaceStoreFailed) },
			wantCodeID:      "err.server.workplace.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "update workplace data",
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
