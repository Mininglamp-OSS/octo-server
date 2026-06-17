package group

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// YUJ-39: groupInviteDetail（公开 H5 预览端点）引入可选鉴权。
//
// 背景：PR#1174 已让 groupInviteAuthorize 对同 Space 成员放行 external_blocked，
// 但 detail 端点仍无条件返回 external_blocked（因为它本就是公开端点），
// 导致 H5 前端根据 detail.status 隐藏「加入群聊」按钮——同 Space 成员
// 明明能入群，却完全看不到按钮，必须整端点走一次 authorize 才能发现。
//
// 方案 A：detail 支持可选鉴权。request 带合法 token 时解析 loginUID，
// 当 loginUID 属于群的 Space → 返回 joinable / invite_required，
// 和 authorize 的放行路径语义一致，前端按钮就会显示出来。
//
// 契约（必须全部成立，顺序独立）：
//   1. 无 token + Space 群 + AllowExternal=0 → external_blocked（保持公开
//      预览旧行为，本文件不重复覆盖，已有 TestGroupInviteDetail_ExternalBlocked）。
//   2. 合法 token + 同 Space 成员 → joinable + is_external=0（本文件覆盖）。
//   3. 合法 token + 跨 Space 登录者 → external_blocked + is_external=1（本文件覆盖）。
//   4. token 无效 / 不在缓存 → 视同匿名 → external_blocked + is_external=0
//      （降级路径，不得因为脏 token 崩溃，本文件覆盖）。

// 合法 token + 同 Space 成员：detail 应该返回 joinable，让前端显示加入按钮。
// 这是 YUJ-39 的核心正向路径——修复 PR#1174 遗留 UX 短板。
func TestGroupInviteDetail_OptionalAuth_SameSpaceMember_Joinable(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-yuj39-same"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "研发空间", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	// 当前登录用户 testutil.UID 是 Space 成员
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-invite-detail-same-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "Space 内部群",
		Creator:       testutil.UID,
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 0, // 关键：如果走公开预览会被 external_blocked
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-yuj39-same-space-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	// 构造带 token 的请求（可选鉴权路径）
	req := newInviteRequest(t, "/v1/group/invite/detail?code="+code)
	req.Header.Set("token", testutil.Token)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "joinable", resp["status"],
		"同 Space 成员应看到 joinable，让前端显示加入按钮（YUJ-39 核心修复）")
	assert.EqualValues(t, 0, resp["is_external"], "同 Space 成员视角 is_external=0")
}

// 合法 token + 同 Space 成员 + invite=1：detail 应该返回 invite_required，
// 而不是被 external_blocked 盖住。验证可选鉴权后 invite_required 状态也能正常透出。
func TestGroupInviteDetail_OptionalAuth_SameSpaceMember_InviteRequired(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-yuj39-same-invite"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "审批空间", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-invite-detail-same-invite"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "审批 + 内部群",
		Creator:       testutil.UID,
		Status:        1,
		Invite:        1, // 开启邀请审批
		SpaceID:       spaceID,
		AllowExternal: 0,
	})
	assert.NoError(t, err)

	code := "test-yuj39-same-space-invite-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	req := newInviteRequest(t, "/v1/group/invite/detail?code="+code)
	req.Header.Set("token", testutil.Token)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "invite_required", resp["status"],
		"同 Space 成员的审批群应返回 invite_required，不得被 external_blocked 覆盖")
	assert.EqualValues(t, 0, resp["is_external"])
}

// 合法 token + 跨 Space 访问者：detail 仍返回 external_blocked 并标记 is_external=1。
// 回归保护：可选鉴权不得让跨 Space 用户错误获得 joinable。
func TestGroupInviteDetail_OptionalAuth_CrossSpace_Blocked(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// GH #1319: 让 testutil.UID 有自己的 home Space（与群所属 space-yuj39-cross 不同），
	// 这样仍然是「登录 + 跨 Space」场景而不是零 Space，能继续断言 external_blocked
	// 不被可选鉴权放行。零 Space 路径由 TestGroupInviteDetail_NeedSpace_OptionalAuth 覆盖。
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values("space-yuj39-cross-home-for-uid", "uid-home", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values("space-yuj39-cross-home-for-uid", testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)

	spaceID := "space-yuj39-cross"
	// 只有 10001 是 Space 成员，当前登录用户 testutil.UID 不是
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "外部空间", "10001", 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, "10001", 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-invite-detail-cross-space"
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

	code := "test-yuj39-cross-space-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	req := newInviteRequest(t, "/v1/group/invite/detail?code="+code)
	req.Header.Set("token", testutil.Token)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "external_blocked", resp["status"],
		"跨 Space 登录者不得被可选鉴权放行")
	assert.EqualValues(t, 1, resp["is_external"],
		"跨 Space 登录者视角 is_external=1，前端渲染外部徽标")
}

// 无效 / 过期 token：detail 应降级为匿名路径（external_blocked + is_external=0），
// 不得因为脏 token 崩溃或泄漏敏感状态。
func TestGroupInviteDetail_OptionalAuth_InvalidToken_Degrades(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-detail-bad-token"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "Space 内部群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		SpaceID:       "space-yuj39-bad-token",
		AllowExternal: 0,
	})
	assert.NoError(t, err)

	code := "test-yuj39-bad-token-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	req := newInviteRequest(t, "/v1/group/invite/detail?code="+code)
	// 伪造 token：没有在 Cache 里 Set，Cache.Get 会返回空串
	req.Header.Set("token", "definitely-not-a-real-token")

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "external_blocked", resp["status"],
		"脏 token 应降级为匿名预览路径，不得让非登录者绕过 external_blocked")
	assert.EqualValues(t, 0, resp["is_external"],
		"无效 token 等价于未登录，is_external=0")
}
