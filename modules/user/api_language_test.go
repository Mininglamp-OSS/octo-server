package user

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
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
