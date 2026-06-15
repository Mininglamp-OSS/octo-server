package app_bot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestAppBotNoLegacyResponseError pins that the module's HTTP surface renders
// every error through the i18n envelope (httperr.ResponseErrorL* +
// errcode.ErrAppBot* / shared codes) and never regresses to octo-lib raw
// responses. Comments are stripped first so the migration breadcrumbs in
// api_i18n.go (which name the old c.ResponseError / c.AbortWithStatusJSON forms)
// don't trip the guard. Add any new handler file to the list below.
func TestAppBotNoLegacyResponseError(t *testing.T) {
	files := []string{"app_bot.go", "api_i18n.go", "messages.go"}
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
					t.Fatalf("modules/app_bot/%s must render errors via httperr.ResponseErrorL* / errcode.ErrAppBot*, not legacy %s", f, b)
				}
			}
		})
	}
}

// TestAppBotNotFoundPinnedIsWire400 pins the apply-path responder: it carries the
// real 404 in the envelope but keeps the legacy fixed-400 wire status (D14), so
// existing /v1/app_bot/apply SDK clients that branch on 400 don't break.
func TestAppBotNotFoundPinnedIsWire400(t *testing.T) {
	r := appBotHelperHarness(respondAppBotNotFoundPinned)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wire status = %d, want 400 (D14 pinned for the apply path); body=%s", rec.Code, rec.Body.String())
	}
	var env appBotEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "err.server.app_bot.not_found" {
		t.Fatalf("error.code = %q, want err.server.app_bot.not_found", env.Error.Code)
	}
	if env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("error.http_status = %d, want 404 (envelope keeps the real status)", env.Error.HTTPStatus)
	}
}

// appBotEnvelope is the partial shape of an httperr.ResponseErrorL* response.
type appBotEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func appBotHelperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

// TestAppBotRespondHelpers asserts each responder renders its registered code at
// the correct wire status: validation pins 400 (D14), while not-found/conflict/
// forbidden/internal preserve the code's real status via ResponseErrorLWithStatus.
// No DB/Redis needed — it only exercises the renderer.
func TestAppBotRespondHelpers(t *testing.T) {
	cases := []struct {
		name       string
		probe      func(c *wkhttp.Context)
		wantStatus int
		wantCodeID string
	}{
		{"requestInvalid", func(c *wkhttp.Context) { respondAppBotRequestInvalid(c, "") }, http.StatusBadRequest, "err.server.app_bot.request_invalid"},
		{"idInvalid", respondAppBotIDInvalid, http.StatusBadRequest, "err.server.app_bot.id_invalid"},
		{"notFound", respondAppBotNotFound, http.StatusNotFound, "err.server.app_bot.not_found"},
		{"idConflict", respondAppBotIDConflict, http.StatusConflict, "err.server.app_bot.id_conflict"},
		{"tokenRotationConflict", respondAppBotTokenRotationConflict, http.StatusConflict, "err.server.app_bot.token_rotation_conflict"},
		{"queryFailed", respondAppBotQueryFailed, http.StatusInternalServerError, "err.server.app_bot.query_failed"},
		{"storeFailed", respondAppBotStoreFailed, http.StatusInternalServerError, "err.server.app_bot.store_failed"},
		{"imTokenFailed", respondAppBotIMTokenFailed, http.StatusInternalServerError, "err.server.app_bot.im_token_failed"},
		{"internal", respondAppBotInternal, http.StatusInternalServerError, "err.server.app_bot.internal"},
		{"forbidden", respondAppBotForbidden, http.StatusForbidden, "err.shared.auth.forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := appBotHelperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var env appBotEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantStatus)
			}
		})
	}
}
