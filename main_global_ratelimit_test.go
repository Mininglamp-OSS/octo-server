package main

import (
	crand "crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

func TestGlobalRateLimitPermissionFactsSetNoStoreOn429(t *testing.T) {
	client := globalRateLimitTestRedis(t)
	route := libwkhttp.New()
	route.Use(globalRateLimitMiddleware(route, client, 0.000001, 1))

	type testRoute struct {
		name        string
		method      string
		routePath   string
		requestPath string
	}
	factRoutes := []testRoute{
		{
			name: "membership epochs", method: http.MethodGet,
			routePath: "/v1/internal/membership/epochs", requestPath: "/v1/internal/membership/epochs",
		},
		{
			name: "project membership verify", method: http.MethodPost,
			routePath: "/v1/internal/project-memberships/_verify", requestPath: "/v1/internal/project-memberships/_verify",
		},
		{
			name: "project lifecycle", method: http.MethodGet,
			routePath: "/v1/internal/projects/:project_id/lifecycle", requestPath: "/v1/internal/projects/project-a/lifecycle",
		},
	}
	hits := make(map[string]int)
	register := func(method, path string, handler libwkhttp.HandlerFunc) {
		switch method {
		case http.MethodGet:
			route.GET(path, handler)
		case http.MethodPost:
			route.POST(path, handler)
		default:
			t.Fatalf("unsupported test method %q", method)
		}
	}
	for _, tc := range factRoutes {
		tc := tc
		register(tc.method, tc.routePath, func(c *libwkhttp.Context) {
			hits[tc.name]++
			c.Header("Cache-Control", "no-store")
			c.Status(http.StatusNoContent)
		})
	}

	const ordinaryName = "ordinary project route"
	register(http.MethodGet, "/v1/internal/projects/:project_id/summary", func(c *libwkhttp.Context) {
		hits[ordinaryName]++
		c.Header("Cache-Control", "public, max-age=60")
		c.Status(http.StatusNoContent)
	})

	excludedRoutes := []testRoute{
		{name: "ping", method: http.MethodGet, routePath: "/v1/ping", requestPath: "/v1/ping"},
		{name: "health", method: http.MethodGet, routePath: "/v1/health", requestPath: "/v1/health"},
		{name: "bot heartbeat", method: http.MethodPost, routePath: "/v1/bot/heartbeat", requestPath: "/v1/bot/heartbeat"},
		{name: "bot register", method: http.MethodPost, routePath: "/v1/bot/register", requestPath: "/v1/bot/register"},
	}
	for _, tc := range excludedRoutes {
		tc := tc
		register(tc.method, tc.routePath, func(c *libwkhttp.Context) {
			hits[tc.name]++
			c.Status(http.StatusNoContent)
		})
	}

	request := func(method, path, clientIP string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Real-Ip", clientIP)
		recorder := httptest.NewRecorder()
		route.ServeHTTP(recorder, req)
		return recorder
	}

	for _, tc := range factRoutes {
		t.Run(tc.name, func(t *testing.T) {
			clientIP := globalRateLimitTestIP(t)
			first := request(tc.method, tc.requestPath, clientIP)
			require.Equal(t, http.StatusNoContent, first.Code)
			require.Equal(t, "no-store", first.Header().Get("Cache-Control"))

			rejected := request(tc.method, tc.requestPath, clientIP)
			require.Equal(t, http.StatusTooManyRequests, rejected.Code)
			require.Equal(t, "no-store", rejected.Header().Get("Cache-Control"))
			require.Equal(t, 1, hits[tc.name], "a rejected request must not enter the endpoint")
		})
	}

	t.Run(ordinaryName, func(t *testing.T) {
		clientIP := globalRateLimitTestIP(t)
		first := request(http.MethodGet, "/v1/internal/projects/project-a/summary", clientIP)
		require.Equal(t, http.StatusNoContent, first.Code)
		require.Equal(t, "public, max-age=60", first.Header().Get("Cache-Control"))

		rejected := request(http.MethodGet, "/v1/internal/projects/project-a/summary", clientIP)
		require.Equal(t, http.StatusTooManyRequests, rejected.Code)
		require.Empty(t, rejected.Header().Get("Cache-Control"))
		require.Equal(t, 1, hits[ordinaryName], "a rejected request must not enter the endpoint")
	})

	for _, tc := range excludedRoutes {
		t.Run("existing exclusion: "+tc.name, func(t *testing.T) {
			clientIP := globalRateLimitTestIP(t)
			for range 2 {
				response := request(tc.method, tc.requestPath, clientIP)
				require.Equal(t, http.StatusNoContent, response.Code)
			}
			require.Equal(t, 2, hits[tc.name])
		})
	}
}

func globalRateLimitTestRedis(t *testing.T) *rd.Client {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	client := rd.NewClient(&rd.Options{Addr: addr, DialTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping().Err(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("required Redis at %s is unavailable: %v", addr, err)
		}
		t.Skipf("Redis at %s is unavailable: %v", addr, err)
	}
	return client
}

func globalRateLimitTestIP(t *testing.T) string {
	t.Helper()
	var octets [4]byte
	_, err := crand.Read(octets[:])
	require.NoError(t, err)
	return net.IPv4(octets[0], octets[1], octets[2], octets[3]).String()
}
