package usersecret

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// 跨表迁移依赖:resolve 反查 robot 表。
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

func newTestSvc(t *testing.T) (*service, *store) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	st := newStore(ctx)
	enc, err := newEncryptor()
	require.NoError(t, err)
	return newService(st, enc), st
}

func TestService_CreateListResolve_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-owner-1"

	view, err := svc.create(owner, "Claude 密钥", KindLLM, "sk-claude-xyz789")
	require.NoError(t, err)
	assert.NotEmpty(t, view.SecretID)
	assert.Equal(t, "Claude 密钥", view.DisplayName)
	assert.Equal(t, KindLLM, view.Kind)
	assert.Equal(t, "****z789", view.Masked)

	// list 脱敏:无明文/密文字段。
	views, err := svc.list(owner, "")
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, "****z789", views[0].Masked)

	// resolve by secret_id → 明文。
	out, err := svc.resolve(owner, view.SecretID)
	require.NoError(t, err)
	assert.Equal(t, resultOK, out.result)
	assert.Equal(t, "sk-claude-xyz789", out.plaintext)

	// resolve by display_name(精确)→ 明文。
	out, err = svc.resolve(owner, "claude密钥")
	require.NoError(t, err)
	assert.Equal(t, "sk-claude-xyz789", out.plaintext)
}

func TestService_DuplicateName_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-owner-2"
	_, err := svc.create(owner, "OpenAI Key", KindLLM, "sk-a")
	require.NoError(t, err)
	// 归一化撞名(大小写 + 空格)。
	_, err = svc.create(owner, "openai  key", KindLLM, "sk-b")
	assert.ErrorIs(t, err, errDuplicateName)
}

func TestService_OwnerIsolation_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	a, err := svc.create("u-a", "shared name", KindExternal, "secret-A")
	require.NoError(t, err)

	// 另一 owner 同名不冲突。
	_, err = svc.create("u-b", "shared name", KindExternal, "secret-B")
	require.NoError(t, err)

	// u-b 不能 resolve u-a 的 secret_id。
	out, err := svc.resolve("u-b", a.SecretID)
	assert.ErrorIs(t, err, errNotFound)
	assert.Equal(t, resultNotFound, out.result)
}

func TestService_UpdateKey_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-upd"
	v, err := svc.create(owner, "rotate me", KindExternal, "old-value-1111")
	require.NoError(t, err)

	v2, err := svc.updateKey(owner, v.SecretID, "new-value-2222")
	require.NoError(t, err)
	assert.Equal(t, v.SecretID, v2.SecretID, "换 key 不改 secret_id")
	assert.Equal(t, "****2222", v2.Masked)

	out, err := svc.resolve(owner, v.SecretID)
	require.NoError(t, err)
	assert.Equal(t, "new-value-2222", out.plaintext)
}

func TestService_Rename_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-ren"
	v, err := svc.create(owner, "old name", KindExternal, "val-keep")
	require.NoError(t, err)

	v2, err := svc.rename(owner, v.SecretID, "new name")
	require.NoError(t, err)
	assert.Equal(t, v.SecretID, v2.SecretID, "改名不断引用")
	assert.Equal(t, "new name", v2.DisplayName)

	// 密文不变:仍能解出原值。
	out, err := svc.resolve(owner, v.SecretID)
	require.NoError(t, err)
	assert.Equal(t, "val-keep", out.plaintext)

	// 改名后旧名解不到,新名解得到。
	_, err = svc.resolve(owner, "old name")
	assert.ErrorIs(t, err, errNotFound)
	out, err = svc.resolve(owner, "new name")
	require.NoError(t, err)
	assert.Equal(t, "val-keep", out.plaintext)
}

func TestService_Delete_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-del"
	v, err := svc.create(owner, "to delete", KindExternal, "v")
	require.NoError(t, err)
	require.NoError(t, svc.delete(owner, v.SecretID))
	assert.ErrorIs(t, svc.delete(owner, v.SecretID), errNotFound)
}

// TestService_Resolve_TouchesLastUsed_Integration 验证 resolve 成功后回写
// last_used_at,且不污染 updated_at(「最后使用」≠「最后修改」)。
func TestService_Resolve_TouchesLastUsed_Integration(t *testing.T) {
	svc, st := newTestSvc(t)
	owner := "u-lastused"
	v, err := svc.create(owner, "touch me", KindExternal, "val-touch")
	require.NoError(t, err)

	before, err := st.queryBySecretID(owner, v.SecretID)
	require.NoError(t, err)
	require.Nil(t, before.LastUsedAt, "新建时 last_used_at 应为空")
	updatedBefore := before.UpdatedAt

	out, err := svc.resolve(owner, v.SecretID)
	require.NoError(t, err)
	require.Equal(t, "val-touch", out.plaintext)

	after, err := st.queryBySecretID(owner, v.SecretID)
	require.NoError(t, err)
	require.NotNil(t, after.LastUsedAt, "resolve 成功后 last_used_at 必须回写")
	assert.Equal(t, time.Time(updatedBefore).Unix(), time.Time(after.UpdatedAt).Unix(),
		"回写 last_used_at 不应改动 updated_at")
}

func TestService_ResolveAmbiguous_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-amb"
	// 两个拼音键相同(密钥/米要 → miyao)的别名,模糊查询应返候选而非明文。
	_, err := svc.create(owner, "我的密钥", KindExternal, "v1")
	require.NoError(t, err)
	_, err = svc.create(owner, "我的米要", KindExternal, "v2")
	require.NoError(t, err)

	out, err := svc.resolve(owner, "我的miyao")
	assert.ErrorIs(t, err, errAmbiguous)
	assert.Equal(t, resultAmbiguous, out.result)
	assert.Len(t, out.candidates, 2)
	assert.Empty(t, out.plaintext, "歧义不返明文")
}

// TestService_Resolve_UniqueFuzzy_NeverAutoPlaintext_Integration 回归 P1:唯一模糊
// 命中(score==1,双向 pinyin 子串)绝不能自动解密返明文,必须降级为单个候选走
// ambiguous 确认路径。
//
// 复刻 issue 场景:owner 同时有别名 `openai` 与 `Claude密钥`,query `pen` 无 exact
// 命中,旧逻辑会因「唯一模糊命中 openai」直接 finishHit 返回 openai 明文——即静默
// 返回用户没指定的那把自有密钥。修复后必须返候选确认而非明文。
func TestService_Resolve_UniqueFuzzy_NeverAutoPlaintext_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-fuzzy-pen"

	v1, err := svc.create(owner, "openai", KindLLM, "sk-openai-secret")
	require.NoError(t, err)
	_, err = svc.create(owner, "Claude密钥", KindLLM, "sk-claude-secret")
	require.NoError(t, err)

	// `pen` 是 `openai` 的子串(pinyinKey 同),旧逻辑下是唯一模糊命中。
	out, err := svc.resolve(owner, "pen")
	assert.ErrorIs(t, err, errAmbiguous, "唯一模糊命中必须走候选确认,不能 not_found 也不能自动解密")
	assert.Equal(t, resultAmbiguous, out.result)
	assert.Empty(t, out.plaintext, "唯一模糊命中绝不返明文")
	assert.NotEqual(t, "sk-openai-secret", out.plaintext, "绝不静默返回未指定的密钥明文")
	require.Len(t, out.candidates, 1, "唯一模糊命中应作为单个脱敏候选返回")
	assert.Equal(t, v1.SecretID, out.candidates[0].SecretID)
	assert.NotEmpty(t, out.candidates[0].Masked, "候选返回脱敏视图")

	// 用候选给出的 secret_id 显式 resolve → 才返明文(保留可用性)。
	confirmed, err := svc.resolve(owner, v1.SecretID)
	require.NoError(t, err)
	assert.Equal(t, "sk-openai-secret", confirmed.plaintext)
}

// TestService_Resolve_ResultClassification_Integration 回归 P1.5:resolve 的非命中
// 分支结果分类必须可区分(空入参 → request_invalid,真实零命中 → not_found),不能都
// 混成 not_found,否则审计无法把「坏请求」「真未命中」「基础设施故障」分开。
func TestService_Resolve_ResultClassification_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-classify"

	// 空 query → request_invalid(而非 not_found)。
	out, err := svc.resolve(owner, "   ")
	assert.ErrorIs(t, err, errInvalidInput)
	assert.Equal(t, resultRequestInvalid, out.result)

	// 真实零命中 → not_found。
	out, err = svc.resolve(owner, "nonexistent-name")
	assert.ErrorIs(t, err, errNotFound)
	assert.Equal(t, resultNotFound, out.result)
}

// TestService_Rename_Idempotent_Integration 回归 R3.1:用「与当前完全相同」的
// display_name 改名,在默认 MySQL DSN(未设 clientFoundRows)下 UPDATE 报 0 changed
// rows,但行存在 —— 必须幂等成功返回当前视图,而非误返 not_found。否则前端提交
// 「未改的 display_name + 新 key」会在 rename 步 404,走不到 key 轮换。
func TestService_Rename_Idempotent_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-ren-idem"
	v, err := svc.create(owner, "stable name", KindExternal, "val-1")
	require.NoError(t, err)

	// 同名改名:无字段变化,RowsAffected()==0,但必须幂等成功。
	v2, err := svc.rename(owner, v.SecretID, "stable name")
	require.NoError(t, err, "同名幂等改名不得返回 not_found")
	assert.Equal(t, v.SecretID, v2.SecretID)
	assert.Equal(t, "stable name", v2.DisplayName)

	// 归一化等价(大小写/空格)同样应幂等成功,不撞唯一键、不误判 not_found。
	v3, err := svc.rename(owner, v.SecretID, "  Stable   Name  ")
	require.NoError(t, err)
	assert.Equal(t, v.SecretID, v3.SecretID)

	// 真正不存在的 secret_id 仍是 not_found。
	_, err = svc.rename(owner, "no-such-id", "whatever")
	assert.ErrorIs(t, err, errNotFound)
}

// TestService_RenameThenRotate_Idempotent_Integration 模拟前端「整表提交」:
// 未改 display_name + 新 key。rename 步幂等成功(不 404),key 轮换随后生效。
func TestService_RenameThenRotate_Idempotent_Integration(t *testing.T) {
	svc, _ := newTestSvc(t)
	owner := "u-ren-rotate"
	v, err := svc.create(owner, "combo name", KindExternal, "old-val-1111")
	require.NoError(t, err)

	// rename(同名,幂等)→ updateKey(换 key),复刻 api.update 的两步编排。
	_, err = svc.rename(owner, v.SecretID, "combo name")
	require.NoError(t, err, "同名 rename 不得 404,否则到不了 key 轮换")
	v2, err := svc.updateKey(owner, v.SecretID, "new-val-2222")
	require.NoError(t, err)
	assert.Equal(t, "****2222", v2.Masked)

	out, err := svc.resolve(owner, v.SecretID)
	require.NoError(t, err)
	assert.Equal(t, "new-val-2222", out.plaintext, "key 轮换必须生效")
}

func TestStore_QueryBotByToken_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	st := newStore(ctx)

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, creator_uid, bot_token, status) VALUES (?, ?, ?, 1)",
		"bot-1", "owner-77", "bf_token_abc",
	).Exec()
	require.NoError(t, err)

	id, err := st.queryBotByToken("bf_token_abc")
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, "bot-1", id.RobotID)
	assert.Equal(t, "owner-77", id.OwnerUID)

	// 未知 token → nil。
	id, err = st.queryBotByToken("bf_unknown")
	require.NoError(t, err)
	assert.Nil(t, id)

	// Organization Avatars and arbitrary ownerless robots must not inherit a
	// person's secret namespace merely because they authenticate with a bf_ token.
	_, err = ctx.DB().InsertBySql(`INSERT INTO robot
		(robot_id,creator_uid,bot_token,status,auto_approve,kind,management_scope,
		 management_space_id,publication_state,lifecycle_pending)
		VALUES (?,'',?,1,1,'avatar','space','secret-space','published',0)`,
		"avatar-secret", "bf_avatar_secret").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id,creator_uid,bot_token,status,kind) VALUES (?,'',?,1,'user')",
		"ownerless-secret", "bf_ownerless_secret",
	).Exec()
	require.NoError(t, err)
	for _, token := range []string{"bf_avatar_secret", "bf_ownerless_secret"} {
		id, err = st.queryBotByToken(token)
		require.NoError(t, err)
		assert.Nil(t, id, "token %s must not resolve a human-owned secret namespace", token)
	}
}
