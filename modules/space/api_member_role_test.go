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
	err = f.db.removeMemberLocked(spaceId, "m-target", 2)
	assert.NoError(t, err)

	err = f.db.transferOwnerAdmin(spaceId, "m-target")
	assert.ErrorIs(t, err, ErrTransferTargetMissing)

	// owner 必须保持 role=2，不能出现「目标没升、自己已降」的无主状态
	owner, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, owner)
	assert.Equal(t, 2, owner.Role)
}

// TestRemoveMemberLockedGuards 移除原语的锁内角色守卫（PR #339 review）：
// owner 拒绝（ErrCannotRemoveOwner）、同级及更高拒绝（ErrRemoveHierarchy）、
// 更低角色移除成功、目标不存在幂等返回 nil。
// 模拟「pre-check 读到低角色后目标被并发提升」的最终态——锁内重读必须兜住。
func TestRemoveMemberLockedGuards(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "remove-locked"
	memberRoleFixture(t, f, spaceId, 2, 1) // testutil.UID=owner, m-target=admin

	// owner 不可移除，无论调用方上限是多少
	err = f.db.removeMemberLocked(spaceId, testutil.UID, 2)
	assert.ErrorIs(t, err, ErrCannotRemoveOwner)
	owner, _ := f.db.queryMember(spaceId, testutil.UID)
	assert.Equal(t, 2, owner.Role)

	// 操作者 admin(1) 移除 admin(1)：同级拒绝
	err = f.db.removeMemberLocked(spaceId, "m-target", 1)
	assert.ErrorIs(t, err, ErrRemoveHierarchy)
	target, _ := f.db.queryMember(spaceId, "m-target")
	assert.NotNil(t, target)

	// 操作者 owner(2) 移除 admin(1)：成功
	err = f.db.removeMemberLocked(spaceId, "m-target", 2)
	assert.NoError(t, err)
	target, _ = f.db.queryMember(spaceId, "m-target")
	assert.Nil(t, target)

	// 目标已不存在：幂等 nil
	err = f.db.removeMemberLocked(spaceId, "m-target", 2)
	assert.NoError(t, err)
}

// TestLeaveSpace 退出空间：普通成员/管理员可退出；owner 必须先转让。
func TestLeaveSpace(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "leave-ok"
	memberRoleFixture(t, f, spaceId, 1, 2) // testutil.UID=admin，m-target=owner 保证空间有主

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/"+spaceId+"/leave", nil)
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	mem, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.Nil(t, mem)
}

// TestLeaveSpaceOwnerRejected owner 退出被 owner_constraint 拒绝。
func TestLeaveSpaceOwnerRejected(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "leave-owner"
	memberRoleFixture(t, f, spaceId, 2, 0)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/"+spaceId+"/leave", nil)
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.owner_constraint")
	owner, _ := f.db.queryMember(spaceId, testutil.UID)
	assert.Equal(t, 2, owner.Role)
}

// TestRemoveMembersSkipsOwnerAndPeers 管理员批量移除：owner 与同级管理员
// 静默跳过（既有语义），更低角色移除成功。
func TestRemoveMembersSkipsOwnerAndPeers(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "remove-batch"
	memberRoleFixture(t, f, spaceId, 1, 2) // testutil.UID=admin 操作者，m-target=owner
	err = f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceId, UID: "m-peer", Role: 1, Status: 1})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceId, UID: "m-low", Role: 0, Status: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/"+spaceId+"/members/remove",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"uids": []string{"m-target", "m-peer", "m-low"},
		}))))
	req.Header.Set("token", testutil.Token)
	testSrv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	ownerMem, _ := f.db.queryMember(spaceId, "m-target")
	assert.NotNil(t, ownerMem, "owner must be skipped")
	assert.Equal(t, 2, ownerMem.Role)
	peer, _ := f.db.queryMember(spaceId, "m-peer")
	assert.NotNil(t, peer, "peer admin must be skipped")
	low, _ := f.db.queryMember(spaceId, "m-low")
	assert.Nil(t, low, "lower role must be removed")
}

// setMemberRoleRaw 测试 fixture 专用：绕过生产侧 updateMemberRole 的
// role<>2 守卫直接改角色，用于构造（含非法的）任意角色状态。
func setMemberRoleRaw(t *testing.T, spaceId, uid string, role int) {
	t.Helper()
	_, err := testSpaceDB.session.Update("space_member").
		Set("role", role).
		Where("space_id=? and uid=?", spaceId, uid).Exec()
	assert.NoError(t, err)
}

// TestUpdateMemberRoleDBGuardSkipsOwner 守卫回归（PR #339 review F1）：
// updateMemberRole 的 WHERE 带 role<>2。模拟「pre-check 读到 role<2 后，
// 目标被并发转让升为 owner」的最终态——降级 UPDATE 必须空转，owner 不被降级。
func TestUpdateMemberRoleDBGuardSkipsOwner(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)
	spaceId := "role-db-guard"
	memberRoleFixture(t, f, spaceId, 2, 0)

	// 并发转让的最终态：m-target 已是 owner（raw 直写，绕过守卫构造场景）
	setMemberRoleRaw(t, spaceId, "m-target", 2)

	// 此刻才落地的降级 UPDATE（pre-check 早已通过）必须被 SQL 守卫挡住
	err = f.db.updateMemberRole(spaceId, "m-target", 1)
	assert.NoError(t, err)
	mem, err := f.db.queryMember(spaceId, "m-target")
	assert.NoError(t, err)
	assert.Equal(t, 2, mem.Role, "owner must not be demoted by the bare update path")
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
