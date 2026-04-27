package space

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	redis "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
)

// resetSpaceInviteRateLimit 清空 space_invite 限流桶，避免共享 IP 桶在多用例下耗尽。
// httptest 不带 X-Forwarded-For，所有请求走同一个 fallback bucket（unknown_ip）。
func resetSpaceInviteRateLimit(t *testing.T) {
	t.Helper()
	rdsClient := redis.NewClient(&redis.Options{
		Addr:     testCtx.GetConfig().DB.RedisAddr,
		Password: testCtx.GetConfig().DB.RedisPass,
	})
	defer rdsClient.Close()
	keys, err := rdsClient.Keys("ratelimit:strict:space_invite:*").Result()
	if err == nil && len(keys) > 0 {
		_ = rdsClient.Del(keys...).Err()
	}
}

// seedUserWithEmail 在 user 表插入一行带邮箱的用户。
func seedUserWithEmail(t *testing.T, uid, email, name string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, email) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE email=VALUES(email), name=VALUES(name)",
		uid, name, email,
	).Exec()
	assert.NoError(t, err)
}

// seedEmailInviteWithToken 直接落库一条邀请，返回 raw token。
func seedEmailInviteWithToken(t *testing.T, m *spaceEmailInviteModel) (rawToken string, id int64) {
	t.Helper()
	raw, hash, err := generateEmailInviteToken()
	assert.NoError(t, err)
	m.TokenHash = hash
	id, err = testSpaceDB.insertEmailInvite(m)
	assert.NoError(t, err)
	return raw, id
}

func TestPreviewEmailInvite_Owner(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	seedUserWithEmail(t, "admin-creator", "", "管理员A")

	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:      EmailInviteTypeOwner,
		Email:           "u@x.com",
		PlannedName:     "新空间",
		PlannedMaxUsers: 50,
		Status:          EmailInviteStatusPending,
		CreatedBy:       "admin-creator",
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/email-invite/"+raw, nil)
	srv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp emailInvitePreviewResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, EmailInviteTypeOwner, resp.InviteType)
	assert.Equal(t, "u@x.com", resp.Email)
	assert.Equal(t, "新空间", resp.PlannedName)
	assert.Equal(t, 50, resp.PlannedMaxUsers)
	assert.Equal(t, "管理员A", resp.InviterName)
	assert.Empty(t, resp.SpaceId, "owner 类型不应回显 space_id")
}

func TestPreviewEmailInvite_Member(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	spaceId := "sp-prev-mem"
	seedSpaceWithMemberRole(t, spaceId, testutil.UID, 2)
	seedUserWithEmail(t, testutil.UID, "owner@x.com", "Owner")

	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType: EmailInviteTypeMember,
		Email:      "newuser@x.com",
		SpaceId:    spaceId,
		Role:       EmailInviteRoleAdmin,
		Status:     EmailInviteStatusPending,
		CreatedBy:  testutil.UID,
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/email-invite/"+raw, nil)
	srv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp emailInvitePreviewResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, EmailInviteTypeMember, resp.InviteType)
	assert.Equal(t, spaceId, resp.SpaceId)
	assert.Equal(t, "测试空间", resp.SpaceName)
	assert.Equal(t, EmailInviteRoleAdmin, resp.Role)
	assert.GreaterOrEqual(t, resp.MemberCount, 1)
}

func TestPreviewEmailInvite_NotFoundReturnsRevoked(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/email-invite/nonexistent", nil)
	srv.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp emailInvitePreviewResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, EmailInviteStatusRevoked, resp.Status)
}

func TestPreviewEmailInvite_ExpiredShowsExpired(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	past := time.Now().Add(-1 * time.Hour)
	expires := db.Time(past)

	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:  EmailInviteTypeOwner,
		Email:       "a@x.com",
		PlannedName: "x",
		Status:      EmailInviteStatusPending,
		ExpiresAt:   &expires,
		CreatedBy:   "admin-1",
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/email-invite/"+raw, nil)
	srv.GetRoute().ServeHTTP(w, req)
	var resp emailInvitePreviewResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, EmailInviteStatusExpired, resp.Status)
}

// --- accept ---

// acceptInviteHelper 封装 accept 请求构造。
func acceptInviteHelper(t *testing.T, srv *server.Server, raw, authToken string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/email-invite/"+raw+"/accept", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("token", authToken)
	}
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func TestAcceptEmailInvite_OwnerSuccess(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "newowner@x.com", "RecipientA")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:      EmailInviteTypeOwner,
		Email:           "newowner@x.com",
		PlannedName:     "我的新空间",
		PlannedJoinMode: JoinModeApproval,
		PlannedMaxUsers: 10,
		Status:          EmailInviteStatusPending,
		CreatedBy:       "admin-1",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		SpaceID string `json:"space_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.SpaceID)

	// 邀请已 consumed
	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusConsumed, got.Status)
	assert.Equal(t, testutil.UID, got.ConsumedBy)

	// 空间存在且 testutil.UID 是 owner
	sp, _ := testSpaceDB.querySpaceByID(resp.SpaceID)
	assert.NotNil(t, sp)
	assert.Equal(t, "我的新空间", sp.Name)
	assert.Equal(t, JoinModeApproval, sp.JoinMode)
	mem, _ := testSpaceDB.queryMember(resp.SpaceID, testutil.UID)
	assert.NotNil(t, mem)
	assert.Equal(t, 2, mem.Role)
}

func TestAcceptEmailInvite_MemberSuccess(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	spaceId := "sp-acc-mem"
	// 由其他人创建空间
	err = testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "已有空间", Creator: "owner-x", Status: SpaceStatusNormal,
	})
	assert.NoError(t, err)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: "owner-x", Role: 2, Status: 1,
	})
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "joiner@x.com", "Joiner")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType: EmailInviteTypeMember,
		Email:      "joiner@x.com",
		SpaceId:    spaceId,
		Role:       EmailInviteRoleAdmin,
		Status:     EmailInviteStatusPending,
		CreatedBy:  "owner-x",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusConsumed, got.Status)

	mem, _ := testSpaceDB.queryMember(spaceId, testutil.UID)
	assert.NotNil(t, mem)
	assert.Equal(t, 1, mem.Role, "应为管理员角色")
}

func TestAcceptEmailInvite_MemberDefaultRole(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	spaceId := "sp-acc-defrole"
	err = testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "x", Creator: "owner-x", Status: SpaceStatusNormal,
	})
	assert.NoError(t, err)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{SpaceId: spaceId, UID: "owner-x", Role: 2, Status: 1})
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "j2@x.com", "")

	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType: EmailInviteTypeMember,
		Email:      "j2@x.com",
		SpaceId:    spaceId,
		Role:       EmailInviteRoleMember,
		Status:     EmailInviteStatusPending,
		CreatedBy:  "owner-x",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	mem, _ := testSpaceDB.queryMember(spaceId, testutil.UID)
	assert.Equal(t, 0, mem.Role)
}

func TestAcceptEmailInvite_EmailMismatch(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "actual@x.com", "")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:  EmailInviteTypeOwner,
		Email:       "different@x.com",
		PlannedName: "x",
		Status:      EmailInviteStatusPending,
		CreatedBy:   "admin-1",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.NotEqual(t, http.StatusOK, w.Code)
	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusPending, got.Status)
}

func TestAcceptEmailInvite_AlreadyConsumed(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "x@x.com", "")

	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:  EmailInviteTypeOwner,
		Email:       "x@x.com",
		PlannedName: "y",
		Status:      EmailInviteStatusConsumed,
		CreatedBy:   "admin-1",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestAcceptEmailInvite_Expired(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "x@x.com", "")

	past := db.Time(time.Now().Add(-time.Hour))
	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:  EmailInviteTypeOwner,
		Email:       "x@x.com",
		PlannedName: "y",
		Status:      EmailInviteStatusPending,
		ExpiresAt:   &past,
		CreatedBy:   "admin-1",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestAcceptEmailInvite_DisbandedSpaceForMember(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	spaceId := "sp-disb"
	err = testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "x", Creator: "owner-x", Status: SpaceStatusDisbanded,
	})
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "z@x.com", "")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType: EmailInviteTypeMember,
		Email:      "z@x.com",
		SpaceId:    spaceId,
		Status:     EmailInviteStatusPending,
		CreatedBy:  "owner-x",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.NotEqual(t, http.StatusOK, w.Code)
	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusPending, got.Status, "空间已解散应保留邀请为 pending")
}

func TestAcceptEmailInvite_AlreadyMember_KeepsConsumed(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	spaceId := "sp-already-mem"
	// testutil.UID 已是空间成员（owner）
	seedSpaceWithMemberRole(t, spaceId, testutil.UID, 2)
	seedUserWithEmail(t, testutil.UID, "dup@x.com", "")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType: EmailInviteTypeMember,
		Email:      "dup@x.com",
		SpaceId:    spaceId,
		Role:       EmailInviteRoleMember,
		Status:     EmailInviteStatusPending,
		CreatedBy:  testutil.UID,
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusConsumed, got.Status,
		"已是成员场景应保留 consumed，避免 token 被回退到 pending 后重复使用")
	assert.Equal(t, testutil.UID, got.ConsumedBy)
}

// 回归 PR #1172 review 反馈：管理员对已是普通成员的用户发 admin 邀请，
// 应该提升为 admin 而非静默吞掉提权意图。
func TestAcceptEmailInvite_AlreadyMember_PromotesToAdmin(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	spaceId := "sp-promote"
	// 由其他人创建空间，testutil.UID 是 role=0 的普通成员
	err = testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "x", Creator: "owner-x", Status: SpaceStatusNormal,
	})
	assert.NoError(t, err)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{SpaceId: spaceId, UID: "owner-x", Role: 2, Status: 1})
	assert.NoError(t, err)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1})
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "promote@x.com", "")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType: EmailInviteTypeMember,
		Email:      "promote@x.com",
		SpaceId:    spaceId,
		Role:       EmailInviteRoleAdmin,
		Status:     EmailInviteStatusPending,
		CreatedBy:  "owner-x",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	mem, _ := testSpaceDB.queryMember(spaceId, testutil.UID)
	assert.NotNil(t, mem)
	assert.Equal(t, 1, mem.Role, "已是成员收到 admin 邀请应被提升为管理员")

	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusConsumed, got.Status)
}

func TestAcceptEmailInvite_EmptyInviteEmail_Rejected(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	seedUserWithEmail(t, testutil.UID, "x@x.com", "")

	raw, id := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:  EmailInviteTypeOwner,
		Email:       "", // 历史脏数据兜底
		PlannedName: "y",
		Status:      EmailInviteStatusPending,
		CreatedBy:   "admin-1",
	})

	w := acceptInviteHelper(t, srv, raw, testutil.Token)
	assert.NotEqual(t, http.StatusOK, w.Code)
	got, _ := testSpaceDB.queryEmailInviteByID(id)
	assert.Equal(t, EmailInviteStatusPending, got.Status)
}

func TestAcceptEmailInvite_RequiresAuth(t *testing.T) {
	srv, _, err := setup(t)
	resetSpaceInviteRateLimit(t)
	assert.NoError(t, err)
	raw, _ := seedEmailInviteWithToken(t, &spaceEmailInviteModel{
		InviteType:  EmailInviteTypeOwner,
		Email:       "x@x.com",
		PlannedName: "y",
		Status:      EmailInviteStatusPending,
		CreatedBy:   "admin-1",
	})

	w := acceptInviteHelper(t, srv, raw, "")
	assert.NotEqual(t, http.StatusOK, w.Code)
}
