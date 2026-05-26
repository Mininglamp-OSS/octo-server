package space

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// 用户侧 PUT /v1/space/:space_id 测试。
//
// 复用 manager 端测试基础设施（seedSpace / readSpace / testutil.Token），
// 重点覆盖 issue #163 acceptance：partial update 不再误清空、字段级校验、
// preset_group_ids JSON 校验、auth、disbanded 拒绝、幂等重放、审计日志路径。
//
// TOCTOU 已由 TestManagerDB_UpdateSpaceProfile_TOCTOU 覆盖，本文件不再重复。

// seedUserSpace 为当前测试用户（testutil.UID）创建一个 spaceId + member 行。
// role: 0=普通成员 / 1=admin / 2=owner。
func seedUserSpace(t *testing.T, spaceId, name string, callerRole int, status int) {
	t.Helper()
	err := testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    name,
		Creator: testutil.UID,
		Status:  status,
	})
	assert.NoError(t, err)
	err = testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    callerRole,
		Status:  1,
	})
	assert.NoError(t, err)
}

func putSpace(t *testing.T, spaceId, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/space/"+spaceId, bytes.NewReader([]byte(body)))
	assert.NoError(t, err)
	if token != "" {
		req.Header.Set("token", token)
	}
	testSrv.GetRoute().ServeHTTP(w, req)
	return w
}

func TestUserUpdateSpace_PartialUpdate(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	t.Run("update name only leaves other fields untouched", func(t *testing.T) {
		spaceId := "usr-upd-name"
		seedUserSpace(t, spaceId, "old name", 2, SpaceStatusNormal)
		// 给空间补一些初始非默认值，便于验证未变更字段
		_, err := testCtx.DB().Update("space").
			Set("description", "orig desc").
			Set("logo", "orig-logo").
			Set("join_mode", JoinModeApproval).
			Set("preset_group_ids", `["g_old"]`).
			Where("space_id=?", spaceId).Exec()
		assert.NoError(t, err)

		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": "new shiny name"}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

		sp := readSpace(t, spaceId)
		assert.Equal(t, "new shiny name", sp.Name)
		assert.Equal(t, "orig desc", sp.Description, "description 未提供时不应被覆盖为空（回归 issue #163 第 1 点）")
		assert.Equal(t, "orig-logo", sp.Logo, "logo 未提供时不应被覆盖为空")
		assert.Equal(t, JoinModeApproval, sp.JoinMode)
		assert.NotNil(t, sp.PresetGroupIds)
		assert.Equal(t, `["g_old"]`, *sp.PresetGroupIds)
	})

	t.Run("update description only", func(t *testing.T) {
		spaceId := "usr-upd-desc"
		seedUserSpace(t, spaceId, "keep me", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"description": "new desc"}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		sp := readSpace(t, spaceId)
		assert.Equal(t, "keep me", sp.Name)
		assert.Equal(t, "new desc", sp.Description)
	})

	t.Run("update logo only", func(t *testing.T) {
		spaceId := "usr-upd-logo"
		seedUserSpace(t, spaceId, "keep me", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"logo": "https://cdn.example/x.png"}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "https://cdn.example/x.png", readSpace(t, spaceId).Logo)
	})

	t.Run("update join_mode only", func(t *testing.T) {
		spaceId := "usr-upd-jm"
		seedUserSpace(t, spaceId, "keep", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"join_mode": JoinModeApproval}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, JoinModeApproval, readSpace(t, spaceId).JoinMode)
	})

	t.Run("update preset_group_ids only", func(t *testing.T) {
		spaceId := "usr-upd-pgi"
		seedUserSpace(t, spaceId, "keep", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"preset_group_ids": `["g_new1","g_new2"]`}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		sp := readSpace(t, spaceId)
		assert.NotNil(t, sp.PresetGroupIds)
		assert.Equal(t, `["g_new1","g_new2"]`, *sp.PresetGroupIds)
	})

	t.Run("preset_group_ids JSON null is treated as no-change", func(t *testing.T) {
		// 区分两种"未提供"语义：
		//   - 字段缺省 → req.PresetGroupIds == nil → 不变更
		//   - JSON null → Go 解码也得 nil → 不变更（不应被当成"清空"）
		// 客户端要清空请显式传空字符串 ""。
		spaceId := "usr-upd-pgi-null"
		seedUserSpace(t, spaceId, "keep", 2, SpaceStatusNormal)
		_, err := testCtx.DB().Update("space").
			Set("preset_group_ids", `["g_keep"]`).
			Where("space_id=?", spaceId).Exec()
		assert.NoError(t, err)

		// 拼一个带有其他字段的 body（仅 null preset_group_ids 会被 handler 拒为 all-nil），
		// 验证 null 不会触发"清空"行为。
		body := `{"name":"renamed","preset_group_ids":null}`
		w := putSpace(t, spaceId, body, testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		sp := readSpace(t, spaceId)
		assert.Equal(t, "renamed", sp.Name)
		assert.NotNil(t, sp.PresetGroupIds)
		assert.Equal(t, `["g_keep"]`, *sp.PresetGroupIds, "JSON null 不应清空 preset_group_ids")
	})

	t.Run("preset_group_ids empty string clears the list", func(t *testing.T) {
		spaceId := "usr-upd-pgi-clr"
		seedUserSpace(t, spaceId, "keep", 2, SpaceStatusNormal)
		_, err := testCtx.DB().Update("space").
			Set("preset_group_ids", `["g_old"]`).
			Where("space_id=?", spaceId).Exec()
		assert.NoError(t, err)

		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"preset_group_ids": ""}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		sp := readSpace(t, spaceId)
		assert.NotNil(t, sp.PresetGroupIds)
		assert.Equal(t, "", *sp.PresetGroupIds, "空字符串应落地为空值，表示清空预设群组")
	})
}

func TestUserUpdateSpace_CombinedUpdate(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "usr-upd-combo"
	seedUserSpace(t, spaceId, "old", 2, SpaceStatusNormal)

	body := util.ToJson(map[string]interface{}{
		"name":             "new name",
		"description":      "new desc",
		"logo":             "https://cdn.example/y.png",
		"join_mode":        JoinModeApproval,
		"preset_group_ids": `["g_a","g_b"]`,
	})
	w := putSpace(t, spaceId, body, testutil.Token)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	sp := readSpace(t, spaceId)
	assert.Equal(t, "new name", sp.Name)
	assert.Equal(t, "new desc", sp.Description)
	assert.Equal(t, "https://cdn.example/y.png", sp.Logo)
	assert.Equal(t, JoinModeApproval, sp.JoinMode)
	assert.NotNil(t, sp.PresetGroupIds)
	assert.Equal(t, `["g_a","g_b"]`, *sp.PresetGroupIds)
}

func TestUserUpdateSpace_TrimsWhitespace(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	t.Run("trim name and persist trimmed", func(t *testing.T) {
		spaceId := "usr-upd-trim-n"
		seedUserSpace(t, spaceId, "old", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": "   padded   "}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "padded", readSpace(t, spaceId).Name)
	})

	t.Run("trim description and persist trimmed", func(t *testing.T) {
		spaceId := "usr-upd-trim-d"
		seedUserSpace(t, spaceId, "old", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"description": "  hi  "}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "hi", readSpace(t, spaceId).Description)
	})
}

func TestUserUpdateSpace_Validation(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "usr-upd-v"
	seedUserSpace(t, spaceId, "v space", 2, SpaceStatusNormal)

	t.Run("reject empty body / all-nil", func(t *testing.T) {
		w := putSpace(t, spaceId, `{}`, testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})

	t.Run("reject empty/whitespace name", func(t *testing.T) {
		for _, name := range []string{"", "   "} {
			w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": name}), testutil.Token)
			assert.NotEqual(t, http.StatusOK, w.Code, "empty name %q should be rejected", name)
		}
	})

	t.Run("reject name longer than 100 chars (ASCII)", func(t *testing.T) {
		long := strings.Repeat("a", 101)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": long}), testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})

	t.Run("accept 100 CJK chars but reject 101", func(t *testing.T) {
		// 与 manager 端 utf8 长度修复对齐：100 个汉字（300 字节）应通过，101 个被拒。
		spaceIdOK := "usr-upd-cjk-ok"
		seedUserSpace(t, spaceIdOK, "old", 2, SpaceStatusNormal)
		nameOK := strings.Repeat("空", 100)
		w := putSpace(t, spaceIdOK, util.ToJson(map[string]interface{}{"name": nameOK}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, "100 个汉字应被接受")
		assert.Equal(t, nameOK, readSpace(t, spaceIdOK).Name)

		nameBad := strings.Repeat("空", 101)
		w = putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": nameBad}), testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code, "101 个汉字应被拒")
	})

	t.Run("reject description longer than 500 chars", func(t *testing.T) {
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"description": strings.Repeat("d", 501)}), testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})

	t.Run("reject logo longer than 200 chars", func(t *testing.T) {
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"logo": strings.Repeat("l", 201)}), testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})

	t.Run("reject invalid join_mode", func(t *testing.T) {
		for _, jm := range []int{-1, 2, 99} {
			w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"join_mode": jm}), testutil.Token)
			assert.NotEqual(t, http.StatusOK, w.Code, "join_mode=%d should be rejected", jm)
		}
	})

	t.Run("reject preset_group_ids not a JSON array of strings", func(t *testing.T) {
		// 覆盖 json.Unmarshal 到 []string 时静默放过的坑：
		//   - top-level "null"   → 解到 []string 得 nil slice，无 error
		//   - ["a", null]        → 解到 []string 把 null 写成 ""，无 error
		// 必须显式拒绝，否则会把无效配置写入 DB，后续 joinPresetGroups 静默跳过。
		cases := []string{
			`{"not":"array"}`, // object
			`["ok", 123]`,     // 混合类型
			`"just a string"`, // 裸字符串
			`12345`,           // number
			`[`,               // 非法 JSON
			`null`,            // top-level null（旧实现会放过）
			`[null]`,          // 数组含 null（旧实现会放过并落库 [""]）
			`["a", null, "b"]`,
			`[{}]`,            // 数组含对象
			`[[]]`,            // 数组含数组
			`[true]`,          // 数组含布尔
		}
		for _, raw := range cases {
			w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"preset_group_ids": raw}), testutil.Token)
			assert.NotEqual(t, http.StatusOK, w.Code, "preset_group_ids=%q should be rejected", raw)
		}
	})

	t.Run("accept empty array as valid (semantically equal to empty string)", func(t *testing.T) {
		spaceIdOK := "usr-upd-pgi-empty-arr"
		seedUserSpace(t, spaceIdOK, "keep", 2, SpaceStatusNormal)
		w := putSpace(t, spaceIdOK, util.ToJson(map[string]interface{}{"preset_group_ids": `[]`}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		sp := readSpace(t, spaceIdOK)
		assert.NotNil(t, sp.PresetGroupIds)
		assert.Equal(t, `[]`, *sp.PresetGroupIds)
	})

	t.Run("reject preset_group_ids exceeding sanity cap", func(t *testing.T) {
		// 构造一个超过 65535 字节的字符串
		raw := `["` + strings.Repeat("x", userSpacePresetGroupIdsMaxBytes+10) + `"]`
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"preset_group_ids": raw}), testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
	})
}

func TestUserUpdateSpace_Auth(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	body := util.ToJson(map[string]interface{}{"name": "x"})

	t.Run("non-member is rejected", func(t *testing.T) {
		spaceId := "usr-upd-auth-nm"
		// 空间存在但 testutil.UID 不是成员
		err := testSpaceDB.insertSpaceNoTx(&SpaceModel{
			SpaceId: spaceId,
			Name:    "no-member",
			Creator: "other-user",
			Status:  SpaceStatusNormal,
		})
		assert.NoError(t, err)

		w := putSpace(t, spaceId, body, testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "无权限")
	})

	t.Run("role=0 plain member is rejected", func(t *testing.T) {
		spaceId := "usr-upd-auth-mem"
		err := testSpaceDB.insertSpaceNoTx(&SpaceModel{
			SpaceId: spaceId,
			Name:    "plain-member",
			Creator: "other-user",
			Status:  SpaceStatusNormal,
		})
		assert.NoError(t, err)
		err = testSpaceDB.insertMemberNoTx(&MemberModel{
			SpaceId: spaceId,
			UID:     testutil.UID,
			Role:    0,
			Status:  1,
		})
		assert.NoError(t, err)

		w := putSpace(t, spaceId, body, testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "无权限")
	})

	t.Run("role=1 admin is accepted", func(t *testing.T) {
		spaceId := "usr-upd-auth-adm"
		err := testSpaceDB.insertSpaceNoTx(&SpaceModel{
			SpaceId: spaceId,
			Name:    "admin-space",
			Creator: "other-user",
			Status:  SpaceStatusNormal,
		})
		assert.NoError(t, err)
		err = testSpaceDB.insertMemberNoTx(&MemberModel{
			SpaceId: spaceId,
			UID:     testutil.UID,
			Role:    1,
			Status:  1,
		})
		assert.NoError(t, err)

		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": "admin-renamed"}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "admin-renamed", readSpace(t, spaceId).Name)
	})

	t.Run("role=2 owner is accepted", func(t *testing.T) {
		spaceId := "usr-upd-auth-own"
		seedUserSpace(t, spaceId, "owner-space", 2, SpaceStatusNormal)
		w := putSpace(t, spaceId, util.ToJson(map[string]interface{}{"name": "owner-renamed"}), testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "owner-renamed", readSpace(t, spaceId).Name)
	})
}

func TestUserUpdateSpace_ActiveGuard(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)
	body := util.ToJson(map[string]interface{}{"name": "x"})

	t.Run("rejects disbanded space at active guard", func(t *testing.T) {
		spaceId := "usr-upd-dead"
		seedUserSpace(t, spaceId, "dead", 2, SpaceStatusDisbanded)
		w := putSpace(t, spaceId, body, testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "解散")
	})

	t.Run("rejects banned space at active guard", func(t *testing.T) {
		spaceId := "usr-upd-ban"
		seedUserSpace(t, spaceId, "ban", 2, SpaceStatusBanned)
		w := putSpace(t, spaceId, body, testutil.Token)
		assert.NotEqual(t, http.StatusOK, w.Code)
		// banned 走 checkSpaceActive，返回"空间不存在或已解散"提示
	})
}

func TestUserUpdateSpace_IdempotentReplay(t *testing.T) {
	// 回归：MySQL 默认 RowsAffected = 实际变更行数。
	// helper 走 SELECT ... FOR UPDATE + sentinel，不依赖 RowsAffected，
	// 字段值完全相同的重放仍应返回 200。
	_, _, err := setup(t)
	assert.NoError(t, err)

	spaceId := "usr-upd-idem"
	seedUserSpace(t, spaceId, "same name", 2, SpaceStatusNormal)
	body := util.ToJson(map[string]interface{}{"name": "same name"})

	for i := 0; i < 2; i++ {
		w := putSpace(t, spaceId, body, testutil.Token)
		assert.Equal(t, http.StatusOK, w.Code, fmt.Sprintf("第 %d 次重放应仍为 200", i+1))
		assert.NotContains(t, w.Body.String(), "不存在")
	}
}
