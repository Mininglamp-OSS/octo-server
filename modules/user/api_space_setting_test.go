package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSpaceID = "space_setting_test_001"

func seedSpaceForSettingTest(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(testSpaceID, "Test Space", testutil.UID, 1).Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(testSpaceID, testutil.UID, 0, 1).Exec()
	require.NoError(t, err)
}

func doSpaceSettingRequest(t *testing.T, s *server.Server, method, path string, body interface{}) *httptest.ResponseRecorder {
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
	s.GetRoute().ServeHTTP(w, req)
	return w
}

func parseSpaceSettingResp(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var result map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &result)
	require.NoError(t, err)
	return result
}

func TestGetSpaceSetting_DefaultValues(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceForSettingTest(t, ctx)

	w := doSpaceSettingRequest(t, s, "GET", "/v1/user/space/setting?space_id="+testSpaceID, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	resp := parseSpaceSettingResp(t, w)
	assert.Equal(t, float64(1), resp["voice_feedback_on"])
	assert.Equal(t, float64(0), resp["voice_feedback_notice_acked"])
}

func TestPutSpaceSetting_SingleField(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceForSettingTest(t, ctx)

	w := doSpaceSettingRequest(t, s, "PUT", "/v1/user/space/setting?space_id="+testSpaceID, map[string]interface{}{
		"voice_feedback_on": 0,
	})
	assert.Equal(t, http.StatusOK, w.Code)

	w = doSpaceSettingRequest(t, s, "PUT", "/v1/user/space/setting?space_id="+testSpaceID, map[string]interface{}{
		"voice_feedback_notice_acked": 1,
	})
	assert.Equal(t, http.StatusOK, w.Code)

	w = doSpaceSettingRequest(t, s, "GET", "/v1/user/space/setting?space_id="+testSpaceID, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	resp := parseSpaceSettingResp(t, w)
	assert.Equal(t, float64(0), resp["voice_feedback_on"])
	assert.Equal(t, float64(1), resp["voice_feedback_notice_acked"])
}

func TestPutSpaceSetting_InvalidValue(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceForSettingTest(t, ctx)

	w := doSpaceSettingRequest(t, s, "PUT", "/v1/user/space/setting?space_id="+testSpaceID, map[string]interface{}{
		"voice_feedback_on": 2,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPutSpaceSetting_InvalidField(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceForSettingTest(t, ctx)

	w := doSpaceSettingRequest(t, s, "PUT", "/v1/user/space/setting?space_id="+testSpaceID, map[string]interface{}{
		"unknown_field": 1,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetSpaceSetting_MissingSpaceID(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	w := doSpaceSettingRequest(t, s, "GET", "/v1/user/space/setting", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPutSpaceSetting_MissingSpaceID(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	w := doSpaceSettingRequest(t, s, "PUT", "/v1/user/space/setting", map[string]interface{}{
		"voice_feedback_on": 1,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
