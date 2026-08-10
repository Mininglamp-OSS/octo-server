package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInsertUserWithoutPhoneSucceeds 是 CRITICAL-1 的行为回归：
// Model.PhoneEncrypted 是 []byte，无手机号的用户（username / email / OAuth 注册、
// 后台管理员）以及未配置 OCTO_PII_ENCRYPTION_SECRET 的环境下它是 nil。user 表写入
// 走 util.AttrToUnderscore 反射，会把每一列都显式带进 INSERT，于是 nil 被绑成显式
// NULL —— 显式 NULL 打到 NOT NULL 列上不会回落到 DEFAULT，在 STRICT_TRANS_TABLES
// 下直接 Error 1048，结果是所有建号流程全挂。
//
// 这条测试跑的是真实迁移 + 真实 Model（而不是手抄的表结构），所以能真正验证
// 迁移里的可空性声明与 Go 侧字段类型是自洽的。
func TestInsertUserWithoutPhoneSucceeds(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	err := d.Insert(&Model{
		UID:      "u-no-phone",
		Username: "nophoneuser",
		Name:     "无手机号用户",
		ShortNo:  "sn-nophone",
		Status:   1,
		// Phone 为空 → PhoneEncrypted 保持 nil，这正是触发 1048 的组合
	})
	require.NoError(t, err, "没有手机号的用户必须能建号；报 1048 说明 phone_encrypted 被改回 NOT NULL 了")

	got, err := d.QueryByUID("u-no-phone")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.PhoneHash, "无手机号时盲索引应为空")
	assert.Empty(t, got.PhoneLast4)
}

// TestInsertUserWithPhoneEncryptionRoundTrip 验证配好主密钥时 DB.Insert 会自动把三个
// 影子列写好（调用方不需要手工加密），且 QueryByPhone 能靠盲索引而非明文列命中。
func TestInsertUserWithPhoneEncryptionRoundTrip(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)
	require.NotNil(t, d.phoneEnc, "主密钥已配置时 NewDB 应构造出加密器")

	// 注意：调用方只设 Zone/Phone，影子列由 Insert 内部的 syncPhoneShadow 负责
	require.NoError(t, d.Insert(&Model{
		UID:      "u-with-phone",
		Username: "008613800001234",
		Name:     "有手机号用户",
		ShortNo:  "sn-withphone",
		Status:   1,
		Zone:     "0086",
		Phone:    "13800001234",
	}))

	got, err := d.QueryByUID("u-with-phone")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "1234", got.PhoneLast4, "Insert 应自动填充 phone_last4")
	assert.NotEmpty(t, got.PhoneHash, "Insert 应自动填充盲索引")
	assert.NotEmpty(t, got.PhoneEncrypted, "Insert 应自动填充密文")

	// 密文经 VARBINARY 存取后仍可解密回原文
	plain, err := d.phoneEnc.decryptPhone(got.PhoneEncrypted)
	require.NoError(t, err)
	assert.Equal(t, phoneCryptoInput("0086", "13800001234"), plain)

	// 盲索引路径能命中：把明文列改脏后仍然查得到，证明走的是 phone_hash 而不是 phone
	_, err = ctx.DB().Update("user").Set("phone", "not-a-real-phone").Where("uid=?", "u-with-phone").Exec()
	require.NoError(t, err)
	found, err := d.QueryByPhone("0086", "13800001234")
	require.NoError(t, err)
	require.NotNil(t, found, "盲索引应命中，即使明文列已被改动")
	assert.Equal(t, "u-with-phone", found.UID)
}

// TestDestroyFreesPhoneForReuse 是 CRITICAL-2 的行为回归：
// 注销会把 phone 匿名化成 `<原号>@<stamp>@delete` 来释放手机号。若 phone_hash 仍留着
// 原号的盲索引，QueryByPhone 的盲索引优先路径就会把已注销账号当成"该手机号已注册"
// 命中 —— sendRegisterCode 会永久拒绝该号码重新注册，pwdforget 更会在号码被新用户
// 复用后把重置目标落到已注销那一行。
func TestDestroyFreesPhoneForReuse(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.Insert(&Model{
		UID:      "u-doomed",
		Username: "008613900002222",
		Name:     "将注销的用户",
		ShortNo:  "sn-doomed",
		Status:   1,
		Zone:     "0086",
		Phone:    "13900002222",
	}))

	// 注销前：查得到
	before, err := d.QueryByPhone("0086", "13900002222")
	require.NoError(t, err)
	require.NotNil(t, before)

	// 走真实注销路径（legacy 即时注销：要求 is_destroy=0）
	require.NoError(t, d.destroyAccount("u-doomed", "anon-doomed", "13900002222@1700000000000@delete"))

	// 注销后：该手机号必须彻底查不到，否则号码永远无法复用
	after, err := d.QueryByPhone("0086", "13900002222")
	require.NoError(t, err)
	assert.Nil(t, after, "已注销账号不得再占用原手机号；非 nil 说明 phone_hash 没被清空")

	// 影子列全部清空
	destroyed, err := d.QueryByUID("u-doomed")
	require.NoError(t, err)
	require.NotNil(t, destroyed)
	assert.Empty(t, destroyed.PhoneHash, "phone_hash 必须随 phone 匿名化一起清空")
	assert.Empty(t, destroyed.PhoneLast4)
	assert.Empty(t, destroyed.PhoneEncrypted)

	// 号码可被新用户复用，且 QueryByPhone 命中的是新用户
	require.NoError(t, d.Insert(&Model{
		UID:      "u-newcomer",
		Username: "008613900002222-new",
		Name:     "复用号码的新用户",
		ShortNo:  "sn-newcomer",
		Status:   1,
		Zone:     "0086",
		Phone:    "13900002222",
	}))
	reused, err := d.QueryByPhone("0086", "13900002222")
	require.NoError(t, err)
	require.NotNil(t, reused)
	assert.Equal(t, "u-newcomer", reused.UID, "复用号码后必须命中新用户，而不是已注销的旧行")
}

// TestRegisterRejectsWeakPassword 覆盖 handler 层的密码强度接入：弱密码必须被
// 专用错误码拒绝，而不是被放行或塌成通用参数错误。
//
// 同时锁住校验顺序：策略闸门（注册关闭 / 仅中国号码）先于密码强度校验，否则弱密码
// 会把 only_china 这类安全闸门的响应挡掉（见 TestPhoneRegisterBlockedByOnlyChina）。
//
// 每个子用例用独立 IP 并在开跑前清掉该 IP 的 strict 限流桶：/v1/user/register 挂了
// registerLimit（5 req/min, burst 3），桶存在 Redis 且 CleanAllTables 不清理，
// 共用 IP 会让第 3 个子用例拿到 rate.limited 而不是密码错误码（同
// CLAUDE.md 里 resetUIDRateLimit 的约定）。
func TestRegisterRejectsWeakPassword(t *testing.T) {
	cases := []struct {
		name     string
		ip       string
		password string
		wantCode string
	}{
		{"7 位太短", "9.9.9.101", "Ab1@xyz", "err.server.user.password_too_short"},
		{"8 位但只有一类字符", "9.9.9.102", "12345678", "err.server.user.password_too_weak"},
		{"4 个中文字符按 rune 计不足 8 位", "9.9.9.103", "密码密码", "err.server.user.password_too_short"},
	}

	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStrictRateLimitForUserTest(t, ctx, "register", tc.ip)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/user/register", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
				"name":     "weakpwd",
				"zone":     "0086",
				"phone":    "13700003333",
				"code":     "123456",
				"password": tc.password,
			}))))
			setPublicIPForUserTest(req, tc.ip)
			s.GetRoute().ServeHTTP(w, req)

			assert.Contains(t, w.Body.String(), tc.wantCode)
		})
	}
}

// resetStrictRateLimitForUserTest 清掉 StrictIPRateLimitMiddleware 在 Redis 里的
// per-IP 桶。桶的生命周期跟随 Redis 而非测试，CleanAllTables 不会清它，所以任何命中
// 限流路由的测试都必须在 setup 阶段自己重置，否则会跨用例/跨轮次互相污染。
func resetStrictRateLimitForUserTest(t *testing.T, ctx *config.Context, tag, ip string) {
	t.Helper()
	if err := ctx.GetRedisConn().Del("ratelimit:strict:" + tag + ":" + ip); err != nil {
		t.Logf("reset strict ratelimit bucket %s/%s failed: %v", tag, ip, err)
	}
}
