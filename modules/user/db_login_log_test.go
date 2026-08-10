package user

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 注:本文件依赖 testutil.NewTestServer() 起真实 MySQL。若本地/CI 的 modules/user
// 迁移集里存在无关的历史迁移问题(如 category 表的 collation 冲突)，
// NewTestServer 会在与本文件无关的迁移上 panic——这是环境问题，不代表本文件的
// 断言有误，需要先修复/重建测试库再跑。

func TestLoginLog_RecordSuccessAndFailure_Integration(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	l := NewLoginLog(ctx)
	logDB := NewLoginLogDB(ctx.DB())

	// 手机号注册用户的 username 就是 zone+phone，正是不能落明文的那种输入
	const phoneAccount = "008613800001234"
	l.recordSuccess("u-1", phoneAccount, "1.2.3.4", "username")
	l.recordFailure("bob@example.com", "5.6.7.8", "email")
	l.recordFailure("bob@example.com", "5.6.7.8", "email")

	var rows []*LoginLogModel
	_, err := ctx.DB().Select("*").From("login_log").OrderAsc("id").Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	assert.Equal(t, "u-1", rows[0].UID)
	assert.Equal(t, loginStatusSuccess, rows[0].Status)
	assert.Equal(t, "username", rows[0].LoginType)

	assert.Equal(t, "", rows[1].UID, "failed attempts must not carry a uid")
	assert.Equal(t, loginStatusFailure, rows[1].Status)
	assert.Equal(t, loginStatusFailure, rows[2].Status)

	// 关键契约：整张表不得出现账号明文
	assert.Equal(t, "008****1234", rows[0].AccountMasked)
	assert.Equal(t, "b***@example.com", rows[1].AccountMasked)
	for i, r := range rows {
		assert.NotContains(t, r.AccountMasked, "13800001234", "row %d 泄露了手机号明文", i)
		assert.NotContains(t, r.AccountMasked, "bob@", "row %d 泄露了邮箱本地部分", i)
	}

	// 盲索引可用于精确检索，且同一账号的多次尝试落到同一个 hash
	assert.NotEmpty(t, rows[1].AccountHash)
	assert.Equal(t, rows[1].AccountHash, rows[2].AccountHash, "同一账号的失败尝试应命中同一盲索引")
	assert.NotEqual(t, rows[0].AccountHash, rows[1].AccountHash, "不同账号的盲索引必须不同")

	// 按账号精确检索：拿原始账号算 hash 就能捞出它的全部失败尝试
	hash, err := LoginAccountBlindHash("bob@example.com")
	require.NoError(t, err)
	var failures []*LoginLogModel
	_, err = ctx.DB().Select("*").From("login_log").
		Where("account_hash=? AND status=?", hash, loginStatusFailure).Load(&failures)
	require.NoError(t, err)
	assert.Len(t, failures, 2, "应能按账号盲索引精确检索到 2 次失败")

	// queryLastLoginIP 只看 status=1，不会被后续的失败尝试污染。
	last, err := logDB.queryLastLoginIP("u-1")
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.Equal(t, "1.2.3.4", last.LoginIP)
}

func TestMaskLoginAccount(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"13800001234", "138****1234"},
		{"008613800001234", "008****1234"},
		{"bob@example.com", "b***@example.com"},
		// 单字符 local part 整体掩掉：保留它等于暴露 100% 的本地部分
		{"a@example.com", "*@example.com"},
		// 没有 local part（LastIndex 为 0）不认为是邮箱，走通用分支（掩得更多，安全侧）
		{"@example.com", "@***"},
		{"superAdmin", "s***"},
		{"ab", "**"},
		{"x", "*"},
		{"123456", "1***"},       // 6 位纯数字不足 7，走通用分支
		{"1234567", "1***"},      // 7 位纯数字不能把 7 位全部暴露
		{"用户名字", "用***"},         // 多字节按 rune 处理，不产出非法 UTF-8
		{"13800001234x", "1***"}, // 非纯数字走通用分支
	}
	for _, c := range cases {
		if got := maskLoginAccount(c.in); got != c.want {
			t.Errorf("maskLoginAccount(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskLoginAccountCapsDatabaseValueByRune(t *testing.T) {
	account := "a@" + strings.Repeat("界", 120) + ".example"
	masked := maskLoginAccount(account)
	if !utf8.ValidString(masked) {
		t.Fatal("masked account must remain valid UTF-8")
	}
	if got := len([]rune(masked)); got > 100 {
		t.Fatalf("masked account has %d runes, exceeds login_log.account_masked VARCHAR(100)", got)
	}
}

func TestLoginAccountBlindHash_NormalizesAndDegrades(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	a, err := LoginAccountBlindHash("Bob@Example.com")
	require.NoError(t, err)
	b, err := LoginAccountBlindHash("  bob@example.com  ")
	require.NoError(t, err)
	assert.Equal(t, a, b, "大小写/空白差异应归一化到同一盲索引")

	empty, err := LoginAccountBlindHash("")
	require.NoError(t, err)
	assert.Empty(t, empty, "空账号返回空 hash 而不是报错")

	// 主密钥缺失时降级：只剩掩码，不阻断登录日志
	withPhoneSecretForTest(t, "")
	masked, hash := resolveAccountFields("13800001234")
	assert.Equal(t, "138****1234", masked)
	assert.Empty(t, hash, "主密钥缺失时盲索引降级为空")
}

func TestLoginLog_GetLastLoginIP_NoRecordsReturnsNil(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	l := NewLoginLog(ctx)
	assert.Nil(t, l.getLastLoginIP("no-such-uid"))
}
