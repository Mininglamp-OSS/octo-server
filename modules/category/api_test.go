package category

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	redis "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetUIDRateLimit clears the per-uid token-bucket keys
// (ratelimit:uid:{uid}) so subsequent HTTP calls in this test start from a
// full bucket.  Without this, tests that came earlier in the same go test
// binary will have consumed tokens, and a later high-burst test (e.g.
// TestCategory_CreateLimit, which makes 20+ category POSTs back-to-back)
// fails with HTTP 429 even though the per-test logic is correct.  See
// pkg/wkhttp/ratelimit_helper.go SharedUIDRateLimiter for the bucket key
// scheme.  Pattern mirrors modules/space/api_email_invite_public_test.go's
// resetSpaceInviteRateLimit.
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

// ---------- helpers ----------

func resetDefaultCategoryName() {
	_defaultCategoryNameOnce = sync.Once{}
	_defaultCategoryName = ""
}

// seedSpaceAndMember inserts a space and makes testutil.UID a member with given role.
func seedSpaceAndMember(t *testing.T, f *Category, spaceID string, role int) {
	_, err := f.db.session.InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "测试空间", testutil.UID, 1).Exec()
	assert.NoError(t, err)

	_, err = f.db.session.InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, role, 1).Exec()
	assert.NoError(t, err)
}

// seedGroup inserts a group into the `group` table and adds testutil.UID as a member.
func seedGroup(t *testing.T, f *Category, groupNo, spaceID string) {
	_, err := f.db.session.InsertBySql("INSERT INTO `group` (group_no, name, creator, status, space_id) VALUES (?, ?, ?, ?, ?)",
		groupNo, "测试群组", testutil.UID, 1, spaceID).Exec()
	assert.NoError(t, err)

	_, err = f.db.session.InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status").
		Values(groupNo, testutil.UID, 0, 0, 1).Exec()
	assert.NoError(t, err)
}

// doRequest builds and executes an authenticated HTTP request against the test router.
func doRequest(t *testing.T, route *wkhttp.WKHttp, method, path string, body interface{}) *httptest.ResponseRecorder {
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		reqBody = bytes.NewReader(nil)
	}

	w := httptest.NewRecorder()
	req, err := http.NewRequest(method, path, reqBody)
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	return w
}

// parseJSONArray parses the response body as a JSON array.
func parseJSONArray(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	var result []map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &result)
	assert.NoError(t, err)
	return result
}

// parseJSON parses the response body as a JSON object.
func parseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	var result map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &result)
	assert.NoError(t, err)
	return result
}

// createCategory is a convenience to POST /v1/spaces/:space_id/categories.
func createCategory(t *testing.T, route *wkhttp.WKHttp, spaceID, name string) *httptest.ResponseRecorder {
	return doRequest(t, route, "POST", "/v1/spaces/"+spaceID+"/categories", map[string]string{"name": name})
}

// ---------- Happy Path Tests ----------

func TestCategory_Create(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-create-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	w := createCategory(t, s.GetRoute(), spaceID, "工作")
	assert.Equal(t, http.StatusOK, w.Code)

	body := parseJSON(t, w)
	assert.Equal(t, "工作", body["name"])
	assert.NotNil(t, body["category_id"])
	assert.Equal(t, float64(0), body["sort"])
	assert.NotNil(t, body["groups"])
}

func TestCategory_List(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-list-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create two categories
	w1 := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusOK, w1.Code)
	cat1 := parseJSON(t, w1)

	w2 := createCategory(t, route, spaceID, "生活")
	assert.Equal(t, http.StatusOK, w2.Code)

	// create a group and assign it to category 1
	groupNo := "group-list-001"
	seedGroup(t, f, groupNo, spaceID)

	catID := cat1["category_id"].(string)
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusOK, wm.Code)

	// create another group (uncategorized)
	groupNo2 := "group-list-002"
	seedGroup(t, f, groupNo2, spaceID)

	// list categories
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)

	cats := parseJSONArray(t, wl)
	// should have 3 entries: 工作, 生活, 默认分组(default)
	assert.Equal(t, 3, len(cats))

	// last entry is 默认分组 (now with a real ID)
	assert.NotNil(t, cats[2]["category_id"])
	assert.Equal(t, defaultCategoryNameFallback, cats[2]["name"])

	// 工作 category should have 1 group
	workGroups := cats[0]["groups"].([]interface{})
	assert.Equal(t, 1, len(workGroups))

	// 默认分组 should have 1 group
	uncatGroups := cats[2]["groups"].([]interface{})
	assert.Equal(t, 1, len(uncatGroups))
}

func TestCategory_Update(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-update-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create a category
	wc := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// update category name
	wu := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/"+catID, map[string]string{"name": "工作（更新）"})
	assert.Equal(t, http.StatusOK, wu.Code)

	// verify via DB
	updated, err := f.db.queryCategoryByID(catID)
	assert.NoError(t, err)
	assert.Equal(t, "工作（更新）", updated.Name)
}

func TestCategory_Delete(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-delete-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create a category
	wc := createCategory(t, route, spaceID, "待删除")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// assign a group to this category
	groupNo := "group-delete-001"
	seedGroup(t, f, groupNo, spaceID)
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusOK, wm.Code)

	// delete the category
	wd := doRequest(t, route, "DELETE", "/v1/spaces/"+spaceID+"/categories/"+catID, nil)
	assert.Equal(t, http.StatusOK, wd.Code)

	// verify category is deleted (status=2, not returned by query)
	deleted, err := f.db.queryCategoryByID(catID)
	assert.NoError(t, err)
	assert.Nil(t, deleted) // queryCategoryByID filters status=1

	// verify group's category_id is cleared
	setting, err := f.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, setting)
	assert.Nil(t, setting.CategoryID) // category_id should be nil after delete

	// 删除分组应同时取消关注分组下的群（退订语义，前端已提示用户）。
	// 该 ext 行在 delete 之前并不存在——moveGroupToCategory 不会预先 seed
	// user_conversation_ext，而是 delete 中 UnfollowGroupsTx 的 upsert 写出来的。
	// 若将来改成"不再为仅退订的群创建 ext 行"（例如改成软标记），需同步调整这条断言。
	var groupUnfollowed int
	_, err = f.db.session.SelectBySql(
		"SELECT group_unfollowed FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&groupUnfollowed)
	assert.NoError(t, err)
	assert.Equal(t, 1, groupUnfollowed)
}

// TestCategory_DeleteUnfollowsContents 验证删除分组时同步取消关注分组下的
// 群（含 thread 级联）与 DM —— 前端提示「分组下的所有会话将取消关注」对应
// 的服务端行为。
func TestCategory_DeleteUnfollowsContents(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-delete-cascade"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// 1. 建一个分组，挂一个群进去
	wc := createCategory(t, route, spaceID, "待退订")
	assert.Equal(t, http.StatusOK, wc.Code)
	catID := parseJSON(t, wc)["category_id"].(string)

	groupNo := "group-cascade-001"
	seedGroup(t, f, groupNo, spaceID)
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusOK, wm.Code)

	// 2. 直接在 user_conversation_ext 里塞一行该群的 thread ext，
	// 以及一行 dm_category_id 指向当前分组的 DM ext。
	threadID := groupNo + "____abc12345"
	_, err = f.db.session.InsertBySql(
		"INSERT INTO user_conversation_ext (uid, space_id, target_type, target_id) VALUES (?, ?, 5, ?)",
		testutil.UID, spaceID, threadID,
	).Exec()
	assert.NoError(t, err)

	dmPeer := "peer-cascade-001"
	_, err = f.db.session.InsertBySql(
		"INSERT INTO user_conversation_ext (uid, space_id, target_type, target_id, followed_dm, dm_category_id) VALUES (?, ?, 1, ?, 1, ?)",
		testutil.UID, spaceID, dmPeer, catID,
	).Exec()
	assert.NoError(t, err)

	// 记录删除前的 follow_version 以验证 bump。
	versionDB := convext.NewFollowVersionDB(ctx)
	beforeVer, err := versionDB.Get(testutil.UID, spaceID)
	assert.NoError(t, err)

	// 3. 删除分组
	wd := doRequest(t, route, "DELETE", "/v1/spaces/"+spaceID+"/categories/"+catID, nil)
	assert.Equal(t, http.StatusOK, wd.Code)

	// 4a. 群取消关注：group_unfollowed=1
	var groupUnfollowed int
	_, err = f.db.session.SelectBySql(
		"SELECT group_unfollowed FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&groupUnfollowed)
	assert.NoError(t, err)
	assert.Equal(t, 1, groupUnfollowed)

	// 4b. thread ext 行被级联删除
	var threadCount int
	_, err = f.db.session.SelectBySql(
		"SELECT COUNT(*) FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=5 AND target_id=?",
		testutil.UID, spaceID, threadID,
	).Load(&threadCount)
	assert.NoError(t, err)
	assert.Equal(t, 0, threadCount)

	// 4c. DM ext 行被删除
	var dmCount int
	_, err = f.db.session.SelectBySql(
		"SELECT COUNT(*) FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=1 AND target_id=?",
		testutil.UID, spaceID, dmPeer,
	).Load(&dmCount)
	assert.NoError(t, err)
	assert.Equal(t, 0, dmCount)

	// 4d. follow_version 至少 +1，让客户端能感知到关注集合变化
	afterVer, err := versionDB.Get(testutil.UID, spaceID)
	assert.NoError(t, err)
	assert.Greater(t, afterVer, beforeVer)
}

func TestCategory_Sort(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-sort-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create 3 categories
	wc1 := createCategory(t, route, spaceID, "A")
	assert.Equal(t, http.StatusOK, wc1.Code)
	cat1 := parseJSON(t, wc1)

	wc2 := createCategory(t, route, spaceID, "B")
	assert.Equal(t, http.StatusOK, wc2.Code)
	cat2 := parseJSON(t, wc2)

	wc3 := createCategory(t, route, spaceID, "C")
	assert.Equal(t, http.StatusOK, wc3.Code)
	cat3 := parseJSON(t, wc3)

	catID1 := cat1["category_id"].(string)
	catID2 := cat2["category_id"].(string)
	catID3 := cat3["category_id"].(string)

	// reorder: C, A, B
	ws := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{catID3, catID1, catID2},
	})
	assert.Equal(t, http.StatusOK, ws.Code)

	// verify sort order via list
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)
	cats := parseJSONArray(t, wl)

	assert.Equal(t, 3, len(cats))
	// first named category should be C (sort=0)
	assert.Equal(t, "C", cats[0]["name"])
	// second should be A (sort=1)
	assert.Equal(t, "A", cats[1]["name"])
	// third should be B (sort=2)
	assert.Equal(t, "B", cats[2]["name"])
}

func TestCategory_MoveGroupToCategory(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-move-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create a category
	wc := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// create a group
	groupNo := "group-move-001"
	seedGroup(t, f, groupNo, spaceID)

	// move group into category
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusOK, wm.Code)

	// verify setting
	setting, err := f.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, setting)
	assert.NotNil(t, setting.CategoryID)
	assert.Equal(t, catID, *setting.CategoryID)

	// move group out of category (empty category_id)
	wm2 := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": "",
	})
	assert.Equal(t, http.StatusOK, wm2.Code)

	// verify setting - category_id should be nil
	setting2, err := f.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, setting2)
	assert.Nil(t, setting2.CategoryID)
}

// TestCategory_MoveGroupOutOfCategory_ClearsAutoFollowThreads is the
// regression test for issue #151 review #3 (yujiawei).  When a user moves a
// group out of any category, the auto_follow_threads flag on the
// user_conversation_ext row (which the new sidebar materialization may have
// set to 1) must be cleared in the same transaction.  Without this cleanup,
// selectEligibleForFanoutTx would keep this user eligible for OnThreadCreated
// fan-out — the read side (buildFollowItems) drops the group because
// CategoryID is now nil, but the write side only checks auto_follow_threads.
//
// Repro before fix:
//  1. group g placed in category c, no ext row yet (default-followed).
//  2. /v1/sidebar/sync follow tab materializes (uid, space, g) with
//     auto_follow_threads=1, group_unfollowed=0.
//  3. User moves g out of category via PUT /v1/groups/g/category {"":""}.
//  4. ext row still has auto_follow_threads=1 — fan-out continues.
//
// After fix the move-out branch in api.go calls ClearAutoFollowThreadsTx
// in the same tx, restoring the read/write contract.
func TestCategory_MoveGroupOutOfCategory_ClearsAutoFollowThreads(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	spaceID := "space-move-clr-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// 1. Set up: category + group + group-in-category.
	wc := createCategory(t, route, spaceID, "工作")
	require.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	groupNo := "group-move-clr-001"
	seedGroup(t, f, groupNo, spaceID)

	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	require.Equal(t, http.StatusOK, wm.Code)

	// 2. Simulate the sidebar materialization step (would normally fire on
	//    /v1/sidebar/sync).  Insert ext row with auto_follow_threads=1,
	//    matching what MaterializeDefaultFollowedGroups writes.
	_, err = f.db.session.InsertBySql(
		"INSERT INTO user_conversation_ext (uid, space_id, target_type, target_id, group_unfollowed, auto_follow_threads) "+
			"VALUES (?, ?, 2, ?, 0, 1)",
		testutil.UID, spaceID, groupNo,
	).Exec()
	require.NoError(t, err, "seed materialized ext row")

	// Precondition sanity check.
	var preAutoFollow int
	_, err = f.db.session.SelectBySql(
		"SELECT auto_follow_threads FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&preAutoFollow)
	require.NoError(t, err)
	require.Equal(t, 1, preAutoFollow, "precondition: row is materialized auto_follow_threads=1")

	// 3. Move group OUT of category.
	wm2 := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": "",
	})
	require.Equal(t, http.StatusOK, wm2.Code)

	// 4. Assert auto_follow_threads is now 0 — the actual regression fix.
	var postAutoFollow int
	_, err = f.db.session.SelectBySql(
		"SELECT auto_follow_threads FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&postAutoFollow)
	require.NoError(t, err)
	assert.Equal(t, 0, postAutoFollow,
		"auto_follow_threads must be cleared after move-out (issue #151 review #3); "+
			"otherwise selectEligibleForFanoutTx would still target this user")

	// 5. Other flags MUST be preserved — uncategorize is NOT a full unfollow.
	var groupUnfollowed int
	_, err = f.db.session.SelectBySql(
		"SELECT group_unfollowed FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&groupUnfollowed)
	require.NoError(t, err)
	assert.Equal(t, 0, groupUnfollowed,
		"group_unfollowed must NOT be set — uncategorize ≠ explicit unfollow; "+
			"the cleanup only revokes auto-subscribe to NEW threads, not all subscriptions")
}

// TestCategory_MoveGroupBetweenCategories_PreservesAutoFollowThreads pins the
// non-regression: moving a group from category A to category B preserves the
// implicit follow, so auto_follow_threads stays 1.  Without this guard, a
// future change might over-eagerly clear in every move and break the
// "default-followed across category-to-category move" contract.
func TestCategory_MoveGroupBetweenCategories_PreservesAutoFollowThreads(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	spaceID := "space-move-keep-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// Two categories, one group, materialized ext row.
	wcA := createCategory(t, route, spaceID, "工作A")
	require.Equal(t, http.StatusOK, wcA.Code)
	catA := parseJSON(t, wcA)["category_id"].(string)
	wcB := createCategory(t, route, spaceID, "工作B")
	require.Equal(t, http.StatusOK, wcB.Code)
	catB := parseJSON(t, wcB)["category_id"].(string)

	groupNo := "group-move-keep-001"
	seedGroup(t, f, groupNo, spaceID)
	require.Equal(t, http.StatusOK, doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": catA}).Code)
	_, err = f.db.session.InsertBySql(
		"INSERT INTO user_conversation_ext (uid, space_id, target_type, target_id, group_unfollowed, auto_follow_threads) "+
			"VALUES (?, ?, 2, ?, 0, 1)",
		testutil.UID, spaceID, groupNo,
	).Exec()
	require.NoError(t, err)

	// Move A → B.
	w := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": catB})
	require.Equal(t, http.StatusOK, w.Code)

	var autoFollow int
	_, err = f.db.session.SelectBySql(
		"SELECT auto_follow_threads FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&autoFollow)
	require.NoError(t, err)
	assert.Equal(t, 1, autoFollow,
		"category A→B move must NOT clear auto_follow_threads — the group is "+
			"still in the follow tab, just under a different category")
}

// TestCategory_MoveGroupBackIntoCategory_RestoresAutoFollowThreads pins
// issue #151 review #4 (an9xyz) symptom #1: a default-followed group that
// has been materialized and then moved out must have auto_follow_threads
// restored to 1 when it is moved back into any category.  Otherwise the
// sidebar materialization branch skips the existing groupExts entry,
// the group reappears in the follow tab via buildFollowItems, but
// selectEligibleForFanoutTx still excludes the user (=0) — phantom missing
// fan-out.
func TestCategory_MoveGroupBackIntoCategory_RestoresAutoFollowThreads(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	spaceID := "space-move-back-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	wc := createCategory(t, route, spaceID, "工作")
	require.Equal(t, http.StatusOK, wc.Code)
	catID := parseJSON(t, wc)["category_id"].(string)

	groupNo := "group-move-back-001"
	seedGroup(t, f, groupNo, spaceID)

	// Cycle: move into category → simulate sidebar materialization → move
	// out (clears auto_follow_threads) → move back into the SAME category.
	require.Equal(t, http.StatusOK, doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": catID}).Code)
	_, err = f.db.session.InsertBySql(
		"INSERT INTO user_conversation_ext (uid, space_id, target_type, target_id, group_unfollowed, auto_follow_threads) "+
			"VALUES (?, ?, 2, ?, 0, 1)",
		testutil.UID, spaceID, groupNo,
	).Exec()
	require.NoError(t, err, "simulate sidebar materialization")
	require.Equal(t, http.StatusOK, doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": ""}).Code,
		"move out — must clear auto_follow_threads")

	var afterOut int
	_, err = f.db.session.SelectBySql(
		"SELECT auto_follow_threads FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&afterOut)
	require.NoError(t, err)
	require.Equal(t, 0, afterOut, "precondition: move-out cleared auto_follow_threads")

	// Move BACK into the same category.  Sidebar materialization would skip
	// this row (groupExts hit), so the move-in path itself must restore =1.
	require.Equal(t, http.StatusOK, doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": catID}).Code)

	var afterIn int
	_, err = f.db.session.SelectBySql(
		"SELECT auto_follow_threads FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&afterIn)
	require.NoError(t, err)
	assert.Equal(t, 1, afterIn,
		"move-in must restore auto_follow_threads=1 on an existing ext row "+
			"(issue #151 review #4 symptom #1); sidebar materialization would "+
			"otherwise skip the existing row, leaving OnThreadCreated fan-out "+
			"disabled even though the group is back in the follow tab")
}

// TestCategory_MoveFirstTimeIntoCategory_NoOpRestore ensures the move-in
// restore call is a safe no-op when no ext row has been materialized yet —
// sidebar materialization at the next /v1/sidebar/sync creates the row with
// auto_follow_threads=1 anyway, and the move-in handler must not
// short-circuit any subsequent paths or write inappropriate rows.
func TestCategory_MoveFirstTimeIntoCategory_NoOpRestore(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	spaceID := "space-move-first-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	wc := createCategory(t, route, spaceID, "工作")
	require.Equal(t, http.StatusOK, wc.Code)
	catID := parseJSON(t, wc)["category_id"].(string)

	groupNo := "group-move-first-001"
	seedGroup(t, f, groupNo, spaceID)

	// Move into category for the first time — no ext row exists yet.
	require.Equal(t, http.StatusOK, doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": catID}).Code)

	// No row should have been written by the move-in path — sidebar's
	// MaterializeDefaultFollowedGroups is the canonical materialization site
	// and stays solely responsible for creating ext rows.  Letting the
	// move-in path INSERT here would race with the unique key and silently
	// pick whichever flag set ends up committing first.
	var count int
	_, err = f.db.session.SelectBySql(
		"SELECT COUNT(*) FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, spaceID, groupNo,
	).Load(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count,
		"first-time move-in must NOT create an ext row — sidebar materialization "+
			"is the single materialization site; RestoreAutoFollowThreadsTx is "+
			"strictly UPDATE")
}

// ---------- Validation / Error Tests ----------

func TestCategory_CreateLimit(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-limit-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create 20 categories
	for i := 0; i < 20; i++ {
		w := createCategory(t, route, spaceID, fmt.Sprintf("Cat-%d", i))
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// 21st should fail
	w := createCategory(t, route, spaceID, "Cat-20")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.limit_exceeded")
}

func TestCategory_CreateEmptyName(t *testing.T) {
	s, ctx := newCategoryTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-emptyname-001"
	f := New(ctx)
	seedSpaceAndMember(t, f, spaceID, 0)

	w := createCategory(t, s.GetRoute(), spaceID, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.request_invalid")
}

func TestCategory_UpdateNotOwner(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-updnotowner-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// insert a category owned by another user
	otherCatID := "other-cat-001"
	err = f.db.insertCategory(&CategoryModel{
		CategoryID: otherCatID,
		SpaceID:    spaceID,
		UID:        "other-user",
		Name:       "别人的分类",
		Sort:       0,
		Status:     1,
	})
	assert.NoError(t, err)

	// try to update it
	w := doRequest(t, s.GetRoute(), "PUT", "/v1/spaces/"+spaceID+"/categories/"+otherCatID, map[string]string{"name": "我要改"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.permission_denied")
}

func TestCategory_DeleteNotOwner(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-delnotowner-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// insert a category owned by another user
	otherCatID := "other-cat-002"
	err = f.db.insertCategory(&CategoryModel{
		CategoryID: otherCatID,
		SpaceID:    spaceID,
		UID:        "other-user",
		Name:       "别人的分类",
		Sort:       0,
		Status:     1,
	})
	assert.NoError(t, err)

	// try to delete it
	w := doRequest(t, s.GetRoute(), "DELETE", "/v1/spaces/"+spaceID+"/categories/"+otherCatID, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.permission_denied")
}

func TestCategory_MoveGroupNotMember(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-movenotmember-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create a group but do NOT add testutil.UID as a member
	groupNo := "group-notmember-001"
	_, err = f.db.session.InsertBySql("INSERT INTO `group` (group_no, name, creator, status, space_id) VALUES (?, ?, ?, ?, ?)",
		groupNo, "测试群组", "other-user", 1, spaceID).Exec()
	assert.NoError(t, err)

	// create a category first
	wc := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// try to move the group (testutil.UID is not a group member)
	w := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.group_member_required")
}

func TestCategory_NonSpaceMember(t *testing.T) {
	s, ctx := newCategoryTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// create a space but do NOT add testutil.UID as a member
	spaceID := "space-notmember-001"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "别人的空间", "other-user", 1).Exec()
	assert.NoError(t, err)

	route := s.GetRoute()

	// try to create a category
	w := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.space_member_required")

	// try to list categories
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusBadRequest, wl.Code)
	assertCategoryErrorCode(t, wl, "err.server.category.space_member_required")
}

func TestCategory_UpdateNotFound(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-updnotfound-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// try to update a non-existent category
	w := doRequest(t, s.GetRoute(), "PUT", "/v1/spaces/"+spaceID+"/categories/nonexistent-cat", map[string]string{"name": "不存在"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.not_found")
}

func TestCategory_DeleteNotFound(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-delnotfound-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// try to delete a non-existent category
	w := doRequest(t, s.GetRoute(), "DELETE", "/v1/spaces/"+spaceID+"/categories/nonexistent-cat", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.not_found")
}

func TestCategory_UpdateEmptyName(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-updempty-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create a category
	wc := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// try to update with empty name
	wu := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/"+catID, map[string]string{"name": ""})
	assert.Equal(t, http.StatusBadRequest, wu.Code)
	assertCategoryErrorCode(t, wu, "err.server.category.request_invalid")
}

func TestCategory_SortEmptyList(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-sortempty-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// try to sort with empty list
	ws := doRequest(t, s.GetRoute(), "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{},
	})
	assert.Equal(t, http.StatusBadRequest, ws.Code)
	assertCategoryErrorCode(t, ws, "err.server.category.request_invalid")
}

func TestCategory_SortUnknownCategory(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-sortunknown-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create 1 category
	wc := createCategory(t, route, spaceID, "A")
	assert.Equal(t, http.StatusOK, wc.Code)

	// try to sort with the right count but unknown ID
	ws := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{"fake-id-001"},
	})
	assert.Equal(t, http.StatusBadRequest, ws.Code)
	assertCategoryErrorCode(t, ws, "err.server.category.not_found")
}

func TestCategory_SortNonSpaceMember(t *testing.T) {
	s, ctx := newCategoryTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// create a space but do NOT add testutil.UID as a member
	spaceID := "space-sortnotmember-001"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "别人的空间", "other-user", 1).Exec()
	assert.NoError(t, err)

	ws := doRequest(t, s.GetRoute(), "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{"some-id"},
	})
	assert.Equal(t, http.StatusBadRequest, ws.Code)
	assertCategoryErrorCode(t, ws, "err.server.category.space_member_required")
}

func TestCategory_MoveGroupCategoryNotFound(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-movenotfound-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	groupNo := "group-movenotfound-001"
	seedGroup(t, f, groupNo, spaceID)

	// try to move group to non-existent category
	w := doRequest(t, s.GetRoute(), "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": "nonexistent-cat-id",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.not_found")
}

func TestCategory_MoveGroupCategoryNotOwner(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-movenotowner-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	groupNo := "group-movenotowner-001"
	seedGroup(t, f, groupNo, spaceID)

	// insert a category owned by another user
	otherCatID := "other-cat-move-001"
	err = f.db.insertCategory(&CategoryModel{
		CategoryID: otherCatID,
		SpaceID:    spaceID,
		UID:        "other-user",
		Name:       "别人的分类",
		Sort:       0,
		Status:     1,
	})
	assert.NoError(t, err)

	// try to move group to other user's category
	w := doRequest(t, s.GetRoute(), "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": otherCatID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.permission_denied")
}

func TestCategory_MoveGroupCrossSpace(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// create two spaces
	spaceID1 := "space-cross-001"
	spaceID2 := "space-cross-002"
	seedSpaceAndMember(t, f, spaceID1, 0)
	seedSpaceAndMember(t, f, spaceID2, 0)
	route := s.GetRoute()

	// create category in space 1
	wc := createCategory(t, route, spaceID1, "分类A")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// create group in space 2
	groupNo := "group-cross-001"
	seedGroup(t, f, groupNo, spaceID2)

	// try to move group from space 2 into category from space 1
	w := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.space_mismatch")
}

// TestCategory_MoveExternalGroupToCurrentSpaceCategory is the regression test
// for issue #191. A user who is an external member of a group (the group lives
// in another Space) follows/categorizes it into a category under the user's own
// current Space. The space-consistency check must use the user's source Space
// (group_member.source_space_id), not the group's owning Space, otherwise the
// request is wrongly rejected with "群组和分类不在同一空间".
//
// It also asserts the follow_version is bumped under the user's source Space
// (not the group's owning Space) so the group surfaces in the follow tab the
// sidebar queries for the user's current Space.
func TestCategory_MoveExternalGroupToCurrentSpaceCategory(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	require.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	// userSpace = where the current user lives and owns categories.
	// groupSpace = the external group's owning Space (a different Space).
	userSpace := "space-ext191-user"
	groupSpace := "space-ext191-group"
	seedSpaceAndMember(t, f, userSpace, 0)
	route := s.GetRoute()

	// category lives under the user's current Space.
	wc := createCategory(t, route, userSpace, "外部群关注")
	require.Equal(t, http.StatusOK, wc.Code)
	catID := parseJSON(t, wc)["category_id"].(string)

	// group lives in groupSpace; the current user joined as an external member
	// whose source_space_id points back to their own (user) Space.
	groupNo := "group-ext191-001"
	_, err = f.db.session.InsertBySql("INSERT INTO `group` (group_no, name, creator, status, space_id) VALUES (?, ?, ?, ?, ?)",
		groupNo, "外部群", "owner-uid", 1, groupSpace).Exec()
	require.NoError(t, err)
	_, err = f.db.session.InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status", "is_external", "source_space_id").
		Values(groupNo, testutil.UID, 0, 0, 1, 1, userSpace).Exec()
	require.NoError(t, err)

	// categorizing the external group into the user-Space category must succeed.
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	require.Equal(t, http.StatusOK, wm.Code)

	setting, err := f.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, setting)
	assert.NotNil(t, setting.CategoryID)
	assert.Equal(t, catID, *setting.CategoryID)

	// follow_version must be bumped under the user's source Space, not groupSpace.
	var userSpaceVer int
	_, err = f.db.session.Select("IFNULL(MAX(version),0)").From("user_follow_version").
		Where("uid=? and space_id=?", testutil.UID, userSpace).Load(&userSpaceVer)
	assert.NoError(t, err)
	assert.Greater(t, userSpaceVer, 0)

	var groupSpaceVer int
	_, err = f.db.session.Select("IFNULL(MAX(version),0)").From("user_follow_version").
		Where("uid=? and space_id=?", testutil.UID, groupSpace).Load(&groupSpaceVer)
	assert.NoError(t, err)
	assert.Equal(t, 0, groupSpaceVer)
}

// TestCategory_MoveExternalGroupEmptySourceSpaceFallsBackToDefaultSpace covers
// the legacy external-member path flagged in PR #192 review: an external member
// row with is_external=1 but empty source_space_id is a legitimate state (e.g.
// users/bots not bound to a Space). The rest of the codebase
// (space_filter.decideConvKeepInSpace, api_sidebar.sidebarMySourceSpaceID)
// resolves that state to the user's default Space, so categorize must do the
// same — otherwise these groups stay un-categorizable and follow_version /
// auto_follow_threads writes land in the group's owning Space.
func TestCategory_MoveExternalGroupEmptySourceSpaceFallsBackToDefaultSpace(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	require.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	// The user's only Space membership → resolves as their default Space.
	defaultSpace := "space-ext191-default"
	groupSpace := "space-ext191-group2"
	seedSpaceAndMember(t, f, defaultSpace, 0)
	route := s.GetRoute()

	wc := createCategory(t, route, defaultSpace, "外部群关注-legacy")
	require.Equal(t, http.StatusOK, wc.Code)
	catID := parseJSON(t, wc)["category_id"].(string)

	// External member with EMPTY source_space_id (legacy row).
	groupNo := "group-ext191-legacy-001"
	_, err = f.db.session.InsertBySql("INSERT INTO `group` (group_no, name, creator, status, space_id) VALUES (?, ?, ?, ?, ?)",
		groupNo, "外部群legacy", "owner-uid", 1, groupSpace).Exec()
	require.NoError(t, err)
	_, err = f.db.session.InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status", "is_external", "source_space_id").
		Values(groupNo, testutil.UID, 0, 0, 1, 1, "").Exec()
	require.NoError(t, err)

	// Must succeed by falling back to the user's default Space.
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	require.Equal(t, http.StatusOK, wm.Code)

	setting, err := f.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, setting)
	assert.NotNil(t, setting.CategoryID)
	assert.Equal(t, catID, *setting.CategoryID)

	// follow_version bumped under the default Space, not the group's owning Space.
	var defVer int
	_, err = f.db.session.Select("IFNULL(MAX(version),0)").From("user_follow_version").
		Where("uid=? and space_id=?", testutil.UID, defaultSpace).Load(&defVer)
	assert.NoError(t, err)
	assert.Greater(t, defVer, 0)

	var groupVer int
	_, err = f.db.session.Select("IFNULL(MAX(version),0)").From("user_follow_version").
		Where("uid=? and space_id=?", testutil.UID, groupSpace).Load(&groupVer)
	assert.NoError(t, err)
	assert.Equal(t, 0, groupVer)
}

// TestCategory_MoveExternalGroupOutClearsAutoFollowThreadsInSourceSpace pins the
// move-out (categoryIDPtr == nil) branch for an external group: the
// ClearAutoFollowThreadsTx write must target the user's source Space, where the
// sidebar materialized the ext row — not the group's owning Space. Without the
// effectiveSpaceID fix this clear would miss the row entirely.
func TestCategory_MoveExternalGroupOutClearsAutoFollowThreadsInSourceSpace(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	userSpace := "space-ext191-mo-user"
	groupSpace := "space-ext191-mo-group"
	seedSpaceAndMember(t, f, userSpace, 0)
	route := s.GetRoute()

	wc := createCategory(t, route, userSpace, "外部群关注-moveout")
	require.Equal(t, http.StatusOK, wc.Code)
	catID := parseJSON(t, wc)["category_id"].(string)

	groupNo := "group-ext191-mo-001"
	_, err = f.db.session.InsertBySql("INSERT INTO `group` (group_no, name, creator, status, space_id) VALUES (?, ?, ?, ?, ?)",
		groupNo, "外部群moveout", "owner-uid", 1, groupSpace).Exec()
	require.NoError(t, err)
	_, err = f.db.session.InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status", "is_external", "source_space_id").
		Values(groupNo, testutil.UID, 0, 0, 1, 1, userSpace).Exec()
	require.NoError(t, err)

	// Move IN.
	wm := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": catID,
	})
	require.Equal(t, http.StatusOK, wm.Code)

	// Simulate sidebar materialization in the SOURCE space (where the follow tab
	// is). target_type=2 → group.
	_, err = f.db.session.InsertBySql(
		"INSERT INTO user_conversation_ext (uid, space_id, target_type, target_id, group_unfollowed, auto_follow_threads) "+
			"VALUES (?, ?, 2, ?, 0, 1)",
		testutil.UID, userSpace, groupNo,
	).Exec()
	require.NoError(t, err, "seed materialized ext row in source space")

	// Move OUT.
	wm2 := doRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": "",
	})
	require.Equal(t, http.StatusOK, wm2.Code)

	// auto_follow_threads must be cleared on the row in the SOURCE space.
	var postAutoFollow int
	_, err = f.db.session.SelectBySql(
		"SELECT auto_follow_threads FROM user_conversation_ext"+
			" WHERE uid=? AND space_id=? AND target_type=2 AND target_id=?",
		testutil.UID, userSpace, groupNo,
	).Load(&postAutoFollow)
	require.NoError(t, err)
	assert.Equal(t, 0, postAutoFollow,
		"move-out must clear auto_follow_threads on the ext row under the source Space")
}

func TestCategory_ListEmpty(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-listempty-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// list without creating any categories or groups
	wl := doRequest(t, s.GetRoute(), "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)

	cats := parseJSONArray(t, wl)
	// no groups → no default category → empty list
	assert.Equal(t, 0, len(cats))
}

// ---------- Default Category (is_default=1) Tests ----------

func TestCategory_ListAutoCreatesDefault(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// create a group (will be uncategorized)
	groupNo := "group-default-001"
	seedGroup(t, f, groupNo, spaceID)

	// list — should auto-create default category with real UUID
	wl := doRequest(t, s.GetRoute(), "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)

	cats := parseJSONArray(t, wl)
	assert.Equal(t, 1, len(cats))

	// default category should have a real string ID (not null)
	assert.NotNil(t, cats[0]["category_id"])
	catID, ok := cats[0]["category_id"].(string)
	assert.True(t, ok)
	assert.NotEmpty(t, catID)
	assert.Equal(t, defaultCategoryNameFallback, cats[0]["name"])

	// the uncategorized group should be under this default category
	groups := cats[0]["groups"].([]interface{})
	assert.Equal(t, 1, len(groups))

	// verify DB row has is_default=1 and stores placeholder (not display name)
	defaultCat, err := f.db.queryDefaultCategory(testutil.UID, spaceID)
	assert.NoError(t, err)
	assert.NotNil(t, defaultCat)
	assert.Equal(t, intPtr(1), defaultCat.IsDefault)
	assert.Equal(t, defaultCategoryNamePlaceholder, defaultCat.Name)
}

func TestCategory_ListDefaultIdempotent(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-idem-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	seedGroup(t, f, "group-idem-001", spaceID)
	route := s.GetRoute()

	// list twice — should not create duplicate default categories
	wl1 := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl1.Code)
	cats1 := parseJSONArray(t, wl1)

	wl2 := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl2.Code)
	cats2 := parseJSONArray(t, wl2)

	// same default category ID both times
	assert.Equal(t, cats1[0]["category_id"], cats2[0]["category_id"])
	assert.Equal(t, len(cats1), len(cats2))
}

func TestCategory_ListWithCategoriesAndDefault(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-mixed-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create a real category
	wc := createCategory(t, route, spaceID, "工作")
	assert.Equal(t, http.StatusOK, wc.Code)
	cat := parseJSON(t, wc)
	catID := cat["category_id"].(string)

	// create two groups, assign one to the category
	seedGroup(t, f, "group-mixed-001", spaceID)
	seedGroup(t, f, "group-mixed-002", spaceID)
	wm := doRequest(t, route, "PUT", "/v1/groups/group-mixed-001/category", map[string]string{
		"category_id": catID,
	})
	assert.Equal(t, http.StatusOK, wm.Code)

	// list
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)
	cats := parseJSONArray(t, wl)

	// should have 2 entries: 工作 + 默认未分类
	assert.Equal(t, 2, len(cats))

	// find the default category
	var defaultCat map[string]interface{}
	for _, c := range cats {
		if c["name"] == defaultCategoryNameFallback {
			defaultCat = c
		}
	}
	assert.NotNil(t, defaultCat)
	assert.NotNil(t, defaultCat["category_id"])

	// default should have 1 uncategorized group
	groups := defaultCat["groups"].([]interface{})
	assert.Equal(t, 1, len(groups))
}

func TestCategory_DeleteDefaultRejected(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-del-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	seedGroup(t, f, "group-default-del-001", spaceID)
	route := s.GetRoute()

	// list to trigger default creation
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)
	cats := parseJSONArray(t, wl)
	defaultCatID := cats[0]["category_id"].(string)

	// try to delete — should be rejected
	wd := doRequest(t, route, "DELETE", "/v1/spaces/"+spaceID+"/categories/"+defaultCatID, nil)
	assert.Equal(t, http.StatusBadRequest, wd.Code)
	assertCategoryErrorCode(t, wd, "err.server.category.default_undeletable")
}

func TestCategory_UpdateDefaultRejected(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-upd-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	seedGroup(t, f, "group-default-upd-001", spaceID)
	route := s.GetRoute()

	// list to trigger default creation
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)
	cats := parseJSONArray(t, wl)
	defaultCatID := cats[0]["category_id"].(string)

	// try to update — should be rejected
	wu := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/"+defaultCatID, map[string]string{"name": "改名"})
	assert.Equal(t, http.StatusBadRequest, wu.Code)
	assertCategoryErrorCode(t, wu, "err.server.category.default_immutable")
}

func TestCategory_SortWithDefault(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-sort-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	seedGroup(t, f, "group-default-sort-001", spaceID)
	route := s.GetRoute()

	// create two categories
	wc1 := createCategory(t, route, spaceID, "A")
	assert.Equal(t, http.StatusOK, wc1.Code)
	cat1 := parseJSON(t, wc1)

	wc2 := createCategory(t, route, spaceID, "B")
	assert.Equal(t, http.StatusOK, wc2.Code)
	cat2 := parseJSON(t, wc2)

	// list to get the default category ID
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)
	cats := parseJSONArray(t, wl)

	var defaultCatID string
	for _, c := range cats {
		if c["name"] == defaultCategoryNameFallback {
			defaultCatID = c["category_id"].(string)
		}
	}
	assert.NotEmpty(t, defaultCatID)

	catID1 := cat1["category_id"].(string)
	catID2 := cat2["category_id"].(string)

	// sort: 未分类, B, A (put default first)
	ws := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{defaultCatID, catID2, catID1},
	})
	assert.Equal(t, http.StatusOK, ws.Code)

	// verify order
	wl2 := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl2.Code)
	cats2 := parseJSONArray(t, wl2)

	assert.Equal(t, 3, len(cats2))
	assert.Equal(t, defaultCategoryNameFallback, cats2[0]["name"])
	assert.Equal(t, "B", cats2[1]["name"])
	assert.Equal(t, "A", cats2[2]["name"])
}

func TestCategory_DefaultNameFromEnv(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-env-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	seedGroup(t, f, "group-default-env-001", spaceID)

	t.Setenv("DM_DEFAULT_CATEGORY_NAME", "自定义分组")
	resetDefaultCategoryName()
	t.Cleanup(resetDefaultCategoryName)

	wl := doRequest(t, s.GetRoute(), "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)

	cats := parseJSONArray(t, wl)
	assert.Equal(t, 1, len(cats))
	assert.Equal(t, "自定义分组", cats[0]["name"])
}

func TestCategory_DefaultNotCountedInLimit(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-default-limit-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	seedGroup(t, f, "group-default-limit-001", spaceID)
	route := s.GetRoute()

	// list to trigger default category creation
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)

	// create 20 normal categories — should all succeed
	for i := 0; i < 20; i++ {
		w := createCategory(t, route, spaceID, fmt.Sprintf("Cat-%d", i))
		assert.Equal(t, http.StatusOK, w.Code, "creating category %d should succeed", i)
	}

	// 21st should fail
	w := createCategory(t, route, spaceID, "Cat-20")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCategory_ListNoGroupsNoDefault(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-nogroups-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// list without any groups — no default category should be created
	wl := doRequest(t, s.GetRoute(), "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)

	cats := parseJSONArray(t, wl)
	assert.Equal(t, 0, len(cats))

	// verify no default row in DB
	defaultCat, err := f.db.queryDefaultCategory(testutil.UID, spaceID)
	assert.NoError(t, err)
	assert.Nil(t, defaultCat)
}

func TestCategory_MoveGroupToDefaultCategory(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-movedefault-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	seedGroup(t, f, "group-md-001", spaceID)
	seedGroup(t, f, "group-md-002", spaceID)

	// list to trigger default category creation
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl.Code)
	cats := parseJSONArray(t, wl)
	assert.Equal(t, 1, len(cats))
	defaultCatID := cats[0]["category_id"].(string)

	// --- Phase 1: move one group into default, one stays uncategorized ---
	wm := doRequest(t, route, "PUT", "/v1/groups/group-md-001/category", map[string]string{
		"category_id": defaultCatID,
	})
	assert.Equal(t, http.StatusOK, wm.Code)

	wl2 := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl2.Code)
	cats2 := parseJSONArray(t, wl2)
	assert.Equal(t, 1, len(cats2))

	groups2 := cats2[0]["groups"].([]interface{})
	assert.Equal(t, 2, len(groups2), "phase 1: default category should merge explicit + uncategorized")

	// --- Phase 2: move all groups into default ---
	wm2 := doRequest(t, route, "PUT", "/v1/groups/group-md-002/category", map[string]string{
		"category_id": defaultCatID,
	})
	assert.Equal(t, http.StatusOK, wm2.Code)

	wl3 := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl3.Code)
	cats3 := parseJSONArray(t, wl3)
	assert.Equal(t, 1, len(cats3))

	groups3 := cats3[0]["groups"].([]interface{})
	assert.Equal(t, 2, len(groups3), "phase 2: all groups explicitly in default should still appear")

	// --- Phase 3: create a new group without category after all moved ---
	seedGroup(t, f, "group-md-003", spaceID)

	wl4 := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusOK, wl4.Code)
	cats4 := parseJSONArray(t, wl4)
	assert.Equal(t, 1, len(cats4))

	groups4 := cats4[0]["groups"].([]interface{})
	assert.Equal(t, 3, len(groups4), "phase 3: new uncategorized group should also appear alongside explicit ones")
}

// ---------- Edge Cases ----------

func TestCategory_InsertDefaultCategoryIdempotent(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-idem-default-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// simulate concurrent inserts: call insertDefaultCategory twice with different UUIDs
	m1 := &CategoryModel{
		CategoryID: "default-uuid-001",
		SpaceID:    spaceID,
		UID:        testutil.UID,
		Name:       defaultCategoryNamePlaceholder,
		Sort:       0,
	}
	err = f.db.insertDefaultCategory(m1)
	assert.NoError(t, err)

	m2 := &CategoryModel{
		CategoryID: "default-uuid-002",
		SpaceID:    spaceID,
		UID:        testutil.UID,
		Name:       defaultCategoryNamePlaceholder,
		Sort:       0,
	}
	err = f.db.insertDefaultCategory(m2)
	assert.NoError(t, err)

	// should only have 1 default category in DB
	var count int
	_, err = f.db.session.Select("count(*)").From("group_category").
		Where("uid=? and space_id=? and is_default=1 and status=1", testutil.UID, spaceID).
		Load(&count)
	assert.NoError(t, err)
	assert.Equal(t, 1, count, "insertDefaultCategory should be idempotent — only one default row")
}

func TestCategory_UniqueIndexPreventsDefaultDuplicate(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-uidx-default-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// first insert succeeds
	err = f.db.insertCategory(&CategoryModel{
		CategoryID: "uidx-default-001",
		SpaceID:    spaceID,
		UID:        testutil.UID,
		Name:       defaultCategoryNamePlaceholder,
		Sort:       0,
		Status:     1,
		IsDefault:  intPtr(1),
	})
	assert.NoError(t, err)

	// second insert with different ID but same (uid, space_id, is_default=1) should be rejected
	err = f.db.insertCategory(&CategoryModel{
		CategoryID: "uidx-default-002",
		SpaceID:    spaceID,
		UID:        testutil.UID,
		Name:       defaultCategoryNamePlaceholder,
		Sort:       0,
		Status:     1,
		IsDefault:  intPtr(1),
	})
	assert.Error(t, err, "unique index should prevent duplicate default categories")
}

func TestCategory_SortCountMismatch(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-sortmismatch-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create 2 categories
	wc1 := createCategory(t, route, spaceID, "A")
	assert.Equal(t, http.StatusOK, wc1.Code)
	cat1 := parseJSON(t, wc1)

	wc2 := createCategory(t, route, spaceID, "B")
	assert.Equal(t, http.StatusOK, wc2.Code)
	_ = wc2

	// try to sort with only 1 ID (mismatch)
	ws := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{cat1["category_id"].(string)},
	})
	assert.Equal(t, http.StatusBadRequest, ws.Code)
	assertCategoryErrorCode(t, ws, "err.server.category.sort_list_mismatch")
}

// TestCategory_SortDuplicateIDs covers the repeated-ID branch in sort: a
// same-length category_ids list that contains a duplicate must be rejected as
// sort_list_duplicate (not sort_list_mismatch). The duplicate guard runs before
// the catMap membership check, so the repeat is caught even though every ID is a
// real category. (PR #214 reviewer-requested endpoint coverage.)
func TestCategory_SortDuplicateIDs(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	resetUIDRateLimit(t, ctx)

	spaceID := "space-sortdup-001"
	seedSpaceAndMember(t, f, spaceID, 0)
	route := s.GetRoute()

	// create 2 categories so the list length matches but contains a repeat
	wc1 := createCategory(t, route, spaceID, "A")
	require.Equal(t, http.StatusOK, wc1.Code)
	cat1 := parseJSON(t, wc1)
	wc2 := createCategory(t, route, spaceID, "B")
	require.Equal(t, http.StatusOK, wc2.Code)

	catID1 := cat1["category_id"].(string)
	// same length (2) as the user's categories, but catID1 is repeated
	ws := doRequest(t, route, "PUT", "/v1/spaces/"+spaceID+"/categories/sort", map[string]interface{}{
		"category_ids": []string{catID1, catID1},
	})
	assert.Equal(t, http.StatusBadRequest, ws.Code)
	assertCategoryErrorCode(t, ws, "err.server.category.sort_list_duplicate")
}

func TestCategory_MoveGroupNoSpace(t *testing.T) {
	s, ctx := newCategoryTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// create a group WITHOUT a space_id
	groupNo := "group-nospace-001"
	_, err = f.db.session.InsertBySql("INSERT INTO `group` (group_no, name, creator, status) VALUES (?, ?, ?, ?)",
		groupNo, "无空间群组", testutil.UID, 1).Exec()
	assert.NoError(t, err)

	_, err = f.db.session.InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status").
		Values(groupNo, testutil.UID, 0, 0, 1).Exec()
	assert.NoError(t, err)

	// try to move it into a category (should fail because group has no space)
	w := doRequest(t, s.GetRoute(), "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
		"category_id": "some-fake-cat",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCategoryErrorCode(t, w, "err.server.category.group_space_missing")
}
