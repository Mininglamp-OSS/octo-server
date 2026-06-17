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
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// GH #1319 / Direction A: 零 Space 用户禁止通过邀请链接 / 扫码入群。
//
// 这些测试锁定三个入群入口在 loginUID != "" && zero-Space 场景下的短路契约：
//   - groupInviteAuthorize (/v1/group/invite/authorize)
//   - groupScanJoin (/v1/groups/:group_no/scanjoin) ——  端到端路径被
//     testutil.NewTestServer 未初始化 ctx.Event 的 EventBegin nil-deref 阻塞
//     （同目录 api_scanjoin_bot_test.go:42 已记录），因此这里用源码断言替代。
//   - groupInviteDetail (/v1/group/invite/detail 可选鉴权)
//
// 共同契约：status == "need_space"，不生成 auth_code，不写入 group_member，
// 优先级高于 external_blocked / already_member / invite_required，因为这是
// 账号状态问题（加完 Space 重试即可），比群级硬拦截更根本、更可操作。

// seedDefaultSpaceForTestUID 为 testutil.UID 建一个独立 Space 并把它加进去，
// 让 space.GetUserDefaultSpaceID(UID) 不返回空串。仅用于那些「期望 UID 已通过
// Space Gate，然后测下一级分支」的用例。需要 UID 真正落在某个特定 Space 的
// 测试（如 TestGroupInviteAuthorize_SameSpaceMemberNotBlocked）不要用这个
// helper——它们自己已经插好了 space_member。
//
// 返回的 spaceID 是稳定、可预测的标识符，和任何「群所属 Space」(测试里常用
// "space-a"/"space-yuj38-*") 都不重合，避免误让 UID 变成群的同 Space 成员。
func seedDefaultSpaceForTestUID(t *testing.T, ctx *config.Context) string {
	t.Helper()
	spaceID := "space-need-space-test-uid-home"
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "need-space-home", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)
	return spaceID
}

// TestGroupInviteAuthorize_NeedSpace_ExternalGroup：零 Space 用户扫外部群
// （allow_external=1）邀请码 → authorize 必须短路返回 need_space，不生成 auth_code。
// 这是 GH #1319 的核心回归：Direction A 决策后，任何入群路径都必须前置检查。
func TestGroupInviteAuthorize_NeedSpace_ExternalGroup(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 注意：**不**插入 testutil.UID 的 space_member —— 模拟新注册零 Space 用户。

	groupNo := "g-need-space-external-allowed"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "外部可加入群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		AllowExternal: 1, // 外部群，允许外部成员加入
	})
	assert.NoError(t, err)

	code := "test-need-space-external-code"
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
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "need_space", resp["status"],
		"零 Space 用户扫外部群邀请码必须被 need_space 拦截（GH #1319）")
	assert.NotEmpty(t, resp["msg"], "need_space 分支必须带用户可见的中文 msg")
	_, authCodeExists := resp["auth_code"]
	assert.False(t, authCodeExists, "need_space 场景不应返回 auth_code")
}

// TestGroupInviteAuthorize_NeedSpace_PriorityOverExternalBlocked：
// 零 Space 用户扫 allow_external=0 的内部群 → 必须优先返回 need_space，
// 而不是 external_blocked。need_space 是用户侧修复路径（加 Space 后重试），
// 比群级 external_blocked 更根本、更可操作，三端才能正确引导用户。
func TestGroupInviteAuthorize_NeedSpace_PriorityOverExternalBlocked(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	spaceID := "space-need-space-priority"
	// 只给其他用户分配 space_member，当前登录用户（testutil.UID）零 Space。
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "内部空间", "10001", 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, "10001", 0, 1).Exec()
	assert.NoError(t, err)

	groupNo := "g-need-space-priority"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "内部群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		SpaceID:       spaceID,
		AllowExternal: 0, // 禁止外部成员
	})
	assert.NoError(t, err)

	code := "test-need-space-priority-code"
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
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "need_space", resp["status"],
		"need_space 优先级必须高于 external_blocked（GH #1319）")
	_, authCodeExists := resp["auth_code"]
	assert.False(t, authCodeExists, "need_space 场景不应返回 auth_code")
}

// TestGroupInviteAuthorize_HasSpaceGoesNormalPath：
// 有 Space 的登录用户扫外部群邀请码 → 正常流程（不被 need_space 误杀），
// 生成 auth_code。回归保护：避免 need_space 条件误把所有用户挡在外面。
func TestGroupInviteAuthorize_HasSpaceGoesNormalPath(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 给 testutil.UID 建一个独立 Space（不与群所属 Space 重合）。
	seedDefaultSpaceForTestUID(t, ctx)

	groupNo := "g-need-space-has-space"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "外部可加入群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		AllowExternal: 1,
	})
	assert.NoError(t, err)

	code := "test-need-space-has-space-code"
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
	req, _ := http.NewRequest("POST", "/v1/group/invite/authorize?code="+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEqual(t, "need_space", resp["status"],
		"有 Space 用户不应被 need_space 误杀")
	authCode, _ := resp["auth_code"].(string)
	assert.NotEmpty(t, authCode, "有 Space 用户应正常拿到 auth_code")
}

// TestGroupInviteDetail_NeedSpace_OptionalAuth：
// 登录用户 + 零 Space 打开公开邀请落地页 → detail 返回 need_space，
// 让 H5 直接展示「请先加入 Space」引导 + 「回主页加入 Space」CTA。
// 未登录访问者（loginUID=""）不触发，保持公共预览体验（回归保护：
// TestGroupInviteDetail_Joinable / TestGroupInviteDetail_ExternalBlocked 无 token）。
func TestGroupInviteDetail_NeedSpace_OptionalAuth(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 不插入 testutil.UID 的 space_member —— 零 Space 用户

	groupNo := "g-need-space-detail"
	err = f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "外部可加入群",
		Creator:       "10001",
		Status:        1,
		Invite:        0,
		AllowExternal: 1,
	})
	assert.NoError(t, err)

	code := "test-need-space-detail-code"
	err = ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": "10001",
		})),
		time.Hour,
	)
	assert.NoError(t, err)

	// 可选鉴权路径：带 token 让后端能识别登录态并下发 need_space。
	req := newInviteRequest(t, "/v1/group/invite/detail?code="+code)
	req.Header.Set("token", testutil.Token)

	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "need_space", resp["status"],
		"登录 + 零 Space 的 detail 请求必须返回 need_space（GH #1319）")
}

// TestQRCodeHandleJoinGroup_NeedSpaceBranchExists：qrcode/api.go handleJoinGroup
// 是三端「扫码入群」入口的 dispatch 处（通过通用 /v1/qrcode 端点分发），它自己
// 持有业务分支而不调用 HTTP；端到端 HTTP 测试同样被 testutil ctx.Event 限制，
// 因此这里对源码做 grep 断言，锁定 need_space 预检存在且位置正确。
// 与同目录 api_scanjoin_bot_test.go / api_scanjoin_space_info_test.go 的做法一致。
func TestQRCodeHandleJoinGroup_NeedSpaceBranchExists(t *testing.T) {
	src, err := os.ReadFile("../qrcode/api.go")
	assert.NoError(t, err, "读取 qrcode/api.go 失败")
	text := string(src)

	// 以 handleJoinGroup 函数开头为锚点，截到下一个 func 开头为止作为 grep 作用域，
	// 避免扫到 handleScanLogin 等其它 handler 的 SpaceID/AllowExternal 片段。
	start := strings.Index(text, "func (q *QRCode) handleJoinGroup(")
	if start == -1 {
		t.Fatalf("qrcode/api.go 中 handleJoinGroup 函数不存在（被误删或 rename？）")
	}
	rest := text[start:]
	nextFuncOffset := strings.Index(rest[1:], "\nfunc ")
	body := rest
	if nextFuncOffset != -1 {
		body = rest[:nextFuncOffset+1]
	}

	assert.Contains(t, body, "spacemod.GetUserDefaultSpaceID(q.ctx, loginUID)",
		"handleJoinGroup 必须在入口处用 GetUserDefaultSpaceID 做零 Space 预检")
	assert.Contains(t, body, `"need_space"`,
		"handleJoinGroup 必须在零 Space 分支返回 status=need_space")

	// 位置约束：need_space 预检必须早于现有的 allow_external / ExistMember 业务判定。
	needSpaceIdx := indexMust(t, body, "GetUserDefaultSpaceID")
	externalIdx := indexMust(t, body, "AllowExternal == 0")
	assert.Less(t, needSpaceIdx, externalIdx,
		"need_space 预检必须放在 allow_external 判定之前（GH #1319 Direction A 优先级）")
}

// indexMust 返回 needle 在 haystack 里的首次出现下标；找不到则 t.Fatal，
// 避免下游 assert.Less 用 -1 产出困惑的失败信息。
func indexMust(t *testing.T, haystack, needle string) int {
	t.Helper()
	idx := strings.Index(haystack, needle)
	if idx == -1 {
		t.Fatalf("未在被测源码片段中找到 %q", needle)
	}
	return idx
}

// TestGroupScanJoin_NeedSpaceBranchExists：/v1/groups/:group_no/scanjoin 的
// HTTP 端到端在 testutil 环境下会命中 EventBegin nil-deref（见 api_scanjoin_bot_test.go:42），
// 本测用源码断言锁定 groupScanJoin handler 的 need_space 预检存在且位置正确。
// 契约：loginUID 校验之后、auth_code / 群资料查询之前，与 groupInviteAuthorize 对齐。
func TestGroupScanJoin_NeedSpaceBranchExists(t *testing.T) {
	body := mustReadFunc(t, "api.go", "func (g *Group) groupScanJoin(")

	assert.Contains(t, body, "spacemod.GetUserDefaultSpaceID(g.ctx, loginUID)",
		"groupScanJoin 必须在入口处用 GetUserDefaultSpaceID 做零 Space 预检")
	assert.Contains(t, body, "groupInviteStatusNeedSpace",
		"groupScanJoin 必须使用 groupInviteStatusNeedSpace 常量返回 status")

	loginUIDIdx := indexMust(t, body, `loginUID := c.GetLoginUID()`)
	needSpaceIdx := indexMust(t, body, `spacemod.GetUserDefaultSpaceID(g.ctx, loginUID)`)
	authCodeIdx := indexMust(t, body, `c.Query("auth_code")`)

	assert.Less(t, loginUIDIdx, needSpaceIdx,
		"need_space 检查必须在 loginUID 校验之后")
	assert.Less(t, needSpaceIdx, authCodeIdx,
		"need_space 检查必须早于 auth_code 读取，避免白占 Redis TTL")
}
