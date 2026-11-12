package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	redis "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

func TestGlobalRateLimitUsesVersionedErrorEnvelope(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() { _ = client.Close() })
	route := wkhttp.New()
	route.SetErrorRenderer(newVersionedErrorRenderer(i18n.DefaultLanguage))
	route.Use(globalRateLimitMiddleware(route, client, 0.01, 1))
	for _, path := range []string{"/v1/ratelimit-probe", "/v2/cli/ratelimit-probe"} {
		route.GET(path, func(c *wkhttp.Context) { c.Status(http.StatusNoContent) })
	}
	seed := uint64(time.Now().UnixNano())
	for i, tc := range []struct {
		path   string
		code   string
		legacy bool
	}{
		{"/v1/ratelimit-probe", "err.shared.rate.limited", true},
		{"/v2/cli/ratelimit-probe", "RATE_LIMITED", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ip := fmt.Sprintf("fd00:266:%x:%x:%x:%x::%x", seed>>48, seed>>32&0xffff, seed>>16&0xffff, seed&0xffff, i+1)
			key := "ratelimit:ip:" + ip
			t.Cleanup(func() { _ = client.Del(key).Err() })
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				req.RemoteAddr = "[" + ip + "]:1234"
				w := httptest.NewRecorder()
				route.ServeHTTP(w, req)
				return w
			}
			require.Equal(t, http.StatusNoContent, request().Code)
			w := request()
			require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			var inner struct {
				Code string `json:"code"`
			}
			require.NoError(t, json.Unmarshal(body["error"], &inner))
			require.Equal(t, tc.code, inner.Code)
			if tc.legacy {
				require.Contains(t, body, "msg")
			} else {
				require.NotContains(t, body, "msg")
				require.NotContains(t, body, "status")
			}
		})
	}
}
