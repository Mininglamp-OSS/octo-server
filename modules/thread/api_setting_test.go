package thread

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestUpdateThreadSetting_Mute(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "话题1")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT",
		"/v1/groups/"+groupNo+"/threads/"+shortID+"/setting",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"mute": 1}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 落库验证
	db := NewDB(ctx)
	setting, err := db.QuerySetting(groupNo, shortID, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, setting)
	assert.Equal(t, 1, setting.Mute)
}

func TestUpdateThreadSetting_InvalidShortID(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT",
		"/v1/groups/"+groupNo+"/threads/invalid/setting",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"mute": 1}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateThreadSetting_NotGroupMember(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "话题1")

	// 非群成员用户
	outsiderToken := "token_outsider"
	err := ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+outsiderToken, "outsider@test")
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT",
		"/v1/groups/"+groupNo+"/threads/"+shortID+"/setting",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"mute": 1}))))
	req.Header.Set("token", outsiderToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not a group member")
}

func TestUpdateThreadSetting_EmptyBody(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "话题1")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT",
		"/v1/groups/"+groupNo+"/threads/"+shortID+"/setting",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
