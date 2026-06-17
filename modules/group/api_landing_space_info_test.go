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

// YUJ-168 / GH octo-server#1243: 外部群 H5 邀请 / 加群 landing 页信任锚点。
//
// 契约：
//   - groupDetail (/v1/groups/:group_no/detail)
//   - groupInviteDetail (/v1/group/invite/detail 公开)
//   - groupMemberInviteDetail (/v1/group/invites/:invite_no)
//   必须在响应里附带 space_name + is_external，供 join_group.html /
//   invite_detail.html / group_invite.html 渲染"来自 ${space_name}" +「外部」徽标。
//
// 字段语义（保持三端视图一致）：
//   - space_name: 群所属 Space 的 name；群无 SpaceID 时为空字符串
//   - is_external:
//        0 = 无 Space、未登录、或登录者是该 Space 成员（内部视角 / 无锚点）
//        1 = 登录者存在但不是该 Space 成员（跨 Space 外部视角，渲染徽标）

// ---------- /v1/groups/:group_no/detail ----------

// 群挂在 Space 下，登录者本身是该 Space 成员 → 只显示 space_name，不标外部。
func TestGroupDetailGet_WithSpace_SameSpaceMember(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-landing-same"
	spaceName := "研发空间"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, spaceName, testutil.UID, 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-detail-same-space"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "Space 内部群",
		Creator: testutil.UID,
		Status:  1,
		SpaceID: spaceID,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/detail", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, spaceName, resp["space_name"], "space_name 应来自 JOIN space 表")
	assert.EqualValues(t, 0, resp["is_external"], "同 Space 成员视角 is_external=0")
}

// 群挂在 Space 下但登录者不在该 Space 成员表中 → is_external=1，前端渲染外部徽标。
func TestGroupDetailGet_WithSpace_CrossSpaceViewer(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-landing-cross"
	spaceName := "外部空间"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, spaceName, "10001", 1).Exec()
	assert.NoError(t, err)
	// 群主 10001 是 Space 成员，但登录者 testutil.UID 不是。
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, "10001", 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-detail-cross-space"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "外部可见群",
		Creator: "10001",
		Status:  1,
		SpaceID: spaceID,
	})
	assert.NoError(t, err)
	// 访问者本身需要是群成员，groupDetailGet 会先校验群成员身份。
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCommon, Version: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "10001", Role: MemberRoleCreator, Version: 2})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/detail", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, spaceName, resp["space_name"])
	assert.EqualValues(t, 1, resp["is_external"], "跨 Space 登录者视角 is_external=1")
}

// 群无 SpaceID（App 内创建的普通群） → space_name="" / is_external=0，前端完全不渲染。
func TestGroupDetailGet_NoSpace(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-detail-no-space"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "普通群",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/detail", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["space_name"], "无 Space 时 space_name 为空")
	assert.EqualValues(t, 0, resp["is_external"])
}

// ---------- /v1/group/invite/detail （公开 H5 预览，未登录） ----------

// 公开接口永远没有登录态，is_external 只会是 0；但 space_name 仍然要下发。
func TestGroupInviteDetail_IncludesSpaceName_Anonymous(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-invite-anon"
	spaceName := "公开预览 Space"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, spaceName, testutil.UID, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-invite-detail-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "邀请预览群",
		Creator:       testutil.UID,
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 1, // 避免触发 external_blocked 分支
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-invite-detail-space-anon"
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
	assert.Equal(t, spaceName, resp["space_name"])
	assert.EqualValues(t, 0, resp["is_external"], "公开接口无登录态 → is_external=0")
}

// 群无 Space 时公开接口返回的 space_name 应为空字符串，前端不渲染 Space 信息。
func TestGroupInviteDetail_NoSpace(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-detail-no-space"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "普通群",
		Creator: testutil.UID,
		Status:  1,
		Invite:  0,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-invite-detail-no-space"
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
	assert.Equal(t, "", resp["space_name"])
	assert.EqualValues(t, 0, resp["is_external"])
}

// ---------- /v1/group/invites/:invite_no （登录邀请详情） ----------

// 群挂 Space，登录的被邀请者不是该 Space 成员 → is_external=1 + space_name 下发。
func TestGroupMemberInviteDetail_CrossSpaceViewer(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-invite-member-cross"
	spaceName := "目标 Space"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, spaceName, "10001", 1).Exec()
	assert.NoError(t, err)
	// 仅邀请者 10001 在 Space 内，testutil.UID 作为跨 Space 被邀请者。
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, "10001", 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-invite-member-cross"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "目标群",
		Creator: "10001",
		Status:  1,
		SpaceID: spaceID,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "10001", Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	inviteNo := "inv-landing-cross-space"
	_, err = ctx.DB().InsertInto("group_invite").
		Columns("invite_no", "group_no", "inviter", "remark", "status").
		Values(inviteNo, groupNo, "10001", "邀请加入", 0).Exec()
	assert.NoError(t, err)
	// 把 testutil.UID 放在 invite_item 里，使其成为"被邀请者"从而有权查看。
	_, err = ctx.DB().InsertInto("invite_item").
		Columns("invite_no", "group_no", "inviter", "uid", "status").
		Values(inviteNo, groupNo, "10001", testutil.UID, 0).Exec()
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/group/invites/"+inviteNo, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, inviteNo, resp["invite_no"])
	assert.Equal(t, spaceName, resp["space_name"])
	assert.EqualValues(t, 1, resp["is_external"], "跨 Space 被邀请者视角 is_external=1")
}

// 群无 Space 时邀请详情 space_name 为空，is_external 为 0。
func TestGroupMemberInviteDetail_NoSpace(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-invite-member-no-space"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "普通群",
		Creator: "10001",
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "10001", Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	inviteNo := "inv-landing-no-space"
	_, err = ctx.DB().InsertInto("group_invite").
		Columns("invite_no", "group_no", "inviter", "status").
		Values(inviteNo, groupNo, "10001", 0).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("invite_item").
		Columns("invite_no", "group_no", "inviter", "uid", "status").
		Values(inviteNo, groupNo, "10001", testutil.UID, 0).Exec()
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/group/invites/"+inviteNo, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["space_name"])
	assert.EqualValues(t, 0, resp["is_external"])
}
