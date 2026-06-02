package oidc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestOIDCBindNoLegacyResponseError is a source guard: api_bind.go must route
// every error through httperr.ResponseErrorLWithStatus (via respondBindError),
// never raw c.AbortWithStatusJSON / c.AbortWithStatus / non-OK c.JSON.
//
// api.go is intentionally NOT guarded: its OAuth2/OIDC protocol endpoints
// (authorize/callback/logout) keep raw responses by design (browser redirect
// flow, not the dmwork front-end) — see the EXEMPT note in
// tools/lint-direct-error-response/baseline.txt.
func TestOIDCBindNoLegacyResponseError(t *testing.T) {
	data, err := os.ReadFile("api_bind.go")
	if err != nil {
		t.Fatalf("read api_bind.go: %v", err)
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

	for _, b := range []string{
		".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(",
		"c.AbortWithStatusJSON", "c.AbortWithStatus(", "errMsg(",
	} {
		if strings.Contains(cleaned, b) {
			t.Fatalf("modules/oidc/api_bind.go must use respondBindError / httperr.ResponseErrorLWithStatus instead of legacy %s", b)
		}
	}
	// Raw non-OK c.JSON(http.Status…) bypasses the envelope just as completely;
	// the bind success responses (c.JSON(http.StatusOK, …)) are allowed.
	for _, line := range strings.Split(cleaned, "\n") {
		if strings.Contains(line, "c.JSON(http.Status") && !strings.Contains(line, "c.JSON(http.StatusOK") {
			t.Fatalf("modules/oidc/api_bind.go must not emit raw non-OK c.JSON: %s", strings.TrimSpace(line))
		}
	}
}

// newBindRouterWithRenderer builds a wkhttp router with the real i18n renderer
// injected and an optional language pinned into the request context, so the
// bind handlers render the full dual envelope (the gin.New()-based
// newTestBindRouter falls back to defaultErrorRenderer and cannot exercise
// error.code / translation).
func newBindRouterWithRenderer(o *OIDC, lang string) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	g := r.Group("/v1/auth/oidc/aegis", func(c *wkhttp.Context) {
		if lang != "" {
			c.Request = c.Request.WithContext(i18n.WithLanguage(c.Request.Context(), i18n.LanguageDecision{
				Language: lang,
				Source:   i18n.LanguageSourceAccept,
			}))
		}
		c.Next()
	})
	g.GET("/bind/info", o.bindInfo)
	g.POST("/bind/verify/password", o.bindVerifyPassword)
	return r
}

// decodeBindError unmarshals the response and returns (wireStatus, error-object).
func decodeBindError(t *testing.T, w *httptest.ResponseRecorder) (int, map[string]any) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response missing error{} envelope: %s", w.Body.String())
	}
	// Dual envelope: legacy msg/status must also be present.
	if _, ok := body["msg"]; !ok {
		t.Fatalf("response missing legacy msg field: %s", w.Body.String())
	}
	if _, ok := body["status"]; !ok {
		t.Fatalf("response missing legacy status field: %s", w.Body.String())
	}
	return w.Code, errObj
}

// TestBindError_TokenInvalidKeepsRealStatus verifies an unknown bind token yields
// the real 410 wire status (not the legacy 400) plus the localized envelope.
func TestBindError_TokenInvalidKeepsRealStatus(t *testing.T) {
	o, _, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), nil, true)
	r := newBindRouterWithRenderer(o, "")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token=fake-jti", nil))

	code, errObj := decodeBindError(t, w)
	if code != http.StatusGone {
		t.Fatalf("wire status = %d, want 410", code)
	}
	if errObj["code"] != errcode.ErrOIDCBindTokenInvalid.ID {
		t.Fatalf("error.code = %v, want %s", errObj["code"], errcode.ErrOIDCBindTokenInvalid.ID)
	}
	if errObj["http_status"] != float64(http.StatusGone) {
		t.Fatalf("error.http_status = %v, want 410", errObj["http_status"])
	}
}

// TestBindError_RequestInvalid verifies a malformed token yields 400 +
// bind_request_invalid.
func TestBindError_RequestInvalid(t *testing.T) {
	o, _, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), nil, true)
	r := newBindRouterWithRenderer(o, "")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token=a.b", nil))

	code, errObj := decodeBindError(t, w)
	if code != http.StatusBadRequest {
		t.Fatalf("wire status = %d, want 400", code)
	}
	if errObj["code"] != errcode.ErrOIDCBindRequestInvalid.ID {
		t.Fatalf("error.code = %v, want %s", errObj["code"], errcode.ErrOIDCBindRequestInvalid.ID)
	}
}

// TestBindError_AntiEnumeration verifies a rejected password (unknown username)
// returns the single generic 401 invalid_credentials code — never a more
// specific reason that could be used to probe account existence.
func TestBindError_AntiEnumeration(t *testing.T) {
	o, jti, auth, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), sampleClaims(), false)
	auth.verifyPasswordResp.matched = false // unknown user / wrong password
	r := newBindRouterWithRenderer(o, "")

	body, _ := json.Marshal(map[string]string{
		"token": jti, "identifier": "ghost", "password": "Whatever@123",
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	code, errObj := decodeBindError(t, w)
	if code != http.StatusUnauthorized {
		t.Fatalf("wire status = %d, want 401", code)
	}
	if errObj["code"] != errcode.ErrOIDCBindInvalidCredentials.ID {
		t.Fatalf("error.code = %v, want %s (anti-enumeration)", errObj["code"], errcode.ErrOIDCBindInvalidCredentials.ID)
	}
}

// TestBindError_ZhCNTranslation verifies the localized message follows the
// request language.
func TestBindError_ZhCNTranslation(t *testing.T) {
	o, _, _, _, _ := newTestOIDCWithBind(t, defaultBindCfg(), nil, true)
	r := newBindRouterWithRenderer(o, "zh-CN")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/auth/oidc/aegis/bind/info?token=fake-jti", nil))

	_, errObj := decodeBindError(t, w)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "过期") {
		t.Fatalf("zh-CN message = %q, want Chinese translation", msg)
	}
}
