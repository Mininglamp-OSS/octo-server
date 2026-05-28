package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// Handler-level tests for PUT /v1/user/language. We deliberately don't go
// through testutil.NewTestServer here — the integration suite is gated on a
// migration TODO (issue #17) — but we still exercise the real
// LanguageService against the same fakeLangDB / fakeLangCache used by the
// service unit tests, so the DB-update + cache-DEL contract is covered
// end-to-end at the HTTP layer.
//
// Tests omit t.Parallel: log.NewTLog wraps zap's global logger, and
// parallel subtests calling Error() concurrently trip the race detector.
// This is a pre-existing constraint shared with the rest of
// modules/user/*_test.go.

func newLanguageHandlerHarness(t *testing.T, db languageReader, c *fakeLangCache) (*wkhttp.WKHttp, *User) {
	t.Helper()
	u := &User{Log: log.NewTLog("user-test")}
	u.languageService = NewLanguageService(db, c)
	r := wkhttp.New()
	// The handler now responds via httperr.ResponseErrorL, which delegates to
	// the wkhttp ErrorRenderer. Wire the i18n renderer so the test exercises
	// the same envelope path production hits — without this the renderer
	// falls back to legacy {msg,status} with the unsuppressed English source
	// message, defeating the Internal=true contract.
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.Group("/v1/user").PUT("language", func(ctx *wkhttp.Context) {
		// Inject the login UID the way octo-lib's AuthMiddleware would.
		// Bypassing AuthMiddleware keeps the test focused on the handler
		// branches; auth is exercised by upstream octo-lib tests.
		ctx.Set("uid", testHandlerUID)
		u.setLanguage(ctx)
	})
	return r, u
}

const testHandlerUID = "u1"

func TestSetLanguageHandler_HappyPath(t *testing.T) {
	db := newFakeLangDB()
	db.lang[testHandlerUID] = "zh-CN" // existing preference; should be overwritten
	c := newFakeLangCache()
	_ = c.Set(LanguageCacheKeyPrefix+testHandlerUID, "zh-CN") // hot entry to verify DEL

	r, _ := newLanguageHandlerHarness(t, db, c)

	body := strings.NewReader(`{"language":"en-US"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if db.updates[testHandlerUID] != "en-US" {
		t.Fatalf("db updates = %v, want u1 → en-US", db.updates)
	}
	if len(c.deletes) == 0 || c.deletes[0] != LanguageCacheKeyPrefix+testHandlerUID {
		t.Fatalf("expected hot cache DEL on write, deletes = %v", c.deletes)
	}
}

func TestSetLanguageHandler_ClearsPreference(t *testing.T) {
	db := newFakeLangDB()
	db.lang[testHandlerUID] = "zh-CN"
	c := newFakeLangCache()
	r, _ := newLanguageHandlerHarness(t, db, c)

	body := strings.NewReader(`{"language":""}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if db.updates[testHandlerUID] != "" {
		t.Fatalf("expected empty string in db, got %q", db.updates[testHandlerUID])
	}
}

func TestSetLanguageHandler_RejectsUnsupported(t *testing.T) {
	db := newFakeLangDB()
	r, _ := newLanguageHandlerHarness(t, db, newFakeLangCache())

	body := strings.NewReader(`{"language":"klingon"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400), body = %s", rec.Code, rec.Body.String())
	}
	if len(db.updates) != 0 {
		t.Fatalf("unsupported language must not touch DB, got %v", db.updates)
	}
	// User-facing message is the classified "unsupported" copy, not the raw
	// service-layer error (which contains the `user:` package prefix). Guards
	// against information disclosure flagged in PR #182 review.
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "不支持的语言") {
		t.Fatalf("response body should carry classified message, got %s", respBody)
	}
	if strings.Contains(respBody, "user:") {
		t.Fatalf("response body must not leak `user:` package prefix, got %s", respBody)
	}
}

// TestSetLanguageHandler_LongLanguageRejected pins the 64-char length cap
// applied at handler entry. The gate runs before any zap.String of the raw
// body so a multi-KB payload from a misbehaving client can't be amplified
// into the log pipeline before the supported-matrix gate rejects it (PR #182
// reviewer log-amplification finding).
func TestSetLanguageHandler_LongLanguageRejected(t *testing.T) {
	db := newFakeLangDB()
	r, _ := newLanguageHandlerHarness(t, db, newFakeLangCache())

	oversized := strings.Repeat("x", languageMaxLen+1)
	body := strings.NewReader(`{"language":"` + oversized + `"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400), body = %s", rec.Code, rec.Body.String())
	}
	if len(db.updates) != 0 {
		t.Fatalf("oversized language must short-circuit before DB, got %v", db.updates)
	}
}

// TestSetLanguageHandler_InternalErrorReturnsGenericMsg verifies the
// Internal=true contract for infra errors (DB outage, etc.): the wire
// response collapses to the shared internal copy ("服务器内部错误。")
// regardless of which err.server.user.* code the handler picked, and the
// wrapped `user: persist language: ...` / driver text never leaves the
// process. PR #182 reviewer P2 finding, adapted for Phase 2.1
// httperr.ResponseErrorL migration.
func TestSetLanguageHandler_InternalErrorReturnsGenericMsg(t *testing.T) {
	db := newFakeLangDB()
	db.updateErr = errors.New("driver: connection refused")
	r, _ := newLanguageHandlerHarness(t, db, newFakeLangCache())

	body := strings.NewReader(`{"language":"en-US"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400), body = %s", rec.Code, rec.Body.String())
	}
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "服务器内部错误") {
		t.Fatalf("response body should carry shared internal copy, got %s", respBody)
	}
	if !strings.Contains(respBody, "err.server.user.language_set_failed") {
		t.Fatalf("response body should still carry the specific error.code for ops dashboards, got %s", respBody)
	}
	if strings.Contains(respBody, "driver:") || strings.Contains(respBody, "user: persist") ||
		strings.Contains(respBody, "Failed to set language preference") {
		t.Fatalf("response body must not leak internal error text or source DefaultMessage, got %s", respBody)
	}
}

func TestSetLanguageHandler_MalformedJSON(t *testing.T) {
	db := newFakeLangDB()
	r, _ := newLanguageHandlerHarness(t, db, newFakeLangCache())

	body := bytes.NewReader([]byte(`{"language":`)) // truncated JSON
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400), body = %s", rec.Code, rec.Body.String())
	}
	if len(db.updates) != 0 {
		t.Fatalf("malformed JSON must not touch DB, got %v", db.updates)
	}
}

func TestSetLanguageHandler_Unauthorized(t *testing.T) {
	db := newFakeLangDB()
	u := &User{Log: log.NewTLog("user-test")}
	u.languageService = NewLanguageService(db, newFakeLangCache())
	r := wkhttp.New()
	// Wire the i18n renderer so the unauthorized path exercises the same
	// envelope production hits — every other handler test in this file goes
	// through newLanguageHandlerHarness which already wires it.
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	// No ctx.Set("uid", …) — simulates a request that somehow reached the
	// handler without AuthMiddleware. The handler's own uid guard must
	// reject it; SetLanguage must not even be called.
	r.Group("/v1/user").PUT("language", u.setLanguage)

	body := strings.NewReader(`{"language":"en-US"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400 from ResponseError), body = %s", rec.Code, rec.Body.String())
	}
	if len(db.updates) != 0 {
		t.Fatalf("unauthorized request must not touch DB, got %v", db.updates)
	}
}

// TestSetLanguageHandler_RequestContextPropagated guards that the handler
// forwards c.Request.Context() to LanguageService.SetLanguage — a deadline
// or cancellation on the inbound request must be honoured by the DB write
// path. Without this hook a slow caller-aborting client could leave the
// request blocked on MySQL/Redis.
// TestLoginUserDetailResp_LanguageEncoded guards the JSON contract that
// loginUserDetailResp emits the `language` field on every response that flows
// through it (login / current / register-then-auto-login). A struct-level
// test rather than an HTTP integration one because all the /v1/user/current
// integration tests in this package are t.Skip'd behind issue #17's
// migration gate — a struct-level assertion runs in CI today and guards
// against accidental removal of either the field or the json tag. Tracks
// the regression-guard ask in PR #182 review.
func TestLoginUserDetailResp_LanguageEncoded(t *testing.T) {
	cases := []struct {
		name string
		lang string
	}{
		{"populated", "zh-CN"},
		{"empty_means_unset", ""}, // must still be present in JSON, not omitempty
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(&loginUserDetailResp{UID: "u1", Language: tc.lang})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got map[string]interface{}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if _, present := got["language"]; !present {
				t.Fatalf("language field missing from JSON: %s", string(b))
			}
			if got["language"] != tc.lang {
				t.Fatalf("language = %v, want %q", got["language"], tc.lang)
			}
		})
	}
}

// TestLanguageService_PersistsCanonicalForm pins the contract that
// LanguageService normalises mixed-case / underscore inputs to the canonical
// BCP 47 form before persisting. Without this, a client sending "ZH_cn" today
// and "zh-CN" tomorrow would write two different DB values for the same
// preference. PR #182 reviewer asked for explicit coverage of this path.
func TestLanguageService_PersistsCanonicalForm(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"underscore", "zh_CN", "zh-CN"},
		{"mixed_case", "ZH-cn", "zh-CN"},
		{"upper_en", "EN-us", "en-US"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeLangDB()
			c := newFakeLangCache()
			svc := NewLanguageService(db, c)
			if err := svc.SetLanguage(context.Background(), "u1", tc.in); err != nil {
				t.Fatalf("SetLanguage(%q): %v", tc.in, err)
			}
			if got := db.updates["u1"]; got != tc.want {
				t.Fatalf("db value = %q, want canonical %q (input was %q)", got, tc.want, tc.in)
			}
		})
	}
}

func TestSetLanguageHandler_RequestContextPropagated(t *testing.T) {
	db := newFakeLangDB()
	c := newFakeLangCache()
	u := &User{Log: log.NewTLog("user-test")}
	u.languageService = NewLanguageService(db, c)

	r := wkhttp.New()
	r.Group("/v1/user").PUT("language", func(ctx *wkhttp.Context) {
		ctx.Set("uid", testHandlerUID)
		cancelled, cancel := context.WithCancel(ctx.Request.Context())
		cancel()
		ctx.Request = ctx.Request.WithContext(cancelled)
		u.setLanguage(ctx)
	})

	body := strings.NewReader(`{"language":"en-US"}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/user/language", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400 because cancelled), body = %s", rec.Code, rec.Body.String())
	}
	if len(db.updates) != 0 {
		t.Fatalf("cancelled context must abort before DB write, got %v", db.updates)
	}
}
