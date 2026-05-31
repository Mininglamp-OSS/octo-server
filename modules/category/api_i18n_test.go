package category

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
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

// TestCategoryNoLegacyResponseError pins the Phase 2.1 contract that the
// migrated modules/category handlers do not regress to legacy octo-lib error
// responses. Comments are stripped first so commented-out breadcrumbs do not
// trip the guard. The c.Error(...) zap LOG calls are not responses and are
// intentionally allowed (they match neither banned token).
func TestCategoryNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go"}
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
					t.Fatalf("modules/category/%s must use httperr.ResponseErrorL via respondCategory* helpers / errcode.ErrCategory* instead of legacy %s", f, b)
				}
			}
		})
	}
}

// newCategoryTestServer wraps testutil.NewTestServer and injects the i18n
// ErrorRenderer onto the route, mirroring what main.go does at boot. Post-
// Phase-2.1, modules/category handlers respond via httperr.ResponseErrorL →
// c.RenderError; without a renderer wired the route falls back to the legacy
// {msg,status} envelope carrying the English source DefaultMessage instead of
// the localized zh-CN copy production clients receive. testutil.NewTestServer
// lives in octo-lib and is intentionally not touched from this PR, so the
// integration tests funnel construction through this single helper.
func newCategoryTestServer() (*server.Server, *config.Context) {
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return s, ctx
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

func decodeErrEnvelope(t *testing.T, body []byte) errEnvelope {
	t.Helper()
	var env errEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, body)
	}
	return env
}

// assertCategoryErrorCode asserts the migrated error envelope carries the
// expected error.code. Used by the integration tests in api_test.go in place of
// brittle zh-CN substring matching, so future copy edits do not break them.
func assertCategoryErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	env := decodeErrEnvelope(t, w.Body.Bytes())
	if env.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q\nbody: %s", env.Error.Code, wantCode, w.Body.String())
	}
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

func TestRespondCategoryHelpers(t *testing.T) {
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
			name:            "respondCategoryRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondCategoryRequestInvalid(c, "name") },
			wantCodeID:      "err.server.category.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "name"},
		},
		{
			name:            "respondCategoryRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondCategoryRequestInvalid(c, "") },
			wantCodeID:      "err.server.category.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondCategoryNameTooLong surfaces the length cap",
			probe:           func(c *wkhttp.Context) { respondCategoryNameTooLong(c, 100) },
			wantCodeID:      "err.server.category.name_too_long",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "100",
			wantDetails:     map[string]any{"field": "name", "max_length": float64(100)},
		},
		{
			name:            "respondCategoryLimitExceeded surfaces the per-space cap",
			probe:           func(c *wkhttp.Context) { respondCategoryLimitExceeded(c, 20) },
			wantCodeID:      "err.server.category.limit_exceeded",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "20",
			wantDetails:     map[string]any{"max": float64(20)},
		},
		{
			name:            "ErrCategorySpaceMemberRequired surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategorySpaceMemberRequired) },
			wantCodeID:      "err.server.category.space_member_required",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "你不是该空间成员",
		},
		{
			name:            "ErrCategoryGroupMemberRequired surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryGroupMemberRequired) },
			wantCodeID:      "err.server.category.group_member_required",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "你不是该群成员",
		},
		{
			name:            "ErrCategoryPermissionDenied surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryPermissionDenied) },
			wantCodeID:      "err.server.category.permission_denied",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权操作此分类",
		},
		{
			name:            "ErrCategoryDefaultImmutable surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryDefaultImmutable) },
			wantCodeID:      "err.server.category.default_immutable",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "默认分类不可修改",
		},
		{
			name:            "ErrCategoryDefaultUndeletable surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryDefaultUndeletable) },
			wantCodeID:      "err.server.category.default_undeletable",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "默认分类不可删除",
		},
		{
			name:            "ErrCategoryNotFound surfaces 404 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryNotFound) },
			wantCodeID:      "err.server.category.not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "分类不存在",
		},
		{
			name:            "ErrCategorySpaceMismatch surfaces 400 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategorySpaceMismatch) },
			wantCodeID:      "err.server.category.space_mismatch",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不在同一空间",
		},
		{
			name:            "ErrCategorySortListMismatch surfaces 400 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategorySortListMismatch) },
			wantCodeID:      "err.server.category.sort_list_mismatch",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "数量不匹配",
		},
		{
			name:            "ErrCategoryGroupSpaceMissing surfaces 400 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryGroupSpaceMissing) },
			wantCodeID:      "err.server.category.group_space_missing",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "不属于任何空间",
		},
		{
			name:            "ErrCategoryQueryFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryQueryFailed) },
			wantCodeID:      "err.server.category.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query category data",
		},
		{
			name:            "ErrCategoryStoreFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrCategoryStoreFailed) },
			wantCodeID:      "err.server.category.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "update category data",
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
			env := decodeErrEnvelope(t, rec.Body.Bytes())
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
