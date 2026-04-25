package group

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// newInviteRequest 构造一个落地页测试请求，使用每个测试唯一的伪 X-Real-Ip
// 头隔离 per-IP 限流桶（生产是 10 req/min, burst 5；同 IP 顺序跑多个 case 会触发 429）。
func newInviteRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	assert.NoError(t, err)
	req.Header.Set("X-Real-Ip", fmt.Sprintf("203.0.113.%d", deterministicIPSuffix(t.Name())))
	return req
}

// deterministicIPSuffix 由测试名生成 1-254 的字节，避免冲突且可重现。
func deterministicIPSuffix(name string) int {
	var sum int
	for _, r := range name {
		sum = (sum*31 + int(r)) % 254
	}
	return sum + 1
}

func TestGroupInviteDetail_Joinable(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-h5-joinable"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "研发协调群",
		Creator: testutil.UID,
		Status:  1,
		Invite:  0,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-h5-joinable-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "joinable", resp["status"])
	assert.Equal(t, "研发协调群", resp["group_name"])
	assert.Equal(t, groupNo, resp["group_no"])
	assert.Equal(t, fmt.Sprintf("groups/%s/avatar", groupNo), resp["avatar"])
	assert.EqualValues(t, 1, resp["member_count"])
}

func TestGroupInviteDetail_InviteRequired(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-h5-required"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "审批群",
		Creator: testutil.UID,
		Status:  1,
		Invite:  1,
	})
	assert.NoError(t, err)

	code := "test-h5-required-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "invite_required", resp["status"])
}

func TestGroupInviteDetail_Expired(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code=does-not-exist-"+util.GenerUUID()))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "expired", resp["status"])
}

func TestGroupInviteDetail_NotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-h5-notfound"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "已解散群",
		Creator: testutil.UID,
		Status:  GroupStatusDisband,
	})
	assert.NoError(t, err)

	code := "test-h5-notfound-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "not_found", resp["status"])
}

func TestGroupInviteDetail_EmptyCode(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// 非 group 类型的二维码（如 scanLogin）应返回 expired，避免跨类型数据被透出。
func TestGroupInviteDetail_WrongQRCodeType(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	code := "test-h5-wrong-type-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
			"foo": "bar",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "expired", resp["status"])
}

// 落地页渲染需要从 repo 根目录读取 assets/web/group_invite.html。
// 测试时 cwd 是 modules/group/，所以临时切到 repo 根再切回。
func TestGroupInvitePage_RendersHTMLWithAPIBase(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	wd, err := os.Getwd()
	assert.NoError(t, err)
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("chdir to repo root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite?code=anything"))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	body := w.Body.String()
	assert.NotContains(t, body, "{{API_BASE_URL}}")
	assert.True(t, strings.Contains(body, "群邀请"))
	assert.True(t, strings.Contains(body, "/v1/group/invite/detail"))
}

// 已登录用户用公开 code 换取 auth_code：基础路径。
func TestGroupInviteAuthorize_OK(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-auth-ok"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "研发群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		AllowExternal: 1,
	})
	assert.NoError(t, err)

	code := "test-auth-ok-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, groupNo, resp["group_no"])
	authCode, _ := resp["auth_code"].(string)
	assert.NotEmpty(t, authCode)

	// 校验 Redis 里写入的 auth_code 记录和移动端 handleJoinGroup 保持一致的 shape
	cached, err := ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	assert.NoError(t, err)
	var payload map[string]interface{}
	assert.NoError(t, json.Unmarshal([]byte(cached), &payload))
	assert.Equal(t, groupNo, payload["group_no"])
	assert.Equal(t, "10001", payload["generator"])
	assert.Equal(t, testutil.UID, payload["scaner"])
	assert.Equal(t, string(common.AuthCodeTypeJoinGroup), payload["type"])
}

// invite=1 的群（需审批）不应通过 authorize 生成 auth_code。
func TestGroupInviteAuthorize_InviteRequired(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-auth-req"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "审批群",
		Creator: "10001",
		Status:  1,
		Invite:  1,
	})
	assert.NoError(t, err)

	code := "test-auth-invite-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请模式")
}

// code 已过期 / 不存在：返回错误。
func TestGroupInviteAuthorize_Expired(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code=does-not-exist-"+util.GenerUUID(), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请链接已过期")
}

// 未登录：AuthMiddleware 直接拦截。
func TestGroupInviteAuthorize_RequiresAuth(t *testing.T) {
	s, _ := testutil.NewTestServer()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code=foo", nil)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// 落地页 HTML 应包含「加入群聊」按钮与 authorize 端点引用，
// 确保前端改动不会被后端模板替换误伤。
func TestGroupInvitePage_ContainsJoinButton(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	_ = New(ctx)

	wd, err := os.Getwd()
	assert.NoError(t, err)
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("chdir to repo root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite?code=join-button-check"))

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.True(t, strings.Contains(body, "加入群聊"))
	assert.True(t, strings.Contains(body, "/v1/group/invite/authorize"))
	assert.True(t, strings.Contains(body, "/scanjoin"))
}

// authorize 必须挂 per-UID 限流（SharedUIDRateLimiter）：每次调用会往 Redis 写
// TTL=30min 的 auth_code，登录用户可能高频批量调用灌满 Redis。契约测试只断言
// 中间件已挂载（X-RateLimit-Scope: uid），不验证具体 rps/burst 数值——
// 那是 pkg/wkhttp/ratelimit_test.go 的职责，此处避开依赖 SharedUIDRateLimiter
// 的进程级单例 + 环境变量带来的耦合。
func TestGroupInviteAuthorize_HasUIDRateLimit(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-auth-ratelimit"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "限流测试群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		AllowExternal: 1,
	})
	assert.NoError(t, err)

	code := "test-auth-ratelimit-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// 证明路由组挂上了 UIDRateLimitMiddleware；X-RateLimit-Scope 由
	// setRateLimitHeaders 在每次放行时写入，属于稳定对外契约。
	assert.Equal(t, "uid", w.Header().Get("X-RateLimit-Scope"))
	assert.NotEmpty(t, w.Header().Get("X-RateLimit-Limit"))
	assert.NotEmpty(t, w.Header().Get("X-RateLimit-Remaining"))
}

// 群属于某 Space 且 allow_external=0 时，detail 接口应提前返回 external_blocked，
// 让 H5 落地页直接给明确提示、藏掉「加入群聊」按钮，而不是等用户点了才被
// groupScanJoin 拒绝。
func TestGroupInviteDetail_ExternalBlocked(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-h5-external"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "内部协作群",
		Creator:       testutil.UID,
		Status:        1,
		Invite:        0,
		SpaceID:       "space-a",
		AllowExternal: 0,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-h5-external-blocked-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "external_blocked", resp["status"])
	assert.Equal(t, groupNo, resp["group_no"])
}

// allow_external=1（默认）或群无 SpaceID 时不应触发 external_blocked。
// 这里确保我们没把默认路径（App 内创建的无 Space 群）误伤。
func TestGroupInviteDetail_NoSpaceNotExternalBlocked(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-h5-no-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "默认群",
		Creator:       testutil.UID,
		Status:        1,
		Invite:        0,
		SpaceID:       "",
		AllowExternal: 0, // 即便 allow_external=0, 只要无 SpaceID 也不触发
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-h5-no-space-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "joinable", resp["status"])
}

// 登录用户已经在群内时，authorize 应返回 already_member=true 并且不写
// Redis auth_code（与 qrcode/api.go handleJoinGroup 的预检对齐，避免
// scanjoin 回「已经在群内」的模糊错误 + 白占 30min TTL）。
func TestGroupInviteAuthorize_AlreadyMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-auth-already"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "已在群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		AllowExternal: 1,
	})
	assert.NoError(t, err)
	// 当前登录用户已经是群成员
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCommon, Version: 1})
	assert.NoError(t, err)

	code := "test-auth-already-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, groupNo, resp["group_no"])
	assert.Equal(t, true, resp["already_member"])
	// already_member 不应生成 auth_code，避免白占 30min Redis TTL
	_, authCodeExists := resp["auth_code"]
	assert.False(t, authCodeExists, "already_member 场景不应返回 auth_code")
}

// 群属于某 Space 且 allow_external=0 时，authorize 应短路返回 external_blocked，
// 不生成 auth_code。这是 H5 版本错位时的兜底路径（正常情况下 detail 已经藏掉按钮）。
func TestGroupInviteAuthorize_ExternalBlocked(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-auth-external"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "Space 内部群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		SpaceID:       "space-a",
		AllowExternal: 0,
	})
	assert.NoError(t, err)

	code := "test-auth-external-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "external_blocked", resp["status"])
	assert.Equal(t, groupNo, resp["group_no"])
	_, authCodeExists := resp["auth_code"]
	assert.False(t, authCodeExists, "external_blocked 场景不应返回 auth_code")
}
