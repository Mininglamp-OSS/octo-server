package user

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOIDCBindUser 准备一个带 bcrypt 密码的可登录用户(默认 Status=1, IsDestroy=0)。
//
// 同时清理本 uid 在 loginGuard 上的失败计数:testutil 不 flush Redis,
// 同 binary 多次跑同一 uid 时遗留的 login:fail:oidc-bind:<uid> 会让阈值测试
// 直接 rate_limited 提前失败。在 seed 阶段统一清场让每个 case 从零计数。
func seedOIDCBindUser(t *testing.T, u *User, uid, username, password string) {
	t.Helper()
	u.loginGuard.ResetLogged(oidcBindGuardPrefix + uid)
	hashed, err := HashPassword(password)
	require.NoError(t, err)
	err = u.db.Insert(&Model{
		UID:      uid,
		Username: username,
		Password: hashed,
		Name:     username,
		ShortNo:  uid + "_sn",
		Status:   1,
	})
	require.NoError(t, err)
}

// TestUser_VerifyPasswordByUID_Matches 锁定正例:bcrypt 密码命中 -> matched=true,
// reason 空,无 err。
func TestUser_VerifyPasswordByUID_Matches(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	seedOIDCBindUser(t, u, "u-ok", "okuser01", "Pwd@12345")

	matched, reason, err := u.VerifyPasswordByUID(context.Background(), "u-ok", "Pwd@12345")
	require.NoError(t, err)
	assert.True(t, matched)
	assert.Empty(t, reason)
}

// TestUser_VerifyPasswordByUID_NegativeBranches 表驱动覆盖各类拒绝原因:
// 不存在 uid / 密码错 / 已注销账号 / Status=0 / 空 uid。
func TestUser_VerifyPasswordByUID_NegativeBranches(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	seedOIDCBindUser(t, u, "u-pw", "pwuser01", "Pwd@12345")

	// 额外种子:已注销 + 已封禁。两个都先清 loginGuard 残留(说明详见
	// seedOIDCBindUser 注释),否则同 binary 多次跑会 rate_limited 假阳。
	for _, uid := range []string{"u-destroyed", "u-banned", "u-nope"} {
		u.loginGuard.ResetLogged(oidcBindGuardPrefix + uid)
	}
	destroyed := &Model{
		UID: "u-destroyed", Username: "destroyed01",
		Password: mustHash(t, "Pwd@12345"),
		Name:     "d", ShortNo: "d_sn", Status: 1, IsDestroy: IsDestroyDone,
	}
	require.NoError(t, u.db.Insert(destroyed))
	banned := &Model{
		UID: "u-banned", Username: "banned01",
		Password: mustHash(t, "Pwd@12345"),
		Name:     "b", ShortNo: "b_sn", Status: 0,
	}
	require.NoError(t, u.db.Insert(banned))

	cases := []struct {
		name       string
		uid, pwd   string
		wantMatch  bool
		wantReason string
	}{
		{"empty uid returns error", "", "Pwd@12345", false, ""},
		{"uid not found", "u-nope", "Pwd@12345", false, BindReasonUserNotFound},
		{"password mismatch", "u-pw", "WrongPwd", false, BindReasonPasswordMismatch},
		{"destroyed account", "u-destroyed", "Pwd@12345", false, BindReasonUserUnavailable},
		{"banned account (status=0)", "u-banned", "Pwd@12345", false, BindReasonUserUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, reason, err := u.VerifyPasswordByUID(context.Background(), tc.uid, tc.pwd)
			if tc.uid == "" {
				assert.Error(t, err, "empty uid must surface as error, not silent false")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMatch, matched)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

// TestUser_VerifyPasswordByUID_RateLimits 验证:连续失败超阈值后即便密码正确
// 也会被锁定(reason=rate_limited),且成功路径会清场失败计数。
//
// 默认 LoginGuard 阈值 5 次/15min。
func TestUser_VerifyPasswordByUID_RateLimits(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	seedOIDCBindUser(t, u, "u-rl", "rluser01", "Pwd@12345")

	// 连失败 5 次累计触发阈值
	for i := 0; i < 5; i++ {
		matched, reason, err := u.VerifyPasswordByUID(context.Background(), "u-rl", "wrong")
		require.NoError(t, err)
		assert.False(t, matched)
		assert.Equal(t, BindReasonPasswordMismatch, reason)
	}
	// 第 6 次即便密码对也被锁
	matched, reason, err := u.VerifyPasswordByUID(context.Background(), "u-rl", "Pwd@12345")
	require.NoError(t, err)
	assert.False(t, matched, "after threshold must be rate_limited even on correct password")
	assert.Equal(t, "rate_limited", reason)
}

// TestUser_VerifyPasswordByUID_SuccessClearsCounter 锁定一条关键 UX 不变式:
// 失败几次后输入正确密码必须解锁,且未触发阈值时正例不留累计。
func TestUser_VerifyPasswordByUID_SuccessClearsCounter(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	seedOIDCBindUser(t, u, "u-rs", "rsuser01", "Pwd@12345")

	// 4 次失败(未到 5 次阈值)
	for i := 0; i < 4; i++ {
		matched, _, _ := u.VerifyPasswordByUID(context.Background(), "u-rs", "wrong")
		require.False(t, matched)
	}
	// 第 5 次输正确密码:Reset 后下一轮可以从头错 5 次
	matched, _, err := u.VerifyPasswordByUID(context.Background(), "u-rs", "Pwd@12345")
	require.NoError(t, err)
	assert.True(t, matched)

	// Reset 生效:再连错 4 次仍不锁
	for i := 0; i < 4; i++ {
		matched, reason, _ := u.VerifyPasswordByUID(context.Background(), "u-rs", "wrong")
		assert.False(t, matched)
		assert.Equal(t, BindReasonPasswordMismatch, reason,
			"reset 后失败计数应从 0 重新累计,不应立即 rate_limited")
	}
}

// TestUser_VerifyPasswordByUID_GuardKeyspaceIsolation 验证 oidc-bind:* 维度与
// username 登录维度独立计数 —— 同账号在登录路径已被锁,绑定路径仍可尝试
// (反之亦然)。
//
// 失败维度独立是 SR-2.2 与登录限流共生的关键:用户即便忘了 dmwork 密码连续登录失败,
// 也不应在 OIDC 绑定时被立刻拒绝(他可能用短信 OTP 完成绑定)。
func TestUser_VerifyPasswordByUID_GuardKeyspaceIsolation(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)
	seedOIDCBindUser(t, u, "u-iso", "isouser01", "Pwd@12345")

	// 在 username 登录路径上把 isouser01 打到锁定阈值
	for i := 0; i < 5; i++ {
		u.loginGuard.RecordFailureLogged("isouser01")
	}
	require.Error(t, u.loginGuard.Check("isouser01"),
		"前置:登录路径已锁定")

	// OIDC 绑定路径用 uid 维度,不应被 username 锁定波及
	matched, reason, err := u.VerifyPasswordByUID(context.Background(), "u-iso", "Pwd@12345")
	require.NoError(t, err)
	assert.True(t, matched, "OIDC 绑定路径使用独立 oidc-bind:<uid> 命名空间,不应被 username 锁定影响")
	assert.Empty(t, reason)
}

// TestUser_VerifyPasswordByUID_LegacyMD5Migration 锁定与 username 登录同款的
// MD5→bcrypt 自动迁移:旧账号(MD5(MD5(pwd)) hash)首次 OIDC 绑定登录成功
// 后,DB 里的 hash 应当被升级为 bcrypt。
func TestUser_VerifyPasswordByUID_LegacyMD5Migration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	u := New(ctx)

	// 手工写入 MD5(MD5(pwd)) hash(模拟历史账号)
	legacyHash := legacyMD5(t, "Pwd@12345")
	err := u.db.Insert(&Model{
		UID: "u-md5", Username: "md5user01",
		Password: legacyHash, Name: "m5", ShortNo: "m5_sn", Status: 1,
	})
	require.NoError(t, err)

	matched, _, err := u.VerifyPasswordByUID(context.Background(), "u-md5", "Pwd@12345")
	require.NoError(t, err)
	require.True(t, matched)

	// DB 里的 hash 应已迁移
	after, err := u.db.QueryByUID("u-md5")
	require.NoError(t, err)
	assert.True(t, isBcryptHash(after.Password),
		"MD5 命中后应自动写回 bcrypt,实际还是 %s", after.Password)
}

func mustHash(t *testing.T, pwd string) string {
	t.Helper()
	h, err := HashPassword(pwd)
	require.NoError(t, err)
	return h
}

// legacyMD5 复制 password.go 内部的旧版哈希生成方式(util.MD5(util.MD5(pwd))),
// 仅供 LegacyMD5Migration 测试制造老账号种子数据。
func legacyMD5(t *testing.T, pwd string) string {
	t.Helper()
	return util.MD5(util.MD5(pwd))
}
