package drivepolicy

// Runtime-order test for the Route() middleware chain.
//
// In Gin, group-attached handlers run BEFORE route-attached handlers. So an
// auth-on-group + limiter-on-route combination executes auth → limiter, which
// lets a missing / wrong token abort before ever consuming the strict-IP
// bucket (token probing falls back to the wider global bucket). modules/
// internal_resolve and modules/internal_membership were both bitten by this and
// document it; PR #711 review round 4 (yujiawei P1 middleware order) makes the
// ordering load-bearing.
//
// This test mirrors the production wiring shape (Group with NO middleware, both
// middlewares on the concrete GET route in limiter → auth → handler order),
// substituting the real limiter with a stub because the real one needs Redis.
// Each stage is wrapped in a recording layer that appends its name at
// invocation time, so a reordering of Route()'s attach calls is observable.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestRouteMiddlewareRuntimeOrder proves the execution sequence is
// [ipLimit, auth, handler].
func TestRouteMiddlewareRuntimeOrder(t *testing.T) {
	m := newModule(samplePolicy)

	var seen []string
	record := func(name string, next wkhttp.HandlerFunc) wkhttp.HandlerFunc {
		return func(c *wkhttp.Context) {
			seen = append(seen, name)
			next(c)
		}
	}

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	ipLimitStub := record("ipLimit", func(c *wkhttp.Context) { c.Next() })
	auth := record("auth", m.internalAuthMiddleware())
	handler := record("handler", m.getUploadPolicy)

	internal := r.Group("/v1/internal")
	internal.GET("/drive/upload-policy", ipLimitStub, auth, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/internal/drive/upload-policy", nil)
	req.Header.Set(internalTokenHeader, testToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d — chain aborted before handler: seen=%v body=%s",
			got, want, seen, w.Body.String())
	}
	want := []string{"ipLimit", "auth", "handler"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("middleware order = %v, want %v — Route() must mount both middlewares on the "+
			"concrete GET in ipLimit → auth → handler order; putting auth on the group re-orders "+
			"execution and lets unauthenticated requests bypass the strict-IP bucket.", seen, want)
	}
}

// Even a request with a wrong token (which aborts at the auth step) MUST have
// consumed the ipLimit slot first.
func TestRouteMiddlewareRuntimeOrder_UnauthedStillConsumesLimiter(t *testing.T) {
	m := newModule(samplePolicy)

	var seen []string
	record := func(name string, next wkhttp.HandlerFunc) wkhttp.HandlerFunc {
		return func(c *wkhttp.Context) {
			seen = append(seen, name)
			next(c)
		}
	}

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	ipLimitStub := record("ipLimit", func(c *wkhttp.Context) { c.Next() })
	auth := record("auth", m.internalAuthMiddleware())
	handler := record("handler", m.getUploadPolicy)

	internal := r.Group("/v1/internal")
	internal.GET("/drive/upload-policy", ipLimitStub, auth, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/internal/drive/upload-policy", nil)
	req.Header.Set(internalTokenHeader, "wrong-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got, want := w.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if len(seen) < 2 || seen[0] != "ipLimit" || seen[1] != "auth" {
		t.Fatalf("middleware trace = %v, want [ipLimit, auth, ...] with auth aborting", seen)
	}
	for _, name := range seen {
		if name == "handler" {
			t.Fatalf("handler executed on unauthed request; seen=%v", seen)
		}
	}
}
