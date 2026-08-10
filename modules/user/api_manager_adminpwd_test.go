package user

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateManagerAccountRejectsWeakAdminPwd 守卫超管播种密码必须过口令复杂度策略。
//
// createManagerAccount 从配置项 adminPwd（env/配置文件）读初始密码并直接落库，此前
// 完全没有强度校验 —— 运维把它设成 123456，超级管理员就是弱口令，而这正是审计和真实
// 攻击面上最贵的那个账号。
//
// 语义是 fail-closed（宁可不建账号也不建弱口令超管）。这只影响"超管尚不存在"的全新
// 部署：已有环境在 createManagerAccount 开头就 early-return 了，不会被锁在外面。
func TestCreateManagerAccountRejectsWeakAdminPwd(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	cfg := ctx.GetConfig()
	adminUID := cfg.Account.AdminUID
	db := NewDB(ctx)

	// 弱口令：拒绝创建
	cfg.AdminPwd = "123456"
	NewManager(ctx)
	got, err := db.QueryByUID(adminUID)
	require.NoError(t, err)
	assert.Nil(t, got, "AdminPwd 不合复杂度时不得创建超级管理员账号")

	// 合规口令：正常创建，且落库的是 bcrypt 哈希（不是明文/MD5）
	const strongPwd = "Str0ng@Admin"
	cfg.AdminPwd = strongPwd
	NewManager(ctx)
	created, err := db.QueryByUID(adminUID)
	require.NoError(t, err)
	require.NotNil(t, created, "AdminPwd 合规时应创建超级管理员账号")

	assert.NotEqual(t, strongPwd, created.Password, "口令不得明文落库")
	assert.True(t, isBcryptHash(created.Password), "口令应以 bcrypt 存储，实际=%q", created.Password)
	matched, needsMigration := CheckPassword(strongPwd, created.Password)
	assert.True(t, matched, "落库的哈希应能校验通过原口令")
	assert.False(t, needsMigration, "新建账号不应命中 MD5 迁移分支")
}
