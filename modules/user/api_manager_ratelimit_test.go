package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerLoginHasStrictIPRateLimit(t *testing.T) {
	const clientIP = "198.51.100.42"

	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	ctx.SetHttpRoute(route)

	rateLimitKey := "ratelimit:strict:" + managerLoginRateLimitTag + ":" + clientIP
	require.NoError(t, ctx.GetRedisConn().Del(rateLimitKey))
	t.Cleanup(func() {
		assert.NoError(t, ctx.GetRedisConn().Del(rateLimitKey))
	})

	// A malformed body keeps the request independent of MySQL while still
	// exercising the middleware chain registered by Manager.Route.
	(&Manager{ctx: ctx}).Route(route)
	doLogin := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewBufferString("{"))
		req.Header.Set("Content-Type", "application/json")
		setPublicIPForUserTest(req, clientIP)
		route.ServeHTTP(w, req)
		return w
	}

	for i := 0; i < managerLoginRateLimitBurst; i++ {
		w := doLogin()
		require.Equal(t, http.StatusBadRequest, w.Code, "request %d unexpectedly rejected: %s", i+1, w.Body.String())
	}

	w := doLogin()
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Equal(t, "strict:"+managerLoginRateLimitTag, w.Header().Get("X-RateLimit-Scope"))
}

// TestManagerTwoFactorEndpointsHaveOwnStrictIPRateLimit pins that the two
// second-factor endpoints are throttled, and that each has its OWN bucket.
//
// Sharing the sign-in bucket would make one ordinary sign-in cost two tokens and
// let a few mistyped codes exhaust the operator's budget for retrying the
// password — turning a hardening feature into a self-inflicted lockout.
func TestManagerTwoFactorEndpointsHaveOwnStrictIPRateLimit(t *testing.T) {
	const clientIP = "198.51.100.43"

	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	ctx.SetHttpRoute(route)
	(&Manager{ctx: ctx}).Route(route)

	for _, tc := range []struct {
		path  string
		tag   string
		burst int
	}{
		{"/v1/manager/login/verify", manager2FAVerifyRateLimitTag, managerLoginRateLimitBurst},
		{"/v1/manager/login/resend", manager2FAResendRateLimitTag, manager2FAResendRateLimitBurst},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			key := "ratelimit:strict:" + tc.tag + ":" + clientIP
			require.NoError(t, ctx.GetRedisConn().Del(key))
			t.Cleanup(func() { assert.NoError(t, ctx.GetRedisConn().Del(key)) })

			// A malformed body keeps the request off MySQL while still walking
			// the middleware chain registered by Manager.Route.
			do := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString("{"))
				req.Header.Set("Content-Type", "application/json")
				setPublicIPForUserTest(req, clientIP)
				route.ServeHTTP(w, req)
				return w
			}
			for i := 0; i < tc.burst; i++ {
				require.Equal(t, http.StatusBadRequest, do().Code, "request %d must reach the handler", i+1)
			}
			w := do()
			require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
			assert.Equal(t, "strict:"+tc.tag, w.Header().Get("X-RateLimit-Scope"))
		})
	}
}
