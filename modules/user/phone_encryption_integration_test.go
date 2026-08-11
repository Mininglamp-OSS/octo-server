package user

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestInsertUserWithPhoneDegradesToPlaintextWithoutKey 锁住降级契约：主密钥缺失时
// 写入"带手机号的用户"不失败，而是只写明文 phone、三个影子列留空。
//
// 选择降级而不是 fail-closed 的理由见 DB.syncPhoneShadow 的注释：手机号注册是主
// 注册路径，而该环境变量在部署侧尚无接通路径，fail-closed 会让标准升级直接打断它。
//
// 这个取舍成立的前提是「降级可收敛」，所以本测试不止断言写入成功，还断言：
//   - 降级行与存量未回填行同形（phone 非空 + 影子列全空），因此被 remaining 计入；
//   - 补配密钥后跑回填，这批行会被补齐并可经盲索引命中。
//
// 少了后半段，这个测试就只是在给"明文入库"背书，而不是在锁住它能被修回来。
func TestInsertUserWithPhoneDegradesToPlaintextWithoutKey(t *testing.T) {
	withPhoneSecretForTest(t, "") // 清掉 TestMain 兜底的密钥
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)
	require.Nil(t, d.phoneEnc, "密钥缺失时不应构造出加密器")

	require.NoError(t, d.Insert(&Model{
		UID:      "u-phone-nokey",
		Username: "008613800009999",
		Name:     "缺密钥带手机号",
		ShortNo:  "sn-nokey",
		Status:   1,
		Zone:     "0086",
		Phone:    "13800009999",
	}), "缺密钥时带手机号的写入应降级放行，不得阻断建号")

	got, err := d.QueryByUID("u-phone-nokey")
	require.NoError(t, err)
	require.NotNil(t, got, "降级写入的行必须落库")
	assert.Equal(t, "13800009999", got.Phone, "降级时明文手机号照常写入")
	assert.Empty(t, got.PhoneHash, "降级时盲索引必须留空")
	assert.Empty(t, got.PhoneEncrypted, "降级时密文列必须留空")
	assert.Empty(t, got.PhoneLast4, "降级时后4位列必须留空")

	// 降级期的行仍可被明文腿检索到（查重 / 找回密码 / OIDC 绑定定位都依赖这条）
	byPhone, err := d.QueryByPhone("0086", "13800009999")
	require.NoError(t, err)
	require.NotNil(t, byPhone, "降级写入的手机号必须仍能被 QueryByPhone 命中")
	assert.Equal(t, "u-phone-nokey", byPhone.UID)

	// 同一个 DB 实例下，无手机号的用户仍可正常建号
	require.NoError(t, d.Insert(&Model{
		UID:      "u-nophone-nokey",
		Username: "nophone_nokey",
		Name:     "缺密钥无手机号",
		ShortNo:  "sn-nophone-nokey",
		Status:   1,
	}), "缺密钥不得阻断无手机号用户的建号")

	// 补配密钥后回填：降级行必须被 remaining 计入并补齐
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	repaired := NewDB(ctx)
	require.NotNil(t, repaired.phoneEnc)

	pending, err := repaired.CountPhoneShadowPending()
	require.NoError(t, err)
	assert.EqualValues(t, 1, pending, "降级行必须与存量未回填行一样计入 remaining")

	result, err := repaired.BackfillPhoneShadow(context.Background(), 0, 10, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.Updated)
	assert.EqualValues(t, 0, result.Remaining)
	assert.True(t, result.Done, "补齐降级行后回填应收敛")

	healed, err := repaired.QueryByUID("u-phone-nokey")
	require.NoError(t, err)
	require.NotNil(t, healed)
	assert.NotEmpty(t, healed.PhoneHash, "回填后盲索引应补齐")
	assert.NotEmpty(t, healed.PhoneEncrypted, "回填后密文列应补齐")
	assert.Equal(t, "9999", healed.PhoneLast4)

	// 补齐后必须能走盲索引腿命中，否则"可收敛"只是形式上的
	byHash, err := repaired.QueryByPhone("0086", "13800009999")
	require.NoError(t, err)
	require.NotNil(t, byHash)
	assert.Equal(t, "u-phone-nokey", byHash.UID)
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

// TestQueryByPhoneIgnoresRollbackStaleDestroyedShadow simulates the documented
// rollback sequence: an older binary destroys the account but cannot clear the
// shadow columns it does not know about. A later roll-forward must not resolve
// the released phone number to that tombstone row.
func TestQueryByPhoneIgnoresRollbackStaleDestroyedShadow(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.Insert(&Model{
		UID:      "u-rollback-destroyed",
		Username: "008613900003333",
		Name:     "rollback destroyed",
		ShortNo:  "sn-rollback-destroyed",
		Status:   1,
		Zone:     "0086",
		Phone:    "13900003333",
	}))

	// Mirror the pre-shadow binary's destroy update: mutate the plaintext fields
	// and state only, deliberately leaving phone_hash/ciphertext/last4 stale.
	_, err := ctx.DB().Update("user").SetMap(map[string]interface{}{
		"phone":      "13900003333@1700000000000@delete",
		"username":   "anon-rollback-destroyed",
		"is_destroy": IsDestroyDone,
	}).Where("uid=?", "u-rollback-destroyed").Exec()
	require.NoError(t, err)

	destroyed, err := d.QueryByUID("u-rollback-destroyed")
	require.NoError(t, err)
	require.NotNil(t, destroyed)
	require.NotEmpty(t, destroyed.PhoneHash, "fixture must retain the stale blind index")

	got, err := d.QueryByPhone("0086", "13900003333")
	require.NoError(t, err)
	assert.Nil(t, got, "destroyed account with a rollback-stale blind index must not occupy the phone")
}

func TestQueryByPhonePrefersEarliestMixedBackfillDuplicate(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	// 先插入尚未回填的存量重复行，再通过新写路径插入带 hash 的后来行。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) " +
			"VALUES ('u-duplicate-old','duplicate-old','old','0086','13800008888','sn-dup-old','v-dup-old@1',1,0)").Exec()
	require.NoError(t, err)
	require.NoError(t, d.Insert(&Model{
		UID:      "u-duplicate-new",
		Username: "duplicate-new",
		Name:     "new",
		Zone:     "0086",
		Phone:    "13800008888",
		ShortNo:  "sn-dup-new",
		Vercode:  "v-dup-new@1",
		Status:   1,
	}))

	got, err := d.QueryByPhone("0086", "13800008888")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "u-duplicate-old", got.UID,
		"partial backfill must not let a later hashed duplicate outrank the earliest plaintext row")
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

// TestBackfillPhoneShadow 覆盖存量回填：迁移只新增空列，存量行没有密文和盲索引，
// 而读路径的明文兜底会让这件事看不出异常 —— 回填任务是这一阶真正生效的前提。
func TestBackfillPhoneShadow(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	// 造 5 个"存量"行：绕过 DB.Insert 直接写裸 SQL，模拟迁移前就存在、影子列为空的数据
	for i := 0; i < 5; i++ {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) "+
				"VALUES (?,?,?,?,?,?,?,1,0)",
			fmt.Sprintf("u-legacy-%d", i), fmt.Sprintf("0086139000000%02d", i),
			fmt.Sprintf("legacy%d", i), "0086", fmt.Sprintf("139000000%02d", i),
			fmt.Sprintf("sn-legacy-%d", i), fmt.Sprintf("v-legacy-%d@1", i),
		).Exec()
		require.NoError(t, err)
	}
	// 一个无手机号的行：不应被回填任务计入
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) " +
			"VALUES ('u-legacy-nophone','nophone','nophone','','','sn-legacy-np','v-np@1',1,0)").Exec()
	require.NoError(t, err)

	pending, err := d.CountPhoneShadowPending()
	require.NoError(t, err)
	assert.EqualValues(t, 5, pending, "只有 phone<>'' 且 hash 不是当前版本的行算待回填")

	// 小批次 + 零间隔，验证游标推进
	res, err := d.BackfillPhoneShadow(context.Background(), 0, 2, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 5, res.Updated)
	assert.EqualValues(t, 0, res.Failed)
	assert.True(t, res.Done)
	assert.EqualValues(t, 0, res.Remaining, "回填后应无待回填行")

	// 回填后盲索引可用：按手机号查得到，且命中的是正确的行
	found, err := d.QueryByPhone("0086", "13900000003")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "u-legacy-3", found.UID)
	assert.Equal(t, "0003", found.PhoneLast4)
	assert.NotEmpty(t, found.PhoneEncrypted)

	// 幂等：再跑一次不应重复更新
	res2, err := d.BackfillPhoneShadow(context.Background(), 0, 2, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 0, res2.Updated, "已回填的行不应被再次更新")
	assert.True(t, res2.Done)

	// 无手机号的行始终不被触碰
	np, err := d.QueryByUID("u-legacy-nophone")
	require.NoError(t, err)
	require.NotNil(t, np)
	assert.Empty(t, np.PhoneHash)
}

func TestBackfillPhoneShadowRewritesLegacyHashVersion(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy, phone_encrypted, phone_hash, phone_last4) "+
			"VALUES ('u-v1-hash','008613800006666','legacy-hash','0086','13800006666','sn-v1-hash','v-v1-hash@1',1,0,?,?,'6666')",
		[]byte("legacy-ciphertext"), "1:legacy-hash",
	).Exec()
	require.NoError(t, err)

	pending, err := d.CountPhoneShadowPending()
	require.NoError(t, err)
	assert.EqualValues(t, 1, pending, "legacy hash versions must remain pending")

	res, err := d.BackfillPhoneShadow(context.Background(), 0, 10, time.Nanosecond)
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.Updated)
	assert.True(t, res.Done)

	got, err := d.QueryByUID("u-v1-hash")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, strings.HasPrefix(got.PhoneHash, "2:"), "legacy hash must be rewritten: %q", got.PhoneHash)
	assert.NotEqual(t, []byte("legacy-ciphertext"), got.PhoneEncrypted)
}

// TestBackfillPhoneShadowRequiresKey 回填同样 fail-closed：没有主密钥就直接报错，
// 而不是空跑一遍留下满地空列、让运维误以为回填已完成。
func TestBackfillPhoneShadowRequiresKey(t *testing.T) {
	withPhoneSecretForTest(t, "")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	_, err := d.BackfillPhoneShadow(context.Background(), 0, 10, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPhoneEncryptionUnavailable)
}

// TestBackfillPhoneShadowSkipsAlreadyDestroyedRow 覆盖回填的 CAS 竞态。
//
// 回填是"先读 (id, zone, phone) → 再算密文 → 再 UPDATE"。若 UPDATE 条件只判
// id + phone_hash=”，那么"读取之后账号被并发注销"这条竞态会把注销前手机号的
// 密文/盲索引写回去 —— 已注销账号又重新占用原手机号，正是
// destroyAccountFromState 要消除的那个 P0（注销后手机号无法复用）。
//
// 这里用"回填前先注销"来确定性地模拟该竞态的结果状态：注销后 phone 变成墓碑值、
// phone_hash 被清空，若回填仍按旧 phone 写回，就会重现该缺陷。
func TestBackfillPhoneShadowSkipsAlreadyDestroyedRow(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	// 造一个"存量未回填"且带手机号的行
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) " +
			"VALUES ('u-race','008613900007777','race','0086','13900007777','sn-race','v-race@1',1,0)").Exec()
	require.NoError(t, err)

	// 注销：phone 匿名化 + 影子列清空（destroyAccountFromState 的真实行为）
	require.NoError(t, d.destroyAccount("u-race", "anon-race", "13900007777@1700000000000@delete"))

	// 回填不得把注销前手机号的盲索引写回
	res, err := d.BackfillPhoneShadow(context.Background(), 0, 10, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 0, res.Updated, "已注销账号不应被回填")

	after, err := d.QueryByUID("u-race")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Empty(t, after.PhoneHash, "回填不得为已注销账号重建盲索引")

	// 关键断言：原手机号仍然是"未被占用"的，可以被新用户复用
	byPhone, err := d.QueryByPhone("0086", "13900007777")
	require.NoError(t, err)
	assert.Nil(t, byPhone, "注销释放的手机号不得因回填而重新被占用")
}

// TestBackfillPhoneShadowDoneRequiresRemainingZero 覆盖游标与完成判定：
// 失败行不能永久落在 next_cursor 之后，也不能在 remaining>0 时报告 done=true。
func TestBackfillPhoneShadowDoneRequiresRemainingZero(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	for i := 0; i < 3; i++ {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) "+
				"VALUES (?,?,?,?,?,?,?,1,0)",
			fmt.Sprintf("u-cur-%d", i), fmt.Sprintf("0086138000001%02d", i), fmt.Sprintf("cur%d", i),
			"0086", fmt.Sprintf("138000001%02d", i), fmt.Sprintf("sn-cur-%d", i), fmt.Sprintf("v-cur-%d@1", i),
		).Exec()
		require.NoError(t, err)
	}

	// batchSize=1 让扫描分多批推进，验证游标语义
	res, err := d.BackfillPhoneShadow(context.Background(), 0, 1, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 3, res.Updated)
	assert.True(t, res.Done, "全部回填完成后 done 应为 true")
	assert.EqualValues(t, 0, res.Remaining)
	assert.EqualValues(t, 0, res.NextCursor, "扫到表尾后游标应归 0，便于下一轮重试残留行")
}

// TestBackfillPhoneShadowDefaultIntervalThrottles 请求省略 interval_ms（零值）时
// 必须落到默认节流，而不是被当成"不限速"。
func TestBackfillPhoneShadowDefaultIntervalThrottles(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	// 两行 + batchSize=1 → 至少要跨一次批间隔
	for i := 0; i < 2; i++ {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) "+
				"VALUES (?,?,?,?,?,?,?,1,0)",
			fmt.Sprintf("u-thr-%d", i), fmt.Sprintf("0086137000002%02d", i), fmt.Sprintf("thr%d", i),
			"0086", fmt.Sprintf("137000002%02d", i), fmt.Sprintf("sn-thr-%d", i), fmt.Sprintf("v-thr-%d@1", i),
		).Exec()
		require.NoError(t, err)
	}

	start := time.Now()
	// interval 传 0 —— 等价于 HTTP 请求省略 interval_ms
	res, err := d.BackfillPhoneShadow(context.Background(), 0, 1, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 2, res.Updated)
	assert.GreaterOrEqual(t, time.Since(start), defaultBackfillInterval,
		"interval 零值必须落到默认节流，否则默认请求完全没有限速")
}

// TestPhoneShadowBackfillCASRejectsChangedRow 直接断言回填 UPDATE 的 CAS 谓词：
// 行在读取之后被改写（典型场景是并发注销）时，UPDATE 必须影响 0 行。
//
// 为什么单独测谓词而不是走 BackfillPhoneShadow：CAS 保护的是"读取后、更新前"这个
// 窗口，而扫描与更新在同一次函数调用内完成，没有注入点可以确定性地交错；靠并发
// goroutine 触发会得到一条 flaky 测试。这里复用生产代码同一份
// phoneShadowBackfillCAS 常量，所以谓词不会与生产实现漂移。
func TestPhoneShadowBackfillCASRejectsChangedRow(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user(uid, username, name, zone, phone, short_no, vercode, status, is_destroy) " +
			"VALUES ('u-cas','008613900006666','cas','0086','13900006666','sn-cas','v-cas@1',1,0)").Exec()
	require.NoError(t, err)

	var row struct {
		ID        int64  `db:"id"`
		Zone      string `db:"zone"`
		Phone     string `db:"phone"`
		PhoneHash string `db:"phone_hash"`
	}
	_, err = ctx.DB().Select("id", "zone", "phone", "phone_hash").From("user").
		Where("uid=?", "u-cas").Load(&row)
	require.NoError(t, err)
	require.NotZero(t, row.ID)

	// 模拟"读取之后账号被并发注销"：phone 改成墓碑值，phone_hash 仍为空
	require.NoError(t, d.destroyAccount("u-cas", "anon-cas", "13900006666@1700000000000@delete"))

	// 用回填读到的旧 zone/phone 发起 UPDATE —— CAS 必须失配
	encrypted, hash, last4, err := d.phoneEnc.encryptPhone(row.Zone, row.Phone)
	require.NoError(t, err)
	res, err := ctx.DB().Update("user").SetMap(map[string]interface{}{
		"phone_encrypted": encrypted,
		"phone_hash":      hash,
		"phone_last4":     last4,
	}).Where(phoneShadowBackfillCAS, row.ID, row.PhoneHash, row.Zone, row.Phone, IsDestroyDone).Exec()
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	assert.EqualValues(t, 0, affected,
		"行在读取后被改写时 CAS 必须失配；影响 1 行说明注销释放的手机号会被回填重新占用")

	// 手机号确实仍然是可复用的
	byPhone, err := d.QueryByPhone("0086", "13900006666")
	require.NoError(t, err)
	assert.Nil(t, byPhone)
}
