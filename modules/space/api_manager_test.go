package space

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// adminToken 在 token 缓存中注入一个带 admin 角色的 token，供管理接口测试使用。
func adminToken(t *testing.T) string {
	t.Helper()
	token := "space-mgr-admin-token"
	cfg := testCtx.GetConfig()
	err := testCtx.Cache().Set(cfg.Cache.TokenCachePrefix+token, testutil.UID+"@admin@"+string(wkhttp.Admin))
	assert.NoError(t, err)
	return token
}

// readSpaceStatus 读取空间当前状态（测试辅助，不经过业务过滤）
func readSpaceStatus(t *testing.T, spaceId string) int {
	t.Helper()
	var status int
	_, err := testCtx.DB().SelectBySql("SELECT status FROM space WHERE space_id=?", spaceId).Load(&status)
	assert.NoError(t, err)
	return status
}

// seedSpace 插入一个测试空间 + owner，返回 spaceId。
func seedSpace(t *testing.T, spaceId, name, creator string, status int) {
	t.Helper()
	err := testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    name,
		Creator: creator,
		Status:  status,
	})
	assert.NoError(t, err)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     creator,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)
}

func TestManager_SpaceList(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-space-001", "alpha team", "u-owner-1", 1)
	seedSpace(t, "mgr-space-002", "beta squad", "u-owner-2", 1)
	seedSpace(t, "mgr-space-disbanded", "gone space", "u-owner-3", 0)

	t.Run("full list excludes disbanded", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces?page_index=1&page_size=20", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Count int64 `json:"count"`
			List  []struct {
				SpaceId string `json:"space_id"`
				Name    string `json:"name"`
			} `json:"list"`
		}
		assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.EqualValues(t, 2, resp.Count)
		ids := map[string]bool{}
		for _, it := range resp.List {
			ids[it.SpaceId] = true
		}
		assert.True(t, ids["mgr-space-001"])
		assert.True(t, ids["mgr-space-002"])
		assert.False(t, ids["mgr-space-disbanded"])
	})

	t.Run("keyword filter", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces?keyword=alpha", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"space_id":"mgr-space-001"`)
		assert.NotContains(t, w.Body.String(), "mgr-space-002")
	})
}

func TestManager_DisableList(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-active", "live", "u-a", 1)
	seedSpace(t, "mgr-dead-1", "dead space 1", "u-b", 0)
	seedSpace(t, "mgr-dead-2", "dead space 2", "u-c", 0)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/spaces/disabled?page_index=1&page_size=10", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			SpaceId string `json:"space_id"`
			Status  int    `json:"status"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 2, resp.Count)
	for _, it := range resp.List {
		assert.NotEqual(t, SpaceStatusNormal, it.Status, "disablelist should not include active spaces")
	}
}

func TestManager_SpaceDetail(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-detail", "detail space", "u-owner", 1)

	t.Run("active space returns detail", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces/mgr-detail", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, `"space_id":"mgr-detail"`)
		assert.Contains(t, body, `"name":"detail space"`)
		assert.Contains(t, body, `"member_count":1`)
	})

	t.Run("disbanded space still returns detail", func(t *testing.T) {
		seedSpace(t, "mgr-detail-dead", "dead one", "u-owner-x", 0)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces/mgr-detail-dead", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"status":0`)
	})

	t.Run("unknown space returns error", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces/does-not-exist", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})
}

func TestManager_ForceDisband(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-force", "about to die", "u-owner", 1)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: "mgr-force", UID: "u-member-1", Role: 0, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/manager/spaces/mgr-force", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	active, err := testSpaceDB.isSpaceActive("mgr-force")
	assert.NoError(t, err)
	assert.False(t, active, "space should be disbanded")

	count, err := testSpaceDB.countActiveMembers("mgr-force")
	assert.NoError(t, err)
	assert.Equal(t, 0, count, "all members should be removed")
}

func TestManager_MembersList(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-members", "members space", "u-owner", 1)
	for i := 0; i < 3; i++ {
		uid := fmt.Sprintf("u-m-%d", i)
		err = testSpaceDB.insertMemberNoTx(&MemberModel{
			SpaceId: "mgr-members", UID: uid, Role: 0, Status: 1,
		})
		assert.NoError(t, err)
	}
	// 已移除成员也应被管理后台看到
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: "mgr-members", UID: "u-m-removed", Role: 0, Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/spaces/mgr-members/members?page_index=1&page_size=20", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			UID    string `json:"uid"`
			Role   int    `json:"role"`
			Status int    `json:"status"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 5, resp.Count) // owner + 3 active + 1 removed
	assert.Len(t, resp.List, 5)
	// owner (role=2) 应排在最前
	assert.Equal(t, 2, resp.List[0].Role)
	assert.Equal(t, "u-owner", resp.List[0].UID)
}

// ==================== P1 tests ====================

func TestManager_LiftBan(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-ban-target", "to be banned", "u-owner", SpaceStatusNormal)

	t.Run("ban active space", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-ban-target/status/2", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		status := readSpaceStatus(t, "mgr-ban-target")
		assert.Equal(t, SpaceStatusBanned, status)
	})

	t.Run("unban banned space", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-ban-target/status/1", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		status := readSpaceStatus(t, "mgr-ban-target")
		assert.Equal(t, SpaceStatusNormal, status)
	})

	t.Run("reject invalid status", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-ban-target/status/7", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})

	t.Run("reject ban on disbanded space", func(t *testing.T) {
		seedSpace(t, "mgr-ban-dead", "dead", "u-owner-d", SpaceStatusDisbanded)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-ban-dead/status/2", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "已解散")
	})
}

func TestManager_AddMembers(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-addmem", "add members", "u-owner", SpaceStatusNormal)

	body := util.ToJson(map[string]interface{}{
		"uids": []string{"new-u-1", "new-u-2"},
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-addmem/members", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	count, err := testSpaceDB.countActiveMembers("mgr-addmem")
	assert.NoError(t, err)
	assert.Equal(t, 3, count) // owner + 2 new

	t.Run("reactivate removed member", func(t *testing.T) {
		err := testSpaceDB.removeMember("mgr-addmem", "new-u-1")
		assert.NoError(t, err)

		body2 := util.ToJson(map[string]interface{}{"uids": []string{"new-u-1"}})
		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-addmem/members", bytes.NewReader([]byte(body2)))
		req2.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w2, req2)
		assert.Equal(t, http.StatusOK, w2.Code)

		mem, err := testSpaceDB.queryMember("mgr-addmem", "new-u-1")
		assert.NoError(t, err)
		assert.NotNil(t, mem)
		assert.Equal(t, 1, mem.Status)
	})

	t.Run("bypass max_users cap", func(t *testing.T) {
		seedSpace(t, "mgr-capped", "tiny", "u-owner-c", SpaceStatusNormal)
		_, err := testCtx.DB().Update("space").Set("max_users", 2).Where("space_id=?", "mgr-capped").Exec()
		assert.NoError(t, err)
		// owner 已占 1，再加 3 个应超过 max=2，但管理员应绕过限制
		body3 := util.ToJson(map[string]interface{}{"uids": []string{"x1", "x2", "x3"}})
		w3 := httptest.NewRecorder()
		req3, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-capped/members", bytes.NewReader([]byte(body3)))
		req3.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w3, req3)
		assert.Equal(t, http.StatusOK, w3.Code)
		count, err := testSpaceDB.countActiveMembers("mgr-capped")
		assert.NoError(t, err)
		assert.Equal(t, 4, count)
	})
}

func TestManager_RemoveMembers(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-rm", "remove members", "u-owner", SpaceStatusNormal)
	for _, uid := range []string{"rm-1", "rm-2", "rm-3"} {
		err = testSpaceDB.insertMemberNoTx(&MemberModel{
			SpaceId: "mgr-rm", UID: uid, Role: 0, Status: 1,
		})
		assert.NoError(t, err)
	}

	body := util.ToJson(map[string]interface{}{"uids": []string{"rm-1", "rm-3"}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/manager/spaces/mgr-rm/members", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	count, err := testSpaceDB.countActiveMembers("mgr-rm")
	assert.NoError(t, err)
	assert.Equal(t, 2, count) // owner + rm-2

	mem, err := testSpaceDB.queryMember("mgr-rm", "rm-1")
	assert.NoError(t, err)
	assert.Nil(t, mem)

	t.Run("reject removing owner", func(t *testing.T) {
		body := util.ToJson(map[string]interface{}{"uids": []string{"u-owner"}})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("DELETE", "/v1/manager/spaces/mgr-rm/members", bytes.NewReader([]byte(body)))
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "拥有者")
		owner, err := testSpaceDB.queryMember("mgr-rm", "u-owner")
		assert.NoError(t, err)
		assert.NotNil(t, owner, "owner must remain active")
	})
}

func TestManager_UpdateMemberRole(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-role", "role ops", "u-owner", SpaceStatusNormal)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: "mgr-role", UID: "m-target", Role: 0, Status: 1,
	})
	assert.NoError(t, err)

	t.Run("promote to admin", func(t *testing.T) {
		body := util.ToJson(map[string]interface{}{"role": 1})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-role/members/m-target/role", bytes.NewReader([]byte(body)))
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		mem, err := testSpaceDB.queryMember("mgr-role", "m-target")
		assert.NoError(t, err)
		assert.Equal(t, 1, mem.Role)
	})

	t.Run("reject demoting owner directly", func(t *testing.T) {
		// 此前已通过子测试 "promote to admin" 把 m-target 提到 admin
		// 先把 m-target 提成 owner 来构造"降级 owner"场景
		err := testSpaceDB.updateMemberRole("mgr-role", "m-target", 2)
		assert.NoError(t, err)

		body := util.ToJson(map[string]interface{}{"role": 0})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-role/members/m-target/role", bytes.NewReader([]byte(body)))
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "拥有者")

		mem, err := testSpaceDB.queryMember("mgr-role", "m-target")
		assert.NoError(t, err)
		assert.Equal(t, 2, mem.Role, "owner role must not be dropped")

		// 恢复：把 owner 转回 u-owner 以免影响后续子测试
		err = testSpaceDB.updateMemberRole("mgr-role", "m-target", 1)
		assert.NoError(t, err)
		err = testSpaceDB.updateMemberRole("mgr-role", "u-owner", 2)
		assert.NoError(t, err)
	})

	t.Run("transfer ownership demotes previous owner", func(t *testing.T) {
		body := util.ToJson(map[string]interface{}{"role": 2})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-role/members/m-target/role", bytes.NewReader([]byte(body)))
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		newOwner, err := testSpaceDB.queryMember("mgr-role", "m-target")
		assert.NoError(t, err)
		assert.Equal(t, 2, newOwner.Role)

		oldOwner, err := testSpaceDB.queryMember("mgr-role", "u-owner")
		assert.NoError(t, err)
		assert.Equal(t, 1, oldOwner.Role, "old owner demoted to admin")
	})
}

func TestManager_InvitesList(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv", "inv space", "u-owner", SpaceStatusNormal)
	for i, code := range []string{"inv-aaa111", "inv-bbb222", "inv-ccc333"} {
		status := 1
		if i == 2 {
			status = 0 // last one disabled
		}
		err = testSpaceDB.insertInvitation(&InvitationModel{
			SpaceId: "mgr-inv", InviteCode: code, Creator: "u-owner", Status: status,
		})
		assert.NoError(t, err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/spaces/mgr-inv/invites", nil)
	req.Header.Set("token", token)
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
	assert.EqualValues(t, 3, resp.Count, "admin sees disabled invites too")
}

func TestManager_DisableInvite(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv-del", "inv delete", "u-owner", SpaceStatusNormal)
	err = testSpaceDB.insertInvitation(&InvitationModel{
		SpaceId: "mgr-inv-del", InviteCode: "inv-todel1", Creator: "u-owner", Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/manager/spaces/mgr-inv-del/invites/inv-todel1", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 业务端按 status=1 查找 → 应该找不到
	inv, err := testSpaceDB.queryInvitationByCode("inv-todel1")
	assert.NoError(t, err)
	assert.Nil(t, inv, "disabled invite should not be visible to business query")
}

func TestManager_CreateInvite_Default(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv-c1", "inv create", "u-owner", SpaceStatusNormal)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-inv-c1/invites", bytes.NewBufferString(`{}`))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		InviteCode string `json:"invite_code"`
		ExpiresAt  string `json:"expires_at"`
		MaxUses    int    `json:"max_uses"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.InviteCode, 16, "16 hex 邀请码")
	assert.NotEmpty(t, resp.ExpiresAt, "默认带 72h TTL")
	assert.Equal(t, 0, resp.MaxUses)

	inv, err := testSpaceDB.queryInvitationByCode(resp.InviteCode)
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, "mgr-inv-c1", inv.SpaceId)
	assert.Equal(t, testutil.UID, inv.Creator, "creator = operator admin")
}

func TestManager_CreateInvite_WithOverrides(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv-c2", "inv create 2", "u-owner", SpaceStatusNormal)

	body := `{"max_uses":50,"expires_at":"2030-01-01 00:00:00"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-inv-c2/invites", bytes.NewBufferString(body))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		InviteCode string `json:"invite_code"`
		MaxUses    int    `json:"max_uses"`
		ExpiresAt  string `json:"expires_at"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 50, resp.MaxUses)
	assert.Equal(t, "2030-01-01 00:00:00", resp.ExpiresAt)
}

// TestManager_CreateInvite_ExplicitZeroMaxUses 管理端显式传 max_uses=0 应透传为"不限"，
// 即使 DM_SPACE_INVITE_DEFAULT_MAX_USES 设了非零默认也不被覆盖。（review #1 回归）
func TestManager_CreateInvite_ExplicitZeroMaxUses(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)
	t.Setenv(envInviteDefaultMaxUses, "50") // 非零默认

	seedSpace(t, "mgr-inv-zero", "zero max", "u-owner", SpaceStatusNormal)

	body := `{"max_uses":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-inv-zero/invites", bytes.NewBufferString(body))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		MaxUses    int    `json:"max_uses"`
		InviteCode string `json:"invite_code"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.MaxUses, "显式 0 应保留，不被环境变量默认值覆盖")
}

func TestManager_CreateInvite_NonExistentSpace(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/nope-space/invites", bytes.NewBufferString(`{}`))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestManager_UpdateInvite_AllFields(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv-u1", "inv update", "u-owner", SpaceStatusNormal)
	err = testSpaceDB.insertInvitation(&InvitationModel{
		SpaceId: "mgr-inv-u1", InviteCode: "upd-code-1", Creator: "u-owner", MaxUses: 10, Status: 1,
	})
	assert.NoError(t, err)

	body := `{"max_uses":99,"expires_at":"2029-06-01 00:00:00","status":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-inv-u1/invites/upd-code-1", bytes.NewBufferString(body))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 状态检查：业务端按 status=1 查应找不到（已禁用）
	inv, err := testSpaceDB.queryInvitationByCode("upd-code-1")
	assert.NoError(t, err)
	assert.Nil(t, inv, "status=0 后业务查询应失效")

	// 原始数据检查
	var got struct {
		MaxUses int `db:"max_uses"`
		Status  int `db:"status"`
	}
	_, err = testCtx.DB().SelectBySql("SELECT max_uses, status FROM space_invitation WHERE invite_code=?", "upd-code-1").Load(&got)
	assert.NoError(t, err)
	assert.Equal(t, 99, got.MaxUses)
	assert.Equal(t, 0, got.Status)
}

func TestManager_UpdateInvite_InvalidStatus(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv-u2", "inv update 2", "u-owner", SpaceStatusNormal)
	err = testSpaceDB.insertInvitation(&InvitationModel{
		SpaceId: "mgr-inv-u2", InviteCode: "upd-code-2", Creator: "u-owner", Status: 1,
	})
	assert.NoError(t, err)

	body := `{"status":9}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-inv-u2/invites/upd-code-2", bytes.NewBufferString(body))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestManager_UpdateInvite_CodeNotFound(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-inv-u3", "inv update 3", "u-owner", SpaceStatusNormal)

	body := `{"max_uses":5}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-inv-u3/invites/nonexistent", bytes.NewBufferString(body))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestIncrementInviteUsedCountAtomic_StatusAndExpiryGuard
// 原子消耗过滤必须与 queryInvitationByCode 同步：禁用（status=0）或已过期的邀请码
// 即使 max_uses 未到也不得递增。（review #1 回归：TOCTOU gap）
func TestIncrementInviteUsedCountAtomic_StatusAndExpiryGuard(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	seedSpace(t, "sp-atomic", "atomic guard", "u-owner", SpaceStatusNormal)

	t.Run("disabled code rejected", func(t *testing.T) {
		err := testSpaceDB.insertInvitation(&InvitationModel{
			SpaceId: "sp-atomic", InviteCode: "atomic-disabled", Creator: "u-owner", MaxUses: 10, Status: 0,
		})
		assert.NoError(t, err)
		allowed, err := testSpaceDB.incrementInviteUsedCountAtomic("atomic-disabled")
		assert.NoError(t, err)
		assert.False(t, allowed, "status=0 不得放行")
	})

	t.Run("expired code rejected", func(t *testing.T) {
		past := db.Time(time.Now().Add(-1 * time.Hour))
		err := testSpaceDB.insertInvitation(&InvitationModel{
			SpaceId: "sp-atomic", InviteCode: "atomic-expired", Creator: "u-owner", MaxUses: 10, Status: 1, ExpiresAt: &past,
		})
		assert.NoError(t, err)
		allowed, err := testSpaceDB.incrementInviteUsedCountAtomic("atomic-expired")
		assert.NoError(t, err)
		assert.False(t, allowed, "过期码不得放行")
	})

	t.Run("valid code allowed", func(t *testing.T) {
		future := db.Time(time.Now().Add(1 * time.Hour))
		err := testSpaceDB.insertInvitation(&InvitationModel{
			SpaceId: "sp-atomic", InviteCode: "atomic-valid", Creator: "u-owner", MaxUses: 10, Status: 1, ExpiresAt: &future,
		})
		assert.NoError(t, err)
		allowed, err := testSpaceDB.incrementInviteUsedCountAtomic("atomic-valid")
		assert.NoError(t, err)
		assert.True(t, allowed, "有效码应放行")
	})
}

// TestGetInvitePreview_ExpiredCodeNotFound 公开预览端点遇过期码应视为无效，
// 避免通过"有效/无效"差异确认码曾经有效。（review #5 回归）
func TestGetInvitePreview_ExpiredCodeNotFound(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	seedSpace(t, "sp-expired-inv", "expired code", "u-owner", SpaceStatusNormal)

	// 过期时间为 1 小时前
	past := db.Time(time.Now().Add(-1 * time.Hour))
	err = testSpaceDB.insertInvitation(&InvitationModel{
		SpaceId:    "sp-expired-inv",
		InviteCode: "expired-code-x",
		Creator:    "u-owner",
		Status:     1,
		ExpiresAt:  &past,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/invite/expired-code-x/preview", nil)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, "过期码应等价于无效码")
	assert.NotContains(t, w.Body.String(), "sp-expired-inv", "不应泄露 space_id")
}

// TestGetSpace_NoInviteCodeInResponse 用户侧 GET /v1/space/:id 不再返回 invite_code。
func TestGetSpace_NoInviteCodeInResponse(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	seedSpace(t, "no-invite-in-detail", "detail", testutil.UID, SpaceStatusNormal)
	err = testSpaceDB.insertInvitation(&InvitationModel{
		SpaceId: "no-invite-in-detail", InviteCode: "leak-check", Creator: testutil.UID, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/no-invite-in-detail", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "invite_code")
	assert.NotContains(t, w.Body.String(), "leak-check")
}

// TestCreateSpace_NoInviteCodeInResponse 用户侧 POST /v1/space/create 响应不带 invite_code。
func TestCreateSpace_NoInviteCodeInResponse(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/create", bytes.NewBufferString(`{"name":"hidden invite"}`))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "invite_code")
}

func TestManager_JoinAppliesList(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-apply", "apply list", "u-owner", SpaceStatusNormal)
	for i, u := range []string{"app-u1", "app-u2", "app-u3"} {
		_, err = testSpaceDB.upsertJoinApply(&spaceJoinApplyModel{
			SpaceId: "mgr-apply", UID: u, InviteCode: "xyz",
		})
		assert.NoError(t, err)
		if i == 0 {
			// 把第一个置为 approved
			_, _ = testCtx.DB().Update("space_join_apply").
				Set("status", 1).
				Where("space_id=? AND uid=?", "mgr-apply", u).Exec()
		}
	}

	t.Run("default returns pending only", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces/mgr-apply/join-applies?status=0", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		var resp struct {
			Count int64 `json:"count"`
		}
		assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.EqualValues(t, 2, resp.Count)
	})

	t.Run("all statuses", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces/mgr-apply/join-applies", nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		var resp struct {
			Count int64 `json:"count"`
		}
		assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.EqualValues(t, 3, resp.Count)
	})
}

func TestManager_ApproveAndReject(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-approve", "approve space", "u-owner", SpaceStatusNormal)

	t.Run("approve adds member", func(t *testing.T) {
		assert.NoError(t, testSpaceDB.insertInvitation(&InvitationModel{
			SpaceId: "mgr-approve", InviteCode: "c1", Creator: "u-owner", Status: 1,
		}))
		applyID, err := testSpaceDB.upsertJoinApply(&spaceJoinApplyModel{
			SpaceId: "mgr-approve", UID: "apply-u1", InviteCode: "c1",
		})
		assert.NoError(t, err)

		w := httptest.NewRecorder()
		url := fmt.Sprintf("/v1/manager/spaces/mgr-approve/join-applies/%d/approve", applyID)
		req, _ := http.NewRequest("POST", url, nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		mem, err := testSpaceDB.queryMember("mgr-approve", "apply-u1")
		assert.NoError(t, err)
		assert.NotNil(t, mem, "applicant should be added as member")

		apply, err := testSpaceDB.queryJoinApplyByID(applyID)
		assert.NoError(t, err)
		assert.Equal(t, 1, apply.Status)
	})

	t.Run("approve is idempotent on already-processed apply", func(t *testing.T) {
		assert.NoError(t, testSpaceDB.insertInvitation(&InvitationModel{
			SpaceId: "mgr-approve", InviteCode: "c1x", Creator: "u-owner", Status: 1,
		}))
		applyID, err := testSpaceDB.upsertJoinApply(&spaceJoinApplyModel{
			SpaceId: "mgr-approve", UID: "apply-u1x", InviteCode: "c1x",
		})
		assert.NoError(t, err)

		// first approve
		w1 := httptest.NewRecorder()
		url := fmt.Sprintf("/v1/manager/spaces/mgr-approve/join-applies/%d/approve", applyID)
		req1, _ := http.NewRequest("POST", url, nil)
		req1.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w1, req1)
		assert.Equal(t, http.StatusOK, w1.Code)

		// second approve should fail (already processed)
		w2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("POST", url, nil)
		req2.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w2, req2)
		assert.NotEqual(t, http.StatusOK, w2.Code)
	})

	t.Run("reject marks apply as rejected without adding member", func(t *testing.T) {
		applyID, err := testSpaceDB.upsertJoinApply(&spaceJoinApplyModel{
			SpaceId: "mgr-approve", UID: "apply-u2", InviteCode: "c2",
		})
		assert.NoError(t, err)

		w := httptest.NewRecorder()
		url := fmt.Sprintf("/v1/manager/spaces/mgr-approve/join-applies/%d/reject", applyID)
		req, _ := http.NewRequest("POST", url, nil)
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		mem, err := testSpaceDB.queryMember("mgr-approve", "apply-u2")
		assert.NoError(t, err)
		assert.Nil(t, mem)

		apply, err := testSpaceDB.queryJoinApplyByID(applyID)
		assert.NoError(t, err)
		assert.Equal(t, 2, apply.Status)
	})
}

func TestManager_AuthBoundary(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	t.Run("no token returns 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces", nil)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("non-admin token rejected", func(t *testing.T) {
		// testutil.Token 只有 uid@name，role 为空 → CheckLoginRole 应拒绝
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/spaces", nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "角色")
	})
}

func TestManager_DisableListIncludesBanned(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-dl-active", "a", "u1", SpaceStatusNormal)
	seedSpace(t, "mgr-dl-disbanded", "d", "u2", SpaceStatusDisbanded)
	seedSpace(t, "mgr-dl-banned", "b", "u3", SpaceStatusBanned)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/spaces/disabled", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			SpaceId string `json:"space_id"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 2, resp.Count)
	ids := map[string]bool{}
	for _, it := range resp.List {
		ids[it.SpaceId] = true
	}
	assert.True(t, ids["mgr-dl-disbanded"])
	assert.True(t, ids["mgr-dl-banned"])
	assert.False(t, ids["mgr-dl-active"])
}

func TestManager_RemoveOwnerBlockedInTx(t *testing.T) {
	// 验证 DB 层的 SELECT ... FOR UPDATE 守卫：即便 handler 不做 pre-check，
	// removeMembersForce 直接传 owner uid 也会返回 ErrCannotRemoveOwner。
	_, _, err := setup(t)
	assert.NoError(t, err)
	seedSpace(t, "mgr-rm-tx", "tx guard", "u-owner-tx", SpaceStatusNormal)

	mgrDB := newManagerDB(testCtx.DB())
	err = mgrDB.removeMembersForce("mgr-rm-tx", []string{"u-owner-tx"})
	assert.ErrorIs(t, err, ErrCannotRemoveOwner)

	owner, err := testSpaceDB.queryMember("mgr-rm-tx", "u-owner-tx")
	assert.NoError(t, err)
	assert.NotNil(t, owner, "owner must remain after guarded rollback")
	assert.Equal(t, 2, owner.Role)
}

func TestManager_NormalizeUIDsDedup(t *testing.T) {
	got := normalizeUIDs([]string{"a", "", "b", "a", "c", "", "b"})
	assert.Equal(t, []string{"a", "b", "c"}, got)
	assert.Empty(t, normalizeUIDs(nil))
	assert.Empty(t, normalizeUIDs([]string{"", ""}))
}

func TestManager_BatchSizeCap(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)
	seedSpace(t, "mgr-cap", "cap space", "u-owner", SpaceStatusNormal)

	// 构造超限请求（201 个 uid）
	big := make([]string, managerMaxBatchUIDs+1)
	for i := range big {
		big[i] = fmt.Sprintf("u%d", i)
	}
	body := util.ToJson(map[string]interface{}{"uids": big})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-cap/members", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "200")
}

func TestManager_TransferOwnerTargetMissing(t *testing.T) {
	// 验证 transferOwnerAdmin 对 status=0 目标的原子守卫：
	// 不会发生「降老 owner → 目标已被移除 → 新 owner 提升失败 → 空间无主」
	_, _, err := setup(t)
	assert.NoError(t, err)
	seedSpace(t, "mgr-xfer-miss", "transfer guard", "u-owner-x", SpaceStatusNormal)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: "mgr-xfer-miss", UID: "u-ghost", Role: 0, Status: 0, // 已移除
	})
	assert.NoError(t, err)

	err = newManagerDB(testCtx.DB()).transferOwnerAdmin("mgr-xfer-miss", "u-ghost")
	assert.ErrorIs(t, err, ErrTransferTargetMissing)

	// 原 owner 依然是 owner（未被事务的 step 1 降级）
	owner, err := testSpaceDB.queryMember("mgr-xfer-miss", "u-owner-x")
	assert.NoError(t, err)
	assert.NotNil(t, owner)
	assert.Equal(t, 2, owner.Role, "original owner must stay owner when transfer aborts")
}

func TestManager_LiftBanRefreshesCache(t *testing.T) {
	// 验证 liftBan 成功后会异步调用 loadKnownSpaceIDs 刷新 pkg/space 缓存
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-ban-cache", "cache", "u-owner-c", SpaceStatusBanned)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/manager/spaces/mgr-ban-cache/status/1", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 等待异步 loadKnownSpaceIDs 完成（ParseChannelID 要求 "s" 前缀）
	assert.Eventually(t, func() bool {
		sid, _ := spacepkg.ParseChannelID("smgr-ban-cache_peer1")
		return sid == "mgr-ban-cache"
	}, 2*time.Second, 50*time.Millisecond, "解禁后 spaceId 应出现在 ParseChannelID 缓存里")
}

func TestManager_AddMembersOnDisbandedSpace(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedSpace(t, "mgr-add-dead", "dead", "u-owner-dd", SpaceStatusDisbanded)

	body := util.ToJson(map[string]interface{}{"uids": []string{"new-u"}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces/mgr-add-dead/members", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "已解散")
}

func TestManager_RemoveMembersOnNonExistentSpace(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	body := util.ToJson(map[string]interface{}{"uids": []string{"any"}})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/manager/spaces/does-not-exist/members", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "不存在")
}

// ==================== P2: 管理端代建 ====================

// seedUser 向 user 表写入一条记录，满足代建时的 creator_uid 存在性检查。
func seedUser(t *testing.T, uid, name string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT IGNORE INTO `user` (uid, name) VALUES (?, ?)", uid, name,
	).Exec()
	assert.NoError(t, err)
}

func TestManager_CreateSpace_Success(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedUser(t, "u-target-p2", "Target User")

	// 开启用户侧开关：管理端代建应绕过
	t.Setenv(envDisableUserCreateSpace, "true")

	body := util.ToJson(map[string]interface{}{
		"creator_uid": "u-target-p2",
		"name":        "admin-created",
		"description": "代建测试",
		"join_mode":   1,
		"max_users":   50,
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		SpaceID    string `json:"space_id"`
		CreatorUID string `json:"creator_uid"`
		Name       string `json:"name"`
		InviteCode string `json:"invite_code"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.SpaceID)
	assert.Equal(t, "u-target-p2", resp.CreatorUID)
	assert.Equal(t, "admin-created", resp.Name)
	assert.NotEmpty(t, resp.InviteCode)

	// 目标用户被写为 owner
	mem, err := testSpaceDB.queryMember(resp.SpaceID, "u-target-p2")
	assert.NoError(t, err)
	assert.NotNil(t, mem)
	assert.Equal(t, 2, mem.Role)

	// space.creator 是目标用户，join_mode / max_users 正确落库
	var sp SpaceModel
	_, err = testCtx.DB().Select("*").From("space").Where("space_id=?", resp.SpaceID).Load(&sp)
	assert.NoError(t, err)
	assert.Equal(t, "u-target-p2", sp.Creator)
	assert.Equal(t, 1, sp.JoinMode)
	assert.Equal(t, 50, sp.MaxUsers)
}

func TestManager_CreateSpace_RetrievableViaDetail(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)
	seedUser(t, "u-t-2", "Target 2")

	body := util.ToJson(map[string]interface{}{
		"creator_uid": "u-t-2",
		"name":        "detail-check",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces", bytes.NewReader([]byte(body)))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var created struct {
		SpaceID string `json:"space_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// 用 manager detail 端点读回
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/v1/manager/spaces/"+created.SpaceID, nil)
	req2.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
	body2 := w2.Body.String()
	assert.Contains(t, body2, `"name":"detail-check"`)
	assert.Contains(t, body2, `"creator":"u-t-2"`)
	assert.Contains(t, body2, `"member_count":2`) // owner + botfather
	assert.Contains(t, body2, `"status":1`)
}

func TestManager_CreateSpace_NonAdminRejected(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	// 用普通用户 token（testutil.Token 不是 admin 角色）
	body := util.ToJson(map[string]interface{}{
		"creator_uid": "anyone",
		"name":        "x",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/spaces", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestManager_CreateSpace_ValidationErrors(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	seedUser(t, "u-ok", "Ok")

	cases := []struct {
		name string
		body map[string]interface{}
		msg  string
	}{
		{
			"missing creator_uid",
			map[string]interface{}{"name": "n"},
			"creator_uid",
		},
		{
			"missing name",
			map[string]interface{}{"creator_uid": "u-ok"},
			"名称",
		},
		{
			"invalid join_mode",
			map[string]interface{}{"creator_uid": "u-ok", "name": "n", "join_mode": 9},
			"加入模式",
		},
		{
			"negative max_users",
			map[string]interface{}{"creator_uid": "u-ok", "name": "n", "max_users": -1},
			"max_users",
		},
		{
			"creator not exists",
			map[string]interface{}{"creator_uid": "ghost", "name": "n"},
			"用户不存在",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := util.ToJson(tc.body)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/manager/spaces", bytes.NewReader([]byte(body)))
			req.Header.Set("token", token)
			s.GetRoute().ServeHTTP(w, req)
			assert.NotEqual(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), tc.msg)
		})
	}
}

// ==================== P2: LIKE 通配符转义 ====================

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"foo":           "foo",
		"foo_bar":       `foo\_bar`,
		"100%":          `100\%`,
		`a\b`:           `a\\b`,
		`mix_%\tricky`:  `mix\_\%\\tricky`,
	}
	for in, want := range cases {
		assert.Equal(t, want, escapeLike(in), "in=%q", in)
	}
}

func TestManager_ListKeywordLikeEscape(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	// 两条空间：foo_bar 字面匹配，foobar 仅在通配符未转义时被误匹配
	seedSpace(t, "s-foobar", "foobar", "u-o1", SpaceStatusNormal)
	seedSpace(t, "s-foo_bar", "foo_bar", "u-o2", SpaceStatusNormal)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/spaces?keyword=foo_bar", nil)
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			Name string `json:"name"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 1, resp.Count, "foo_bar 关键字不应匹配 foobar（_ 被转义）")
	if len(resp.List) == 1 {
		assert.Equal(t, "foo_bar", resp.List[0].Name)
	}
}
