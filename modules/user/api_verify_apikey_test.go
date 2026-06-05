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

// authVerifyAPIKey: POST /v1/auth/verify-api-key
// 合并 plan §3 covers 6 cases: valid / unknown / owner-left-space /
// legacy-empty-space / missing-field / multi-space.

const (
	testAPIKeySpaceA = "verify_apikey_space_a"
	testAPIKeySpaceB = "verify_apikey_space_b"
	testAPIKeyUID    = "verify_apikey_uid_001"
)

func seedAPIKeyFixtures(t *testing.T, ctx *config.Context) {
	t.Helper()
	// Two spaces, the test uid is an active member of both.
	for _, sid := range []string{testAPIKeySpaceA, testAPIKeySpaceB} {
		_, err := ctx.DB().InsertInto("space").
			Columns("space_id", "name", "creator", "status").
			Values(sid, "Test "+sid, testAPIKeyUID, 1).Exec()
		require.NoError(t, err)

		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(sid, testAPIKeyUID, 0, 1).Exec()
		require.NoError(t, err)
	}
}

func insertAPIKey(t *testing.T, ctx *config.Context, uid, apiKey, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("user_api_key").
		Columns("uid", "api_key", "space_id").
		Values(uid, apiKey, spaceID).Exec()
	require.NoError(t, err)
}

func doVerifyAPIKey(t *testing.T, s *server.Server, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		reqBody = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/auth/verify-api-key", reqBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// Case 1: 有效 api_key → 200 返 uid + space_id
func TestAuthVerifyAPIKey_Valid(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_valid_test_key_abc12345678901234567", testAPIKeySpaceA)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_valid_test_key_abc12345678901234567"})

	require.Equal(t, http.StatusOK, w.Code)
	// c.Response 直接序列化 data, 不 wrap (octo-lib wkhttp.Context).
	var resp struct {
		UID     string `json:"uid"`
		SpaceID string `json:"space_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, testAPIKeyUID, resp.UID)
	assert.Equal(t, testAPIKeySpaceA, resp.SpaceID)
}

// Case 2: 不存在的 api_key → 401
func TestAuthVerifyAPIKey_Unknown(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	// no INSERT — api_key 不存在

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_never_existed_xxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 3: api_key 存在但 owner 已退出 space (status=0) → 401
func TestAuthVerifyAPIKey_OwnerLeftSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_owner_left_xxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)

	// flip status: owner 退出 space
	_, err := ctx.DB().Update("space_member").
		Set("status", 0).
		Where("space_id=? AND uid=?", testAPIKeySpaceA, testAPIKeyUID).
		Exec()
	require.NoError(t, err)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_owner_left_xxxxxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 3b: space_member.status 不是 1 也不是 0 (其他值, 如 2 "pending") →
// 同样 401. SQL filter 是 `status=1`, 任何非 1 值都被拒, 这个 case 提供
// 显式覆盖避免 future 改成 `status != 0` 引入 regression.
func TestAuthVerifyAPIKey_NonActiveStatus(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_nonactive_xxxxxxxxxxxxxxxxxxxxxxxx", testAPIKeySpaceA)

	// flip status to 2 (e.g. "pending invitation" — 任何非 1 值)
	_, err := ctx.DB().Update("space_member").
		Set("status", 2).
		Where("space_id=? AND uid=?", testAPIKeySpaceA, testAPIKeyUID).
		Exec()
	require.NoError(t, err)

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_nonactive_xxxxxxxxxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 4: legacy space_id='' 的 api_key → 401 (合并 plan §3 显式拒绝)
func TestAuthVerifyAPIKey_LegacyEmptySpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	insertAPIKey(t, ctx, testAPIKeyUID, "uk_legacy_no_space_xxxxxxxxxxxxxxxxx", "")

	w := doVerifyAPIKey(t, s, map[string]string{"api_key": "uk_legacy_no_space_xxxxxxxxxxxxxxxxx"})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Case 5: 缺 api_key 字段 → 400 (respondUserTokenRequired 走 400 系列)
func TestAuthVerifyAPIKey_MissingField(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)

	w := doVerifyAPIKey(t, s, map[string]string{})

	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Less(t, w.Code, http.StatusInternalServerError, "缺字段应 4xx, 不应是 5xx")
}

// Case 6: 同一 user 在两个 space 各有 api_key → 各自返回对应 space_id
func TestAuthVerifyAPIKey_MultiSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedAPIKeyFixtures(t, ctx)
	keyA := "uk_multi_space_a_xxxxxxxxxxxxxxxxxxx"
	keyB := "uk_multi_space_b_xxxxxxxxxxxxxxxxxxx"
	insertAPIKey(t, ctx, testAPIKeyUID, keyA, testAPIKeySpaceA)
	insertAPIKey(t, ctx, testAPIKeyUID, keyB, testAPIKeySpaceB)

	for _, tc := range []struct {
		apiKey  string
		wantSID string
	}{
		{keyA, testAPIKeySpaceA},
		{keyB, testAPIKeySpaceB},
	} {
		w := doVerifyAPIKey(t, s, map[string]string{"api_key": tc.apiKey})
		require.Equal(t, http.StatusOK, w.Code, "api_key=%s", tc.apiKey)
		var resp struct {
			UID     string `json:"uid"`
			SpaceID string `json:"space_id"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, testAPIKeyUID, resp.UID)
		assert.Equal(t, tc.wantSID, resp.SpaceID, "api_key=%s 应返回 %s", tc.apiKey, tc.wantSID)
	}
}
