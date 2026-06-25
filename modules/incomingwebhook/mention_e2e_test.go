package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"testing"

	modulescommon "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mention 的 E2E（需 MySQL/Redis/WuKongIM，CI 提供）：覆盖 HTTP 可观测的行为——广播能力位 +
// 定向 @ 目标的持久化 / 管理端判权 / push 时按【配置】构造 mention（push body 不再接受 mention）/
// 策略未放行时的 mention_ignored 回报 / 适配器同样按配置 @（不再限于 native）/ 无配置的向后兼容。
// 定向 uids 的成员闸 / render / ais 展开落在 WuKongIM payload、刻意不经 HTTP 回显，由
// mention_test.go / mention_directed_render_test.go 的单测穷举，不在此重复。

// adminCreateWebhook 以群主（admin）创建一个 webhook，返回响应体。
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

// TestMention_DefaultSwitchesOff：不带能力位 / mention_uids 字段创建 → 两个开关默认关闭（0）、
// mention_uids 回显为空数组（恒为数组，不是 null）。
func TestMention_DefaultSwitchesOff(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	assert.EqualValues(t, 0, created["allow_mention_all"])
	assert.EqualValues(t, 0, created["allow_mention_bots"])
	muids, ok := created["mention_uids"].([]interface{})
	require.Truef(t, ok, "mention_uids must be an array, body=%v", created)
	assert.Empty(t, muids)
}

// TestMention_ConfigureMentionUidsPersist：create/update 配置定向 @ 目标（本群成员）→ 响应与
// list 如实回显；update 传 [] 显式清空。
func TestMention_ConfigureMentionUidsPersist(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)

	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":         "ci",
		"mention_uids": []string{memberAUID},
	})
	assert.EqualValues(t, []interface{}{memberAUID}, created["mention_uids"])
	whID, _ := created["webhook_id"].(string)

	// list 回显。
	lw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	require.Equalf(t, http.StatusOK, lw.Code, "list body: %s", lw.Body.String())
	list, _ := parseJSON(t, lw)["list"].([]interface{})
	require.Len(t, list, 1)
	assert.EqualValues(t, []interface{}{memberAUID}, list[0].(map[string]interface{})["mention_uids"])

	// update 改成 [A,B]。
	up := do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"mention_uids": []string{memberAUID, memberBUID}}))
	require.Equalf(t, http.StatusOK, up.Code, "update body: %s", up.Body.String())
	assert.EqualValues(t, []interface{}{memberAUID, memberBUID}, parseJSON(t, up)["mention_uids"])

	// update 传 [] 显式清空。
	clr := do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"mention_uids": []string{}}))
	require.Equalf(t, http.StatusOK, clr.Code, "clear body: %s", clr.Body.String())
	assert.Empty(t, parseJSON(t, clr)["mention_uids"])
}

// TestMention_CreateRejectsNonMemberUID：配置的 @ 目标必须是本群当前成员，非成员 → 400。
func TestMention_CreateRejectsNonMemberUID(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"name": "ci", "mention_uids": []string{"ghost_not_member"}}))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "non-member @ target must be rejected, body: %s", w.Body.String())
}

// TestMention_MemberCanEnableSwitches：任意合法成员（role=0，非管理员）可在自建 webhook
// 上开启广播能力位；缺省则默认关闭。
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

// TestMention_UpdateTogglesSwitches：管理员可经 update 开/关能力位；普通成员同样可改自己
// 创建的 webhook 的能力位。
func TestMention_UpdateTogglesSwitches(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)

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

	// 普通成员对【自己创建】的 webhook 改能力位 → 200 且生效。
	mCreated := memberCreate(t, handler, groupNo)
	mID, _ := mCreated["webhook_id"].(string)
	mw := do(handler, userReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, mID),
		map[string]interface{}{"allow_mention_all": true}, memberAToken))
	require.Equalf(t, http.StatusOK, mw.Code, "body: %s", mw.Body.String())
	assert.EqualValues(t, 1, parseJSON(t, mw)["allow_mention_all"])
}

// TestMentionPush_BroadcastAppliedWhenSwitchOn：开了广播开关的 webhook，push（body 不带 mention）
// → 200 且【无】mention_ignored（广播被接受，admin 创建者天然放行）。
func TestMentionPush_BroadcastAppliedWhenSwitchOn(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":               "ci",
		"allow_mention_all":  true,
		"allow_mention_bots": true,
	})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi"}`)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.Falsef(t, hasIgnored, "switch-on broadcast must apply, no ignored; body: %s", pw.Body.String())
}

// TestMentionPush_ConfiguredUIDsSucceed：配置了定向 @ 目标的 webhook，push（body 不带 mention）
// → 200 且无 mention_ignored（定向 @ 不受能力位约束；非成员静默丢弃）。
func TestMentionPush_ConfiguredUIDsSucceed(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":         "ci",
		"mention_uids": []string{memberAUID},
	})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi"}`)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.False(t, hasIgnored, "configured directed @ is not capability-gated")
}

// TestMentionPush_BodyMentionIgnored：push body 里带 mention（all/bots/uids）对一个【未配置】
// 任何 @ 的 webhook 完全无效——200 且无 mention_ignored（body 不再被解析，nothing wanted）。
func TestMentionPush_BodyMentionIgnored(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"}) // 无 @ 配置
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	body := fmt.Sprintf(`{"content":"hi","mention":{"all":true,"bots":true,"uids":["%s"]}}`, memberAUID)
	pw := do(handler, anonReq("POST", pushURL, []byte(body)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.Falsef(t, hasIgnored,
		"body mention must be ignored entirely (no broadcast asked via config), body: %s", pw.Body.String())
}

// TestMentionPush_NoMentionFieldBackwardCompatible：无任何 @ 配置 + body 无 mention → 200，
// 无 mention_ignored（与无 @ 的历史 webhook 行为一致）。
func TestMentionPush_NoMentionFieldBackwardCompatible(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{"name": "ci"})
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", created["webhook_id"], created["token"])

	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi"}`)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.False(t, hasIgnored)
}

// TestMentionPush_BroadcastDeniedByPolicyAcrossAdapters 验证两点：
//  1. member_can_broadcast=0 即时收回【成员】创建的 webhook 的广播（mention_ignored 回报），
//     而【管理员】创建的不受影响——按 webhook 配置驱动，无需迁移能力位列；
//  2. 同一套 mention 构造【对适配器同样生效】：成员 webhook 经 /wecom 推送也回报 mention_ignored，
//     证明 @ 不再限于 native（配置驱动的收益）。
func TestMentionPush_BroadcastDeniedByPolicyAcrossAdapters(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	t.Setenv("OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST", "") // 证明纯由 DB 驱动

	// 成员（非管理员）建一个开了两个广播位的 webhook。
	mw := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"allow_mention_all": true, "allow_mention_bots": true}, memberAToken))
	require.Equalf(t, http.StatusOK, mw.Code, "member create: %s", mw.Body.String())
	mRes := parseJSON(t, mw)
	memberNative := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", mRes["webhook_id"], mRes["token"])
	memberWecom := memberNative + "/wecom"

	// 管理员建一个开了广播位的 webhook。
	aRes := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name": "admin-ci", "allow_mention_all": true, "allow_mention_bots": true})
	adminNative := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", aRes["webhook_id"], aRes["token"])

	// 收回：member_can_broadcast=0 + Reload 共享快照。
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

	// 成员 webhook 广播被即时剥离 → mention_ignored 回报 all+bots（native）。
	mp := do(handler, anonReq("POST", memberNative, []byte(`{"content":"hi"}`)))
	require.Equalf(t, http.StatusOK, mp.Code, "member native push: %s", mp.Body.String())
	ig, ok := parseJSON(t, mp)["mention_ignored"].([]interface{})
	require.Truef(t, ok, "member broadcast must be reported ignored, body: %s", mp.Body.String())
	assert.ElementsMatch(t, []interface{}{"all", "bots"}, ig)

	// 同一成员 webhook 经【wecom 适配器】推送也回报 mention_ignored —— mention 不再限于 native。
	wp := do(handler, anonReq("POST", memberWecom, []byte(`{"msgtype":"text","text":{"content":"hi"}}`)))
	require.Equalf(t, http.StatusOK, wp.Code, "member wecom push: %s", wp.Body.String())
	wig, ok := parseJSON(t, wp)["mention_ignored"].([]interface{})
	require.Truef(t, ok, "adapter must run mention too; body: %s", wp.Body.String())
	assert.ElementsMatch(t, []interface{}{"all", "bots"}, wig)

	// 管理员 webhook 不受影响 → 无 mention_ignored（creatorIsAdmin 仍放行）。
	ap := do(handler, anonReq("POST", adminNative, []byte(`{"content":"hi"}`)))
	require.Equalf(t, http.StatusOK, ap.Code, "admin push: %s", ap.Body.String())
	_, adminIgnored := parseJSON(t, ap)["mention_ignored"]
	assert.Falsef(t, adminIgnored, "admin-created webhook broadcast must be unaffected, body: %s", ap.Body.String())
}

// TestTestPush_AppliesConfiguredMention pins the review fix: the management "test push" now runs
// the same mention assembly as a real push (previously it only called buildPayload and silently
// dropped the configured @, misleading admins). e2e can't read back the WuKongIM payload, so this
// is a smoke assertion that a webhook with configured directed @ + a broadcast switch tests
// end-to-end (200) without the new member-gate/compose wiring erroring; the mention-lands-on-wire
// correctness is exhausted by the directed-render / broadcast unit tests.
func TestTestPush_AppliesConfiguredMention(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":              "ci",
		"mention_uids":      []string{memberAUID},
		"allow_mention_all": true,
	})
	whID, _ := created["webhook_id"].(string)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/test", groupNo, whID), nil))
	require.Equalf(t, http.StatusOK, w.Code,
		"test push with configured mention must succeed end-to-end; body: %s", w.Body.String())
}

// TestMentionPush_BodyMentionIgnoredWhenConfigured locks the non-interaction property the existing
// TestMentionPush_BodyMentionIgnored couldn't prove on its own (an admin webhook with no config):
// a push body's broadcast request must NOT be honored even when the webhook already carries its own
// (directed) config. The webhook is member-created with broadcast policy DENIED, so *if* the body's
// all/bots were (wrongly) read, the denied broadcast would surface as mention_ignored — asserting
// its absence proves the body was dropped, not merged with the webhook config.
func TestMentionPush_BodyMentionIgnoredWhenConfigured(t *testing.T) {
	handler, ctx, groupNo := setupMemberEnv(t)
	t.Setenv("OCTO_INCOMINGWEBHOOK_MEMBER_CAN_BROADCAST", "") // 纯由 DB 策略驱动

	// 成员 webhook：配置定向 @A，广播开关【关】。
	mw := do(handler, userReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo),
		map[string]interface{}{"mention_uids": []string{memberAUID}}, memberAToken))
	require.Equalf(t, http.StatusOK, mw.Code, "member create: %s", mw.Body.String())
	mRes := parseJSON(t, mw)
	pushURL := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", mRes["webhook_id"], mRes["token"])

	// 收回广播策略：member_can_broadcast=0（这样若 body 广播被误读，必被拒并回报 ignored）。
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

	// body 请求 all+bots+另一个 uid —— 全部必须被忽略（仅 webhook 配置说了算）。
	body := fmt.Sprintf(`{"content":"hi","mention":{"all":true,"bots":true,"uids":["%s"]}}`, memberBUID)
	pw := do(handler, anonReq("POST", pushURL, []byte(body)))
	require.Equalf(t, http.StatusOK, pw.Code, "push body: %s", pw.Body.String())
	_, hasIgnored := parseJSON(t, pw)["mention_ignored"]
	assert.Falsef(t, hasIgnored,
		"body broadcast must be ignored even when the webhook is configured; honoring it would report ignored under the denied policy, body: %s", pw.Body.String())
}

// TestMention_UpdateRejectsNonMemberUID: update 与 create 同口径，必须把 @ 目标收敛到本群当前
// 成员，非成员 → 400（create 由 TestMention_CreateRejectsNonMemberUID 覆盖，这里锁 update 契约）。
func TestMention_UpdateRejectsNonMemberUID(t *testing.T) {
	handler, _, groupNo := setupMemberEnv(t)
	created := adminCreateWebhook(t, handler, groupNo, map[string]interface{}{
		"name":         "ci",
		"mention_uids": []string{memberAUID},
	})
	whID, _ := created["webhook_id"].(string)

	w := do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"mention_uids": []string{"ghost_not_member"}}))
	assert.Equalf(t, http.StatusBadRequest, w.Code,
		"update with non-member @ target must be rejected, body: %s", w.Body.String())
}
