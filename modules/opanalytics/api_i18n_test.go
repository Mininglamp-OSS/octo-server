package opanalytics

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

// TestOpanalyticsNoLegacyResponseError pins that the module's HTTP surface renders
// every error through the i18n envelope (httperr.ResponseErrorL + errcode.ErrOpanalytics*)
// and never regresses to legacy octo-lib raw responses. Comments are stripped first so
// commented-out breadcrumbs don't trip the guard. Add any new handler/query file below.
func TestOpanalyticsNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_i18n.go", "service.go", "db.go", "etl.go", "etl_db.go", "scheduler.go"}
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
					t.Fatalf("modules/opanalytics/%s must render errors via httperr.ResponseErrorL / errcode.ErrOpanalytics*, not legacy %s", f, b)
				}
			}
		})
	}
}

type opaEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
	Status int `json:"status"`
}

func opaHelperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

// TestOpanalyticsRespondHelpers asserts each responder emits the expected i18n code
// at the legacy transport-400 (ResponseErrorL) with the real status in error.http_status.
func TestOpanalyticsRespondHelpers(t *testing.T) {
	cases := []struct {
		name           string
		probe          func(c *wkhttp.Context)
		wantHTTPStatus int
		wantCodeID     string
	}{
		{"forbidden", respForbidden, http.StatusForbidden, "err.server.opanalytics.forbidden"},
		{"requestInvalid", func(c *wkhttp.Context) { respRequestInvalid(c, "date_range") }, http.StatusBadRequest, "err.server.opanalytics.request_invalid"},
		{"notFound", respNotFound, http.StatusNotFound, "err.server.opanalytics.not_found"},
		{"queryFailed", respQueryFailed, http.StatusInternalServerError, "err.server.opanalytics.query_failed"},
		{"etlAlreadyRunning", respETLAlreadyRunning, http.StatusConflict, "err.server.opanalytics.etl_already_running"},
		{"etlTriggerFailed", respETLTriggerFailed, http.StatusInternalServerError, "err.server.opanalytics.etl_trigger_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := opaHelperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			// ResponseErrorL pins transport status = 400 (D14 compat).
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("transport status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			var env opaEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantHTTPStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantHTTPStatus)
			}
		})
	}
}
