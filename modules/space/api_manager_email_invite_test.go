package space

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func postJSON(t *testing.T, s *server.Server, path, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	s.GetRoute().ServeHTTP(w, req)
	return w
}

func TestManager_CreateOwnerEmailInvite_Success(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	w := postJSON(t, srv, "/v1/manager/spaces/invites", token, map[string]interface{}{
		"email":             "newowner@example.com",
		"planned_name":      "新团队空间",
		"planned_max_users": 100,
		"planned_join_mode": 0,
	})
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		ID         int64  `json:"id"`
		Email      string `json:"email"`
		Token      string `json:"token"`
		Status     int    `json:"status"`
		InviteType int    `json:"invite_type"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Greater(t, resp.ID, int64(0))
	assert.Equal(t, "newowner@example.com", resp.Email)
	assert.NotEmpty(t, resp.Token, "token 应仅在创建时返回一次")
	assert.Equal(t, EmailInviteStatusPending, resp.Status)
	assert.Equal(t, EmailInviteTypeOwner, resp.InviteType)

	inv, err := testSpaceDB.queryEmailInviteByTokenHash(hashEmailInviteToken(resp.Token))
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, resp.ID, inv.Id)

	// 回归 PR #1172 review：created_at 必须是真实时间，不能是 Go 零值。
	var createdAt string
	json.Unmarshal(w.Body.Bytes(), &struct {
		CreatedAt *string `json:"created_at"`
	}{CreatedAt: &createdAt})
	assert.NotEmpty(t, createdAt)
	assert.NotContains(t, createdAt, "0001-01-01", "created_at 不应是零值")
}

func TestManager_CreateOwnerEmailInvite_Validation(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing email", map[string]interface{}{"planned_name": "x"}},
		{"bad email", map[string]interface{}{"email": "no-at-sign", "planned_name": "x"}},
		{"missing planned_name", map[string]interface{}{"email": "a@b.com"}},
		{"negative max_users", map[string]interface{}{"email": "a@b.com", "planned_name": "x", "planned_max_users": -1}},
		{"invalid join_mode", map[string]interface{}{"email": "a@b.com", "planned_name": "x", "planned_join_mode": 99}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postJSON(t, srv, "/v1/manager/spaces/invites", token, tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}

func TestManager_CreateOwnerEmailInvite_RequiresAdmin(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	// 无 token
	w := postJSON(t, srv, "/v1/manager/spaces/invites", "", map[string]interface{}{
		"email": "a@b.com", "planned_name": "x",
	})
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestManager_ListOwnerEmailInvites(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	// testutil.UID 是 adminToken 对应的登录 UID
	for i := 0; i < 3; i++ {
		postJSON(t, srv, "/v1/manager/spaces/invites", token, map[string]interface{}{
			"email": fmt.Sprintf("u%d@x.com", i), "planned_name": fmt.Sprintf("team-%d", i),
		})
	}
	// 其他管理员创建的不应出现
	seedOwnerInvite(t, "other-hash", "other@x.com", "other-admin", EmailInviteStatusPending, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/spaces/invites?page_index=1&page_size=10", nil)
	req.Header.Set("token", token)
	srv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
		List  []struct {
			Email  string `json:"email"`
			Status int    `json:"status"`
			Token  string `json:"token"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 3, resp.Count)
	for _, it := range resp.List {
		assert.Empty(t, it.Token, "list 响应不应泄露 token 明文")
	}
}

func TestManager_ListOwnerEmailInvites_StatusFilter(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	// 通过端点创建一条 pending，然后直接 DB 造一条 revoked（同 creator）
	postJSON(t, srv, "/v1/manager/spaces/invites", token, map[string]interface{}{
		"email": "a@x.com", "planned_name": "t",
	})
	_, err = testSpaceDB.insertEmailInvite(&spaceEmailInviteModel{
		TokenHash: "rev-hash", InviteType: EmailInviteTypeOwner, Email: "b@x.com",
		PlannedName: "t", Status: EmailInviteStatusRevoked, CreatedBy: testutil.UID,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("/v1/manager/spaces/invites?status=%d", EmailInviteStatusPending), nil)
	req.Header.Set("token", token)
	srv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int64 `json:"count"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 1, resp.Count)
}

func TestManager_RevokeOwnerEmailInvite(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	w := postJSON(t, srv, "/v1/manager/spaces/invites", token, map[string]interface{}{
		"email": "a@x.com", "planned_name": "t",
	})
	var createResp struct {
		ID int64 `json:"id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &createResp))
	id := createResp.ID

	// revoke 成功
	w2 := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/v1/manager/spaces/invites/%d", id), nil)
	req.Header.Set("token", token)
	srv.GetRoute().ServeHTTP(w2, req)
	assert.Equal(t, http.StatusOK, w2.Code)

	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusRevoked, got.Status)

	// 重复 revoke 失败
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("DELETE", fmt.Sprintf("/v1/manager/spaces/invites/%d", id), nil)
	req3.Header.Set("token", token)
	srv.GetRoute().ServeHTTP(w3, req3)
	assert.NotEqual(t, http.StatusOK, w3.Code)
}

func TestManager_RevokeOwnerEmailInvite_ForeignCreator(t *testing.T) {
	srv, _, err := setup(t)
	assert.NoError(t, err)
	token := adminToken(t)

	// 其他管理员创建的邀请
	id := seedOwnerInvite(t, "foreign-hash", "a@x.com", "other-admin", EmailInviteStatusPending, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/v1/manager/spaces/invites/%d", id), nil)
	req.Header.Set("token", token)
	srv.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)

	// 目标邀请仍为 pending
	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusPending, got.Status)
}
