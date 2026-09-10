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

func TestManagerMFAEndpointsUseSeparateStrictIPRateLimit(t *testing.T) {
	const clientIP = "198.51.100.43"

	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	ctx.SetHttpRoute(route)

	loginKey := "ratelimit:strict:" + managerLoginRateLimitTag + ":" + clientIP
	mfaKey := "ratelimit:strict:" + managerMFARateLimitTag + ":" + clientIP
	require.NoError(t, ctx.GetRedisConn().Del(loginKey))
	require.NoError(t, ctx.GetRedisConn().Del(mfaKey))
	t.Cleanup(func() {
		assert.NoError(t, ctx.GetRedisConn().Del(loginKey))
		assert.NoError(t, ctx.GetRedisConn().Del(mfaKey))
	})

	(&Manager{ctx: ctx}).Route(route)
	do := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{"))
		req.Header.Set("Content-Type", "application/json")
		setPublicIPForUserTest(req, clientIP)
		route.ServeHTTP(w, req)
		return w
	}

	for i := 0; i < managerMFARateLimitBurst; i++ {
		w := do("/v1/manager/login/send")
		require.Equal(t, http.StatusBadRequest, w.Code,
			"MFA request %d unexpectedly rejected: %s", i+1, w.Body.String())
	}
	w := do("/v1/manager/login/verify")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Equal(t, "strict:"+managerMFARateLimitTag, w.Header().Get("X-RateLimit-Scope"))

	// Exhausting the MFA bucket must not consume the password-login bucket.
	login := do("/v1/manager/login")
	assert.Equal(t, http.StatusBadRequest, login.Code, login.Body.String())
}
