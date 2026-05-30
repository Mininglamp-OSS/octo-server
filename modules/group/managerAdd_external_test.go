package group

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// TestManagerAdd_RejectsExternalMember 验证 creator 试图将外部成员 (is_external=1)
// 提拔为群管理员时，前置 is_external 校验返回 403
// (YUJ-231 / GH#1289，ReviewBot YUJ-230 P1)。
//
// 构造：creator + 1 个内部成员 + 1 个外部成员；creator 一次请求提拔两人。
// 期望：返回 403 且拒绝写入（两人角色都保持 common）。
func TestManagerAdd_RejectsExternalMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-yuj231-mgradd"

	// internal member
	err = f.userDB.Insert(&user.Model{UID: "internal-target", Name: "internal-target", ShortNo: "yuj231_int"})
	assert.NoError(t, err)
	// external member
	err = f.userDB.Insert(&user.Model{UID: "external-target", Name: "external-target", ShortNo: "yuj231_ext"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "yuj231 managerAdd external test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	// creator (loginUID)
	err = f.db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        testutil.UID,
		Role:       MemberRoleCreator,
		IsExternal: 0,
		Version:    1,
	})
	assert.NoError(t, err)

	// internal target: is_external=0
	err = f.db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "internal-target",
		Role:       MemberRoleCommon,
		IsExternal: 0,
		Version:    2,
	})
	assert.NoError(t, err)

	// external target: is_external=1 — 必须被拦截
	err = f.db.InsertMember(&MemberModel{
		GroupNo:       groupNo,
		UID:           "external-target",
		Role:          MemberRoleCommon,
		IsExternal:    1,
		SourceSpaceID: "spaceX",
		Version:       3,
	})
	assert.NoError(t, err)

	// Body 直接是 []string（与 managerAdd 的 c.BindJSON(&memberUIDs) 契约一致）
	body := util.ToJson([]string{"internal-target", "external-target"})
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/"+groupNo+"/managers", bytes.NewReader([]byte(body)))
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	// D14: wire status 固定 400；403 语义落在 error.http_status / error.code。
	assert.Equal(t, http.StatusBadRequest, w.Code, "外部成员应被拦截（400 信封），body=%s", w.Body.String())
	assert.True(t, strings.Contains(w.Body.String(), "err.server.group.external_cannot_be_admin"),
		"响应缺少拒绝错误码，body=%s", w.Body.String())

	// 双重保险：确认两个目标用户 role 都没被改动（事务未执行）
	internalAfter, err := f.db.QueryMemberWithUID("internal-target", groupNo)
	assert.NoError(t, err)
	if assert.NotNil(t, internalAfter) {
		assert.Equal(t, MemberRoleCommon, internalAfter.Role,
			"请求整体应失败，internal-target 也不应被提拔")
	}
	externalAfter, err := f.db.QueryMemberWithUID("external-target", groupNo)
	assert.NoError(t, err)
	if assert.NotNil(t, externalAfter) {
		assert.Equal(t, MemberRoleCommon, externalAfter.Role)
		assert.Equal(t, 1, externalAfter.IsExternal)
	}
}

// TestTransferGrouper_RejectsExternalMember 验证群主试图将群主位转让给
// 外部成员时，返回 403。防止外部成员获得 creator 角色从而取得所有敏感操作权限
// (YUJ-231 / GH#1289)。
func TestTransferGrouper_RejectsExternalMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-yuj231-transfer"

	err = f.userDB.Insert(&user.Model{UID: "external-new-owner", Name: "external-new-owner", ShortNo: "yuj231_xfer"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "yuj231 transferGrouper external test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	// current creator
	err = f.db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        testutil.UID,
		Role:       MemberRoleCreator,
		IsExternal: 0,
		Version:    1,
	})
	assert.NoError(t, err)

	// 外部成员
	err = f.db.InsertMember(&MemberModel{
		GroupNo:       groupNo,
		UID:           "external-new-owner",
		Role:          MemberRoleCommon,
		IsExternal:    1,
		SourceSpaceID: "spaceY",
		Version:       2,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/"+groupNo+"/transfer/external-new-owner", nil)
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	// D14: wire status 固定 400；403 语义落在 error.http_status / error.code。
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"转让给外部成员应被拦截（400 信封），body=%s", w.Body.String())
	assert.True(t, strings.Contains(w.Body.String(), "err.server.group.external_cannot_be_owner"),
		"响应缺少拒绝错误码，body=%s", w.Body.String())

	// creator 未易主，外部成员仍是 common
	creatorAfter, err := f.db.QueryMemberWithUID(testutil.UID, groupNo)
	assert.NoError(t, err)
	if assert.NotNil(t, creatorAfter) {
		assert.Equal(t, MemberRoleCreator, creatorAfter.Role)
	}
	externalAfter, err := f.db.QueryMemberWithUID("external-new-owner", groupNo)
	assert.NoError(t, err)
	if assert.NotNil(t, externalAfter) {
		assert.Equal(t, MemberRoleCommon, externalAfter.Role)
	}
}
