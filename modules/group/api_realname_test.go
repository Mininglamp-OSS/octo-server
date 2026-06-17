package group

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// YUJ-413 Scope B · memberDetailResp 实名字段契约
//
// 根因（YUJ-411 memory 07c6d080）：Android 气泡 + 群成员列表 + WKSDK
// ChannelMember 缓存路径全瞎，后端 /v1/groups/:group_no/members /
// /membersync / /members/:uid 三个端点漏下发 realname_verified。
//
// 这个文件的断言有两类：
// 1. 源码级 grep 锁定 struct 必须有三个字段 + 三个 handler 必须调 fillRealnameFields
//    —— 任何环境都能跑，哪怕 testutil 起不起来 DB。
// 2. 真 HTTP 端到端：seed user_verification → GET /v1/groups/.../members →
//    assert response payload 含 realname_verified=true / real_name=X。
//    —— 需要 real MySQL via testutil.NewTestServer。
// =============================================================================

// --- 源码级锁定（无 DB 依赖，CI 一定跑） ---

// TestMemberDetailResp_HasRealnameFields_Contract 锁住 memberDetailResp
// 必须下发 realname_verified / real_name / realname_verified_at 三字段，
// JSON tag 完全和 friend/sync、conversation/sync、loginUserDetailResp 对齐。
func TestMemberDetailResp_HasRealnameFields_Contract(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	// 定位到 memberDetailResp struct 块
	startIdx := strings.Index(body, "type memberDetailResp struct {")
	require.NotEqual(t, -1, startIdx, "memberDetailResp struct 不见了？")
	// 找 struct 结束 `\n}` —— 简单切到下一个 `\nfunc ` 即可
	endIdx := strings.Index(body[startIdx:], "\nfunc ")
	require.NotEqual(t, -1, endIdx)
	block := body[startIdx : startIdx+endIdx]

	// JSON tag 必须完全匹配（snake_case）。
	assert.Regexp(t,
		regexp.MustCompile("RealnameVerified\\s+bool\\s+`json:\"realname_verified\"`"),
		block,
		"memberDetailResp 必须有 RealnameVerified bool `json:\"realname_verified\"`（YUJ-413）")
	assert.Regexp(t,
		regexp.MustCompile("RealName\\s+string\\s+`json:\"real_name,omitempty\"`"),
		block,
		"memberDetailResp 必须有 RealName string `json:\"real_name,omitempty\"`（YUJ-413）")
	assert.Regexp(t,
		regexp.MustCompile("RealnameVerifiedAt\\s+int64\\s+`json:\"realname_verified_at,omitempty\"`"),
		block,
		"memberDetailResp 必须有 RealnameVerifiedAt int64 `json:\"realname_verified_at,omitempty\"`（YUJ-413）")
}

// TestMembersHandlers_CallFillRealnameFields_Contract 锁住三个 handler
// 都必须调 fillRealnameFields —— 少一个就会让对应端点的实名字段始终空。
func TestMembersHandlers_CallFillRealnameFields_Contract(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	for _, fn := range []string{
		"func (g *Group) membersGet(",
		"func (g *Group) memberGet(",
		"func (g *Group) syncMembers(",
	} {
		fnStart := strings.Index(body, fn)
		require.NotEqual(t, -1, fnStart, "找不到 handler: %s", fn)
		// 粗略切到下一个 `\nfunc ` 即为函数体范围（够用于锁定 helper 调用）
		fnEnd := strings.Index(body[fnStart+len(fn):], "\nfunc ")
		require.NotEqual(t, -1, fnEnd, "handler 体范围切分失败: %s", fn)
		fnBody := body[fnStart : fnStart+len(fn)+fnEnd]
		assert.Contains(t, fnBody, "g.fillRealnameFields(",
			"%s 必须调 g.fillRealnameFields 回填实名字段（YUJ-413）", strings.TrimSuffix(fn, "("))
	}
}

// TestFillRealnameFields_UsesBatchQuery 锁住 fillRealnameFields 必须走
// user.QueryVerificationsByUIDs 单次 IN 查询，不能循环查（N+1 防护）。
func TestFillRealnameFields_UsesBatchQuery(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	helperStart := strings.Index(body, "func (g *Group) fillRealnameFields(")
	require.NotEqual(t, -1, helperStart, "fillRealnameFields helper 不见了")
	helperEnd := strings.Index(body[helperStart:], "\nfunc ")
	require.NotEqual(t, -1, helperEnd)
	helperBody := body[helperStart : helperStart+helperEnd]

	// 批量调用。
	assert.Contains(t, helperBody, "user.QueryVerificationsByUIDs(",
		"fillRealnameFields 必须走批量 API 防 N+1（YUJ-413）")
	// 不允许在循环里调 QueryByUID：任何 `for` 循环块内出现 QueryByUID 就回归。
	forIdx := strings.Index(helperBody, "for ")
	if forIdx != -1 {
		assert.NotContains(t, helperBody[forIdx:], "QueryByUID(",
			"fillRealnameFields 不允许在循环里调单查 QueryByUID（N+1 回归）")
	}
}

// --- 真 HTTP 测试（需要 MySQL） ---

// seedUserVerification 往 user_verification 表写一条已实名记录。
func seedUserVerification(t *testing.T, f *Group, uid, realName string, verifiedAt time.Time) {
	t.Helper()
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO user_verification (user_id, real_name, source, source_sub, emp_id, dept, email, mobile, verified_at) "+
			"VALUES (?, ?, 'aegis', ?, ?, NULL, NULL, NULL, ?)",
		uid, realName, "cas-sub-"+uid,
		dbr.NullString{NullString: sql.NullString{String: "E" + uid, Valid: true}},
		verifiedAt,
	).Exec()
	require.NoError(t, err)
}

// setupMembersGroup 建一个 loginUID 是成员的普通群，插 2 个其它成员：
// - uid-verified：已实名
// - uid-unverified：未实名
// 便于断言"已实名 realname_verified=true + real_name"和"未实名 realname_verified=false"两 case。
//
// YUJ-413 R6 修复: 必须显式调 f.Route(s.GetRoute()) 让测试请求能真正命中
// membersGet / syncMembers / memberGet handler。参考 modules/group/api_test.go:47
// 和 :227 的已验证 setup 模式。漏这一行三个测试会打到 404 路径,所谓的 pass
// 其实是 vacuous,证明不了 feature works（Jerry + lml2468 R6 独立 confirm）。
func setupMembersGroup(t *testing.T) (*Group, http.Handler, string, string) {
	s, ctx := newTestServer(t)
	f := New(ctx)
	f.Route(s.GetRoute())
	require.NoError(t, testutil.CleanAllTables(ctx))

	_, _ = ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, 1)",
	).Exec()

	// Login user（testutil.UID）
	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "operator", ShortNo: "op_realname"}))
	// 目标成员
	verifiedUID := "uid-verified-realname"
	unverifiedUID := "uid-unverified-realname"
	require.NoError(t, f.userDB.Insert(&user.Model{UID: verifiedUID, Name: "verified-user", ShortNo: "sv_realname"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: unverifiedUID, Name: "unverified-user", ShortNo: "su_realname"}))

	// 只给 verifiedUID 写 user_verification
	seedUserVerification(t, f, verifiedUID, "张三", time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC))

	groupNo := "g_realname_test"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "realname test group",
		Creator: testutil.UID,
		Status:  GroupStatusNormal,
		Version: 1,
	}))
	for _, uid := range []string{testutil.UID, verifiedUID, unverifiedUID} {
		require.NoError(t, f.db.InsertMember(&MemberModel{
			GroupNo: groupNo,
			UID:     uid,
			Role:    MemberRoleCommon,
			Status:  1,
			Version: 1,
			Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
		}))
	}

	return f, s.GetRoute(), verifiedUID, unverifiedUID
}

// TestMembersGet_CarriesRealnameFields：GET /v1/groups/:group_no/members
// 响应里已实名成员带 realname_verified=true + real_name；未实名 realname_verified=false。
func TestMembersGet_CarriesRealnameFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, handler, verifiedUID, unverifiedUID := setupMembersGroup(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/g_realname_test/members?limit=100", nil)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var members []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))

	// 找两个目标成员
	var verified, unverified map[string]interface{}
	for _, m := range members {
		switch m["uid"] {
		case verifiedUID:
			verified = m
		case unverifiedUID:
			unverified = m
		}
	}
	require.NotNil(t, verified, "已实名成员缺失；body=%s", w.Body.String())
	require.NotNil(t, unverified, "未实名成员缺失；body=%s", w.Body.String())

	assert.Equal(t, true, verified["realname_verified"])
	assert.Equal(t, "张三", verified["real_name"])
	ts, ok := verified["realname_verified_at"].(float64)
	require.True(t, ok, "已实名成员必须带 realname_verified_at; body=%s", w.Body.String())
	assert.Greater(t, ts, float64(0))

	assert.Equal(t, false, unverified["realname_verified"])
	if v, ok := unverified["real_name"]; ok {
		assert.Equal(t, "", v)
	}
	if v, ok := unverified["realname_verified_at"]; ok {
		assert.Equal(t, float64(0), v)
	}
}

// TestSyncMembers_CarriesRealnameFields：GET /v1/groups/:group_no/membersync
// 对 Android WKSDK ChannelMember 缓存是唯一数据源，必须同样下发三字段。
func TestSyncMembers_CarriesRealnameFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, handler, verifiedUID, _ := setupMembersGroup(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/g_realname_test/membersync?version=0&limit=100", nil)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var members []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))

	var verified map[string]interface{}
	for _, m := range members {
		if m["uid"] == verifiedUID {
			verified = m
			break
		}
	}
	require.NotNil(t, verified, "body=%s", w.Body.String())
	assert.Equal(t, true, verified["realname_verified"])
	assert.Equal(t, "张三", verified["real_name"])
}

// TestMemberGet_SingleUID_CarriesRealnameFields：GET /v1/groups/:group_no/members/:uid
// 单成员查询也要带实名字段（资料卡 / @提及等路径）。
func TestMemberGet_SingleUID_CarriesRealnameFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, handler, verifiedUID, _ := setupMembersGroup(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/g_realname_test/members/"+verifiedUID, nil)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	require.Equal(t, true, body["exists"])
	member, ok := body["member"].(map[string]interface{})
	require.True(t, ok, "body=%s", w.Body.String())
	assert.Equal(t, true, member["realname_verified"])
	assert.Equal(t, "张三", member["real_name"])
}

// TestFillRealnameFields_EmptyInput_NoPanic 空切片 / 空 uid 不要 panic 也不调 DB。
// 和 fillSpaceRelatedFields 的行为对齐（guard clause）。
func TestFillRealnameFields_EmptyInput_NoPanic_SourceGrep(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)
	helperStart := strings.Index(body, "func (g *Group) fillRealnameFields(")
	require.NotEqual(t, -1, helperStart)
	helperEnd := strings.Index(body[helperStart:], "\nfunc ")
	helperBody := body[helperStart : helperStart+helperEnd]

	// guard clause 必须存在。
	assert.Contains(t, helperBody, "if len(resps) == 0 {",
		"fillRealnameFields 必须有 len(resps)==0 guard，否则空群会走无谓 DB 查询")
}

// TestSetupMembersGroup_RegistersRoutes_Contract YUJ-413 R6 回归锁定:
// setupMembersGroup 必须调 f.Route(s.GetRoute()) 把 Group handler 注册到
// httptest 用的路由树 —— 不加这行,三个 realname 测试打的都是 404,pass
// 是 vacuous 的 (Jerry + lml2468 R6 独立 confirm)。
//
// 源码级锁,无 DB 依赖,CI 一定跑。
func TestSetupMembersGroup_RegistersRoutes_Contract(t *testing.T) {
	src, err := os.ReadFile("api_realname_test.go")
	require.NoError(t, err)
	body := string(src)

	fnStart := strings.Index(body, "func setupMembersGroup(")
	require.NotEqual(t, -1, fnStart, "setupMembersGroup 必须存在")
	fnEnd := strings.Index(body[fnStart:], "\nfunc ")
	require.NotEqual(t, -1, fnEnd)
	fnBody := body[fnStart : fnStart+fnEnd]

	assert.Contains(t, fnBody, "f.Route(s.GetRoute())",
		"setupMembersGroup 必须调 f.Route(s.GetRoute()) 注册路由,否则三个实名测试全 404 (YUJ-413 R6 Blocker)")
}

// Bytes 引用避免未使用 import 报错。
var _ = bytes.Buffer{}
