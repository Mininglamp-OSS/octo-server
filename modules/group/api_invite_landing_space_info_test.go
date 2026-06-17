package group

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// YUJ-211 / GH octo-server#1277: 扫码加入跨 Space 群时 H5 落地页必须显示群归属
// Space，避免用户进群后找不到群挂到哪个 Space。
//
// 契约：
//   1. GET /v1/group/invite/detail 响应必须包含 space_id + space_name 两字段。
//      - 群有归属 Space：space_id 为实际 space_id，space_name 非空。
//      - 公共群（groupModel.SpaceID=""）：space_id="" 且 space_name=""。
//   2. assets/web/group_invite.html 渲染「📁 属于 "XX" Space」归属行（v-space /
//      v-space-name），后端 space_name 为空时保持隐藏（不伤害公共群极简卡片）。
//   3. external_blocked 状态页追加「你不是「XX」Space 的成员」提示，space_name
//      非空时显示，降级时隐藏。
//   4. 新字段对旧客户端向后兼容——JSON 多字段不会让旧 H5 报错，既有 status
//      分支（expired / not_found / rate-limit / server-error / network-error）
//      不得回归。

// ---------- 后端：space_id / space_name 字段契约 ----------

// 群挂在 Space 下：detail 必须下发真实 space_id + space_name，前端才能渲染归属行。
func TestGroupInviteDetail_IncludesSpaceID_WithSpace(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-yuj211-with"
	spaceName := "产品空间"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, spaceName, testutil.UID, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-yuj211-with-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "产品对齐群",
		Creator:       testutil.UID,
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 1, // 避免触发 external_blocked，覆盖 joinable 路径
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-yuj211-with-space-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "joinable", resp["status"])
	assert.Equal(t, spaceID, resp["space_id"],
		"YUJ-211: 群挂 Space 时 detail 必须下发真实 space_id，落地页才能渲染归属行")
	assert.Equal(t, spaceName, resp["space_name"],
		"YUJ-211: space_name 同批下发，与 space_id 成对")
}

// 公共群（无 SpaceID）：space_id 必须为空串而非缺省，前端按空串隐藏归属行，
// 且保证旧客户端 JSON.parse 看到的是稳定字段集合（向后兼容）。
func TestGroupInviteDetail_IncludesSpaceID_PublicGroup(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "g-yuj211-public"
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "公共群",
		Creator: testutil.UID,
		Status:  1,
		Invite:  0,
		// SpaceID 留空——App 内创建的普通群
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-yuj211-public-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "joinable", resp["status"])
	// 关键：空串（而不是 nil / 不存在），让 H5 ` if (data.space_name) ` 空串分支隐藏归属行。
	sid, ok := resp["space_id"]
	assert.True(t, ok, "YUJ-211: 字段必须存在，保证 shape 稳定")
	assert.Equal(t, "", sid, "公共群 space_id 必须是空串，H5 据此隐藏归属行")
	assert.Equal(t, "", resp["space_name"])
}

// external_blocked 状态同样需要下发 space_id + space_name，
// 前端状态页据此渲染「你不是「XX」Space 的成员」。
func TestGroupInviteDetail_ExternalBlocked_IncludesSpaceInfo(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-yuj211-ext"
	spaceName := "内部空间"
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, spaceName, "10001", 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-yuj211-ext-blocked"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "Space 内部协作群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 0,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "10001", Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	code := "test-yuj211-ext-blocked-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite/detail?code="+code))

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "external_blocked", resp["status"])
	assert.Equal(t, spaceID, resp["space_id"],
		"external_blocked 状态也必须下发 space_id，前端状态页据此渲染归属")
	assert.Equal(t, spaceName, resp["space_name"])
}

// ---------- H5 落地页：归属行 + external_blocked 提示渲染 ----------

// 落地页 HTML 必须包含归属行元素 + external_blocked 额外提示锚点，
// 保证前端代码不会被后端模板替换误伤。用 grep 测试钉字面量。
func TestGroupInvitePage_RendersSpaceLandingElements(t *testing.T) {
	s, ctx := newTestServer(t)
	_ = New(ctx)

	wd, err := os.Getwd()
	assert.NoError(t, err)
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("chdir to repo root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, newInviteRequest(t, "/v1/group/invite?code=yuj211-landing-check"))

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	// 归属行锚点：id="v-space" + id="v-space-name" + "属于" + "Space"
	assert.True(t, strings.Contains(body, `id="v-space"`),
		"group_invite.html 必须保留归属行容器（id=v-space），JS 按 space_name 空串开关")
	assert.True(t, strings.Contains(body, `id="v-space-name"`),
		"group_invite.html 必须保留 space-name 锚点（id=v-space-name），用于回填 Space 名称")
	assert.True(t, strings.Contains(body, "属于"),
		"group_invite.html 归属行文案必须包含『属于』，匹配产品 copy")

	// renderInfo JS 必须根据 data.space_name 决定是否显示归属行——这是向后兼容关键：
	// 旧响应没有 space_name → 分支为 falsy → 归属行隐藏，等价于旧行为。
	assert.True(t, strings.Contains(body, "data.space_name"),
		"renderInfo 必须读取 data.space_name，空串分支保持公共群极简卡片")

	// external_blocked 额外提示锚点 + 文案
	assert.True(t, strings.Contains(body, `id="state-external-space"`),
		"external_blocked 状态页必须有 Space 提示容器，YUJ-211 多一条提示")
	assert.True(t, strings.Contains(body, `id="state-external-space-name"`),
		"external_blocked Space 名称回填锚点必须存在")
	assert.True(t, strings.Contains(body, "你不是"),
		"external_blocked 必须包含「你不是…Space 的成员」文案")

	// 其他状态不回归：下列 state-page 必须仍然存在
	for _, id := range []string{
		`id="state-expired"`, `id="state-not-found"`, `id="state-external-blocked"`,
		`id="state-rate-limited"`, `id="state-server-error"`, `id="state-network-error"`,
	} {
		assert.True(t, strings.Contains(body, id),
			"state-page 回归：%s 不得因 YUJ-211 改动丢失", id)
	}
}
