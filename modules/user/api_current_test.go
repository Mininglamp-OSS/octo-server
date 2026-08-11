package user

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserCurrent_VerifiedUser 覆盖 YUJ-413 核心契约:
// 已实名用户调 GET /v1/user/current,response payload 必须包含:
//   - realname_verified = true
//   - real_name        = "余嘉伟"
//   - realname_verified_at > 0 (Unix 秒)
//
// 根因:之前 /v1/user/login 和 /v1/user/current 都漏下发这三字段,
// Web/Android/iOS 三端 self 徽章和 displayName 全部不亮。
func TestUserCurrent_VerifiedUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	require.NoError(t, testutil.CleanAllTables(ctx))

	require.NoError(t, u.db.Insert(&Model{
		UID:      testutil.UID,
		Name:     "admin",
		Username: "admin",
		Sex:      1,
		Password: util.MD5(util.MD5("123456")),
		ShortNo:  "uid_xxx1",
		Zone:     "0086",
		Phone:    "13600000001",
	}))

	// 模拟 OIDC callback 已写入 user_verification(Aegis 权威源 → OCTO cache)
	verifiedAt := time.Date(2026, 5, 11, 8, 0, 0, 0, time.UTC)
	require.NoError(t, u.verificationDB.Upsert(&verificationModel{
		UserID:     testutil.UID,
		RealName:   "余嘉伟",
		Source:     "aegis",
		SourceSub:  "cas-sub-1",
		EmpID:      dbr.NullString{NullString: sql.NullString{String: "0000031", Valid: true}},
		VerifiedAt: verifiedAt,
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/current", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, true, body["realname_verified"], "realname_verified 必须为 true")
	assert.Equal(t, "余嘉伟", body["real_name"], "real_name 必须是权威姓名")
	// JSON number 反序列化为 float64；Unix 秒需为正数。
	ts, ok := body["realname_verified_at"].(float64)
	require.True(t, ok, "realname_verified_at 必须存在且为 number；body=%s", w.Body.String())
	assert.Equal(t, float64(verifiedAt.Unix()), ts)

	// 结构对齐 login response:uid / name / username 等字段必须在
	assert.Equal(t, testutil.UID, body["uid"])
	assert.Equal(t, "admin", body["name"])
}

// TestUserCurrent_UnverifiedUser 未实名用户:realname_verified=false,
// real_name / realname_verified_at 被 omitempty 剥离或为零值,**不能 500**。
func TestUserCurrent_UnverifiedUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	require.NoError(t, testutil.CleanAllTables(ctx))

	require.NoError(t, u.db.Insert(&Model{
		UID:      testutil.UID,
		Name:     "admin",
		Username: "admin",
		Password: util.MD5(util.MD5("123456")),
		ShortNo:  "uid_xxx1",
		Zone:     "0086",
		Phone:    "13600000002",
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/current", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, false, body["realname_verified"], "未实名用户 realname_verified=false")
	// omitempty:real_name / realname_verified_at 要么不存在,要么零值,不能报错。
	if v, ok := body["real_name"]; ok {
		assert.Equal(t, "", v, "未实名 real_name 必须为空")
	}
	if v, ok := body["realname_verified_at"]; ok {
		assert.Equal(t, float64(0), v, "未实名 realname_verified_at 必须为 0")
	}
}

// TestUserLogin_CarriesRealnameFields /v1/user/login response 必须带实名字段。
// 这是交付 #2:Web fresh login 后客户端能直接从 loginInfo 拿到实名态,
// 不再依赖 R9 的"self channelInfo listener"兜底(该兜底在 fresh login 永不触发)。
//
// 注意端点选型:这里选 /v1/user/login 而非 /v1/user/usernamelogin,因为:
//   - login 走 execLoginAndRespose → c.Response(result),顶层直接是 loginUserDetailResp;
//   - usernamelogin 在 api_usernamelogin.go:138 包了一层 {"data": result,
//     "need_upload_web3publickey": ...},顶层断言路径会失败。
// 两条路径共用 execLogin → applyRealnameToLoginResp,测 login 就覆盖契约。
func TestUserLogin_CarriesRealnameFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	u.Route(s.GetRoute())
	require.NoError(t, testutil.CleanAllTables(ctx))

	require.NoError(t, u.db.Insert(&Model{
		UID:      "uid-login-realname-1",
		Name:     "admin",
		Username: "admin-login-1",
		Password: util.MD5(util.MD5("123456")),
		ShortNo:  "uid_xxx2",
		Zone:     "0086",
		Phone:    "13600000003",
	}))

	verifiedAt := time.Date(2026, 5, 11, 9, 30, 0, 0, time.UTC)
	require.NoError(t, u.verificationDB.Upsert(&verificationModel{
		UserID:     "uid-login-realname-1",
		RealName:   "张三",
		Source:     "aegis",
		SourceSub:  "cas-sub-2",
		VerifiedAt: verifiedAt,
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "admin-login-1",
		"password": "123456",
		"flag":     0,
		"device": map[string]interface{}{
			"device_id":    "device_id_login_1",
			"device_name":  "device_name_login_1",
			"device_model": "device_model_login_1",
		},
	}))))
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, true, body["realname_verified"])
	assert.Equal(t, "张三", body["real_name"])
	ts, ok := body["realname_verified_at"].(float64)
	require.True(t, ok, "login response 必须带 realname_verified_at; body=%s", w.Body.String())
	assert.Equal(t, float64(verifiedAt.Unix()), ts)
}
