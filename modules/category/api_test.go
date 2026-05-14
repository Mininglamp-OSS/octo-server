package category

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
}

func TestCategory_Sort(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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

// ---------- Validation / Error Tests ----------

func TestCategory_CreateLimit(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "最多创建20个分类")
}

func TestCategory_CreateEmptyName(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-emptyname-001"
	f := New(ctx)
	seedSpaceAndMember(t, f, spaceID, 0)

	w := createCategory(t, s.GetRoute(), spaceID, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "类别名称不能为空")
}

func TestCategory_UpdateNotOwner(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "无权限修改此分类")
}

func TestCategory_DeleteNotOwner(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "无权限删除此分类")
}

func TestCategory_MoveGroupNotMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "你不是该群成员")
}

func TestCategory_NonSpaceMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "你不是该空间成员")

	// try to list categories
	wl := doRequest(t, route, "GET", "/v1/spaces/"+spaceID+"/categories", nil)
	assert.Equal(t, http.StatusBadRequest, wl.Code)
	assert.Contains(t, wl.Body.String(), "你不是该空间成员")
}

func TestCategory_UpdateNotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-updnotfound-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// try to update a non-existent category
	w := doRequest(t, s.GetRoute(), "PUT", "/v1/spaces/"+spaceID+"/categories/nonexistent-cat", map[string]string{"name": "不存在"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "分类不存在")
}

func TestCategory_DeleteNotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-delnotfound-001"
	seedSpaceAndMember(t, f, spaceID, 0)

	// try to delete a non-existent category
	w := doRequest(t, s.GetRoute(), "DELETE", "/v1/spaces/"+spaceID+"/categories/nonexistent-cat", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "分类不存在")
}

func TestCategory_UpdateEmptyName(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, wu.Body.String(), "类别名称不能为空")
}

func TestCategory_SortEmptyList(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, ws.Body.String(), "分类列表不能为空")
}

func TestCategory_SortUnknownCategory(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, ws.Body.String(), "分类不存在或无权限")
}

func TestCategory_SortNonSpaceMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, ws.Body.String(), "你不是该空间成员")
}

func TestCategory_MoveGroupCategoryNotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "分类不存在")
}

func TestCategory_MoveGroupCategoryNotOwner(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "无权限使用此分类")
}

func TestCategory_MoveGroupCrossSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "群组和分类不在同一空间")
}

func TestCategory_ListEmpty(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, wd.Body.String(), "默认分类不可删除")
}

func TestCategory_UpdateDefaultRejected(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, wu.Body.String(), "默认分类不可修改")
}

func TestCategory_SortWithDefault(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, ws.Body.String(), "分类列表数量不匹配")
}

func TestCategory_MoveGroupNoSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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
	assert.Contains(t, w.Body.String(), "该群组不属于任何空间")
}
