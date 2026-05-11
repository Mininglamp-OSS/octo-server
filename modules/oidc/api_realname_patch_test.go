package oidc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// YUJ-413 R5 Critical #1 — patchLoginRespJSONWithRealname 单测。
//
// 修复场景:OIDC callback 的三步时序 IssueSession → UpsertVerification →
// SetAuthcode,IssueSession 生成 LoginRespJSON 时 user_verification 还没
// 这次 upsert 的行,缓存给前端的 JSON 会漏带 realname_verified=true。
// 本 helper 负责在 upsert 成功后 in-place 修正 JSON,让 fresh login 的
// 客户端直接拿到正确实名态,不必依赖 Custom Tabs 回跳后 GET /user/current。

func TestPatchLoginRespJSONWithRealname_AddsThreeFields(t *testing.T) {
	// 模拟 sessResp.LoginRespJSON —— IssueSession 刚吐出来,还没带实名:
	original := `{"uid":"u1","token":"t1","name":"admin","realname_verified":false}`

	patched, err := patchLoginRespJSONWithRealname(original, "余嘉伟", 1778263617)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(patched), &m))

	// 核心契约:三个字段必须出现且值正确。
	assert.Equal(t, true, m["realname_verified"])
	assert.Equal(t, "余嘉伟", m["real_name"])
	// JSON number 反序列化为 float64
	assert.Equal(t, float64(1778263617), m["realname_verified_at"])

	// 原有字段不被抹掉。
	assert.Equal(t, "u1", m["uid"])
	assert.Equal(t, "t1", m["token"])
	assert.Equal(t, "admin", m["name"])
}

func TestPatchLoginRespJSONWithRealname_OverwritesStaleValues(t *testing.T) {
	// 现有用户二次登录:old realname_verified=false 已落进 JSON,这次 Aegis
	// 返回 is_verified=true,patch 必须把 false 覆写成 true。
	original := `{"uid":"u2","realname_verified":false,"real_name":"旧名"}`

	patched, err := patchLoginRespJSONWithRealname(original, "张三", 1778263700)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(patched), &m))

	assert.Equal(t, true, m["realname_verified"])
	assert.Equal(t, "张三", m["real_name"], "stale 旧名必须被 claims 值覆写")
	assert.Equal(t, float64(1778263700), m["realname_verified_at"])
}

func TestPatchLoginRespJSONWithRealname_EmptyNameAndTs_OnlyVerifiedFlag(t *testing.T) {
	// 极端:upsert 了但 claims.LegalName 意外为空 / verifiedAt=0,只下发
	// realname_verified=true 作 sentinel,其它字段让客户端后续 /user/current
	// 刷新补齐(而不是写空字符串污染本地缓存)。
	original := `{"uid":"u3"}`

	patched, err := patchLoginRespJSONWithRealname(original, "", 0)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(patched), &m))

	assert.Equal(t, true, m["realname_verified"])
	_, hasRealName := m["real_name"]
	assert.False(t, hasRealName, "real_name 空字符串时不应写入 JSON")
	_, hasTs := m["realname_verified_at"]
	assert.False(t, hasTs, "verifiedAt<=0 时不应写入 JSON")
}

func TestPatchLoginRespJSONWithRealname_EmptyInput_ReturnsAsIs(t *testing.T) {
	// 空输入是合法的(sessResp 出错降级时 LoginRespJSON 可能为空),
	// helper 必须不报错也不改动。
	patched, err := patchLoginRespJSONWithRealname("", "x", 1)
	require.NoError(t, err)
	assert.Equal(t, "", patched)
}

func TestPatchLoginRespJSONWithRealname_MalformedJSON_ReturnsOriginalPlusErr(t *testing.T) {
	// 非法 JSON:callback handler 的降级是 warn log + 保留原 JSON,
	// helper 契约:返回原值 + 非 nil err,调用方决定是否回退。
	original := `{not json`
	patched, err := patchLoginRespJSONWithRealname(original, "x", 1)
	require.Error(t, err)
	assert.Equal(t, original, patched, "非法 JSON 必须保留原值")
}
