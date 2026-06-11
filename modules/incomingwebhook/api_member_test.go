package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 成员权限模型测试（#member-perms）：
//   - 任意（内部、正常状态）群成员可创建 webhook：名称可自定义（缺省自动命名
//     Webhook-xxxxxx），头像仅管理员可设置/修改；
//   - update/delete/regenerate/deliveries/test：创建者或群管理员；其他成员 403；
//   - list：任意成员只读可见（token 永不回显）；
//   - 普通成员受 per-creator 配额约束（管理员豁免，仅受群级配额）；
//   - 创建者退群：webhook 不可被启用/换 token/测试推送（409），push 懒级联禁用。

const (
	memberAUID   = "iwh_member_a"
	memberAToken = "iwh_member_a_token"
	memberBUID   = "iwh_member_b"
	memberBToken = "iwh_member_b_token"
)

var autoNameRe = regexp.MustCompile(`^Webhook-[0-9a-f]{6}$`)

// seedLoginUser 给指定 uid 造一个可过 AuthMiddleware 的登录 token（与
// testutil.NewTestServer 给 testutil.UID 灌 token 的方式一致）。
func seedLoginUser(t *testing.T, ctx *config.Context, uid, token string) {
	t.Helper()
	err := ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+token, uid+"@test")
	require.NoError(t, err)
}

// seedGroupMember 把 uid 以指定角色插入群成员表（内部成员、正常状态）。
func seedGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, ?, 1, 0, 1)",
		groupNo, uid, role).Exec()
	require.NoError(t, err)
}

// removeGroupMember 软删群成员（退群/被移出的 DB 形态）。
func removeGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?", groupNo, uid).Exec()
	require.NoError(t, err)
}

// userReq 与 authReq 相同，但允许指定登录 token（第二/第三用户）。
func userReq(method, path string, body interface{}, token string) *http.Request {
	req := authReq(method, path, body)
	req.Header.Set("token", token)
	return req
}

// setupMemberEnv 在 setupTestEnv 基础上再造两个普通成员 A/B（各自带登录态）。
func setupMemberEnv(t *testing.T) (http.Handler, *config.Context, string) {
	handler, ctx, groupNo := setupTestEnv(t)
	seedLoginUser(t, ctx, memberAUID, memberAToken)
	seedLoginUser(t, ctx, memberBUID, memberBToken)
	seedGroupMember(t, ctx, groupNo, memberAUID, 0)
	seedGroupMember(t, ctx, groupNo, memberBUID, 0)
	return handler, ctx, groupNo
}

// memberCreate 以成员 A 创建一个 webhook，返回解析后的响应体。
func memberCreate(t *testing.T, handler http.Handler, groupNo string) map[string]interface{} {
	t.Helper()
	w := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{}, memberAToken))
	require.Equalf(t, http.StatusOK, w.Code, "member create body: %s", w.Body.String())
	return parseJSON(t, w)
}

// ============================================================
// 创建
// ============================================================

// 普通成员可创建：名称缺省时自动命名 Webhook-xxxxxx、creator 记为本人；
// 自定义名称同样允许。
func TestMemberCreate_AutoNameWhenOmitted(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := memberCreate(t, handler, groupNo)

	name, _ := created["name"].(string)
	assert.Regexp(t, autoNameRe, name)
	assert.Equal(t, memberAUID, created["creator_uid"])
	assert.NotEmpty(t, created["token"])
	// 自动名的后缀必须来自 webhook_id（可追溯、确定性）。
	whID, _ := created["webhook_id"].(string)
	assert.Contains(t, whID, name[len("Webhook-"):])

	// 自定义名称 → 200，落库时强制带 "Webhook-" 前缀（防冒充真实成员/部门）。
	w := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"name": "my ci bot"}, memberAToken))
	require.Equalf(t, http.StatusOK, w.Code, "named create body: %s", w.Body.String())
	assert.Equal(t, "Webhook-my ci bot", parseJSON(t, w)["name"])

	// 已带前缀的名称不被二次加前缀（幂等）。
	w = do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"name": "Webhook-already"}, memberAToken))
	require.Equalf(t, http.StatusOK, w.Code, "prefixed create body: %s", w.Body.String())
	assert.Equal(t, "Webhook-already", parseJSON(t, w)["name"])

	// 恰好等于裸前缀（无有效内容）视同未填 → 自动命名。
	w = do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"name": "Webhook-"}, memberAToken))
	require.Equalf(t, http.StatusOK, w.Code, "bare-prefix create body: %s", w.Body.String())
	assert.Regexp(t, autoNameRe, parseJSON(t, w)["name"])
}

// 成员的 webhook push 时 username/avatar_url 覆盖被忽略（创建者非管理员 →
// resolveFromIdentity 判权关闭），推送本身仍成功——管理面的前缀/头像限制不可被
// push 路径绕过（PR #340 review，yujiawei P1）。展示字段的口径由 in-package 的
// TestResolveFromIdentity 钉住，这里钉 E2E 不拒绝（覆盖被静默忽略而非 4xx）。
func TestPush_MemberWebhookOverrideSilentlyIgnored(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := memberCreate(t, handler, groupNo)
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL,
		[]byte(`{"content":"hi","username":"HR 公告","avatar_url":"https://evil.example.com/ceo.png"}`)))
	assert.NotEqualf(t, http.StatusUnauthorized, pw.Code, "push body: %s", pw.Body.String())
	assert.NotEqualf(t, http.StatusBadRequest, pw.Code, "push body: %s", pw.Body.String())
}

// 普通成员自定义头像 → 400（头像仅管理员可设置）。
func TestMemberCreate_RejectsCustomAvatar(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	w := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"avatar": "https://example.com/x.png"}, memberAToken))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

// 非群成员 → 403。
func TestMemberCreate_NonMemberForbidden(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	removeGroupMember(t, ctx, groupNo, memberAUID)
	w := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{}, memberAToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// 外部成员（is_external=1）→ 403（与管理员判定的 fail-safe 口径一致）。
func TestMemberCreate_ExternalMemberForbidden(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_external=1 WHERE group_no=? AND uid=?", groupNo, memberAUID).Exec()
	require.NoError(t, err)
	w := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{}, memberAToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// 普通成员 per-creator 配额：达到上限 → 409；管理员不受 per-creator 配额限制。
func TestMemberCreate_PerCreatorQuota(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_MAX_PER_CREATOR", "2")
	handler, _, groupNo := setupMemberEnv(t)

	for i := 0; i < 2; i++ {
		memberCreate(t, handler, groupNo)
	}
	w := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{}, memberAToken))
	assert.Equalf(t, http.StatusConflict, w.Code, "3rd member create body: %s", w.Body.String())

	// 另一个成员 B 配额独立。
	w = do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{}, memberBToken))
	assert.Equalf(t, http.StatusOK, w.Code, "member B create body: %s", w.Body.String())

	// 管理员（testutil.UID 是群主）超过 per-creator 上限仍可建（只受群级配额）。
	for i := 0; i < 3; i++ {
		w = do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
			map[string]interface{}{"name": fmt.Sprintf("admin-wh-%d", i)}))
		assert.Equalf(t, http.StatusOK, w.Code, "admin create #%d body: %s", i, w.Body.String())
	}
}

// ============================================================
// 管理（所有权）
// ============================================================

// 成员只能动自己创建的：B 对 A 的 webhook update/delete/regenerate/deliveries/test
// 一律 403；list 对 B 仍可见（只读、不回显 token）。
func TestMemberManage_OwnOnly(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := memberCreate(t, handler, groupNo)
	whID := created["webhook_id"].(string)
	base := fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID)

	cases := []struct {
		method, path string
		body         interface{}
	}{
		{"PUT", base, map[string]interface{}{"status": 0}},
		{"DELETE", base, nil},
		{"POST", base + "/regenerate", nil},
		{"GET", base + "/deliveries", nil},
		{"POST", base + "/test", nil},
	}
	for _, tc := range cases {
		w := do(handler, userReq(tc.method, tc.path, tc.body, memberBToken))
		assert.Equalf(t, http.StatusForbidden, w.Code, "%s %s body: %s", tc.method, tc.path, w.Body.String())
	}

	// B list 可见 A 的 webhook，且响应不含 token。
	lw := do(handler, userReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil, memberBToken))
	require.Equalf(t, http.StatusOK, lw.Code, "list body: %s", lw.Body.String())
	assert.Contains(t, lw.Body.String(), whID)
	assert.NotContains(t, lw.Body.String(), created["token"].(string))

	// A 自己可以禁用/启用、换 token、看 deliveries。
	w := do(handler, userReq("PUT", base, map[string]interface{}{"status": 0}, memberAToken))
	assert.Equalf(t, http.StatusOK, w.Code, "self disable body: %s", w.Body.String())
	w = do(handler, userReq("PUT", base, map[string]interface{}{"status": 1}, memberAToken))
	assert.Equalf(t, http.StatusOK, w.Code, "self enable body: %s", w.Body.String())
	w = do(handler, userReq("POST", base+"/regenerate", nil, memberAToken))
	assert.Equalf(t, http.StatusOK, w.Code, "self regenerate body: %s", w.Body.String())
	w = do(handler, userReq("GET", base+"/deliveries", nil, memberAToken))
	assert.Equalf(t, http.StatusOK, w.Code, "self deliveries body: %s", w.Body.String())
}

// 创建者可改自己 webhook 的名称；头像仅管理员可改 → 成员 400。
func TestMemberUpdate_NameAllowedAvatarRejected(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	whID := memberCreate(t, handler, groupNo)["webhook_id"].(string)
	base := fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID)

	w := do(handler, userReq("PUT", base, map[string]interface{}{"name": "deploy-bot"}, memberAToken))
	require.Equalf(t, http.StatusOK, w.Code, "rename body: %s", w.Body.String())
	assert.Equal(t, "Webhook-deploy-bot", parseJSON(t, w)["name"])

	w = do(handler, userReq("PUT", base, map[string]interface{}{"avatar": "https://x/y.png"}, memberAToken))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "avatar body: %s", w.Body.String())

	// 管理员可改任意 webhook 的头像。
	w = do(handler, authReq("PUT", base, map[string]interface{}{"avatar": "https://x/admin.png"}))
	assert.Equalf(t, http.StatusOK, w.Code, "admin avatar body: %s", w.Body.String())
}

// 群主/管理员可管理任意成员创建的 webhook（改名、删除）。
func TestAdmin_ManagesMemberWebhook(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	whID := memberCreate(t, handler, groupNo)["webhook_id"].(string)
	base := fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID)

	w := do(handler, authReq("PUT", base, map[string]interface{}{"name": "ops-renamed"}))
	require.Equalf(t, http.StatusOK, w.Code, "admin rename body: %s", w.Body.String())
	assert.Equal(t, "ops-renamed", parseJSON(t, w)["name"])

	w = do(handler, authReq("DELETE", base, nil))
	assert.Equalf(t, http.StatusOK, w.Code, "admin delete body: %s", w.Body.String())
}

// ============================================================
// 创建者退群
// ============================================================

// 创建者退群后：启用 / regenerate / 测试推送被 409 拒绝（删除、禁用仍可）。
func TestCreatorLeft_BlocksEnableRegenerateTest(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	whID := memberCreate(t, handler, groupNo)["webhook_id"].(string)
	base := fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID)

	// 管理员先禁用，再移除创建者。
	w := do(handler, authReq("PUT", base, map[string]interface{}{"status": 0}))
	require.Equalf(t, http.StatusOK, w.Code, "disable body: %s", w.Body.String())
	removeGroupMember(t, ctx, groupNo, memberAUID)

	w = do(handler, authReq("PUT", base, map[string]interface{}{"status": 1}))
	assert.Equalf(t, http.StatusConflict, w.Code, "enable body: %s", w.Body.String())
	w = do(handler, authReq("POST", base+"/regenerate", nil))
	assert.Equalf(t, http.StatusConflict, w.Code, "regenerate body: %s", w.Body.String())
	w = do(handler, authReq("POST", base+"/test", nil))
	assert.Equalf(t, http.StatusConflict, w.Code, "test body: %s", w.Body.String())

	// 管理员仍可删除（清理路径不被堵死）。
	w = do(handler, authReq("DELETE", base, nil))
	assert.Equalf(t, http.StatusOK, w.Code, "delete body: %s", w.Body.String())
}

// 创建者退群后 push 一律 401，且 webhook 被懒级联禁用（status→0）；创建者重新入群
// 并由管理员重新启用后恢复可推送。
func TestPush_CreatorLeft_LazyDisable(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	created := memberCreate(t, handler, groupNo)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)

	removeGroupMember(t, ctx, groupNo, memberAUID)

	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi"}`)))
	assert.Equalf(t, http.StatusUnauthorized, pw.Code, "push body: %s", pw.Body.String())

	// 懒级联禁用：行状态翻为禁用（0）。
	var status int
	_, err := ctx.DB().SelectBySql(
		"SELECT status FROM incoming_webhook WHERE webhook_id=?", whID).Load(&status)
	require.NoError(t, err)
	assert.Equal(t, 0, status, "creator-left push must lazily disable the webhook")

	// 重新入群 + 管理员重新启用 → 推送恢复（非 401）。
	_, err = ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted=0 WHERE group_no=? AND uid=?", groupNo, memberAUID).Exec()
	require.NoError(t, err)
	w := do(handler, authReq("PUT",
		fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"status": 1}))
	require.Equalf(t, http.StatusOK, w.Code, "re-enable body: %s", w.Body.String())
	pw = do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi again"}`)))
	assert.NotEqualf(t, http.StatusUnauthorized, pw.Code, "push after re-enable body: %s", pw.Body.String())
}
