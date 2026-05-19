package oidc

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// 强制注册下游模块以满足 space / 其他模块的跨表迁移依赖。
	// oidc → user → space 的传递链已带上 space,但 group / robot 不在传递路径里。
	// space-20260307-03.sql ALTER TABLE group; space-20260308-01.sql FROM robot。
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

// 集成测试:打通 oidc.DB 的全部 SQL 操作。
// 依赖真 MySQL(testenv-mysql-1)+ 完整迁移。

func TestDB_InsertAndQueryIdentity_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	now := time.Now()
	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID:           "u-1",
		Issuer:        "https://aegis",
		Subject:       "sub-1",
		Email:         "alice@example.com",
		EmailVerified: 1,
		LinkedAt:      now,
	}))

	got, err := d.QueryIdentityByIssuerSubject("https://aegis", "sub-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "u-1", got.UID)
	assert.Equal(t, "alice@example.com", got.Email)
	assert.Equal(t, 1, got.EmailVerified)
}

func TestDB_QueryIdentityByIssuerSubject_NotFound_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	got, err := d.QueryIdentityByIssuerSubject("https://aegis", "nope")
	require.NoError(t, err)
	assert.Nil(t, got, "missing binding should return nil, not error")
}

func TestDB_QueryUIDsByEmail_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	// 直接插 user 表(避开 user 模块全套依赖)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, email, short_no, vercode, status, is_destroy) VALUES ('u-a','ua','A','same@x.com','sa','va@1',1,0)",
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, email, short_no, vercode, status, is_destroy) VALUES ('u-b','ub','B','same@x.com','sb','vb@1',1,0)",
	).Exec()
	require.NoError(t, err)
	// 已注销用户不该返回
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, email, short_no, vercode, status, is_destroy) VALUES ('u-c','uc','C','same@x.com','sc','vc@1',1,1)",
	).Exec()
	require.NoError(t, err)
	// 已封禁用户(status=0)也不该返回:之前缺这个过滤,SMS bind 会给停用账号
	// 写 user_oidc_identity 行,然后 IssueSession 才拒绝,残留脏数据让该用户
	// 后续 OIDC 登录持续失败。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, email, short_no, vercode, status, is_destroy) VALUES ('u-d','ud','D','same@x.com','sd','vd@1',0,0)",
	).Exec()
	require.NoError(t, err)

	uids, err := d.QueryUIDsByEmail("same@x.com")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"u-a", "u-b"}, uids)

	none, err := d.QueryUIDsByEmail("nobody@x.com")
	require.NoError(t, err)
	assert.Empty(t, none)

	// 空邮箱直接返回 nil,不执行 SQL
	none, err = d.QueryUIDsByEmail("")
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestDB_InsertRefresh_AndRotate_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-r", Issuer: "https://aegis", Subject: "sub-r", LinkedAt: time.Now(),
	}))
	idRow, err := d.QueryIdentityByIssuerSubject("https://aegis", "sub-r")
	require.NoError(t, err)
	require.NotNil(t, idRow)

	rt := &RefreshModel{
		IdentityID:      idRow.Id,
		TokenHash:       "hash-1",
		TokenCiphertext: []byte("cipher-1"),
		ExpiresAt:       time.Now().Add(24 * time.Hour),
	}
	require.NoError(t, d.InsertRefresh(rt))

	got, err := d.QueryRefreshByHash("hash-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "hash-1", got.TokenHash)

	newRT := &RefreshModel{
		IdentityID:      idRow.Id,
		TokenHash:       "hash-2",
		TokenCiphertext: []byte("cipher-2"),
		ExpiresAt:       time.Now().Add(24 * time.Hour),
	}
	require.NoError(t, d.RotateRefresh(got.Id, newRT))

	// 旧 RT 应已 revoked
	old, err := d.QueryRefreshByHash("hash-1")
	require.NoError(t, err)
	require.NotNil(t, old)
	assert.NotNil(t, old.RevokedAt, "old RT should be revoked after rotate")

	// 新 RT 应可查
	created, err := d.QueryRefreshByHash("hash-2")
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Nil(t, created.RevokedAt)
}

func TestDB_QueryIdentitiesByUID_AndUpdateIdentityLogin_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-multi", Issuer: "https://aegis", Subject: "sub-aegis", LinkedAt: time.Now(),
	}))
	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-multi", Issuer: "https://google", Subject: "sub-google", LinkedAt: time.Now(),
	}))

	rows, err := d.QueryIdentitiesByUID("u-multi")
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// UpdateIdentityLogin 把 last_login_at 等字段刷新
	target := rows[0]
	require.NoError(t, d.UpdateIdentityLogin(target.Id,
		"new@example.com", 1, "+8613900000000", 1))

	updated, err := d.QueryIdentityByIssuerSubject(target.Issuer, target.Subject)
	require.NoError(t, err)
	assert.Equal(t, "new@example.com", updated.Email)
	assert.Equal(t, 1, updated.EmailVerified)
	assert.Equal(t, "+8613900000000", updated.Phone)
	require.NotNil(t, updated.LastLoginAt)
}

func TestDB_QueryUIDsByPhone_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) VALUES ('u-p1','up1','P1','0086','13900000001','sp1','vp1@1',1,0)",
	).Exec()
	require.NoError(t, err)
	// 停用账号(status=0)不该出现在 bind locator 结果里,与 email 路径同款回归。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) VALUES ('u-p2','up2','P2','0086','13900000002','sp2','vp2@1',0,0)",
	).Exec()
	require.NoError(t, err)

	uids, err := d.QueryUIDsByPhone("0086", "13900000001")
	require.NoError(t, err)
	assert.Equal(t, []string{"u-p1"}, uids)

	// status=0 用户的 phone 查不到 (anti-binding-to-disabled-account)
	none, err := d.QueryUIDsByPhone("0086", "13900000002")
	require.NoError(t, err)
	assert.Empty(t, none, "disabled (status=0) user must not appear in bind locator results")

	none, err = d.QueryUIDsByPhone("0086", "13800000000")
	require.NoError(t, err)
	assert.Empty(t, none)

	// 空 phone 直接 nil
	none, err = d.QueryUIDsByPhone("0086", "")
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestDB_MarkRefreshRevoked_Idempotent_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-rev", Issuer: "https://aegis", Subject: "sub-rev", LinkedAt: time.Now(),
	}))
	idRow, err := d.QueryIdentityByIssuerSubject("https://aegis", "sub-rev")
	require.NoError(t, err)
	require.NoError(t, d.InsertRefresh(&RefreshModel{
		IdentityID: idRow.Id, TokenHash: "h-rev",
		TokenCiphertext: []byte("c"), ExpiresAt: time.Now().Add(time.Hour),
	}))
	rt, err := d.QueryRefreshByHash("h-rev")
	require.NoError(t, err)

	// 第一次标记吊销:rowsAffected=1
	n1, err := d.MarkRefreshRevoked(rt.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n1, "first revoke should affect 1 row")

	got, err := d.QueryRefreshByHash("h-rev")
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt)

	// 第二次幂等不报错,但 rowsAffected=0 —— 多实例竞态检测的关键信号
	n2, err := d.MarkRefreshRevoked(rt.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2, "second revoke must report 0 rows for race detection")
}

func TestDB_DueRefreshes_JoinsUID_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-due", Issuer: "https://aegis", Subject: "sub-due", LinkedAt: time.Now(),
	}))
	idRow, err := d.QueryIdentityByIssuerSubject("https://aegis", "sub-due")
	require.NoError(t, err)
	require.NoError(t, d.InsertRefresh(&RefreshModel{
		IdentityID: idRow.Id, TokenHash: "h-due",
		TokenCiphertext: []byte("ct-due"),
		ExpiresAt:       time.Now().Add(time.Hour),
	}))
	// 已 revoked 的不该出现
	rev := &RefreshModel{
		IdentityID: idRow.Id, TokenHash: "h-rev",
		TokenCiphertext: []byte("ct-rev"),
		ExpiresAt:       time.Now().Add(time.Hour),
	}
	require.NoError(t, d.InsertRefresh(rev))
	got, err := d.QueryRefreshByHash("h-rev")
	require.NoError(t, err)
	_, err = d.MarkRefreshRevoked(got.Id)
	require.NoError(t, err)

	due, err := d.DueRefreshes(10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, "u-due", due[0].UID)
	assert.Equal(t, idRow.Id, due[0].IdentityID)
	assert.Equal(t, []byte("ct-due"), due[0].TokenCiphertext)
}

func TestDB_RevokeRefreshByUID_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	// 一个 uid 绑两个 issuer,各一个 RT,RevokeRefreshByUID 应同时吊销两条
	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-out", Issuer: "https://aegis", Subject: "sub-1", LinkedAt: time.Now(),
	}))
	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-out", Issuer: "https://google", Subject: "sub-2", LinkedAt: time.Now(),
	}))
	rows, err := d.QueryIdentitiesByUID("u-out")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for i, ir := range rows {
		require.NoError(t, d.InsertRefresh(&RefreshModel{
			IdentityID:      ir.Id,
			TokenHash:       "h-out-" + string(rune('a'+i)),
			TokenCiphertext: []byte("ct"),
			ExpiresAt:       time.Now().Add(time.Hour),
		}))
	}
	// 另一个 uid 的 RT 不该被波及
	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-bystander", Issuer: "https://aegis", Subject: "sub-bys", LinkedAt: time.Now(),
	}))
	bys, err := d.QueryIdentityByIssuerSubject("https://aegis", "sub-bys")
	require.NoError(t, err)
	require.NoError(t, d.InsertRefresh(&RefreshModel{
		IdentityID: bys.Id, TokenHash: "h-bys",
		TokenCiphertext: []byte("ct-bys"),
		ExpiresAt:       time.Now().Add(time.Hour),
	}))

	n, err := d.RevokeRefreshByUID("u-out")
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	// 旁观者仍 active
	bysRT, err := d.QueryRefreshByHash("h-bys")
	require.NoError(t, err)
	assert.Nil(t, bysRT.RevokedAt)

	// 再 revoke 一次幂等(已吊销不计)
	n2, err := d.RevokeRefreshByUID("u-out")
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2)

	// 不存在的 uid 返回 0,无副作用
	n3, err := d.RevokeRefreshByUID("u-noexist")
	require.NoError(t, err)
	assert.EqualValues(t, 0, n3)
}

func TestDB_InsertAudit_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertAudit(&AuditModel{
		UID:     "u-1",
		Event:   EventCallbackOK,
		IP:      "127.0.0.1",
		TraceID: "trace-1",
	}))
}

// TestDB_UkUidIssuer_RejectsDuplicate_Integration 锁定迁移
// 20260515000001_oidc_bind_uniques.sql 引入的 uk_uid_issuer 行为:
//   - 同 (uid, issuer) 第二次插入(sub 不同)必须被 DB 唯一约束拒绝
//   - 同 uid 跨不同 issuer 仍允许(多 IdP 绑定保留)
//
// 这是 OIDC 自助绑定 P0 FR-5.3 / SR-5 的硬保证 —— confirm 路径上层应用
// 的 CAS 是次要防护,DB 约束才是兜底。
func TestDB_UkUidIssuer_RejectsDuplicate_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-uk", Issuer: "https://idp-a", Subject: "sub-1", LinkedAt: time.Now(),
	}))

	// 同 uid 同 issuer + 不同 sub: 必须被 uk_uid_issuer 拒绝,
	// 而不是被 uk_issuer_subject(那条约束只看 issuer+subject)误判通过。
	err := d.InsertIdentity(&IdentityModel{
		UID: "u-uk", Issuer: "https://idp-a", Subject: "sub-2", LinkedAt: time.Now(),
	})
	require.Error(t, err, "duplicate (uid, issuer) must be rejected")
	assert.True(t, isDuplicateKeyError(err),
		"expected MySQL 1062 duplicate-key, got %v", err)
	// 断言具体约束名,避免未来引入别的 unique key 时误判通过 ——
	// MySQL 1062 错误消息携带 key 名,例如:
	//   "Duplicate entry 'u-uk-https://idp-a' for key 'user_oidc_identity.uk_uid_issuer'"
	assert.Contains(t, err.Error(), "uk_uid_issuer",
		"rejection must come from uk_uid_issuer, not another constraint")

	// 同 uid + 不同 issuer 允许(同一用户绑多个 IdP)
	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-uk", Issuer: "https://idp-b", Subject: "sub-3", LinkedAt: time.Now(),
	}))

	rows, err := d.QueryIdentitiesByUID("u-uk")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}
