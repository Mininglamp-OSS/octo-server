package httperr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

func TestResponseErrorLBuildsCompatibleSpec(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	r.GET("/x", func(c *wkhttp.Context) {
		c.Request = c.Request.WithContext(i18n.WithLanguage(c.Request.Context(), i18n.LanguageDecision{
			Language: "zh-CN",
			Source:   i18n.LanguageSourceAccept,
		}))
		ResponseErrorL(c, errcode.ErrThreadGroupNoInvalid, nil, i18n.Details{
			"field":   "group_no",
			"raw_err": "secret",
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP status = %d, want 400", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["status"]; got != float64(http.StatusBadRequest) {
		t.Fatalf("status = %v, want 400", got)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["code"]; got != errcode.ErrThreadGroupNoInvalid.ID {
		t.Fatalf("error.code = %q", got)
	}
	if got := errObj["http_status"]; got != float64(http.StatusBadRequest) {
		t.Fatalf("error.http_status = %v, want 400", got)
	}
	details := errObj["details"].(map[string]any)
	if got := details["field"]; got != "group_no" {
		t.Fatalf("details.field = %v", got)
	}
	if _, ok := details["raw_err"]; ok {
		t.Fatal("unsafe detail leaked")
	}
}

func TestResponseErrorLUnknownCodeFallsBackToInternal(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	r.GET("/x", func(c *wkhttp.Context) {
		ResponseErrorL(c, codes.Code{
			ID:             "err.server.thread.unregistered",
			HTTPStatus:     http.StatusForbidden,
			DefaultMessage: "unregistered",
		}, nil, nil)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP status = %d, want 400", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["code"]; got != "err.shared.internal" {
		t.Fatalf("error.code = %q", got)
	}
	if got := errObj["http_status"]; got != float64(http.StatusInternalServerError) {
		t.Fatalf("error.http_status = %v, want 500", got)
	}
}


// TestResponseErrorLWithStatusKeepsSemanticStatus verifies the WithStatus facade
// emits the code's canonical HTTPStatus on the wire (not the legacy 400), while
// keeping the body envelope identical to ResponseErrorL.
func TestResponseErrorLWithStatusKeepsSemanticStatus(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	r.GET("/x", func(c *wkhttp.Context) {
		ResponseErrorLWithStatus(c, errcode.ErrThreadDeleted, nil, nil)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusGone {
		t.Fatalf("wire status = %d, want 410", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// Both legacy (status) and v2 (error.http_status) agree on the real status.
	if got := body["status"]; got != float64(http.StatusGone) {
		t.Fatalf("body status = %v, want 410", got)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["code"]; got != errcode.ErrThreadDeleted.ID {
		t.Fatalf("error.code = %q", got)
	}
	if got := errObj["http_status"]; got != float64(http.StatusGone) {
		t.Fatalf("error.http_status = %v, want 410", got)
	}
}

// TestResponseErrorLWithStatusInternalStays5xx verifies a 5xx Internal code keeps
// its real status on the wire AND still hides the underlying message/details.
func TestResponseErrorLWithStatusInternalStays5xx(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	r.GET("/x", func(c *wkhttp.Context) {
		ResponseErrorLWithStatus(c, errcode.ErrThreadStoreFailed, nil, i18n.Details{"raw_err": "secret"})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("wire status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["http_status"]; got != float64(http.StatusInternalServerError) {
		t.Fatalf("error.http_status = %v, want 500", got)
	}
	// Internal=true must not surface the raw default message or unsafe details.
	if msg, _ := errObj["message"].(string); msg == errcode.ErrThreadStoreFailed.DefaultMessage {
		t.Fatalf("internal code leaked its default message: %q", msg)
	}
	if details, ok := errObj["details"].(map[string]any); ok {
		if _, leaked := details["raw_err"]; leaked {
			t.Fatal("internal code leaked unsafe detail")
		}
	}
}

// TestResponseErrorLWithStatusUsesRealStatus pins the facade on an integration
// (Octo-link) code: the wire + error.http_status must both be the real 403.
func TestResponseErrorLWithStatusUsesRealStatus(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	r.GET("/x", func(c *wkhttp.Context) {
		ResponseErrorLWithStatus(c, errcode.ErrIntegrationUserNotLinked, nil, nil)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("HTTP status = %d, want 403", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["status"]; got != float64(http.StatusForbidden) {
		t.Fatalf("status = %v, want 403", got)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["code"]; got != errcode.ErrIntegrationUserNotLinked.ID {
		t.Fatalf("error.code = %q", got)
	}
	if got := errObj["http_status"]; got != float64(http.StatusForbidden) {
		t.Fatalf("error.http_status = %v, want 403", got)
	}
}
