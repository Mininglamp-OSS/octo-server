package i18n

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

func TestErrorRenderer_RendersLocalizedDualEnvelope(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(NewErrorRenderer(NewLocalizer(SourceLanguage)))
	r.GET("/limited", func(c *wkhttp.Context) {
		c.Request = c.Request.WithContext(WithLanguage(c.Request.Context(), LanguageDecision{
			Language: "zh-CN",
			Source:   LanguageSourceAccept,
		}))
		c.RenderError(wkhttp.ErrorSpec{
			Code:            "err.shared.rate.limited",
			DefaultMessage:  "raw fallback",
			TransportStatus: http.StatusTooManyRequests,
			SemanticStatus:  http.StatusTooManyRequests,
			Details: map[string]any{
				"retry_after": 3,
				"raw_err":     "do not leak",
			},
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/limited", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("HTTP status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Content-Language"); got != "zh-CN" {
		t.Fatalf("Content-Language = %q, want zh-CN", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["msg"]; got != "请求过于频繁，请稍后再试。" {
		t.Fatalf("msg = %q", got)
	}
	if got := body["status"]; got != float64(http.StatusTooManyRequests) {
		t.Fatalf("status = %v, want %d", got, http.StatusTooManyRequests)
	}

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error object missing: %#v", body["error"])
	}
	if got := errObj["code"]; got != "err.shared.rate.limited" {
		t.Fatalf("error.code = %q", got)
	}
	if got := errObj["message"]; got != body["msg"] {
		t.Fatalf("error.message = %q, want msg %q", got, body["msg"])
	}
	if got := errObj["http_status"]; got != float64(http.StatusTooManyRequests) {
		t.Fatalf("error.http_status = %v", got)
	}
	details, ok := errObj["details"].(map[string]any)
	if !ok {
		t.Fatalf("error.details missing: %#v", errObj["details"])
	}
	if got := details["retry_after"]; got != float64(3) {
		t.Fatalf("details.retry_after = %v", got)
	}
	if _, ok := details["raw_err"]; ok {
		t.Fatal("unsafe detail raw_err leaked")
	}
}

// TestErrorRenderer_TransportStatusDivergesFromSemantic pins the D14
// compatibility contract: on the standard path the wire HTTP status and the
// legacy body `status` are fixed at TransportStatus (400 during the
// compatibility window) while `error.http_status` carries the real
// SemanticStatus. Every other test in this file uses Transport==Semantic, so
// without this case the divergence is never exercised — a renderer regression
// that accidentally emitted SemanticStatus on the wire would go unnoticed and
// break legacy clients that branch on HTTP status.
func TestErrorRenderer_TransportStatusDivergesFromSemantic(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(NewErrorRenderer(NewLocalizer(SourceLanguage)))
	r.GET("/diverge", func(c *wkhttp.Context) {
		c.Request = c.Request.WithContext(WithLanguage(c.Request.Context(), LanguageDecision{
			Language: "zh-CN",
			Source:   LanguageSourceAccept,
		}))
		c.RenderError(wkhttp.ErrorSpec{
			Code:            "err.server.user.not_found",
			DefaultMessage:  "User not found.",
			TransportStatus: http.StatusBadRequest,       // wire + legacy body status
			SemanticStatus:  http.StatusNotFound,          // real semantics, only in error.http_status
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/diverge", nil)
	r.ServeHTTP(rec, req)

	// Wire status fixed at TransportStatus (400), NOT the 404 semantic status.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wire HTTP status = %d, want %d (D14 transport fixed)", rec.Code, http.StatusBadRequest)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["status"]; got != float64(http.StatusBadRequest) {
		t.Fatalf("legacy body status = %v, want %d (must equal transport)", got, http.StatusBadRequest)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["http_status"]; got != float64(http.StatusNotFound) {
		t.Fatalf("error.http_status = %v, want %d (must equal semantic)", got, http.StatusNotFound)
	}
	// The non-internal message is still localized (not suppressed).
	if got := errObj["message"]; got != "用户不存在。" {
		t.Fatalf("error.message = %q, want localized copy", got)
	}
}

// TestErrorRenderer_DualEnvelopeParityRegardlessOfClientHeader pins the v7.2
// contract that the renderer ALWAYS emits both the v2 error.{} object and the
// legacy {msg,status} pair, with no differentiation on the
// X-Octo-Error-Envelope request header. A client that does NOT advertise v2
// must receive the exact same body as one that does — the header is a
// sunset-statistics signal only (D12), never a content switch.
func TestErrorRenderer_DualEnvelopeParityRegardlessOfClientHeader(t *testing.T) {
	render := func(withV2Header bool) string {
		r := wkhttp.New()
		r.SetErrorRenderer(NewErrorRenderer(NewLocalizer(SourceLanguage)))
		r.GET("/parity", func(c *wkhttp.Context) {
			c.Request = c.Request.WithContext(WithLanguage(c.Request.Context(), LanguageDecision{
				Language: "zh-CN",
				Source:   LanguageSourceAccept,
			}))
			c.RenderError(wkhttp.ErrorSpec{
				Code:            "err.shared.not_found",
				DefaultMessage:  "Resource not found.",
				TransportStatus: http.StatusBadRequest,
				SemanticStatus:  http.StatusNotFound,
			})
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/parity", nil)
		if withV2Header {
			req.Header.Set("X-Octo-Error-Envelope", "v2")
		}
		r.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	withHeader := render(true)
	withoutHeader := render(false)
	if withHeader != withoutHeader {
		t.Fatalf("envelope differs on X-Octo-Error-Envelope header:\n  with v2:    %s\n  without v2: %s", withHeader, withoutHeader)
	}

	// Sanity: both forms carry BOTH envelopes, not just one.
	var body map[string]any
	if err := json.Unmarshal([]byte(withoutHeader), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := body["error"].(map[string]any); !ok {
		t.Fatalf("v2 error object missing without header: %s", withoutHeader)
	}
	if _, ok := body["msg"]; !ok {
		t.Fatalf("legacy msg missing: %s", withoutHeader)
	}
	if _, ok := body["status"]; !ok {
		t.Fatalf("legacy status missing: %s", withoutHeader)
	}
}

func TestErrorRenderer_InternalDoesNotExposeSpecData(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(NewErrorRenderer(NewLocalizer(SourceLanguage)))
	r.GET("/internal", func(c *wkhttp.Context) {
		c.Request = c.Request.WithContext(WithLanguage(c.Request.Context(), LanguageDecision{
			Language: "zh-CN",
			Source:   LanguageSourceAccept,
		}))
		c.RenderError(wkhttp.ErrorSpec{
			Code:            "err.server.thread.store_failed",
			DefaultMessage:  "database host 10.0.0.1 failed",
			TransportStatus: http.StatusBadRequest,
			SemanticStatus:  http.StatusInternalServerError,
			Params: map[string]any{
				"table": "thread",
			},
			Details: map[string]any{
				"raw_err": "secret",
			},
			Internal: true,
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal", nil)
	r.ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["msg"]; got != "服务器内部错误。" {
		t.Fatalf("msg = %q", got)
	}
	errObj := body["error"].(map[string]any)
	if got := errObj["message"]; got != "服务器内部错误。" {
		t.Fatalf("error.message = %q", got)
	}
	details := errObj["details"].(map[string]any)
	if len(details) != 0 {
		t.Fatalf("internal details leaked: %#v", details)
	}
}
