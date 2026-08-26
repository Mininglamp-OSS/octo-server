package space

// PR-C acceptance evidence: every Space-membership failure on the B1/B2 routes
// answers through the localized error envelope, and it resolves the same Space
// the legacy middleware would.
//
// This twin exists only because the card catalog's read endpoints must not
// return the legacy raw-JSON shape while every other response they can produce
// is localized. That makes two properties worth pinning: the failures really
// are localized (otherwise the twin has no reason to exist), and the membership
// rule really is unchanged (otherwise the twin is a second, divergent
// definition of which Space a route resolves).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
)

// setupLocalizedRouter mirrors setupRouter, so any behavioural difference
// between the two middlewares shows up as a difference between the two suites
// rather than being hidden by different harnesses.
func setupLocalizedRouter(checker MembershipChecker) (*wkhttp.WKHttp, *InMemoryMembershipCache) {
	return newLocalizedRouter(checker, true)
}

// newLocalizedRouter builds the real wkhttp router with the production error
// renderer installed. A bare gin engine would fall back to wkhttp's default
// renderer, which emits a message but no `error` object — so the whole point of
// this middleware, the localized envelope, would go unasserted.
func newLocalizedRouter(checker MembershipChecker, authenticated bool) (*wkhttp.WKHttp, *InMemoryMembershipCache) {
	cache := NewInMemoryMembershipCache()
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	if authenticated {
		route.Use(func(c *wkhttp.Context) {
			c.Set("uid", "testuser")
			c.Set("name", "Test")
		})
	}
	route.Use(localizedSpaceMiddleware(checker, cache))
	route.GET("/test", func(c *wkhttp.Context) {
		spaceID, _ := c.Get("space_id")
		c.JSON(http.StatusOK, gin.H{"space_id": spaceID})
	})
	return route, cache
}

// localizedEnvelope decodes the shape httperr.ResponseErrorL writes. A legacy
// raw response has no `error` object, so its absence is the failure signal.
type localizedEnvelope struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Error  *struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func requireLocalizedEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var envelope localizedEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, recorder.Body.String())
	}
	if envelope.Error == nil {
		t.Fatalf("response is not on the localized envelope: %s", recorder.Body.String())
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q (body %s)",
			envelope.Error.Code, wantCode, recorder.Body.String())
	}
	if envelope.Msg == "" {
		t.Fatalf("localized envelope carried no message: %s", recorder.Body.String())
	}
}

func TestLocalizedSpaceMiddlewareRejectsANonMemberOnTheEnvelope(t *testing.T) {
	router, _ := setupLocalizedRouter(func(string, string) (bool, error) {
		return false, nil
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/test?space_id=space-1", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusOK {
		t.Fatalf("a non-member reached the handler: %s", recorder.Body.String())
	}
	requireLocalizedEnvelope(t, recorder, "err.shared.auth.forbidden")
}

func TestLocalizedSpaceMiddlewareReportsALookupFailureAsUnavailable(t *testing.T) {
	router, _ := setupLocalizedRouter(func(string, string) (bool, error) {
		return false, errors.New("db unavailable")
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/test?space_id=space-1", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code == http.StatusOK {
		t.Fatalf("a failed membership check reached the handler: %s", recorder.Body.String())
	}
	// An outage must not be reported as a permission decision — the caller
	// would stop retrying and an operator would look in the wrong place.
	requireLocalizedEnvelope(t, recorder, "err.shared.internal")
}

func TestLocalizedSpaceMiddlewareRequiresALoginBeforeCheckingMembership(t *testing.T) {
	checked := false
	router, _ := newLocalizedRouter(func(string, string) (bool, error) {
		checked = true
		return true, nil
	}, false)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/test?space_id=space-1", nil))
	if recorder.Code == http.StatusOK {
		t.Fatalf("an unauthenticated caller reached the handler: %s", recorder.Body.String())
	}
	if checked {
		t.Fatal("membership was checked for a caller with no uid")
	}
	requireLocalizedEnvelope(t, recorder, "err.shared.auth.required")
}

// The membership rule itself must be the legacy one, down to the header
// fallback, the pass-through when no Space is claimed, and the cache write.
// This is the half that keeps the twin from becoming a second definition.
func TestLocalizedSpaceMiddlewareResolvesTheSameSpaceAsTheLegacyOne(t *testing.T) {
	for _, target := range []struct {
		name   string
		url    string
		header string
	}{
		{"query parameter", "/test?space_id=space-1", ""},
		{"header fallback", "/test", "space-1"},
	} {
		t.Run(target.name, func(t *testing.T) {
			router, cache := setupLocalizedRouter(func(spaceID, uid string) (bool, error) {
				if spaceID != "space-1" || uid != "testuser" {
					t.Fatalf("membership asked about %q/%q", spaceID, uid)
				}
				return true, nil
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, target.url, nil)
			if target.header != "" {
				request.Header.Set("X-Space-ID", target.header)
			}
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("a member was rejected: %d %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				SpaceID string `json:"space_id"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.SpaceID != "space-1" {
				t.Fatalf("handler saw space_id = %q", body.SpaceID)
			}
			// The positive result is cached, exactly as the legacy middleware
			// does — a divergence here would mean two different TTL regimes for
			// one membership fact.
			if isMember, found := cache.Get("space-1", "testuser"); !found || !isMember {
				t.Fatalf("membership was not cached (found=%v member=%v)", found, isMember)
			}
		})
	}

	// No Space claimed at all is a pass-through, not a rejection: the catalog's
	// read endpoints answer with the public catalog in that case.
	router, _ := setupLocalizedRouter(func(string, string) (bool, error) {
		t.Fatal("membership was checked without a Space claim")
		return false, nil
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/test", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a request claiming no Space was rejected: %d %s", recorder.Code, recorder.Body.String())
	}
}

// A cached negative must reject without re-checking, the same way the legacy
// middleware trusts its cache — otherwise the twin quietly doubles the DB load
// of every rejected request.
func TestLocalizedSpaceMiddlewareTrustsACachedNegative(t *testing.T) {
	checks := 0
	router, _ := setupLocalizedRouter(func(string, string) (bool, error) {
		checks++
		return false, nil
	})
	for i := 0; i < 3; i++ {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/test?space_id=space-1", nil))
		if recorder.Code == http.StatusOK {
			t.Fatalf("attempt %d let a non-member through", i)
		}
	}
	if checks != 1 {
		t.Fatalf("membership was checked %d times, want one lookup plus two cache hits", checks)
	}
}
