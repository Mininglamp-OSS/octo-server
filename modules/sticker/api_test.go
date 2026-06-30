package sticker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
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

// validStickerPath builds an object key shaped like the multipart uploader's
// output for the test login user (sticker/{uid}/<name>), so add() accepts it.
// add() validates that req.Path names THIS user's sticker upload, so test
// payloads must carry the real testutil.UID, not a placeholder segment.
func validStickerPath(name string) string {
	return "file/preview/sticker/" + testutil.UID + "/" + name
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
		"path":        validStickerPath("abc.png"),
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
	assert.Equal(t, validStickerPath("abc.png"), item["path"])
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
		"path": validStickerPath("a.png"), "format": "png",
	})
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("b.png"), "format": "png",
	})
	env := decodeErrEnvelope(t, w2.Body.Bytes())
	assert.Equal(t, "err.server.sticker.quota_exceeded", env.Error.Code)
	assert.Equal(t, http.StatusConflict, env.Error.HTTPStatus)
	assert.Equal(t, float64(1), env.Error.Details["max"])
}

func TestSticker_DeleteOwnership(t *testing.T) {
	route, _, f := setupSticker(t)

	add := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("a.png"), "format": "png",
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

// TestSticker_AddPathRejected guards the registration-path validation: a client
// must not be able to register a chat-bucket / other-user / external object, or
// a path whose extension contradicts the declared format, as a sticker — which
// would dodge the 1MB + raster-only upload contract enforced on the multipart
// route (PR#508 review). Format is valid in every case, so rejection is the
// path check, not the format check.
func TestSticker_AddPathRejected(t *testing.T) {
	route, _, _ := setupSticker(t)

	cases := []struct {
		name   string
		path   string
		format string
	}{
		{"external non-sticker url", "https://evil.example.com/avatar/x.gif", "gif"},
		{"other user's sticker key", "file/preview/sticker/99999/x.gif", "gif"},
		{"non-sticker bucket", "file/preview/chat/" + testutil.UID + "/x.gif", "gif"},
		{"extension contradicts format", "file/preview/sticker/" + testutil.UID + "/x.png", "gif"},
		{"nested extra segment", "file/preview/sticker/" + testutil.UID + "/sub/x.gif", "gif"},
		{"no extension", "file/preview/sticker/" + testutil.UID + "/x", "gif"},
		{"sticker prefix without uid", "file/preview/sticker/x.gif", "gif"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
				"path": tc.path, "format": tc.format,
			})
			assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
		})
	}
}

// TestSticker_AddAcceptsAbsoluteDownloadURL confirms the pragmatic prefix check
// passes a real storage download URL: absolute host, a bucket segment, the
// stable sticker/{uid}/<name>.<ext> tail, and a signing query string (which is
// stripped before matching).
func TestSticker_AddAcceptsAbsoluteDownloadURL(t *testing.T) {
	route, _, _ := setupSticker(t)

	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":   "https://cdn.example.com/dm-bucket/sticker/" + testutil.UID + "/abc123.gif?X-Amz-Signature=deadbeef",
		"format": "gif",
	})
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// TestSticker_AddConcurrentQuota fires more concurrent adds than the cap and
// asserts the per-user quota holds exactly — exactly `quota` succeed, the rest
// are quota-rejected, none 500. This exercises the user-row record lock
// (lockUserRowTx): the prior `count(*) ... FOR UPDATE` on the non-unique index
// took a gap lock that let concurrent first-adds both pass the count check and
// deadlock on insert. CleanAllTables wipes `user`, so seed the caller's row
// first; otherwise the lock degrades to a no-op and the test proves nothing.
func TestSticker_AddConcurrentQuota(t *testing.T) {
	route, ctx, f := setupSticker(t)

	_, err := ctx.DB().InsertBySql("INSERT INTO `user` (uid) VALUES (?)", testutil.UID).Exec()
	require.NoError(t, err)

	const quota = 3
	const concurrency = 10
	setStickerQuota(t, ctx, quota)

	statuses := make([]int, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
				"path":   validStickerPath(fmt.Sprintf("c%d.png", i)),
				"format": "png",
			})
			statuses[i] = w.Code
		}(i)
	}
	wg.Wait()

	var ok, rejected int
	for _, c := range statuses {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			// quota_exceeded is a 409 in the envelope but ResponseErrorL pins the
			// wire status to 400 (D14 compat); all paths here are valid, so the
			// only 400s are quota rejections.
			rejected++
		default:
			t.Fatalf("unexpected status %d (want 200 or 400) — a 500 means the quota guard deadlocked", c)
		}
	}
	assert.Equal(t, quota, ok, "exactly the quota many concurrent adds must succeed")
	assert.Equal(t, concurrency-quota, rejected, "the rest must be quota-rejected, not error out")

	// And the table must hold exactly `quota` live stickers — no over-admit.
	stickers, err := f.db.listByUID(testutil.UID)
	require.NoError(t, err)
	assert.Equal(t, quota, len(stickers))
}
