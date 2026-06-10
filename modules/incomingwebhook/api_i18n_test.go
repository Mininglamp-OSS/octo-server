package incomingwebhook

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

// TestIncomingWebhookNoLegacyResponseError pins that the module's HTTP surface
// renders every error through the i18n envelope (httperr.ResponseErrorL* +
// errcode.ErrIncomingWebhook*) and never regresses to legacy octo-lib raw
// responses. Comments are stripped first so commented-out breadcrumbs (e.g. the
// #246 note in api_i18n.go) don't trip the guard. Add any new handler/limiter
// file to the list below.
func TestIncomingWebhookNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_i18n.go", "ratelimit.go", "localfloor.go", "cache.go",
		"adapter.go", "adapter_github.go", "adapter_wecom.go"}
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
					t.Fatalf("modules/incomingwebhook/%s must render errors via httperr.ResponseErrorL* / errcode.ErrIncomingWebhook*, not legacy %s", f, b)
				}
			}
		})
	}
}

// iwhEnvelope is the partial shape of an httperr.ResponseErrorL* response.
type iwhEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
	Status int `json:"status"`
}

func iwhHelperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

// TestIncomingWebhookRespondHelpers asserts each push/management responder
// renders the expected i18n code at its real wire status, and that the
// rate-limit 429 carries the Retry-After back-off hint. No DB/Redis needed — it
// only exercises the renderer.
func TestIncomingWebhookRespondHelpers(t *testing.T) {
	cases := []struct {
		name           string
		probe          func(c *wkhttp.Context)
		wantStatus     int
		wantCodeID     string
		wantRetryAfter bool
	}{
		{"pushUnauthorized", pushUnauthorized, http.StatusUnauthorized, "err.server.incomingwebhook.push_unauthorized", false},
		{"pushRateLimited", pushRateLimited, http.StatusTooManyRequests, "err.server.incomingwebhook.push_rate_limited", true},
		{"pushPayloadInvalid", func(c *wkhttp.Context) { pushPayloadInvalid(c, "json") }, http.StatusBadRequest, "err.server.incomingwebhook.push_payload_invalid", false},
		{"pushPayloadTooLarge", pushPayloadTooLarge, http.StatusRequestEntityTooLarge, "err.server.incomingwebhook.push_payload_too_large", false},
		{"pushDeliveryFailed", pushDeliveryFailed, http.StatusBadGateway, "err.server.incomingwebhook.push_delivery_failed", false},
		{"pushDisabled", pushDisabled, http.StatusNotFound, "err.server.incomingwebhook.push_disabled", false},
		{"mgmtForbidden", mgmtForbidden, http.StatusForbidden, "err.server.incomingwebhook.mgmt_forbidden", false},
		{"mgmtFeatureDisabled", mgmtFeatureDisabled, http.StatusForbidden, "err.server.incomingwebhook.mgmt_disabled", false},
		{"mgmtNotFound", mgmtNotFound, http.StatusNotFound, "err.server.incomingwebhook.mgmt_not_found", false},
		{"mgmtQuotaExceeded", func(c *wkhttp.Context) { mgmtQuotaExceeded(c, 10) }, http.StatusConflict, "err.server.incomingwebhook.mgmt_quota_exceeded", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := iwhHelperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var env iwhEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantStatus)
			}
			if tc.wantRetryAfter && rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("Retry-After = %q, want \"1\"", rec.Header().Get("Retry-After"))
			}
		})
	}
}

// TestPushPayloadInvalidSurfacesReason locks the external contract that
// pushPayloadInvalid surfaces details.reason in the i18n error envelope. This is
// asserted HERE (with the i18n ErrorRenderer wired, as in production via main.go)
// rather than in the full e2e harness: testutil.NewTestServer renders only the
// legacy {msg,status} shape, so an e2e body never carries error.details — see the
// note in richtext_push_test.go. No DB/Redis needed; this only exercises the renderer.
func TestPushPayloadInvalidSurfacesReason(t *testing.T) {
	for _, reason := range []string{"blocks", "msg_type", "content", "json", "event"} {
		t.Run(reason, func(t *testing.T) {
			r := iwhHelperHarness(func(c *wkhttp.Context) { pushPayloadInvalid(c, reason) })
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), reason) {
				t.Fatalf("body must surface details.reason=%q; body=%s", reason, rec.Body.String())
			}
		})
	}
}
