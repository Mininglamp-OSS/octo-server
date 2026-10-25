package internal_membership

// Behaviour tests against the PRODUCTION Route().
//
// These drive m.Route() itself over a real config.Context, so the routing table,
// the shared strict-IP bucket (real Redis) and the auth gate are the shipped ones.
// They never touch MySQL: the store is the in-package stub, and every path here is
// rejected before the handler. The database-backed qualification and activation
// coverage lives in modules/project's HTTP test, against the same production Route.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

// newRedisBackedContext builds the same config.Context the module gets at boot.
// config.New() defaults RedisAddr to 127.0.0.1:6379, the instance the repository's
// other rate-limit tests use. A missing Redis would make the limiter fail open, so
// the bucket-dependent tests also touch a key directly to fail loudly instead of
// passing for the wrong reason.
func newRedisBackedContext(t *testing.T) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	return config.NewContext(cfg)
}

func newProductionRoute(t *testing.T, ctx *config.Context, m *Module) *wkhttp.WKHttp {
	t.Helper()
	m.ctx = ctx
	if m.Log == nil {
		m.Log = log.NewTLog("InternalMembershipRouteTest")
	}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	m.Route(r)
	return r
}

func doRouteRequest(t *testing.T, r *wkhttp.WKHttp, w http.ResponseWriter,
	method, path, token, clientIP string, body []byte) {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	// The limiter resolves the caller from this header (see octo-lib ClientIP), so
	// pinning it makes the Redis key deterministic without a real socket.
	req.Header.Set("X-Real-Ip", clientIP)
	r.ServeHTTP(w, req)
}

// deadlineRecorder is an httptest.ResponseRecorder that also accepts
// SetReadDeadline, so it can observe boundBodyReadTime through gin's Unwrap chain.
//
// This proves the read boundary at RUNTIME rather than by a source grep: the
// recorder only sees a deadline if the deadline handler actually ran, and it can
// only run before the limiter/auth aborted the request because gin stops the
// chain at the first handler that aborts.
type deadlineRecorder struct {
	httptest.ResponseRecorder
	deadlines int
}

func (w *deadlineRecorder) SetReadDeadline(time.Time) error {
	w.deadlines++
	return nil
}

// bucketKey is the Redis key the shared strict-IP limiter uses.
func bucketKey(clientIP string) string {
	return "ratelimit:strict:" + membershipRateLimitTag + ":" + clientIP
}

func resetBucket(t *testing.T, ctx *config.Context, clientIP string) {
	t.Helper()
	require.NoError(t, ctx.GetRedisConn().Del(bucketKey(clientIP)),
		"the strict limiter tests need the shared Redis instance; without it the limiter "+
			"fails open and a 429 expectation would be meaningless")
	t.Cleanup(func() { _ = ctx.GetRedisConn().Del(bucketKey(clientIP)) })
}

// TestProductionRoutesApplyLimiterBeforeAuthWithReadDeadlineOnEveryRejection
// covers all shipped routes: a bad token consumes the strict bucket (401),
// the following valid-token request is refused by the bucket (429), and both
// rejects already carry the body read deadline. The handler is never reached.
func TestProductionRoutesApplyLimiterBeforeAuthWithReadDeadlineOnEveryRejection(t *testing.T) {
	ctx := newRedisBackedContext(t)
	t.Setenv(envMembershipIPRPS, "0.000001")
	t.Setenv(envMembershipIPBurst, "1")

	store := &stubStore{}
	r := newProductionRoute(t, ctx, &Module{
		store: store, internalToken: testInternalToken, Log: log.NewTLog("RouteTest"),
	})

	routes := []struct {
		name   string
		method string
		path   string
		body   []byte
		ip     string
	}{
		{
			name: "epochs", method: http.MethodGet,
			path: epochsPath + "?space_id=space-a&project_ids=p1",
			ip:   "198.51.100.21",
		},
		{
			name: "verify", method: http.MethodPost,
			path: verifyPath,
			body: []byte(`{"space_id":"space-a","project_id":"p1","uids":["u1"]}`),
			ip:   "198.51.100.22",
		},
		{
			name: "projects", method: http.MethodGet,
			path: projectsPath + "?space_id=space-a&uid=User-A",
			ip:   "198.51.100.23",
		},
		{
			name: "project members", method: http.MethodGet,
			path: projectMembershipsPath + "?space_id=space-a&project_id=p1",
			ip:   "198.51.100.26",
		},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			resetBucket(t, ctx, tc.ip)

			rejected401 := &deadlineRecorder{}
			doRouteRequest(t, r, rejected401, tc.method, tc.path,
				"definitely-the-wrong-token-value", tc.ip, tc.body)
			require.Equal(t, http.StatusUnauthorized, rejected401.Code, rejected401.Body.String())
			require.Equal(t, 1, rejected401.deadlines,
				"a request rejected by auth must still have the body read deadline installed")

			rejected429 := &deadlineRecorder{}
			doRouteRequest(t, r, rejected429, tc.method, tc.path,
				testInternalToken, tc.ip, tc.body)
			require.Equal(t, http.StatusTooManyRequests, rejected429.Code, rejected429.Body.String())
			require.Equal(t, "strict:"+membershipRateLimitTag,
				rejected429.Header().Get("X-RateLimit-Scope"))
			require.Equal(t, 1, rejected429.deadlines,
				"a request rejected by the strict limiter must still have the body read "+
					"deadline installed")
		})
	}

	require.Zero(t, store.projectCalls, "rejections must not reach the projects handler")
	require.Zero(t, store.epochCalls, "rejections must not reach the epochs handler")
	require.Zero(t, store.memberCalls, "rejections must not reach the verify handler")
}

// TestProductionRoutesShareOneStrictBucket pins that the endpoints draw from ONE
// quota rather than one each: a rejection on the epochs route has already spent
// the bucket the projects route uses.
func TestProductionRoutesShareOneStrictBucket(t *testing.T) {
	ctx := newRedisBackedContext(t)
	t.Setenv(envMembershipIPRPS, "0.000001")
	t.Setenv(envMembershipIPBurst, "1")
	const ip = "198.51.100.24"
	resetBucket(t, ctx, ip)

	store := &stubStore{epochs: map[string]int64{"p1": 1}}
	r := newProductionRoute(t, ctx, &Module{
		store: store, internalToken: testInternalToken, Log: log.NewTLog("RouteTest"),
	})

	first := httptest.NewRecorder()
	doRouteRequest(t, r, first, http.MethodGet,
		epochsPath+"?space_id=space-a&project_ids=p1",
		"wrong-token-wrong-token-wrong-token", ip, nil)
	require.Equal(t, http.StatusUnauthorized, first.Code, first.Body.String())

	second := httptest.NewRecorder()
	doRouteRequest(t, r, second, http.MethodGet,
		projectsPath+"?space_id=space-a&uid=User-A", testInternalToken, ip, nil)
	require.Equal(t, http.StatusTooManyRequests, second.Code, second.Body.String())
	require.Zero(t, store.projectCalls)
	require.Zero(t, store.epochCalls)
}

// TestProductionProjectsRouteIsDisabledWhenTheCapabilityIsOff drives the
// enablement gate through the shipped route: with New() refusing to enable (no
// token, or a token without the declared peer cache bound), the endpoint answers
// 401 even for a request presenting the configured token value.
func TestProductionProjectsRouteIsDisabledWhenTheCapabilityIsOff(t *testing.T) {
	cases := []struct {
		name  string
		token string
		bound string
	}{
		{"no token", "", ""},
		{"token without the declared peer cache bound", testInternalToken, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newRedisBackedContext(t)
			t.Setenv(MembershipInternalTokenEnv, tc.token)
			t.Setenv(PeerMaxCacheAgeEnv, tc.bound)
			gated := New(ctx)
			require.Empty(t, gated.internalToken,
				"the gate must leave the module unable to authenticate anyone")

			const ip = "198.51.100.25"
			resetBucket(t, ctx, ip)
			r := newProductionRoute(t, ctx, gated)
			w := httptest.NewRecorder()
			doRouteRequest(t, r, w, http.MethodGet,
				projectsPath+"?space_id=space-a&uid=User-A", testInternalToken, ip, nil)
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			require.Equal(t, errcode.ErrSharedTokenInvalid.ID, decodeError(t, w).Error.Code,
				"a disabled capability must be indistinguishable from a wrong token")
		})
	}
}
