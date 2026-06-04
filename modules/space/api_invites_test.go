package space

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// seedInvite 插入一条邀请码并返回 code
func seedInvite(t *testing.T, spaceId, code, creator string, status int, expiresAt *time.Time) {
	t.Helper()
	inv := &InvitationModel{
		SpaceId:    spaceId,
		InviteCode: code,
		Creator:    creator,
		MaxUses:    10,
		Status:     status,
	}
	if expiresAt != nil {
		t := db.Time(*expiresAt)
		inv.ExpiresAt = &t
	}
	assert.NoError(t, testSpaceDB.insertInvitation(inv))
}

// seedSpaceWithMember 插入空间 + 指定 uid 作为某 role 的成员
func seedSpaceWithMember(t *testing.T, spaceId, uid string, role int) {
	t.Helper()
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    spaceId,
		Creator: uid,
		Status:  SpaceStatusNormal,
	}))
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     uid,
		Role:    role,
		Status:  1,
	}))
}

// TestListInvites_DefaultFiltersDisabledAndExpired: 默认只返回有效邀请码（status=1 且未过期）。
func TestListInvites_DefaultFiltersDisabledAndExpired(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-list-default"
	seedSpaceWithMember(t, spaceId, testutil.UID, 2) // owner

	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(24 * time.Hour)
	seedInvite(t, spaceId, "list-active-1", testutil.UID, 1, &future)
	seedInvite(t, spaceId, "list-active-2", testutil.UID, 1, nil) // 永不过期
	seedInvite(t, spaceId, "list-disabled", testutil.UID, 0, nil)
	seedInvite(t, spaceId, "list-expired", testutil.UID, 1, &past)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/invites", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			InviteCode string `json:"invite_code"`
			Status     int    `json:"status"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 2, resp.Count, "默认仅返回有效邀请码")
	codes := map[string]bool{}
	for _, it := range resp.List {
		codes[it.InviteCode] = true
	}
	assert.True(t, codes["list-active-1"])
	assert.True(t, codes["list-active-2"])
	assert.False(t, codes["list-disabled"], "禁用码应被过滤")
	assert.False(t, codes["list-expired"], "过期码应被过滤")
}

// TestListInvites_StatusAll: status=all 返回全部（含禁用、过期）。
func TestListInvites_StatusAll(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-list-all"
	seedSpaceWithMember(t, spaceId, testutil.UID, 1) // admin

	past := time.Now().Add(-1 * time.Hour)
	seedInvite(t, spaceId, "all-active", testutil.UID, 1, nil)
	seedInvite(t, spaceId, "all-disabled", testutil.UID, 0, nil)
	seedInvite(t, spaceId, "all-expired", testutil.UID, 1, &past)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/invites?status=all", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 3, resp.Count)
}

// TestListInvites_StatusDisabled: status=0 仅返回禁用的。
func TestListInvites_StatusDisabled(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-list-disabled"
	seedSpaceWithMember(t, spaceId, testutil.UID, 1)

	seedInvite(t, spaceId, "only-active", testutil.UID, 1, nil)
	seedInvite(t, spaceId, "only-disabled1", testutil.UID, 0, nil)
	seedInvite(t, spaceId, "only-disabled2", testutil.UID, 0, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/invites?status=0", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			Status int `json:"status"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 2, resp.Count)
	for _, it := range resp.List {
		assert.Equal(t, 0, it.Status)
	}
}

// TestListInvites_Pagination: 分页参数生效，count 返回总数。
func TestListInvites_Pagination(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-list-page"
	seedSpaceWithMember(t, spaceId, testutil.UID, 2)

	for i := 0; i < 5; i++ {
		seedInvite(t, spaceId, fmt.Sprintf("page-code-%d", i), testutil.UID, 1, nil)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/invites?page_index=1&page_size=2", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			InviteCode string `json:"invite_code"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 5, resp.Count)
	assert.Len(t, resp.List, 2)
}

// TestListInvites_MemberForbidden: 普通成员（role=0）无权限查看。
func TestListInvites_MemberForbidden(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-list-forbid-member"
	// owner 另一个人
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: spaceId, Creator: "other-owner", Status: SpaceStatusNormal,
	}))
	// 当前 uid 只是普通成员
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1,
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/invites", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
}

// TestListInvites_NonMemberForbidden: 非空间成员完全不可访问。
func TestListInvites_NonMemberForbidden(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-list-forbid-nonmember"
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: spaceId, Creator: "other-owner", Status: SpaceStatusNormal,
	}))
	// testutil.UID 不是成员

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/invites", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
}

// TestDeleteInvite_SoftDisable: admin 可软禁用邀请码。
func TestDeleteInvite_SoftDisable(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-del"
	seedSpaceWithMember(t, spaceId, testutil.UID, 1)
	seedInvite(t, spaceId, "todel-code", testutil.UID, 1, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/space/"+spaceId+"/invite/todel-code", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 业务侧按 status=1 过滤 → 应查不到
	inv, err := testSpaceDB.queryInvitationByCode("todel-code")
	assert.NoError(t, err)
	assert.Nil(t, inv)
}

// TestDeleteInvite_MemberForbidden: 普通成员不可删除。
func TestDeleteInvite_MemberForbidden(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-del-forbid"
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: spaceId, Creator: "other", Status: SpaceStatusNormal,
	}))
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1,
	}))
	seedInvite(t, spaceId, "noperm-code", "other", 1, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/space/"+spaceId+"/invite/noperm-code", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.permission_denied")
}

// TestDeleteInvite_CodeNotFound: 删除不存在的邀请码返回错误。
func TestDeleteInvite_CodeNotFound(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-del-404"
	seedSpaceWithMember(t, spaceId, testutil.UID, 2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/space/"+spaceId+"/invite/ghost-code", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请码不存在")
}

// TestUpdateInvite_WithStatusDisable: PUT status=0 等价禁用。
func TestUpdateInvite_WithStatusDisable(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-upd-status-off"
	seedSpaceWithMember(t, spaceId, testutil.UID, 2)
	seedInvite(t, spaceId, "flip-off", testutil.UID, 1, nil)

	body := `{"status":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/flip-off", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	inv, err := testSpaceDB.queryInvitationByCode("flip-off")
	assert.NoError(t, err)
	assert.Nil(t, inv, "status=0 后业务查询应失效")
}

// TestUpdateInvite_WithStatusReEnable: PUT status=1 可对已禁用邀请码重启用。
func TestUpdateInvite_WithStatusReEnable(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-upd-status-on"
	seedSpaceWithMember(t, spaceId, testutil.UID, 2)
	seedInvite(t, spaceId, "flip-on", testutil.UID, 0, nil) // 先禁用

	body := `{"status":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/flip-on", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	inv, err := testSpaceDB.queryInvitationByCode("flip-on")
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, 1, inv.Status)
}

// TestUpdateInvite_StatusPayload_MemberForbidden: role=0 成员即使只传 status 也被权限门拒绝。
// 与现有 TestUpdateInviteNoPermission（测 max_uses 路径）互补，显式覆盖 status 扩展路径。
func TestUpdateInvite_StatusPayload_MemberForbidden(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-upd-role0"
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: spaceId, Creator: "other", Status: SpaceStatusNormal,
	}))
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1,
	}))
	seedInvite(t, spaceId, "role0-status", "other", 1, nil)

	body := `{"status":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/role0-status", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertSpaceErrorCode(t, w, "err.server.space.permission_denied")

	// 底层未变
	var status int
	_, err = testCtx.DB().SelectBySql("SELECT status FROM space_invitation WHERE invite_code=?", "role0-status").Load(&status)
	assert.NoError(t, err)
	assert.Equal(t, 1, status)
}

// TestUpdateInvite_CrossSpaceRejected: 空间 A 的 owner 不能通过指向空间 B 的 URL 改 B 的邀请码，
// 因为 updateInvitationAdmin 的 WHERE 同时锁 space_id + invite_code。
func TestUpdateInvite_CrossSpaceRejected(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceA := "sp-cross-a"
	spaceB := "sp-cross-b"
	seedSpaceWithMember(t, spaceA, testutil.UID, 2) // 当前用户是 A 的 owner
	// B 的 owner 是别人，当前用户不是 B 的成员
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceB, Name: spaceB, Creator: "other-owner", Status: SpaceStatusNormal,
	}))
	assert.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceB, UID: "other-owner", Role: 2, Status: 1,
	}))
	// 邀请码挂在 B 下
	seedInvite(t, spaceB, "cross-code", "other-owner", 1, nil)

	// 当前用户用 A 的 space_id 去改 B 的邀请码 → 第一步 queryMember 拿不到 A 对 cross-code 的 Member 信息
	// 不过真正的攻击向量是：传正确的 A space_id 但试图改挂在 B 的 code。
	// URL /v1/space/spaceA/invite/cross-code → WHERE space_id=A AND invite_code=cross-code → 0 行匹配
	body := `{"max_uses":999}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/space/"+spaceA+"/invite/cross-code", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请码不存在")

	// B 的邀请码未被改动
	inv, err := testSpaceDB.queryInvitationByCode("cross-code")
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, 10, inv.MaxUses, "seedInvite 默认 max_uses=10，未被跨空间改动")
}

// TestDeleteInvite_CrossSpaceRejected: 同上，DELETE 也受 space_id 作用域保护。
func TestDeleteInvite_CrossSpaceRejected(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceA := "sp-cross-del-a"
	spaceB := "sp-cross-del-b"
	seedSpaceWithMember(t, spaceA, testutil.UID, 2)
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceB, Name: spaceB, Creator: "other-owner", Status: SpaceStatusNormal,
	}))
	seedInvite(t, spaceB, "cross-del-code", "other-owner", 1, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/space/"+spaceA+"/invite/cross-del-code", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请码不存在")

	// B 的邀请码仍 status=1
	inv, err := testSpaceDB.queryInvitationByCode("cross-del-code")
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, 1, inv.Status)
}

// TestUpdateInvite_InvalidStatus: status 非 0/1 应拒绝。
func TestUpdateInvite_InvalidStatus(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-upd-bad-status"
	seedSpaceWithMember(t, spaceId, testutil.UID, 2)
	seedInvite(t, spaceId, "bad-status", testutil.UID, 1, nil)

	body := `{"status":9}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/bad-status", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "status")
}
