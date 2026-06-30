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
	"github.com/Mininglamp-OSS/octo-server/pkg/stickersig"
	redis "github.com/go-redis/redis"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
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

// setStickerHandleRequired upserts the admin-tunable enforcement policy
// (system_setting sticker.handle_required) and reloads the shared snapshot so the
// handler sees it immediately. Enforcement now lives in system_setting (DB), not
// an env var, so tests write the row rather than t.Setenv.
func setStickerHandleRequired(t *testing.T, ctx *config.Context, required bool) {
	t.Helper()
	v := "0"
	if required {
		v = "1"
	}
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("sticker", "handle_required", v, "bool").Exec()
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

// validStickerHandle mints the HMAC upload handle that /v1/file/upload would
// have returned for this user's upload of `path`. TestMain sets OCTO_MASTER_KEY,
// so handle signing is active and add() requires it: a shape-valid path with no
// (or a wrong) handle is refused — see TestSticker_AddRejectsForgedTailMatchPath.
func validStickerHandle(t *testing.T, path string) string {
	t.Helper()
	h, ok := stickersig.Sign(testutil.UID, path)
	require.True(t, ok, "OCTO_MASTER_KEY must be set in TestMain so Sign mints a handle")
	return h
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
		"handle":      validStickerHandle(t, validStickerPath("abc.png")),
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

func TestSticker_UpdatePartialAndListSortOrder(t *testing.T) {
	route, _, _ := setupSticker(t)

	addA := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":        validStickerPath("older.png"),
		"format":      "png",
		"placeholder": "[旧]",
		"handle":      validStickerHandle(t, validStickerPath("older.png")),
	})
	require.Equal(t, http.StatusOK, addA.Code)
	olderID := parseJSON(t, addA)["sticker_id"].(string)

	addB := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":        validStickerPath("newer.png"),
		"format":      "png",
		"placeholder": "[新]",
		"handle":      validStickerHandle(t, validStickerPath("newer.png")),
	})
	require.Equal(t, http.StatusOK, addB.Code)
	newerID := parseJSON(t, addB)["sticker_id"].(string)

	updateA := doRequest(t, route, "PUT", "/v1/sticker/user/"+olderID, map[string]interface{}{
		"sort": 5,
	})
	require.Equal(t, http.StatusOK, updateA.Code, "body: %s", updateA.Body.String())
	updatedA := parseJSON(t, updateA)
	assert.Equal(t, "[旧]", updatedA["placeholder"], "omitted placeholder must keep the old value")
	assert.Equal(t, float64(5), updatedA["sort"])

	updateB := doRequest(t, route, "PUT", "/v1/sticker/user/"+newerID, map[string]interface{}{
		"placeholder": "",
		"sort":        10,
	})
	require.Equal(t, http.StatusOK, updateB.Code, "body: %s", updateB.Body.String())
	updatedB := parseJSON(t, updateB)
	assert.Equal(t, defaultStickerPlaceholder, updatedB["placeholder"], "empty placeholder restores the default")
	assert.Equal(t, float64(10), updatedB["sort"])

	w := doRequest(t, route, "GET", "/v1/sticker/user", nil)
	require.Equal(t, http.StatusOK, w.Code)
	body := parseJSON(t, w)
	list := body["list"].([]interface{})
	require.Len(t, list, 2)
	first := list[0].(map[string]interface{})
	second := list[1].(map[string]interface{})
	assert.Equal(t, olderID, first["sticker_id"], "sort ASC must outrank legacy id DESC ordering")
	assert.Equal(t, float64(5), first["sort"])
	assert.Equal(t, newerID, second["sticker_id"])
	assert.Equal(t, float64(10), second["sort"])
}

func TestSticker_UpdateOwnershipAndValidation(t *testing.T) {
	route, _, f := setupSticker(t)

	other := &StickerModel{
		StickerID:   util.GenerUUID(),
		UID:         "other-uid",
		Path:        "file/preview/sticker/other-uid/x.png",
		Placeholder: "[别人的]",
		Format:      "png",
		Status:      1,
	}
	require.NoError(t, f.db.insert(other))

	wOther := doRequest(t, route, "PUT", "/v1/sticker/user/"+other.StickerID, map[string]interface{}{
		"placeholder": "[改]",
		"sort":        1,
	})
	assertStickerErrorCode(t, wOther, "err.server.sticker.not_found")

	stillThere, err := f.db.queryByID(other.StickerID)
	require.NoError(t, err)
	require.NotNil(t, stillThere)
	assert.Equal(t, "[别人的]", stillThere.Placeholder)

	add := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("mine.png"), "format": "png",
		"handle": validStickerHandle(t, validStickerPath("mine.png")),
	})
	require.Equal(t, http.StatusOK, add.Code)
	mineID := parseJSON(t, add)["sticker_id"].(string)

	wBadSort := doRequest(t, route, "PUT", "/v1/sticker/user/"+mineID, map[string]interface{}{"sort": -1})
	assertStickerErrorCode(t, wBadSort, "err.server.sticker.request_invalid")

	wLongPlaceholder := doRequest(t, route, "PUT", "/v1/sticker/user/"+mineID, map[string]interface{}{
		"placeholder": string([]rune("一二三四五六七八九十")) + string(make([]rune, maxStickerPlaceholderLen)),
	})
	assertStickerErrorCode(t, wLongPlaceholder, "err.server.sticker.request_invalid")
}

func TestSticker_ShortcodeKeywordsCreateListAndUpdate(t *testing.T) {
	route, _, _ := setupSticker(t)

	add := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":        validStickerPath("meta.png"),
		"format":      "png",
		"placeholder": "[笑哭]",
		"handle":      validStickerHandle(t, validStickerPath("meta.png")),
		"shortcode":   "XiaoKu",
		"keywords":    []string{"笑哭", "哈哈", "笑哭", " "},
	})
	require.Equal(t, http.StatusOK, add.Code, "body: %s", add.Body.String())
	ab := parseJSON(t, add)
	id := ab["sticker_id"].(string)
	assert.Equal(t, "xiaoku", ab["shortcode"])
	assert.Equal(t, []interface{}{"笑哭", "哈哈"}, ab["keywords"])

	list := parseJSON(t, doRequest(t, route, "GET", "/v1/sticker/user", nil))["list"].([]interface{})
	require.Len(t, list, 1)
	item := list[0].(map[string]interface{})
	assert.Equal(t, "xiaoku", item["shortcode"])
	assert.Equal(t, []interface{}{"笑哭", "哈哈"}, item["keywords"])

	update := doRequest(t, route, "PUT", "/v1/sticker/user/"+id, map[string]interface{}{
		"shortcode": "rofl_1",
		"keywords":  []string{"ROFL", "笑哭"},
	})
	require.Equal(t, http.StatusOK, update.Code, "body: %s", update.Body.String())
	ub := parseJSON(t, update)
	assert.Equal(t, "rofl_1", ub["shortcode"])
	assert.Equal(t, []interface{}{"ROFL", "笑哭"}, ub["keywords"])
}

func TestSticker_ShortcodeValidationAndConflict(t *testing.T) {
	route, _, _ := setupSticker(t)

	invalid := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":      validStickerPath("invalid.png"),
		"format":    "png",
		"handle":    validStickerHandle(t, validStickerPath("invalid.png")),
		"shortcode": "笑哭",
	})
	assertStickerErrorCode(t, invalid, "err.server.sticker.shortcode_invalid")

	tooManyKeywords := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":     validStickerPath("keywords.png"),
		"format":   "png",
		"handle":   validStickerHandle(t, validStickerPath("keywords.png")),
		"keywords": []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"},
	})
	assertStickerErrorCode(t, tooManyKeywords, "err.server.sticker.keywords_invalid")

	addA := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":      validStickerPath("a.png"),
		"format":    "png",
		"handle":    validStickerHandle(t, validStickerPath("a.png")),
		"shortcode": "rofl",
	})
	require.Equal(t, http.StatusOK, addA.Code, "body: %s", addA.Body.String())
	idA := parseJSON(t, addA)["sticker_id"].(string)

	addDup := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":      validStickerPath("b.png"),
		"format":    "png",
		"handle":    validStickerHandle(t, validStickerPath("b.png")),
		"shortcode": "rofl",
	})
	assertStickerErrorCode(t, addDup, "err.server.sticker.shortcode_conflict")

	del := doRequest(t, route, "DELETE", "/v1/sticker/user/"+idA, nil)
	require.Equal(t, http.StatusOK, del.Code)

	addAfterDelete := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":      validStickerPath("c.png"),
		"format":    "png",
		"handle":    validStickerHandle(t, validStickerPath("c.png")),
		"shortcode": "rofl",
	})
	assert.Equal(t, http.StatusOK, addAfterDelete.Code, "deleted sticker must release shortcode; body: %s", addAfterDelete.Body.String())
}

func TestSticker_ShortcodeConflictScopedToUID(t *testing.T) {
	route, ctx, _ := setupSticker(t)

	_, err := ctx.DB().InsertInto("sticker").
		Columns("sticker_id", "uid", "path", "placeholder", "format", "shortcode", "keywords", "status").
		Values(util.GenerUUID(), "other-uid", "file/preview/sticker/other-uid/x.png", "[别人的]", "png", "shared", `["other"]`, 1).
		Exec()
	require.NoError(t, err)

	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]interface{}{
		"path":      validStickerPath("mine-shared.png"),
		"format":    "png",
		"handle":    validStickerHandle(t, validStickerPath("mine-shared.png")),
		"shortcode": "shared",
	})
	assert.Equal(t, http.StatusOK, w.Code, "different users may reuse the same shortcode; body: %s", w.Body.String())
}

func TestSticker_RegisterMetrics(t *testing.T) {
	route, ctx, _ := setupSticker(t)
	setStickerHandleRequired(t, ctx, true)

	beforeMissing := promtestutil.ToFloat64(metricStickerRegisterTotal.WithLabelValues("missing_handle"))
	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("nohandle-metric.png"), "format": "png",
	})
	assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
	assert.Equal(t, beforeMissing+1, promtestutil.ToFloat64(metricStickerRegisterTotal.WithLabelValues("missing_handle")))

	path := validStickerPath("metric-ok.png")
	beforeSuccess := promtestutil.ToFloat64(metricStickerRegisterTotal.WithLabelValues("success"))
	ok := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": path, "format": "png", "handle": validStickerHandle(t, path),
	})
	require.Equal(t, http.StatusOK, ok.Code, "body: %s", ok.Body.String())
	assert.Equal(t, beforeSuccess+1, promtestutil.ToFloat64(metricStickerRegisterTotal.WithLabelValues("success")))
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
		"handle": validStickerHandle(t, validStickerPath("a.png")),
	})
	assert.Equal(t, http.StatusOK, w1.Code)

	w2 := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("b.png"), "format": "png",
		"handle": validStickerHandle(t, validStickerPath("b.png")),
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
		"handle": validStickerHandle(t, validStickerPath("a.png")),
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

	absURL := "https://cdn.example.com/dm-bucket/sticker/" + testutil.UID + "/abc123.gif?X-Amz-Signature=deadbeef"
	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path":   absURL,
		"format": "gif",
		"handle": validStickerHandle(t, absURL),
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

	// Mint paths + handles on the test goroutine (validStickerHandle asserts via
	// require, which must not run off the main test goroutine).
	paths := make([]string, concurrency)
	handles := make([]string, concurrency)
	for i := 0; i < concurrency; i++ {
		paths[i] = validStickerPath(fmt.Sprintf("c%d.png", i))
		handles[i] = validStickerHandle(t, paths[i])
	}

	statuses := make([]int, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
				"path":   paths[i],
				"format": "png",
				"handle": handles[i],
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

// TestSticker_AddRejectsForgedTailMatchPath is the core regression for the
// upload-handle (PR#508 follow-up). The pragmatic path-shape check accepts a
// ".../sticker/{uid}/x.ext" tail ANYWHERE in the key — including a chat-bucket
// object "chat/sticker/{uid}/x.gif" — which is its documented residual and would
// let a 100MB type=chat upload be re-registered as a sticker, dodging the 1MB +
// raster-only sticker upload contract. The handle closes it: the client cannot
// mint a valid handle for an object it didn't upload via type=sticker, so the
// registration is refused even though the shape passes.
//
// NOTE: this strong guarantee holds only under enforcement (required=true). In
// compatibility mode (required=false) a missing handle is allowed through (see
// TestSticker_CompatModeAllowsMissingHandle), so the cross-type bypass defense
// degrades during the rollout window — that is the intended, reversible trade-off.
func TestSticker_AddRejectsForgedTailMatchPath(t *testing.T) {
	route, ctx, _ := setupSticker(t)
	setStickerHandleRequired(t, ctx, true)

	forged := "file/preview/chat/sticker/" + testutil.UID + "/x.gif"
	// Sanity: the forged path DOES pass the shape check (the residual)...
	require.True(t, validateStickerPath(forged, testutil.UID, "gif"),
		"precondition: forged tail-match path must pass the shape check")

	// ...yet without a server-minted handle it is refused.
	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": forged, "format": "gif",
	})
	assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
}

// TestSticker_AddRejectsMissingHandle: under enforcement (required=true) a
// perfectly shaped sticker path is still refused when no handle accompanies it
// (handle signing is active in tests).
func TestSticker_AddRejectsMissingHandle(t *testing.T) {
	route, ctx, _ := setupSticker(t)
	setStickerHandleRequired(t, ctx, true)

	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("nohandle.png"), "format": "png",
	})
	assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
}

// TestSticker_CompatModeAllowsMissingHandle: in compatibility mode (the default,
// required=false) a shape-valid path with NO handle is allowed through so older
// clients keep working during the rollout. The missing handle is recorded
// (compat_missing metric); behavior here is the allow + persisted-sticker outcome.
func TestSticker_CompatModeAllowsMissingHandle(t *testing.T) {
	// Default posture: setupSticker cleans system_setting, so
	// sticker.handle_required is absent → StickerHandleRequired() is false.
	route, _, _ := setupSticker(t)

	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("legacy.png"), "format": "png",
	})
	require.Equal(t, http.StatusOK, w.Code, "compat mode must allow a missing handle: %s", w.Body.String())
	body := parseJSON(t, w)
	assert.NotEmpty(t, body["sticker_id"], "allowed registration must return the new sticker")
}

// TestSticker_AddRejectsTamperedHandle: a tampered/forged handle (non-empty but
// not verifiable — here, one minted for a DIFFERENT object) is rejected
// regardless of the required policy — invalid handles are never compat-allowed.
// Asserted under BOTH modes.
func TestSticker_AddRejectsTamperedHandle(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required=%v", required), func(t *testing.T) {
			route, ctx, _ := setupSticker(t)
			setStickerHandleRequired(t, ctx, required)

			w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
				"path":   validStickerPath("real.png"),
				"format": "png",
				"handle": validStickerHandle(t, validStickerPath("other.png")),
			})
			assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
		})
	}
}

// TestSticker_RequiredWithoutCapability_FailsClosed pins the config-conflict
// contract (brief acceptance): when the policy demands a handle
// (sticker.handle_required=true) but the server has NO signing capability
// (OCTO_MASTER_KEY absent), registration must be REJECTED (fail-closed), not
// silently allowed on the path-shape check. The server boots with a key (common
// setup needs it to encrypt the IM private key); stickersig reads the env live,
// so unsetting it here makes Enabled() false at request time — exactly the
// misconfig: enforcement on, capability gone.
func TestSticker_RequiredWithoutCapability_FailsClosed(t *testing.T) {
	route, ctx, _ := setupSticker(t)
	setStickerHandleRequired(t, ctx, true)
	t.Setenv("OCTO_MASTER_KEY", "") // drop the signing capability at request time
	require.False(t, stickersig.Enabled(), "precondition: no signing capability")

	// A shape-valid path (here without a handle; a handle would be equally
	// unverifiable) must be refused rather than allowed through.
	w := doRequest(t, route, "POST", "/v1/sticker/user", map[string]string{
		"path": validStickerPath("nokey.png"), "format": "png",
	})
	assertStickerErrorCode(t, w, "err.server.sticker.request_invalid")
}
