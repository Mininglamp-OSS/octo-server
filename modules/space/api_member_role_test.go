package space

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// memberRoleFixture 建一个空间：testutil.UID 为 ownerRole 角色的登录成员，
// 另插入一个 m-target 成员（targetRole），返回 spaceId。
func memberRoleFixture(t *testing.T, f *Space, spaceId string, ownerRole, targetRole int) {
	t.Helper()
	err := f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    "角色管理测试",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    ownerRole,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     "m-target",
		Role:    targetRole,
		Status:  1,
	})
	assert.NoError(t, err)
}

func putMemberRole(t *testing.T, spaceId, targetUID string, role int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/space/"+spaceId+"/members/"+targetUID+"/role",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"role": role}))))
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	return w
}

// TestUpdateMemberRoleByOwner owner 提升成员为管理员、再降回成员。
func TestUpdateMemberRoleByOwner(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-owner-ok"
	memberRoleFixture(t, f, spaceId, 2, 0)

	w := putMemberRole(t, spaceId, "m-target", 1)
	assert.Equal(t, http.StatusOK, w.Code)
	mem, err := f.db.queryMember(spaceId, "m-target")
	assert.NoError(t, err)
	assert.NotNil(t, mem)
	assert.Equal(t, 1, mem.Role)

	w = putMemberRole(t, spaceId, "m-target", 0)
	assert.Equal(t, http.StatusOK, w.Code)
	mem, err = f.db.queryMember(spaceId, "m-target")
	assert.NoError(t, err)
	assert.Equal(t, 0, mem.Role)
}

// TestUpdateMemberRoleTransferOwner owner 把 role=2 给其他成员触发原子转让：
// 目标变 owner，原 owner 降为管理员。
func TestUpdateMemberRoleTransferOwner(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-transfer"
	memberRoleFixture(t, f, spaceId, 2, 1)

	w := putMemberRole(t, spaceId, "m-target", 2)
	assert.Equal(t, http.StatusOK, w.Code)

	target, err := f.db.queryMember(spaceId, "m-target")
	assert.NoError(t, err)
	assert.Equal(t, 2, target.Role)
	prevOwner, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, 1, prevOwner.Role)
}

// TestUpdateMemberRoleNoPermission 管理员（role=1）无权修改角色，仅 owner 可以。
func TestUpdateMemberRoleNoPermission(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-no-perm"
	memberRoleFixture(t, f, spaceId, 1, 0)

	w := putMemberRole(t, spaceId, "m-target", 1)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
	mem, err := f.db.queryMember(spaceId, "m-target")
	assert.NoError(t, err)
	assert.Equal(t, 0, mem.Role)
}

// TestUpdateMemberRoleOwnerCannotSelfDemote 防无主空间：owner 不能把自己降级，
// 必须通过把其他成员设为 role=2 转让（回归 GH：用户侧缺管理端的 owner-constraint 守卫）。
func TestUpdateMemberRoleOwnerCannotSelfDemote(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-self-demote"
	memberRoleFixture(t, f, spaceId, 2, 0)

	for _, role := range []int{0, 1} {
		w := putMemberRole(t, spaceId, testutil.UID, role)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assertSpaceErrorCode(t, w, "err.server.space.owner_constraint")
	}
	owner, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, 2, owner.Role)
}

// TestUpdateMemberRoleSelfTransferNoop 防无主空间：owner「转让给自己」幂等成功，
// 不得走转让事务把唯一 owner 先升后降为管理员（回归同上）。
func TestUpdateMemberRoleSelfTransferNoop(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-self-transfer"
	memberRoleFixture(t, f, spaceId, 2, 0)

	w := putMemberRole(t, spaceId, testutil.UID, 2)
	assert.Equal(t, http.StatusOK, w.Code)
	owner, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, 2, owner.Role)
}

// TestUpdateMemberRoleIdempotent 目标已是该角色时幂等成功，不报错。
func TestUpdateMemberRoleIdempotent(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-idempotent"
	memberRoleFixture(t, f, spaceId, 2, 1)

	w := putMemberRole(t, spaceId, "m-target", 1)
	assert.Equal(t, http.StatusOK, w.Code)
	mem, err := f.db.queryMember(spaceId, "m-target")
	assert.NoError(t, err)
	assert.Equal(t, 1, mem.Role)
}

// TestTransferOwnerAdminTargetRemoved 防无主空间（并发路径回归，PR #339 review）：
// 转让原语在事务内用 FOR UPDATE 确认目标 status=1；目标已被移除时必须整体
// 回滚返回 ErrTransferTargetMissing，owner 不得被降级。
// 模拟的是「handler pre-check 通过后、事务提交前目标被并发移除」的最终态。
func TestTransferOwnerAdminTargetRemoved(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-transfer-race"
	memberRoleFixture(t, f, spaceId, 2, 1)

	// 目标被移除（status=0），等价于 pre-check 与事务之间被并发踢出
	err = f.db.removeMember(spaceId, "m-target")
	assert.NoError(t, err)

	err = f.db.transferOwnerAdmin(spaceId, "m-target")
	assert.ErrorIs(t, err, ErrTransferTargetMissing)

	// owner 必须保持 role=2，不能出现「目标没升、自己已降」的无主状态
	owner, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, owner)
	assert.Equal(t, 2, owner.Role)
}

// TestUpdateMemberRoleTargetNotFound 目标非空间成员时返回 member_not_found。
func TestUpdateMemberRoleTargetNotFound(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-no-target"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    "角色管理测试",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	w := putMemberRole(t, spaceId, "ghost-user", 1)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.member_not_found")
}
