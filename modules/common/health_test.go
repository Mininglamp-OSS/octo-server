package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/require"
)

type fakeReadinessChecker struct {
	calls  int32
	result readinessResult
	check  func(context.Context) readinessResult
}

func (f *fakeReadinessChecker) Check(ctx context.Context) readinessResult {
	atomic.AddInt32(&f.calls, 1)
	if f.check != nil {
		return f.check(ctx)
	}
	return f.result
}

func TestHealthIsPureLiveness(t *testing.T) {
	checker := &fakeReadinessChecker{
		result: readinessResult{
			Status:       healthStatusDown,
			Dependencies: map[string]string{"db": healthStatusDown, "redis": healthStatusDown},
			Errors:       map[string]error{"db": errors.New("should not be called")},
		},
	}
	cn := &Common{readinessChecker: checker}
	r := wkhttp.New()
	r.GET("/v1/health", cn.health)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"status":"up","db":"up","redis":"up"}`, w.Body.String())
	require.Equal(t, int32(0), atomic.LoadInt32(&checker.calls), "liveness must not call dependency readiness checker")
}

func TestReadyReturnsSafeDependencyStatus(t *testing.T) {
	checker := &fakeReadinessChecker{
		result: readinessResult{
			Status:       healthStatusDown,
			Dependencies: map[string]string{"db": healthStatusDown, "redis": healthStatusUp},
			Errors:       map[string]error{"db": errors.New("mysql dsn contains secret password")},
		},
	}
	cn := &Common{readinessChecker: checker}
	r := wkhttp.New()
	r.GET("/v1/ready", cn.ready)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.JSONEq(t, `{"status":"down","dependencies":{"db":"down","redis":"up"}}`, w.Body.String())
	require.NotContains(t, strings.ToLower(w.Body.String()), "secret")
	require.NotContains(t, strings.ToLower(w.Body.String()), "password")
	require.Equal(t, int32(1), atomic.LoadInt32(&checker.calls))
}

func TestReadyBoundsCheckerWithTimeout(t *testing.T) {
	checker := &fakeReadinessChecker{
		check: func(ctx context.Context) readinessResult {
			deadline, ok := ctx.Deadline()
			require.True(t, ok, "readiness checker must receive a bounded context")
			require.LessOrEqual(t, time.Until(deadline), readinessProbeTimeout)
			<-ctx.Done()
			return readinessResult{
				Status:       healthStatusDown,
				Dependencies: map[string]string{"db": healthStatusDown, "redis": healthStatusDown},
				Errors:       map[string]error{"timeout": ctx.Err()},
			}
		},
	}
	cn := &Common{readinessChecker: checker}
	r := wkhttp.New()
	r.GET("/v1/ready", cn.ready)

	started := time.Now()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Less(t, time.Since(started), time.Second, "readiness timeout must stay bounded")
	require.Equal(t, int32(1), atomic.LoadInt32(&checker.calls))
}

func TestReadyReturnsWhenCheckerIgnoresContext(t *testing.T) {
	checker := &fakeReadinessChecker{
		check: func(context.Context) readinessResult {
			time.Sleep(readinessProbeTimeout * 2)
			return readinessResult{Status: healthStatusUp}
		},
	}
	cn := &Common{readinessChecker: checker}
	r := wkhttp.New()
	r.GET("/v1/ready", cn.ready)

	started := time.Now()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Less(t, time.Since(started), 750*time.Millisecond, "readiness must return on its own deadline even if a checker ignores ctx")
	require.JSONEq(t, `{"status":"down","dependencies":{"db":"down","redis":"down"}}`, w.Body.String())
	require.Equal(t, int32(1), atomic.LoadInt32(&checker.calls))
}

func TestReadinessRedisOptionsUseBoundedPoolTimeout(t *testing.T) {
	opts := readinessRedisOptions(config.New())

	require.Equal(t, readinessRedisPoolTimeout, opts.PoolTimeout)
	require.Less(t, opts.PoolTimeout, readinessProbeTimeout)
}
