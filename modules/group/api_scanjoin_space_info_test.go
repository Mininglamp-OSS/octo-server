package group

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// YUJ-170 / dmwork-web#1100 — groupScanJoin 成功响应必须带
// space_id / space_name / group_no / group_name，供 H5 join_group.html
// 在单次往返内判断 crossSpace 并写 sessionStorage.pendingJoinSuccessNotice。
//
// 为什么是 grep 测试而不是端到端 httptest：
// 同目录 api_scanjoin_bot_test.go 已记录 testutil.NewTestServer 未初始化
// ctx.Event，任何触达 EventBegin 的 handler 测试都会 nil-deref panic。
// 该限制非本 PR 引入，scanjoin 的响应 shape 也按同样方式用源码断言覆盖，
// 待 testutil 引入 wkevent.NoopEvent 后可升级为端到端断言。
//
// H5 / dmwork-web 消费侧契约（`kind='group'` sessionStorage payload）
// 的 grep 测试见 dmwork-web PR #1102 的 groupInviteCrossSpaceToast.test.ts。
func TestGroupScanJoin_ResponseContainsSpaceInfo_YUJ170(t *testing.T) {
	src, err := os.ReadFile("api.go")
	assert.NoError(t, err)
	text := string(src)

	// 锁定 groupScanJoin 函数体，避免规则误伤其它同名字段引用。
	start := strings.Index(text, "func (g *Group) groupScanJoin(")
	assert.NotEqual(t, -1, start, "groupScanJoin 函数必须存在（handler 未被删除）")
	rest := text[start:]

	// 函数边界：下一个 `func (g *Group) ` 开头就是下一个 handler 的起点。
	// 函数之间可能隔着 doc comment / 普通 comment / 空行，但不会再出现该 prefix。
	nextFuncOffset := strings.Index(rest[1:], "func (g *Group) ")
	assert.NotEqual(t, -1, nextFuncOffset, "groupScanJoin 函数边界定位失败")
	body := rest[:nextFuncOffset+1]

	// 关键契约：函数体内必须有 c.Response(gin.H{...}) 且 payload 含 space_id + space_name。
	// 不再使用 c.ResponseOK() 空载——那会让 H5 读不到 Space 字段，cross-space notice 死路。
	assert.NotContains(t, body, "c.ResponseOK()",
		"groupScanJoin 不应再用 ResponseOK() 空载响应——H5 依赖 space_id/space_name 判定 crossSpace")
	assert.Regexp(t, regexp.MustCompile(`c\.Response\(\s*gin\.H\{`), body,
		"groupScanJoin 必须用 c.Response(gin.H{...}) 带数据返回")

	// 具体字段（H5 pendingJoinSuccessNotice 写入依赖）：
	assert.Regexp(t, regexp.MustCompile(`"space_id"\s*:\s*group\.SpaceID`), body,
		"scanjoin 响应必须含 space_id，来自 group.SpaceID")
	assert.Regexp(t, regexp.MustCompile(`"space_name"\s*:\s*spaceName`), body,
		"scanjoin 响应必须含 space_name（pkg/space.GetSpaceName 结果）")
	assert.Regexp(t, regexp.MustCompile(`"group_no"\s*:\s*groupNo`), body,
		"scanjoin 响应应回带 group_no，便于 H5 直接写 notice.groupNo")
	assert.Regexp(t, regexp.MustCompile(`"group_name"\s*:\s*group\.Name`), body,
		"scanjoin 响应应回带 group_name，便于 H5 写 notice.groupName/entityName")

	// 降级语义：GetSpaceName 失败必须走 Warn + 空串，不可 return error 阻塞入群。
	assert.Regexp(t, regexp.MustCompile(`GetSpaceName\(g\.ctx\.DB\(\),\s*group\.SpaceID\)`), body,
		"scanjoin 必须调用 spacepkg.GetSpaceName 解析 Space 名称")
	assert.Regexp(t, regexp.MustCompile(`(?s)spaceNameErr\s*!=\s*nil.*?spaceName\s*=\s*""`), body,
		"GetSpaceName 失败必须降级空串，不应阻塞已提交的入群事务")
}

// TestGroupScanJoinH5_WritesPendingJoinSuccessNotice_YUJ170 断言 H5
// assets/web/join_group.html 的 scanjoin .then 分支写 sessionStorage
// pendingJoinSuccessNotice（YUJ-106 契约 key）且仅在 crossSpace 时触发。
func TestGroupScanJoinH5_WritesPendingJoinSuccessNotice_YUJ170(t *testing.T) {
	src, err := os.ReadFile("../../assets/web/join_group.html")
	assert.NoError(t, err)
	text := string(src)

	// A. scanjoin .then 块中写 sessionStorage 的 key 必须是 YUJ-106 契约的
	//    pendingJoinSuccessNotice（不是 joinSuccessNotice，也不是其它名字）。
	assert.Contains(t, text, "sessionStorage.setItem('pendingJoinSuccessNotice'",
		"H5 sessionStorage key 必须与 YUJ-106 JoinSuccessNotice helper 对齐")

	// B. payload 字段必须含 kind:'group' + YUJ-106 契约字段 (spaceId/spaceName/
	//    entityName/crossSpace) + YUJ-170 扩展 (groupNo/groupName/ts)。
	assert.Regexp(t, regexp.MustCompile(`kind\s*:\s*'group'`), text)
	assert.Regexp(t, regexp.MustCompile(`spaceId\s*:\s*groupSpaceId`), text)
	assert.Regexp(t, regexp.MustCompile(`spaceName\s*:\s*spaceName`), text)
	assert.Regexp(t, regexp.MustCompile(`entityName\s*:\s*groupName`), text)
	assert.Regexp(t, regexp.MustCompile(`crossSpace\s*:\s*true`), text)
	assert.Regexp(t, regexp.MustCompile(`ts\s*:\s*Date\.now\(\)`), text)

	// C. crossSpace 守卫：必须同时检查 groupSpaceId 非空、prevSpaceId 非空、
	//    且二者不等。私群（space_id=""）/ 单 Space 用户 → 跳过 notice 写入。
	assert.Regexp(t,
		regexp.MustCompile(`if\s*\(\s*groupSpaceId\s*&&\s*prevSpaceId\s*&&\s*groupSpaceId\s*!==\s*prevSpaceId\s*\)`),
		text, "H5 必须 3 条件 AND 守卫才写 notice，防止同 Space / 私群误弹")

	// D. 数据来自 scanjoin 响应 resp（Method B — 单次往返），不是二次 /detail。
	assert.Regexp(t, regexp.MustCompile(`resp\.space_id`), text,
		"H5 必须从 scanjoin 响应读 space_id，不能再 fetch /detail（死代码）")
	assert.NotRegexp(t, regexp.MustCompile(`groups/[^/]+/detail[^"]*\)\.then\([^)]*space_id`), text,
		"H5 不应再在 scanjoin 后二次调用 /detail 拿 space_id（死代码 / 多余往返）")
}

// TestGroupInviteH5_WritesPendingJoinSuccessNotice_YUJ179 断言 Web 端
// /v1/group/invite 实际 serve 的 assets/web/group_invite.html 同样在
// scanjoin 成功分支写 sessionStorage.pendingJoinSuccessNotice。
//
// 背景（octo-server#1255 / YUJ-170 round 3）：
// PR#1250 只改了 assets/web/join_group.html，但 api.go groupInvitePage
// serve 的是 assets/web/group_invite.html（两个文件各有独立 scanjoin
// 调用链）。Yu 测试反馈 Web 跨 Space 加群无 toast / 无 Space 名称，
// 根因就是 group_invite.html 这条路径漏改，本测试守住回归。
func TestGroupInviteH5_WritesPendingJoinSuccessNotice_YUJ179(t *testing.T) {
	src, err := os.ReadFile("../../assets/web/group_invite.html")
	assert.NoError(t, err)
	text := string(src)

	// A. sessionStorage key 必须是 YUJ-106 契约：pendingJoinSuccessNotice。
	assert.Contains(t, text, "sessionStorage.setItem('pendingJoinSuccessNotice'",
		"group_invite.html sessionStorage key 必须与 YUJ-106 JoinSuccessNotice helper 对齐")

	// B. payload 字段必须含 kind:'group' + 契约字段 + crossSpace:true。
	assert.Regexp(t, regexp.MustCompile(`kind\s*:\s*'group'`), text,
		"group_invite.html notice payload 必须含 kind:'group'")
	assert.Regexp(t, regexp.MustCompile(`crossSpace\s*:\s*true`), text,
		"group_invite.html notice payload 必须含 crossSpace:true（首页切换过去按钮依赖）")
	assert.Regexp(t, regexp.MustCompile(`spaceId\s*:\s*groupSpaceId`), text)
	assert.Regexp(t, regexp.MustCompile(`spaceName\s*:\s*r\.space_name`), text,
		"spaceName 必须来自 scanjoin 响应 r.space_name，不能硬编码")
	assert.Regexp(t, regexp.MustCompile(`entityName\s*:\s*r\.group_name`), text)
	assert.Regexp(t, regexp.MustCompile(`ts\s*:\s*Date\.now\(\)`), text)

	// C. scanjoin 响应解析必须保留 space_id / space_name / group_no / group_name，
	//    只保留 msg 会让下游拿不到 Space 字段 — 正是 YUJ-170 round 2 漏掉的路径。
	assert.Regexp(t, regexp.MustCompile(`space_id\s*:\s*data\.space_id`), text,
		"group_invite.html scanjoin 响应解析必须保留 space_id 字段")
	assert.Regexp(t, regexp.MustCompile(`space_name\s*:\s*data\.space_name`), text,
		"group_invite.html scanjoin 响应解析必须保留 space_name 字段")
	assert.Regexp(t, regexp.MustCompile(`group_no\s*:\s*data\.group_no`), text,
		"group_invite.html scanjoin 响应解析必须保留 group_no 字段")
	assert.Regexp(t, regexp.MustCompile(`group_name\s*:\s*data\.group_name`), text,
		"group_invite.html scanjoin 响应解析必须保留 group_name 字段")

	// D. crossSpace 守卫：必须 3 条件 AND，避免同 Space / 私群误弹。
	assert.Regexp(t,
		regexp.MustCompile(`if\s*\(\s*groupSpaceId\s*&&\s*prevSpaceId\s*&&\s*groupSpaceId\s*!==\s*prevSpaceId\s*\)`),
		text, "group_invite.html 必须 3 条件 AND 守卫才写 notice，防止同 Space / 私群误弹")
}
