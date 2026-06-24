package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"testing"

	modulescommon "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mention 的 E2E（需 MySQL/Redis/WuKongIM，CI 提供）：覆盖 HTTP 可观测的行为——
// 广播能力位的持久化 / 管理端判权 / push 时能力位未开的 mention_ignored 回报，以及
// 无 mention 字段的向后兼容。定向 uids 的成员闸 / ais 展开是【刻意不经 HTTP 回显】的
// （反枚举 + 落在 WuKongIM payload），由 mention_test.go 的纯单测穷举，不在此重复。

// adminCreateWebhook 以群主（admin）创建一个 webhook，可选带广播能力位，返回响应体。
func adminCreateWebhook(t *testing.T, handler http.Handler, groupNo string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), body))
	require.Equalf(t, http.StatusOK, w.Code, "admin create body: %s", w.Body.String())
	return parseJSON(t, w)
}

// TestMention_AdminCreateWithSwitchesPersist：管理员可在 create 时开启两个广播能力位，
// 响应与后续 list 都如实回显 1/1。
func TestMention_AdminCreateWithSwitchesPersist(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":               "ci",
		"allow_mention_all":  true,
		"allow_mention_bots": true,
	})
	assert.EqualValues(t, 1, created["allow_mention_all"])
	assert.EqualValues(t, 1, created["allow_mention_bots"])

	// list 回显同样的能力位。
	lw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	require.Equalf(t, http.StatusOK, lw.Code, "list body: %s", lw.Body.String())
	list, _ := parseJSON(t, lw)["list"].([]interface{})
	require.Len(t, list, 1)
	first, _ := list[0].(map[string]interface{})
	assert.EqualValues(t, 1, first["allow_mention_all"])
	assert.EqualValues(t, 1, first["allow_mention_bots"])
}

// TestMention_DefaultSwitchesOff：不带能力位字段创建 → 两个开关默认关闭（0）。
func TestMention_DefaultSwitchesOff(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	assert.EqualValues(t, 0, created["allow_mention_all"])
	assert.EqualValues(t, 0, created["allow_mention_bots"])
}

// TestMention_MemberCanEnableSwitches：任意合法成员（role=0，非管理员）可在自建 webhook
// 上开启广播能力位（与「成员可自建/自管 webhook」一致，不再要求管理员）；缺省则默认关闭。
func TestMention_MemberCanEnableSwitches(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)

	wOn := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"allow_mention_all": true, "allow_mention_bots": true}, memberAToken))
	require.Equalf(t, http.StatusOK, wOn.Code, "body: %s", wOn.Body.String())
	on := parseJSON(t, wOn)
	assert.EqualValues(t, 1, on["allow_mention_all"])
	assert.EqualValues(t, 1, on["allow_mention_bots"])

	// 不带开关 → 默认关闭。
	wOK := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{}, memberAToken))
	require.Equalf(t, http.StatusOK, wOK.Code, "body: %s", wOK.Body.String())
	def := parseJSON(t, wOK)
	assert.EqualValues(t, 0, def["allow_mention_all"])
	assert.EqualValues(t, 0, def["allow_mention_bots"])
}

// TestMention_UpdateTogglesSwitches：管理员可经 update 开/关能力位；普通成员同样可改
// 自己创建的 webhook 的能力位（与「成员可自建/自管 webhook」一致）。
func TestMention_UpdateTogglesSwitches(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)

	// 管理员建（默认 0/0）→ 开启 → 关闭。
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	whID, _ := created["webhook_id"].(string)

	on := do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"allow_mention_all": true, "allow_mention_bots": true}))
	require.Equalf(t, http.StatusOK, on.Code, "body: %s", on.Body.String())
	onRes := parseJSON(t, on)
	assert.EqualValues(t, 1, onRes["allow_mention_all"])
	assert.EqualValues(t, 1, onRes["allow_mention_bots"])

	off := do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"allow_mention_all": false}))
	require.Equalf(t, http.StatusOK, off.Code, "body: %s", off.Body.String())
	assert.EqualValues(t, 0, parseJSON(t, off)["allow_mention_all"])
	assert.EqualValues(t, 1, parseJSON(t, off)["allow_mention_bots"], "untouched switch keeps its value")

	// 普通成员对【自己创建】的 webhook 改能力位 → 200 且生效（成员可自管自建 webhook）。
	mCreated := memberCreate(t, handler, groupNo)
	mID, _ := mCreated["webhook_id"].(string)
	mw := do(handler, userReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, mID),
		map[string]interface{}{"allow_mention_all": true}, memberAToken))
	require.Equalf(t, http.StatusOK, mw.Code, "body: %s", mw.Body.String())
	assert.EqualValues(t, 1, parseJSON(t, mw)["allow_mention_all"])
}

// TestMentionPush_BroadcastIgnoredWhenCapabilityOff：能力位关闭时 push 带 all/bots，
// 消息照发（200），但响应体 mention_ignored 回报这两个广播位被忽略。
func TestMentionPush_BroadcastIgnoredWhenCapabilityOff(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"}) // 默认 0/0
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL,
		[]byte(`{"content":"hi","mention":{"all":true,"bots":true}}`)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	body := parseJSON(t, pw)
	ignored, ok := body["mention_ignored"].([]interface{})
	require.Truef(t, ok, "expected mention_ignored array, body: %s", pw.Body.String())
	assert.ElementsMatch(t, []interface{}{"all", "bots"}, ignored)
}

// TestMentionPush_BroadcastAllowedWhenCapabilityOn：能力位开启时 push 带 all/bots →
// 200 且【无】mention_ignored（广播被接受）。
func TestMentionPush_BroadcastAllowedWhenCapabilityOn(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":               "ci",
		"allow_mention_all":  true,
		"allow_mention_bots": true,
	})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL,
		[]byte(`{"content":"hi","mention":{"all":true,"bots":true}}`)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.Falsef(t, hasIgnored, "no broadcast should be ignored, body: %s", pw.Body.String())
}

// TestMentionPush_TargetedUIDsSucceed：定向 @ 群成员 → 200 且无 mention_ignored
// （定向 @ 不受能力位约束；非成员静默丢弃不回显）。冒烟覆盖 push 路径接 mention 不出错。
func TestMentionPush_TargetedUIDsSucceed(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	body := fmt.Sprintf(`{"content":"hi","mention":{"uids":["%s","not_a_member"]}}`, memberAUID)
	pw := do(handler, anonReq("POST", pushURL, []byte(body)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.False(t, hasIgnored, "targeted uids are not capability-gated; non-members dropped silently")
}

// TestMentionPush_MemberBroadcastRevokedBySystemSetting 验证「预埋的收回开关」(option C)：
// system_setting incomingwebhook.member_can_broadcast=0 时，【成员】创建的 webhook 的广播
// (@所有人/@所有 AI) 在 push 读路径被即时剥离（mention_ignored 回报），而【管理员】创建的
// webhook 不受影响——无需迁移已置 1 的能力位列、改个后台值即可收回。
func TestMentionPush_MemberBroadcastRevokedBySystemSetting(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	t.Setenv("OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST", "") // 证明纯由 DB 驱动

	// 成员（非管理员）建一个开了两个广播位的 webhook。
	mw := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"allow_mention_all": true, "allow_mention_bots": true}, memberAToken))
	require.Equalf(t, http.StatusOK, mw.Code, "member create: %s", mw.Body.String())
	mRes := parseJSON(t, mw)
	memberPush := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", mRes["webhook_id"], mRes["token"])

	// 管理员建一个开了广播位的 webhook。
	aRes := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name": "admin-ci", "allow_mention_all": true, "allow_mention_bots": true})
	adminPush := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", aRes["webhook_id"], aRes["token"])

	// 收回：member_can_broadcast=0 + Reload 共享快照（与 TestFeatureToggle 同法）。
	_, err := ctx.DB().InsertInto("system_setting").
		Pair("category", "incomingwebhook").Pair("key_name", "member_can_broadcast").
		Pair("value", "0").Pair("value_type", "bool").Pair("description", "").Exec()
	require.NoError(t, err)
	settings := modulescommon.EnsureSystemSettings(ctx)
	require.NoError(t, settings.Reload())
	defer func() {
		_, _ = ctx.DB().DeleteFrom("system_setting").Where("category=?", "incomingwebhook").Exec()
		_ = settings.Reload()
	}()

	// 成员 webhook 广播被即时剥离 → mention_ignored 回报 all+bots（无需改其能力位列）。
	mp := do(handler, anonReq("POST", memberPush, []byte(`{"content":"hi","mention":{"all":true,"bots":true}}`)))
	require.Equalf(t, http.StatusOK, mp.Code, "member push: %s", mp.Body.String())
	ig, ok := parseJSON(t, mp)["mention_ignored"].([]interface{})
	require.Truef(t, ok, "member broadcast must be reported ignored, body: %s", mp.Body.String())
	assert.ElementsMatch(t, []interface{}{"all", "bots"}, ig)

	// 管理员 webhook 不受影响 → 无 mention_ignored（creatorIsAdmin 仍放行）。
	ap := do(handler, anonReq("POST", adminPush, []byte(`{"content":"hi","mention":{"all":true,"bots":true}}`)))
	require.Equalf(t, http.StatusOK, ap.Code, "admin push: %s", ap.Body.String())
	_, adminIgnored := parseJSON(t, ap)["mention_ignored"]
	assert.Falsef(t, adminIgnored, "admin-created webhook broadcast must be unaffected, body: %s", ap.Body.String())
}

// TestMentionPush_MalformedMentionIgnored 钉 acceptance #6（review #445 的 P1 阻塞）：
// 形状非法的 mention（uids 非数组 / mention 非对象 等）必须【降级为无 mention、消息照投
// 200】，而不是把整条推送 400 掉（修复前这些都返回 400，把相邻的合法 content 也连累丢弃）。
func TestMentionPush_MalformedMentionIgnored(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	for _, body := range []string{
		`{"content":"hi","mention":"please"}`,
		`{"content":"hi","mention":{"uids":"alice"}}`,
		`{"content":"hi","mention":{"uids":[1,2]}}`,
		`{"content":"hi","mention":{"all":1}}`,
		`{"content":"hi","mention":[]}`,
	} {
		pw := do(handler, anonReq("POST", pushURL, []byte(body)))
		require.Equalf(t, http.StatusOK, pw.Code,
			"malformed mention must still deliver (200), body=%s resp=%s", body, pw.Body.String())
		_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
		assert.Falsef(t, hasIgnored, "malformed mention is dropped, not reported: %s", body)
	}
}

// TestMentionPush_NoMentionFieldBackwardCompatible：不带 mention 字段的历史 native 调用
// 行为不变（200，无 mention_ignored）。
func TestMentionPush_NoMentionFieldBackwardCompatible(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi"}`)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.False(t, hasIgnored)
}

// TestMentionPush_EntitiesAccepted：native 文本路径带【调用方提供的】mention.entities
// （渲染层 @ 区间 {uid,offset,length}）→ 200 且无 mention_ignored。entities 不受广播能力位
// 约束（定向渲染，非广播），非成员 entity 静默丢弃。entities 是否落到 wire、是否按成员闸
// 过滤是【刻意不经 HTTP 回显】的（反枚举 + 落在 WuKongIM payload），由纯单测 finalizeEntities
// 穷举（含 UTF-16 offset parity），此处只冒烟覆盖 push 路径接受 entities 不出错。
func TestMentionPush_EntitiesAccepted(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	// content "@x ok"：@(0)x(1) → entity offset0 length2 指向 "@x"，uid 为本群成员。
	body := fmt.Sprintf(
		`{"content":"@x ok","mention":{"uids":["%s"],"entities":[{"uid":"%s","offset":0,"length":2}]}}`,
		memberAUID, memberAUID)
	pw := do(handler, anonReq("POST", pushURL, []byte(body)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.False(t, hasIgnored, "entities are not capability-gated; non-members dropped silently")
}

// TestMentionPush_MalformedEntitiesDelivered：entities 形状非法不得把推送 400（acceptance #6）。
//   - entities 为非数组标量 → 整个 mention 降级为「无」、消息照投；
//   - entities 数组里单条非法（offset 类型错 / 空对象）→ 仅丢该条，不连累其余 mention 字段。
func TestMentionPush_MalformedEntitiesDelivered(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	for _, body := range []string{
		`{"content":"hi","mention":{"entities":"garbage"}}`,
		fmt.Sprintf(`{"content":"@x ok","mention":{"uids":["%s"],"entities":[{"uid":"x","offset":"y"}]}}`, memberAUID),
		fmt.Sprintf(`{"content":"@x ok","mention":{"uids":["%s"],"entities":[{}]}}`, memberAUID),
	} {
		pw := do(handler, anonReq("POST", pushURL, []byte(body)))
		require.Equalf(t, http.StatusOK, pw.Code,
			"malformed entities must still deliver (200), body=%s resp=%s", body, pw.Body.String())
		_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
		assert.Falsef(t, hasIgnored, "malformed entities are dropped, not reported: %s", body)
	}
}
