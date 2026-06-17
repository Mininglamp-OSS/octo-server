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
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
	_ = New(ctx)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// 非 group 类型的二维码（如 scanLogin）应返回 expired，避免跨类型数据被透出。
func TestGroupInviteDetail_WrongQRCodeType(t *testing.T) {
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
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

	// YUJ-42: 落地页必须区分 rate-limit / server error / network error 与真正的「已过期」，
	// 避免 detail 返回 429 / 5xx 时把所有用户都误导成「邀请链接已过期」。
	assert.True(t, strings.Contains(body, `id="state-rate-limited"`), "rate-limited state block missing")
	assert.True(t, strings.Contains(body, "请稍后再试"), "rate-limited copy missing")
	assert.True(t, strings.Contains(body, `id="state-server-error"`), "server-error state block missing")
	assert.True(t, strings.Contains(body, `id="state-network-error"`), "network-error state block missing")
	assert.True(t, strings.Contains(body, "r.status === 429"), "detail fetch must special-case 429")
	assert.True(t, strings.Contains(body, "r.status >= 500"), "detail fetch must special-case 5xx")

	// YUJ-59: findTokenAndSid 必须先扫 sessionStorage（IM Web / admin 后台实际写入的位置），
	// 并支持 dm-admin-auth JSON fallback。回归 im-test 观察到的 localStorage-only 漏洞——
	// 历史实现只扫 localStorage，导致登录态读不到、落地页短路成「请先登录」，加群按钮不显示。
	assert.True(t, strings.Contains(body, "scanByPrefix(sessionStorage)"),
		"必须先扫 sessionStorage，否则 IM Web / admin 后台的会话读不到，加群按钮不显示")
	assert.True(t, strings.Contains(body, "dm-admin-auth"),
		"必须支持 admin 后台的 dm-admin-auth JSON 结构作为 fallback")
}

// 已登录用户用公开 code 换取 auth_code：基础路径。
func TestGroupInviteAuthorize_OK(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: authorize 在 loginUID 校验后立即做零 Space 预检，测试需先给
	// testutil.UID 一个 home Space（与任何群所属 Space 都不重合）。
	seedDefaultSpaceForTestUID(t, ctx)

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
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: 需要 testutil.UID 过 Space Gate 才能测 invite 审批分支。
	seedDefaultSpaceForTestUID(t, ctx)

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
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	_ = New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: need_space 早于 expired；想测 expired 需要 UID 先过 Space Gate。
	seedDefaultSpaceForTestUID(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code=does-not-exist-"+util.GenerUUID(), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请链接已过期")
}

// 未登录：AuthMiddleware 直接拦截。
func TestGroupInviteAuthorize_RequiresAuth(t *testing.T) {
	s, _ := newTestServer(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code=foo", nil)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// 落地页 HTML 应包含「加入群聊」按钮与 authorize 端点引用，
// 确保前端改动不会被后端模板替换误伤。
func TestGroupInvitePage_ContainsJoinButton(t *testing.T) {
	s, ctx := newTestServer(t)
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

// Mininglamp-OSS/octo-server#1246: 移动端两端均未注册 dmwork:// scheme，
// H5 落地页上的「打开 App」深链按钮是死链，点击会弹错 / 被微信 block。
// 方案 C 是最小改动——删按钮 + 删绑定 href 的 JS，所以用 grep 测试把
// 这两个字面量钉死：任何人再把按钮加回来都会挂 CI。
func TestGroupInvitePage_NoDmworkScheme(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := newTestServer(t)
	_ = New(ctx)

	wd, err := os.Getwd()
	assert.NoError(t, err)
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("chdir to repo root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite?code=no-dmwork-scheme-check"))

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, "dmwork://",
		"group_invite.html 不应再包含 dmwork:// 深链（两端均未注册该 scheme，点击会弹错）")
	assert.NotContains(t, body, `id="btn-open"`,
		"group_invite.html 不应再包含 btn-open 按钮（死链）")
	assert.NotContains(t, body, "打开 App",
		"group_invite.html 不应再包含「打开 App」按钮文案")
}

// authorize 必须挂 per-UID 限流（SharedUIDRateLimiter）：每次调用会往 Redis 写
// TTL=30min 的 auth_code，登录用户可能高频批量调用灌满 Redis。契约测试只断言
// 中间件已挂载（X-RateLimit-Scope: uid），不验证具体 rps/burst 数值——
// 那是 pkg/wkhttp/ratelimit_test.go 的职责，此处避开依赖 SharedUIDRateLimiter
// 的进程级单例 + 环境变量带来的耦合。
func TestGroupInviteAuthorize_HasUIDRateLimit(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: need_space 早于 rate limit header 写入路径；想测 rate limit
	// 响应头需要 UID 先过 Space Gate 让 handler 正常落到成功分支。
	seedDefaultSpaceForTestUID(t, ctx)

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
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
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
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: need_space 早于 already_member；UID 需先有 home Space。
	seedDefaultSpaceForTestUID(t, ctx)

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

// YUJ-39 回归：invite=1 的群里，已是成员的用户扫码 authorize 应返回
// already_member=true，而不是被 400「邀请模式」短路拦截。
// 这固化「already_member 判定必须早于 invite 判定」的顺序约定，
// 与 qrcode/api.go handleJoinGroup 对齐。
func TestGroupInviteAuthorize_Invite1_AlreadyMember(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: UID 需有 home Space 才能测 invite=1 + already_member 顺序。
	seedDefaultSpaceForTestUID(t, ctx)

	groupNo := "g-invite-auth-invite1-already"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "审批群",
		Creator:       "10001",
		Status:        1,
		Invite:        1, // 开启邀请审批
		AllowExternal: 1,
	})
	assert.NoError(t, err)
	// 当前登录用户已经是群成员
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCommon, Version: 1})
	assert.NoError(t, err)

	code := "test-auth-invite1-already-code"
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
	assert.NotContains(t, w.Body.String(), "邀请模式",
		"已是成员的用户不应被 invite 模式 400 拦截")
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, groupNo, resp["group_no"])
	assert.Equal(t, true, resp["already_member"])
	_, authCodeExists := resp["auth_code"]
	assert.False(t, authCodeExists, "already_member 场景不应返回 auth_code")
}

// YUJ-39 回归：invite=1 的群里，非成员扫码 authorize 仍应被 400「邀请模式」
// 拦截（与 TestGroupInviteAuthorize_InviteRequired 互为对照：这里显式断言
// 当 already_member 判定 miss 时，invite 判定仍然生效，顺序重排不影响非成员路径）。
func TestGroupInviteAuthorize_Invite1_NonMember(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: UID 需有 home Space 才能测 invite=1 非成员 400 拦截。
	seedDefaultSpaceForTestUID(t, ctx)

	groupNo := "g-invite-auth-invite1-nonmember"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "审批群",
		Creator:       "10001",
		Status:        1,
		Invite:        1,
		AllowExternal: 1,
	})
	assert.NoError(t, err)
	// 注意：不插入 testutil.UID 为成员 —— 当前登录用户是非成员

	code := "test-auth-invite1-nonmember-code"
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

// 群属于某 Space 且 allow_external=0 时，authorize 应短路返回 external_blocked，
// 不生成 auth_code。这是 H5 版本错位时的兜底路径（正常情况下 detail 已经藏掉按钮）。
func TestGroupInviteAuthorize_ExternalBlocked(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: UID 需在独立 home Space（与群的 space-a 不重合），
	// 才能维持「有 Space 但跨 Space」语义，否则会被 need_space 先短路。
	seedDefaultSpaceForTestUID(t, ctx)

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

// 跨 Space 用户扫 AllowExternal=0 群邀请码：authorize 短路返回 external_blocked。
// 这显式覆盖「登录用户不是该 Space 成员」的分支（回归 YUJ-38 修复）。
func TestGroupInviteAuthorize_ExternalBlocked_NonSpaceMember(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	// GH #1319: 让 UID 有自己的 home Space（与群所属 space-yuj38-cross 不同），
	// 这样仍然是「登录 + 跨 Space」场景而不是零 Space，能继续断言 external_blocked。
	seedDefaultSpaceForTestUID(t, ctx)

	spaceID := "space-yuj38-cross"
	// 只给其他用户授予 space_member，当前登录用户（testutil.UID）不是成员。
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "空间 A", "10001", 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, "10001", 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-invite-auth-cross-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "Space 内部群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 0,
	})
	assert.NoError(t, err)

	code := "test-auth-cross-space-code"
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
	assert.False(t, authCodeExists, "跨 Space 用户不应拿到 auth_code")
}

// 同 Space 成员扫 AllowExternal=0 群邀请码：authorize 不再误杀为 external_blocked，
// 应继续走正常流程并生成 auth_code（回归 YUJ-38 Critical 修复）。
func TestGroupInviteAuthorize_SameSpaceMemberNotBlocked(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-yuj38-same"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "空间 A", "10001", 1).Exec()
	assert.NoError(t, err)
	// 当前登录用户 testutil.UID 以及群主 10001 都是该 Space 成员。
	for _, uid := range []string{testutil.UID, "10001"} {
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(spaceID, uid, 0, 1).Exec()
		assert.NoError(t, err)
	}

	groupNo := "g-invite-auth-same-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "Space 内部群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 0,
	})
	assert.NoError(t, err)

	code := "test-auth-same-space-code"
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
	// 同 Space 成员不应该被 external_blocked 误杀。
	if status, ok := resp["status"].(string); ok {
		assert.NotEqual(t, "external_blocked", status, "同 Space 成员不应被 external_blocked")
	}
	assert.Equal(t, groupNo, resp["group_no"])
	authCode, _ := resp["auth_code"].(string)
	assert.NotEmpty(t, authCode, "同 Space 成员应拿到 auth_code 正常入群")

	// 校验 auth_code 写入 Redis（与 TestGroupInviteAuthorize_OK 同样的 shape）。
	cached, err := ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	assert.NoError(t, err)
	var payload map[string]interface{}
	assert.NoError(t, json.Unmarshal([]byte(cached), &payload))
	assert.Equal(t, groupNo, payload["group_no"])
	assert.Equal(t, testutil.UID, payload["scaner"])
	assert.Equal(t, string(common.AuthCodeTypeJoinGroup), payload["type"])
}
