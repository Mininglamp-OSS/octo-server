package sticker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	redis "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetUIDRateLimit clears the per-uid token-bucket keys (ratelimit:uid:{uid})
// so subsequent HTTP calls start from a full bucket. CleanAllTables does NOT
// clear Redis, so a burst of POSTs across tests could otherwise 429. Mirrors
// modules/category/api_test.go's resetUIDRateLimit.
func resetUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rdsClient := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rdsClient.Close()
	keys, err := rdsClient.Keys("ratelimit:uid:*").Result()
	if err == nil && len(keys) > 0 {
		_ = rdsClient.Del(keys...).Err()
	}
}

// newStickerTestServer wraps testutil.NewTestServer and injects the i18n
// ErrorRenderer onto the route, mirroring main.go at boot so httperr.ResponseErrorL
// renders the localized envelope rather than the legacy fallback.
func newStickerTestServer() (*server.Server, *config.Context) {
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return s, ctx
}

// setupSticker builds a fresh test route + handler, cleans tables, resets the
// rate-limit bucket, and reloads SystemSettings so the per-user quota starts at
// its code default (system_setting truncated → 100).
func setupSticker(t *testing.T) (*wkhttp.WKHttp, *config.Context, *Sticker) {
	t.Helper()
	s, ctx := newStickerTestServer()
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	resetUIDRateLimit(t, ctx)
	require.NoError(t, commonmod.EnsureSystemSettings(ctx).Reload())
	return s.GetRoute(), ctx, f
}

// setStickerQuota upserts the admin-tunable per-user cap and reloads the shared
// snapshot so the handler sees it immediately.
func setStickerQuota(t *testing.T, ctx *config.Context, n int) {
	t.Helper()
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("sticker", "user_max_count", strconv.Itoa(n), "int").Exec()
	require.NoError(t, err)
	require.NoError(t, commonmod.EnsureSystemSettings(ctx).Reload())
}

func doRequest(t *testing.T, route *wkhttp.WKHttp, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		reqBody = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest(method, path, reqBody)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	return w
}

func parseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	return result
}

func assertStickerErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	env := decodeErrEnvelope(t, w.Body.Bytes())
	if env.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q\nbody: %s", env.Error.Code, wantCode, w.Body.String())
	}
}

// TestSticker_ListEmpty is the issue #26 regression guard: an empty collection
// returns 200 {"list":[]} — never a 404.
func TestSticker_ListEmpty(t *testing.T) {
	route, _, _ := setupSticker(t)

	w := doRequest(t, route, "GET", "/v1/sticker/user", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	body := parseJSON(t, w)
	list, ok := body["list"].([]interface{})
	assert.True(t, ok, "response must carry a `list` array; got %s", w.Body.String())
	assert.Equal(t, 0, len(list))
}

func TestSticker_AddAndList(t *testing.T) {
	route, _, _ := setupSticker(t)

	add := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":        "file/preview/sticker/u/abc.png",
		"format":      "png",
		"placeholder": "[笑]",
	})
	assert.Equal(t, http.StatusOK, add.Code)
	ab := parseJSON(t, add)
	assert.NotEmpty(t, ab["sticker_id"])
	assert.Equal(t, "user", ab["category"])
	assert.Equal(t, "png", ab["format"])

	w := doRequest(t, route, "GET", "/v1/sticker/user", nil)
	body := parseJSON(t, w)
	list, ok := body["list"].([]interface{})
	require.True(t, ok)
	require.Equal(t, 1, len(list))
	item := list[0].(map[string]interface{})
	assert.Equal(t, "file/preview/sticker/u/abc.png", item["path"])
	assert.Equal(t, "user", item["category"])
	assert.Equal(t, "[笑]", item["placeholder"])
}

func TestSticker_AddFormatRejected(t *testing.T) {
	route, _, _ := setupSticker(t)

	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":   "file/preview/sticker/u/x.tiff",
		"format": "tiff",
	})
	assertStickerErrorCode(t, w, "err.server.sticker.format_unsupported")
}

func TestSticker_AddEmptyPathRejected(t *testing.T) {
	route, _, _ := setupSticker(t)

	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":   "",
		"format": "png",
	})
	assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
}

func TestSticker_QuotaExceeded(t *testing.T) {
	route, ctx, _ := setupSticker(t)
	setStickerQuota(t, ctx, 1)

	w1 := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": "file/preview/sticker/u/a.png", "format": "png",
	})
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": "file/preview/sticker/u/b.png", "format": "png",
	})
	env := decodeErrEnvelope(t, w2.Body.Bytes())
	assert.Equal(t, "err.server.sticker.quota_exceeded", env.Error.Code)
	assert.Equal(t, http.StatusConflict, env.Error.HTTPStatus)
	assert.Equal(t, float64(1), env.Error.Details["max"])
}

func TestSticker_DeleteOwnership(t *testing.T) {
	route, _, f := setupSticker(t)

	add := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": "file/preview/sticker/u/a.png", "format": "png",
	})
	require.Equal(t, http.StatusOK, add.Code)
	mineID := parseJSON(t, add)["sticker_id"].(string)

	// A sticker owned by a different user, inserted directly.
	other := &StickerModel{
		StickerID: util.GenerUUID(),
		UID:       "other-uid",
		Path:      "file/preview/sticker/o/x.png",
		Format:    "png",
		Status:    1,
	}
	require.NoError(t, f.db.insert(other))

	// Deleting someone else's sticker is reported as not-found and leaves it intact.
	wOther := doRequest(t, route, "DELETE", "/v1/sticker/user/"+other.StickerID, nil)
	assertStickerErrorCode(t, wOther, "err.server.sticker.not_found")
	stillThere, err := f.db.queryByID(other.StickerID)
	require.NoError(t, err)
	assert.NotNil(t, stillThere, "another user's sticker must not be deleted")

	// Deleting own sticker succeeds and removes it.
	wDel := doRequest(t, route, "DELETE", "/v1/sticker/user/"+mineID, nil)
	assert.Equal(t, http.StatusOK, wDel.Code)
	gone, err := f.db.queryByID(mineID)
	require.NoError(t, err)
	assert.Nil(t, gone)
}
