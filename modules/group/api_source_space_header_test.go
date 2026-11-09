package group

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// YUJ-199 / GH#1265 — groupScanJoin 与 addMembersTx 的邀请确认路径都必须
// 把 source_space_id 绑定到**请求发起时的 X-Space-ID header**（当前 Space 视角），
// 而不是操作者的 home Space。否则 A 在测试空间建群、B 在华山派扫码入群后，
// 群会错落在 B 的 home (ExampleCorp) 视图。
//
// 三端 App 拦截器（YUJ-88 / GH#1038 / EP3）早就把 X-Space-ID 注入在每个请求
// 的 header 里；本 bug 仅存在于后端写入路径（+ H5 两个公开页的兜底 fetch）。
//
// 为什么以源码断言 + httptest 两种 style 的测试都用 grep：
// scanjoin / invite-sure handler 都会触达 ctx.EventBegin，而 testutil
// 目前不初始化 wkevent 子系统——任何 HTTP 驱动测试会 nil-deref panic
// （已记录在 api_scanjoin_bot_test.go 的长注释 + Mininglamp-OSS/octo-server#1184）。
// 该回归在 testutil 升级之前只能用源码 grep 断言锁住，待 NoopEvent mock
// 进入 testutil 后可升级为端到端 assert。
// 这里用分函数边界的 grep 精准锁定，避免误匹配同名字段。

// Test 1: groupScanJoin 必须读 X-Space-ID header，并把 header / 成员校验 /
// home 兜底交给 resolveScanJoinIdentity（优先级：header → home Space 兜底）。
// 决策本身的行为由 scanjoin_identity_test.go 的表驱动测试覆盖。
func TestGroupScanJoin_SourceSpaceID_PrefersXSpaceIDHeader_YUJ199(t *testing.T) {
	body := mustReadFunc(t, "api.go", "func (g *Group) groupScanJoin(")

	// A. 必须调用 c.GetHeader("X-Space-ID")，且结果被 TrimSpace。
	assert.Regexp(t,
		regexp.MustCompile(`headerSpaceID\s*:=\s*strings\.TrimSpace\(\s*c\.GetHeader\(\s*"X-Space-ID"\s*\)\s*\)`),
		body,
		"groupScanJoin 必须从 X-Space-ID header 读当前 Space（YUJ-88 / GH#1038 / EP3 三端拦截器注入）")

	// B. header 必须传入 resolveScanJoinIdentity，且结果用作 sourceSpaceID。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)resolveScanJoinIdentity\(\s*group\.SpaceID,\s*group\.AllowExternal,\s*headerSpaceID,`),
		body,
		"groupScanJoin 必须把 X-Space-ID header 交给 resolveScanJoinIdentity")
	assert.Regexp(t,
		regexp.MustCompile(`sourceSpaceID\s*:=\s*identity\.SourceSpaceID`),
		body,
		"groupScanJoin 必须使用 resolveScanJoinIdentity 解析出的 source_space_id")

	// C. home 兜底必须是 scaner 的默认 Space — 保留历史行为。
	assert.Regexp(t,
		regexp.MustCompile(`return\s+spacemod\.GetUserDefaultSpaceID\(g\.ctx,\s*scaner\)`),
		body,
		"groupScanJoin 必须以 scaner 的 home Space 作为兜底")
}

// Test 2: resolveScanJoinIdentity 中 header 优先 / home 兜底**必须同时存在**，
// 任何一边缺失都会把 YUJ-199 regression 放回来。
func TestGroupScanJoin_SourceSpaceID_HasBothPriorityAndFallback_YUJ199(t *testing.T) {
	body := mustReadFunc(t, "scanjoin_identity.go", "func resolveScanJoinIdentity(")

	headerIdx := strings.Index(body, "id.SourceSpaceID = headerSpaceID")
	assert.NotEqual(t, -1, headerIdx, "header 赋值缺失 → YUJ-199 回归")
	fallbackIdx := strings.Index(body, "id.SourceSpaceID = defaultSpaceID()")
	assert.NotEqual(t, -1, fallbackIdx, "home 兜底缺失 → 跨 Space scaner 没有 header 时行为漂移")
}

// Test 3: addMembersTxWithSpace（邀请确认路径）必须接受 inviterSpaceID 参数
// 并让它在跨 Space 写 source_space_id 时取得最高优先级。
func TestAddMembersTxWithSpace_InviterSpaceID_HasPriority_YUJ199(t *testing.T) {
	body := mustReadFunc(t, "api.go", "func (g *Group) addMembersTxWithSpace(")

	// A. 函数签名必须带 inviterSpaceID string 形参（放在 tx 之前，稳定参数位置）。
	src, err := os.ReadFile("api.go")
	assert.NoError(t, err)
	assert.Regexp(t,
		regexp.MustCompile(`func \(g \*Group\) addMembersTxWithSpace\(members \[\]string, groupNo string, operator, operatorName, inviterSpaceID string, tx \*dbr\.Tx\)`),
		string(src),
		"addMembersTxWithSpace 必须有 inviterSpaceID string 形参")

	// B. 函数体内必须有以 inviterSpaceID 非空为第一分支的三段 switch/if。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)case\s+inviterSpaceID\s*!=\s*""\s*:\s*\n\s*sourceSpaceMap\[uid\]\s*=\s*inviterSpaceID`),
		body,
		"inviterSpaceID 非空必须是最高优先级分支（写入 sourceSpaceMap[uid]）")

	// C. operator 外部成员分支与 home 兜底分支保留，顺序在 inviterSpaceID 之后。
	priIdx := strings.Index(body, "sourceSpaceMap[uid] = inviterSpaceID")
	opIdx := strings.Index(body, "operatorMemberForSpace.SourceSpaceID")
	homeIdx := strings.Index(body, "GetUserDefaultSpaceID(g.ctx, uid)")
	assert.NotEqual(t, -1, priIdx, "inviterSpaceID 写入缺失 → YUJ-199 回归")
	assert.NotEqual(t, -1, opIdx, "operator 外部 source space 兜底缺失 → YUJ-58 回归")
	assert.NotEqual(t, -1, homeIdx, "被邀请者 home Space 兜底缺失 → invite_sure 写空值回归")
	assert.Less(t, priIdx, opIdx, "inviterSpaceID 必须先于 operator 兜底判定")
	assert.Less(t, opIdx, homeIdx, "operator 兜底必须先于被邀请者 home")
}

// Test 4: groupMemberInviteSure 必须把请求 X-Space-ID header 透传给
// addMembersTxWithSpace，而不是调用不带 inviterSpaceID 的旧入口。
func TestGroupMemberInviteSure_PassesXSpaceIDHeader_YUJ199(t *testing.T) {
	body := mustReadFunc(t, "invite.go", "func (g *Group) groupMemberInviteSure(")

	// A. 必须从 c.GetHeader 读 X-Space-ID 并 TrimSpace。
	assert.Regexp(t,
		regexp.MustCompile(`inviterSpaceID\s*:=\s*strings\.TrimSpace\(c\.GetHeader\(\s*"X-Space-ID"\s*\)\)`),
		body,
		"groupMemberInviteSure 必须从 X-Space-ID header 读当前 Space 并 TrimSpace")

	// B. 必须调用 addMembersTxWithSpace(..., inviterSpaceID, tx)（不再是旧的 addMembersTx）。
	assert.Regexp(t,
		regexp.MustCompile(`g\.addMembersTxWithSpace\([^)]*inviterSpaceID[^)]*\)`),
		body,
		"groupMemberInviteSure 必须透传 inviterSpaceID 给 addMembersTxWithSpace，不能调旧 addMembersTx")

	// C. 禁止 groupMemberInviteSure 直接调 addMembersTx（会漏 X-Space-ID）。
	assert.NotRegexp(t,
		regexp.MustCompile(`g\.addMembersTx\([^WithSpace]`),
		body,
		"groupMemberInviteSure 不得调 addMembersTx（无 inviterSpaceID 参数版本）")
}

// Test 5: H5 两个公开落地页（join_group.html / group_invite.html）的
// scanjoin 调用必须带 X-Space-ID header（localStorage.currentSpaceId）。
func TestH5ScanJoin_SendsXSpaceIDHeader_YUJ199(t *testing.T) {
	// Case A: assets/web/join_group.html（$.getJSON → $.ajax + headers）。
	joinSrc, err := os.ReadFile("../../assets/web/join_group.html")
	assert.NoError(t, err)
	joinText := string(joinSrc)

	assert.Contains(t, joinText, "localStorage.getItem('currentSpaceId')",
		"join_group.html 必须从 localStorage.currentSpaceId 读当前 Space")
	assert.Regexp(t,
		regexp.MustCompile(`'X-Space-ID'\s*\]\s*=\s*currentSpaceId`),
		joinText,
		"join_group.html 必须把 currentSpaceId 写入 X-Space-ID header")
	// scanjoin 调用必须带 headers（不能是裸 $.getJSON，那个不能发自定义 header）。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)\$\.ajax\(.*?scanjoin.*?headers\s*:\s*joinHeaders`),
		joinText,
		"join_group.html scanjoin 必须走 $.ajax 并挂 headers: joinHeaders（$.getJSON 不支持自定义 header）")

	// Case B: assets/web/group_invite.html（fetch headers 里追加 X-Space-ID）。
	inviteSrc, err := os.ReadFile("../../assets/web/group_invite.html")
	assert.NoError(t, err)
	inviteText := string(inviteSrc)

	assert.Contains(t, inviteText, "localStorage.getItem('currentSpaceId')",
		"group_invite.html 必须从 localStorage.currentSpaceId 读当前 Space")
	assert.Regexp(t,
		regexp.MustCompile(`'X-Space-ID'\s*\]\s*=\s*currentSpaceId`),
		inviteText,
		"group_invite.html 必须把 currentSpaceId 写入 X-Space-ID header")
	// 原有 token 不能丢（登录凭据），跟新增 X-Space-ID 同用一个 headers 对象。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)scanHeaders\s*=\s*\{\s*"token"\s*:\s*token\s*\}`),
		inviteText,
		"group_invite.html 必须保留 token header，不能因加 X-Space-ID 把登录凭据弄丢")
	assert.Regexp(t,
		regexp.MustCompile(`(?s)scanjoin.*?headers:\s*scanHeaders`),
		inviteText,
		"group_invite.html scanjoin fetch 必须使用携带 X-Space-ID 的 scanHeaders 对象")
}

// mustReadFunc 读取 filename 并截取以 prefix 开头的函数体到下一个
// `func (g *Group) ` 开头之前，作为 grep 作用域。若任意一步失败会 t.Fatal。
func mustReadFunc(t *testing.T, filename, prefix string) string {
	t.Helper()
	src, err := os.ReadFile(filename)
	assert.NoError(t, err)
	text := string(src)
	start := strings.Index(text, prefix)
	if start == -1 {
		t.Fatalf("函数 %q 不存在于 %s（是否被误删 / 被 rename）", prefix, filename)
	}
	rest := text[start:]
	nextFuncOffset := strings.Index(rest[1:], "func (g *Group) ")
	if nextFuncOffset == -1 {
		// 文件末尾函数：整段返回。
		return rest
	}
	return rest[:nextFuncOffset+1]
}
