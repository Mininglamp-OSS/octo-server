package sticker

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

// TestStickerNoLegacyResponseError pins that the sticker handlers never regress
// to legacy octo-lib raw error responses — all user-facing errors go through
// httperr.ResponseErrorL via respondSticker* / errcode.ErrSticker*. Comments are
// stripped first so commented-out breadcrumbs do not trip the guard; the
// s.Error(...) zap LOG calls are not responses and are intentionally allowed.
func TestStickerNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_i18n.go"}
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
					t.Fatalf("modules/sticker/%s must use httperr.ResponseErrorL via respondSticker* helpers / errcode.ErrSticker* instead of legacy %s", f, b)
				}
			}
		})
	}
}

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

// helperHarness mounts a single GET /probe route that invokes the supplied
// helper with the i18n renderer wired, so tests can assert the rendered
// envelope without paying the DB / auth setup cost.
func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondStickerHelpers(t *testing.T) {
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
			name:          "respondStickerRequestInvalid carries the field detail",
			probe:         func(c *wkhttp.Context) { respondStickerRequestInvalid(c, "path") },
			wantCodeID:    "err.server.sticker.request_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "请求参数有误",
			wantDetails:   map[string]any{"field": "path"},
		},
		{
			name:          "respondStickerRequestInvalid drops empty field key",
			probe:         func(c *wkhttp.Context) { respondStickerRequestInvalid(c, "") },
			wantCodeID:    "err.server.sticker.request_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "请求参数有误",
			wantDetails:   map[string]any{},
		},
		{
			name:          "respondStickerFormatUnsupported surfaces the format detail",
			probe:         func(c *wkhttp.Context) { respondStickerFormatUnsupported(c, "tiff") },
			wantCodeID:    "err.server.sticker.format_unsupported",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "不支持的贴纸格式",
			wantDetails:   map[string]any{"field": "format", "format": "tiff"},
		},
		{
			name:          "respondStickerQuotaExceeded surfaces the cap",
			probe:         func(c *wkhttp.Context) { respondStickerQuotaExceeded(c, 100) },
			wantCodeID:    "err.server.sticker.quota_exceeded",
			wantSemStatus: http.StatusConflict,
			wantContains:  "上限",
			wantDetails:   map[string]any{"max": float64(100)},
		},
		{
			name:          "respondStickerShortcodeInvalid surfaces the field detail",
			probe:         func(c *wkhttp.Context) { respondStickerShortcodeInvalid(c) },
			wantCodeID:    "err.server.sticker.shortcode_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "快捷码格式",
			wantDetails:   map[string]any{"field": "shortcode"},
		},
		{
			name:          "respondStickerKeywordsInvalid surfaces the field detail",
			probe:         func(c *wkhttp.Context) { respondStickerKeywordsInvalid(c) },
			wantCodeID:    "err.server.sticker.keywords_invalid",
			wantSemStatus: http.StatusBadRequest,
			wantContains:  "关键词格式",
			wantDetails:   map[string]any{"field": "keywords"},
		},
		{
			name:          "respondStickerShortcodeConflict surfaces the field detail",
			probe:         func(c *wkhttp.Context) { respondStickerShortcodeConflict(c) },
			wantCodeID:    "err.server.sticker.shortcode_conflict",
			wantSemStatus: http.StatusConflict,
			wantContains:  "快捷码已存在",
			wantDetails:   map[string]any{"field": "shortcode"},
		},
		{
			name:          "ErrStickerNotFound surfaces 404 zh-CN copy",
			probe:         func(c *wkhttp.Context) { httperrL(c, errcode.ErrStickerNotFound) },
			wantCodeID:    "err.server.sticker.not_found",
			wantSemStatus: http.StatusNotFound,
			wantContains:  "贴纸不存在",
		},
		{
			name:            "ErrStickerQueryFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrStickerQueryFailed) },
			wantCodeID:      "err.server.sticker.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantContains:    "服务器内部错误",
			wantNotContains: "query sticker data",
		},
		{
			name:            "ErrStickerStoreFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrStickerStoreFailed) },
			wantCodeID:      "err.server.sticker.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantContains:    "服务器内部错误",
			wantNotContains: "update sticker data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := helperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Accept-Language", "zh-CN")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			env := decodeErrEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
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
