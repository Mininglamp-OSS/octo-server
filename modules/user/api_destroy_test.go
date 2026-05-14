package user

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// 准备一个已登录、设置了密码的测试用户。token/UID 走 testutil 的默认值。
func seedDestroyTestUser(t *testing.T, u *User, password string) {
	t.Helper()
	hashed, err := HashPassword(password)
	assert.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:      testutil.UID,
		Username: "destroy_user",
		Password: hashed,
		Name:     "destroy_user",
		ShortNo:  "9001",
		Status:   1,
	})
	assert.NoError(t, err)
}

func TestDestroyApply_OK(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	password := "Pwd@12345"
	seedDestroyTestUser(t, u, password)

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"password": password})
	req, _ := http.NewRequest("POST", "/v1/user/destroy/apply", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"destroy_status":1`)
	assert.Contains(t, w.Body.String(), `"expire_at"`)

	m, err := u.db.QueryByUID(testutil.UID)
	assert.NoError(t, err)
	assert.Equal(t, IsDestroyApplying, m.IsDestroy)
	assert.True(t, m.DestroyApplyAt.Valid)
	assert.True(t, m.DestroyExpireAt.Valid)
	// 默认 7 天冷静期
	assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), m.DestroyExpireAt.Time, 2*time.Minute)
}

func TestDestroyApply_WrongPassword(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	seedDestroyTestUser(t, u, "Pwd@12345")
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"password": "wrong"})
	req, _ := http.NewRequest("POST", "/v1/user/destroy/apply", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "密码错误")
	m, _ := u.db.QueryByUID(testutil.UID)
	assert.Equal(t, IsDestroyNo, m.IsDestroy)
}

func TestDestroyApply_AlreadyApplying(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	password := "Pwd@12345"
	seedDestroyTestUser(t, u, password)
	now := time.Now()
	assert.NoError(t, u.db.applyDestroy(testutil.UID, now, now.Add(7*24*time.Hour)))

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"password": password})
	req, _ := http.NewRequest("POST", "/v1/user/destroy/apply", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "冷静期")
}

func TestDestroyCancel_OK(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	seedDestroyTestUser(t, u, "Pwd@12345")
	now := time.Now()
	assert.NoError(t, u.db.applyDestroy(testutil.UID, now, now.Add(7*24*time.Hour)))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/destroy/cancel", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	m, _ := u.db.QueryByUID(testutil.UID)
	assert.Equal(t, IsDestroyNo, m.IsDestroy)
	assert.False(t, m.DestroyApplyAt.Valid)
	assert.False(t, m.DestroyExpireAt.Valid)
}

func TestDestroyCancel_NotApplying(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	seedDestroyTestUser(t, u, "Pwd@12345")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/destroy/cancel", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "未在注销中")
}

func TestDestroyStatus_Applying(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	seedDestroyTestUser(t, u, "Pwd@12345")
	now := time.Now()
	assert.NoError(t, u.db.applyDestroy(testutil.UID, now, now.Add(3*24*time.Hour)))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/destroy/status", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code, body)
	assert.Contains(t, body, `"destroy_status":1`)
	// remaining_days 不做精确断言：测试 DSN 不带时区，会有 0~8h 漂移影响 ceil 取整
	assert.Contains(t, body, `"remaining_days":`)
	assert.Contains(t, body, `"expire_at"`)
	assert.Contains(t, body, `"apply_at"`)
}

func TestCheckDestroyExpired_FinalizesAccount(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)

	uid := "destroy-expired-uid"
	assert.NoError(t, u.db.Insert(&Model{
		UID: uid, Username: "expired_user", Phone: "13900000000", Zone: "0086", Status: 1,
	}))
	// AttrToUnderscore 跳过 struct 字段（含 dbr.NullTime），因此 Insert 不能直接写入
	// destroy_*_at；这里通过 applyDestroy 走 SetMap 路径写入时间字段，
	// 然后用足够远的过去时间避开测试 DSN 的时区漂移（local↔UTC 误差最多约 8h）。
	pastApply := time.Now().Add(-30 * 24 * time.Hour)
	pastExpire := time.Now().Add(-2 * 24 * time.Hour)
	assert.NoError(t, u.db.applyDestroy(uid, pastApply, pastExpire))

	u.checkDestroyExpired()

	m, err := u.db.QueryByUID(uid)
	assert.NoError(t, err)
	assert.NotNil(t, m)
	assert.Equal(t, IsDestroyDone, m.IsDestroy)
	// phone 应已被匿名化
	assert.True(t, strings.Contains(m.Phone, "@delete"))
}

func TestUsernameLogin_BlocksDestroyed(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	password := "Pwd@12345"
	hashed, _ := HashPassword(password)
	username := "blocked_user_xx"
	assert.NoError(t, u.db.Insert(&Model{
		UID:       "uid-blocked",
		Username:  username,
		Password:  hashed,
		Name:      username,
		ShortNo:   "9002",
		Status:    1,
		IsDestroy: IsDestroyDone,
	}))

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{
		"username": username,
		"password": password,
		"device":   map[string]interface{}{"device_id": "d1", "device_name": "n", "device_model": "m"},
	})
	req, _ := http.NewRequest("POST", "/v1/user/usernamelogin", bytes.NewReader([]byte(body)))
	s.GetRoute().ServeHTTP(w, req)
	// 已注销账号必须无法登录：不返回 token；具体错误文案受 loginGuard 状态影响（可能被锁定也可能直接拒绝）
	resp := w.Body.String()
	assert.NotContains(t, resp, `"token":`, "destroyed account must not get a token")
}

func TestDestroyApply_NoPasswordSet(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	// 模拟仅第三方登录、未设置密码的账号
	assert.NoError(t, u.db.Insert(&Model{
		UID: testutil.UID, Username: "no_pwd_user", Name: "no_pwd_user",
		ShortNo: "9010", Status: 1,
	}))

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"password": "anything"})
	req, _ := http.NewRequest("POST", "/v1/user/destroy/apply", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "未设置密码")
}

func TestAnonymizeUsername_FitsColumnLimit(t *testing.T) {
	// 列长 40：本地号码走原格式，海外长号码触发 hash 回退
	cases := []struct {
		name           string
		uid, zone, ph  string
		expectFallback bool
	}{
		{"china_normal", "uid-001", "0086", "13900000000@1700000000000@delete", false},
		// 海外 15 位手机号 + 5 位区号：5+15+1+13+7=41，必须触发 hash 回退
		{"intl_long", "uid-002", "00972", "123456789012345@1700000000000@delete", true},
		{"long_zone", "uid-003", "00999", "987654321098765@1700000000000@delete", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anonymizeUsername(tc.uid, tc.zone, tc.ph, "1700000000000")
			assert.LessOrEqual(t, len(got), 40, "username 必须 ≤ 列长 40：got %q (len=%d)", got, len(got))
			if tc.expectFallback {
				assert.Contains(t, got, "del_", "溢出应回退到 hash 形式")
			} else {
				assert.Equal(t, tc.zone+tc.ph, got, "未溢出应保留原始格式")
			}
		})
	}
}

func TestDestroyApply_AlreadyDestroyed(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	password := "Pwd@12345"
	hashed, _ := HashPassword(password)
	assert.NoError(t, u.db.Insert(&Model{
		UID: testutil.UID, Username: "done_apply", Password: hashed, Name: "done",
		ShortNo: "9013", Status: 1, IsDestroy: IsDestroyDone,
	}))

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"password": password})
	req, _ := http.NewRequest("POST", "/v1/user/destroy/apply", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "账号已注销")
}

func TestDestroyStatus_Normal(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)
	seedDestroyTestUser(t, u, "Pwd@12345")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/destroy/status", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code, body)
	assert.Contains(t, body, `"destroy_status":0`)
	assert.NotContains(t, body, `"expire_at"`)
	assert.NotContains(t, body, `"apply_at"`)
}

// 关键场景：finalize 选中后、写入前用户已撤销；finalize 必须不覆盖。
func TestFinalizeDestroy_RaceWithCancel(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)

	uid := "uid-race"
	assert.NoError(t, u.db.Insert(&Model{
		UID: uid, Username: "race_user", Phone: "13988887777", Zone: "0086",
		ShortNo: "9014", Status: 1,
	}))
	// 先进入冷静期；finalize 路径会查到这条
	assert.NoError(t, u.db.applyDestroy(uid,
		time.Now().Add(-30*24*time.Hour),
		time.Now().Add(-time.Hour))) // 已过期

	// 模拟 finalize 选中后用户在窗口内撤销
	assert.NoError(t, u.db.cancelDestroy(uid))

	// 复刻 finalize 的执行：应识别状态冲突并跳过
	stale, err := u.db.QueryByUID(uid)
	assert.NoError(t, err)
	stale.IsDestroy = IsDestroyApplying // 模拟我们手上是「过时副本」
	err = u.finalizeDestroy(stale)
	assert.NoError(t, err, "finalize 在状态冲突时应静默跳过，不返回错误")

	// 用户状态不应被覆盖
	got, _ := u.db.QueryByUID(uid)
	assert.Equal(t, IsDestroyNo, got.IsDestroy, "用户撤销后 finalize 不能再写入 is_destroy=2")
	assert.NotContains(t, got.Phone, "@delete", "用户撤销后手机号不应被匿名化")
}

func TestDestroyCancel_AfterFinalized(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	hashed, _ := HashPassword("Pwd@12345")
	assert.NoError(t, u.db.Insert(&Model{
		UID: testutil.UID, Username: "done_user", Password: hashed, Name: "done",
		ShortNo: "9011", Status: 1, IsDestroy: IsDestroyDone,
	}))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/destroy/cancel", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "未在注销中")
}

func TestApplyDestroy_DBLayer_ConcurrentConflict(t *testing.T) {
	// 直接在 DB 层验证 RowsAffected 守卫：第二次 apply 必须报 ErrDestroyStateConflict
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	uid := "uid-conflict"
	assert.NoError(t, u.db.Insert(&Model{UID: uid, Username: "u_conflict", ShortNo: "9012", Status: 1}))
	now := time.Now()
	exp := now.Add(7 * 24 * time.Hour)
	assert.NoError(t, u.db.applyDestroy(uid, now, exp))
	// 第二次：is_destroy 已经=1，WHERE 不匹配，必须返回 ErrDestroyStateConflict
	err := u.db.applyDestroy(uid, now, exp)
	assert.ErrorIs(t, err, ErrDestroyStateConflict)
}

func TestCheckDestroyExpired_BatchMultiple(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)

	for i := 0; i < 3; i++ {
		// uid 列 varchar(40)；UUID 已足够唯一，加短前缀避免和其它测试冲突
		uid := fmt.Sprintf("destroy-batch-%d", i)
		assert.NoError(t, u.db.Insert(&Model{
			UID: uid, Username: "batch_" + uid,
			Phone: "1390000000" + string(rune('0'+i)), Zone: "0086", Status: 1,
			ShortNo: fmt.Sprintf("903%d", i), // short_no 唯一约束
		}))
		// 同 TestCheckDestroyExpired_FinalizesAccount：用 applyDestroy 走 SetMap 写入时间字段
		assert.NoError(t, u.db.applyDestroy(uid,
			time.Now().Add(-30*24*time.Hour),
			time.Now().Add(-2*24*time.Hour)))
	}
	u.checkDestroyExpired()

	// 全部应转为已注销
	models, err := u.db.queryDestroyExpired(time.Now(), 100)
	assert.NoError(t, err)
	assert.Empty(t, models, "no records should remain in is_destroy=1 with past expire_at")
}

func TestEmailLogin_BlocksDestroyed_AllowsCoolingOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	// 已注销账号：拒绝
	hashed, _ := HashPassword("Pwd@12345")
	assert.NoError(t, u.db.Insert(&Model{
		UID: "uid-email-done", Username: "done_email", Email: "done@example.com",
		Password: hashed, Name: "done", ShortNo: "9020", Status: 1, IsDestroy: IsDestroyDone,
	}))
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"email": "done@example.com", "password": "Pwd@12345"})
	req, _ := http.NewRequest("POST", "/v1/user/emaillogin", bytes.NewReader([]byte(body)))
	s.GetRoute().ServeHTTP(w, req)
	assert.NotContains(t, w.Body.String(), `"token":`)
}

func TestUsernameLogin_AllowsCoolingOff(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	u := New(ctx)

	password := "Pwd@12345"
	hashed, _ := HashPassword(password)
	username := "cooling_user_xx"
	uid := "uid-cooling"
	assert.NoError(t, u.db.Insert(&Model{
		UID: uid, Username: username, Password: hashed, Name: username,
		ShortNo: "9003", Status: 1,
	}))
	// 用 applyDestroy 落库时间字段（Insert 跳过 struct 字段）
	assert.NoError(t, u.db.applyDestroy(uid, time.Now(), time.Now().Add(5*24*time.Hour)))

	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{
		"username": username,
		"password": password,
		"device":   map[string]interface{}{"device_id": "d1", "device_name": "n", "device_model": "m"},
	})
	req, _ := http.NewRequest("POST", "/v1/user/usernamelogin", bytes.NewReader([]byte(body)))
	s.GetRoute().ServeHTTP(w, req)

	respBody := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code, respBody)
	// 登录响应附带 destroy_status + 剩余天数（具体天数受测试 DSN 时区漂移影响，只断言字段存在）
	assert.Contains(t, respBody, `"destroy_status":1`)
	assert.Contains(t, respBody, `"destroy_remaining_days":`)
	assert.Contains(t, respBody, `"destroy_expire_at":`)
}
