package featuregate

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupManager(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	resetUIDRateLimit(t, ctx)
	return s.GetRoute(), ctx
}

// asSuperAdmin 把默认 testutil.Token 提升为 superAdmin 角色。
func asSuperAdmin(t *testing.T, ctx *config.Context) {
	t.Helper()
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
}

// resetUIDRateLimit 清 per-uid 令牌桶（SharedUIDRateLimiter 的桶不随 CleanAllTables 清）。
func resetUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	cli := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer cli.Close()
	if keys, err := cli.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = cli.Del(keys...).Err()
	}
}

func mgrReq(method, path string, body interface{}) *http.Request {
	var r *bytes.Reader
	if body != nil {
		r = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		r = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", testutil.Token)
	return req
}

func mgrDo(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestManager_RequiresSuperAdmin(t *testing.T) {
	handler, _ := setupManager(t) // 不提升为 superadmin
	key := uniqueKey("ft_")
	w := mgrDo(handler, mgrReq("PUT", "/v1/manager/featuregate/gates/"+key,
		map[string]interface{}{"mode": "on"}))
	assert.Equalf(t, http.StatusBadRequest, w.Code,
		"非 superadmin 应被拒（c.ResponseError → 400），body: %s", w.Body.String())
}

func TestManager_UpdateAndList(t *testing.T) {
	handler, ctx := setupManager(t)
	asSuperAdmin(t, ctx)
	key := uniqueKey("ft_")

	w := mgrDo(handler, mgrReq("PUT", "/v1/manager/featuregate/gates/"+key,
		map[string]interface{}{"mode": "whitelist", "percent": 0, "description": "试点"}))
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = mgrDo(handler, mgrReq("GET", "/v1/manager/featuregate/gates", nil))
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, key)
	assert.Contains(t, body, `"whitelist"`)
}

func TestManager_UpdateInvalid(t *testing.T) {
	handler, ctx := setupManager(t)
	asSuperAdmin(t, ctx)
	key := uniqueKey("ft_")

	for _, tc := range []struct {
		name string
		body map[string]interface{}
	}{
		{"非法 mode", map[string]interface{}{"mode": "bogus"}},
		{"percent 越界", map[string]interface{}{"mode": "percent", "percent": 101}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mgrDo(handler, mgrReq("PUT", "/v1/manager/featuregate/gates/"+key, tc.body))
			assert.Equalf(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		})
	}
}

func TestManager_UpdateRejectsLongKey(t *testing.T) {
	handler, ctx := setupManager(t)
	asSuperAdmin(t, ctx)
	longKey := strings.Repeat("k", maxKeyLen+1) // 超出 feature_key VARCHAR(64)
	w := mgrDo(handler, mgrReq("PUT", "/v1/manager/featuregate/gates/"+longKey,
		map[string]interface{}{"mode": "on"}))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "超长 key 应 400, body: %s", w.Body.String())
}

func TestManager_ScopeAddDelete(t *testing.T) {
	handler, ctx := setupManager(t)
	asSuperAdmin(t, ctx)
	key := uniqueKey("ft_")

	require.Equal(t, http.StatusOK, mgrDo(handler, mgrReq("PUT",
		"/v1/manager/featuregate/gates/"+key,
		map[string]interface{}{"mode": "whitelist"})).Code)

	// 加白名单（含幂等重复）
	for i := 0; i < 2; i++ {
		w := mgrDo(handler, mgrReq("POST", "/v1/manager/featuregate/gates/"+key+"/scopes",
			map[string]interface{}{"scope_type": "group", "scope_id": "g_pilot"}))
		require.Equalf(t, http.StatusOK, w.Code, "第 %d 次 add 应幂等成功，body: %s", i+1, w.Body.String())
	}

	// 列表应包含该白名单
	w := mgrDo(handler, mgrReq("GET", "/v1/manager/featuregate/gates", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "g_pilot")

	// 删除
	delPath := fmt.Sprintf("/v1/manager/featuregate/gates/%s/scopes/%s?scope_type=group", key, "g_pilot")
	require.Equal(t, http.StatusOK, mgrDo(handler, mgrReq("DELETE", delPath, nil)).Code)

	// 再删 → 404
	w = mgrDo(handler, mgrReq("DELETE", delPath, nil))
	assert.Equalf(t, http.StatusNotFound, w.Code, "删除不存在的条目应 404，body: %s", w.Body.String())

	// 列表不再包含
	w = mgrDo(handler, mgrReq("GET", "/v1/manager/featuregate/gates", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "g_pilot")
}
