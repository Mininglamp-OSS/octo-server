package group

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// YUJ-201 / GH#1268 — X-Space-ID header membership 校验（纵深防御）。
//
// 背景：YUJ-199 让后端 groupScanJoin / addMembersTxWithSpace 读 X-Space-ID
// header 作为 source_space_id 的首选来源，但没有做 membership 校验。client
// 若伪造一个 scaner / operator 并非成员的 Space ID，后端会直接写进
// group_member.source_space_id。下游 SpaceFilter 会在可见性层拒绝这类
// 不可见的外部群，实际影响较小，但写脏数据是**纵深防御缺口**，本补丁补齐。
//
// 测试风格沿用 api_source_space_header_test.go 的源码 grep 断言：
//   - scanjoin / invite-sure handler 都会触达 ctx.EventBegin，而 testutil
//     目前不初始化 wkevent 子系统（Mininglamp-OSS/octo-server#1184 TODO），任何
//     HTTP 驱动测试会 nil-deref panic。
//   - 在 testutil 升级之前只能用源码 grep 锁住关键实现特征，防回归。
//   - 每个测试都带「不做 X 就回归」的语义说明，便于后续重构。

// Test 1: groupScanJoin 的 X-Space-ID header 必须用 spacepkg.CheckMembership
// 校验是否收 scaner；不可用（非成员 / 校验出错）时通过 Warn 记录并降级，
// 不抛 5xx、不阻断主流程、不把脏数据写 DB。降级后回落 home Space 的行为由
// scanjoin_identity_test.go 覆盖。
func TestGroupScanJoin_XSpaceIDHeaderNotMember_FallsBackAndLogs(t *testing.T) {
	body := mustReadFunc(t, "api.go", "func (g *Group) groupScanJoin(")

	// A. 成员校验必须是 spacepkg.CheckMembership(db, spaceID, scaner)，
	//    参数顺序与 package 其它调用一致，避免悄悄反调走人。
	assert.Regexp(t,
		regexp.MustCompile(`spacepkg\.CheckMembership\(\s*g\.ctx\.DB\(\)\s*,\s*spaceID\s*,\s*scaner\s*\)`),
		body,
		"groupScanJoin 必须用 spacepkg.CheckMembership 校验 Space 是否收 scaner")

	// B. header 不可用时必须走 g.Warn，文案以 "scanjoin X-Space-ID not" 起头，方便线上 grep。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)identity\.HeaderIgnored\s*\{[^}]*g\.Warn\(\s*"scanjoin X-Space-ID not[^"]*"`),
		body,
		"groupScanJoin 必须在 X-Space-ID 不可用时 Warn")

	// C. header 校验出错只能降级，不能当作群 Space 成员校验失败抛 5xx。
	src, err := os.ReadFile("scanjoin_identity.go")
	assert.NoError(t, err)
	assert.Regexp(t,
		regexp.MustCompile(`(?s)headerErr\s*!=\s*nil\s*\|\|\s*!ok\s*\{\s*id\.HeaderIgnored\s*=\s*true`),
		string(src),
		"X-Space-ID 成员校验出错必须降级为忽略 header")
}

// Test 2: X-Space-ID 合法成员路径必须照常使用 header 作为 source_space_id，
// 且只有校验通过后才使用（不能因为加了校验就无脑兜底 home，也不能跳过校验）。
func TestGroupScanJoin_XSpaceIDHeaderValidMember_UsesIt(t *testing.T) {
	body := mustReadFunc(t, "scanjoin_identity.go", "func resolveScanJoinIdentity(")

	checkIdx := strings.Index(body, "isMember(headerSpaceID)")
	validIdx := strings.Index(body, "headerValid = true")
	assignIdx := strings.Index(body, "id.SourceSpaceID = headerSpaceID")
	assert.NotEqual(t, -1, checkIdx, "X-Space-ID 成员校验缺失 → YUJ-201 回归")
	assert.NotEqual(t, -1, validIdx, "header 合法分支缺失 → YUJ-199 回归")
	assert.NotEqual(t, -1, assignIdx, "header 合法 case 的赋值缺失 → YUJ-199 回归")
	assert.Less(t, checkIdx, validIdx, "header 必须先校验成员关系再标记为可用")
	assert.Regexp(t,
		regexp.MustCompile(`(?s)if\s+headerValid\s*\{\s*id\.SourceSpaceID\s*=\s*headerSpaceID`),
		body,
		"只有 headerValid 时才能把 header 写入 source_space_id")
}

// Test 3: addMembersTxWithSpace 的邀请确认路径必须校验 operator 是否是
// inviterSpaceID 的成员；不是则降级清空，让后面 switch 走 operator/home 兜底。
func TestAddMembersTxWithSpace_OperatorNotInClaimedSpace_FallsBack(t *testing.T) {
	body := mustReadFunc(t, "api.go", "func (g *Group) addMembersTxWithSpace(")

	// A. 必须用 spacepkg.CheckMembership(..., operator) 校验 operator 是否
	//    是 inviterSpaceID 的成员。参数顺序与其它调用一致，避免反调。
	assert.Regexp(t,
		regexp.MustCompile(`spacepkg\.CheckMembership\(\s*g\.ctx\.DB\(\)\s*,\s*inviterSpaceID\s*,\s*operator\s*\)`),
		body,
		"addMembersTxWithSpace 必须用 spacepkg.CheckMembership 校验 operator 在 inviterSpaceID 内")

	// B. 校验必须发生在使用 inviterSpaceID 的 switch 之**前**，而且必须提到
	//    使用 inviterSpaceID 的 members 循环之**外**（否则随成员数线性放大
	//    查询次数、或循环里用脏的 inviterSpaceID 写了 sourceSpaceMap）。
	//    使用 inviterSpaceID 的 members 循环是**紧邻 switch 的那个**，
	//    用 LastIndex 精确定位，避免误匹配到 allow_external=0 检查的第一个循环。
	checkIdx := strings.Index(body, "spacepkg.CheckMembership(g.ctx.DB(), inviterSpaceID, operator)")
	switchIdx := strings.Index(body, "case inviterSpaceID != \"\":")
	// 使用 inviterSpaceID 的循环必定出现在 switch 之前，且是最靠近 switch 的循环。
	inviterLoopIdx := -1
	if switchIdx != -1 {
		inviterLoopIdx = strings.LastIndex(body[:switchIdx], "for _, uid := range members {")
	}
	assert.NotEqual(t, -1, checkIdx, "CheckMembership 缺失 → YUJ-201 回归")
	assert.NotEqual(t, -1, switchIdx, "三段 switch 缺失 → YUJ-199 回归")
	assert.NotEqual(t, -1, inviterLoopIdx, "使用 inviterSpaceID 的 members 遍历循环消失 → 更大范围回归")
	assert.Less(t, checkIdx, inviterLoopIdx,
		"CheckMembership 必须在使用 inviterSpaceID 的 for 循环之前完成，避免 N 次重复 space_member 查询")
	assert.Less(t, checkIdx, switchIdx,
		"CheckMembership 必须在 switch 分支之前，保证降级后的空串能被 switch 捕获走兜底")

	// C. 非成员分支必须把 inviterSpaceID 清空（同一作用域的变量）+ Warn，
	//    且 Warn 文案以 "addmembers X-Space-ID not member" 起头，方便运维 grep。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)!\s*inSpace\b[^}]*g\.Warn\(\s*"addmembers X-Space-ID not member[^"]*"[^}]*inviterSpaceID\s*=\s*""`),
		body,
		"addMembersTxWithSpace 非成员分支必须 Warn 并把 inviterSpaceID 清空")

	// D. DB 错误也必须降级（不 return err 阻断邀请），否则 DB 抖动会把整条
	//    群邀请打挂，线上 P0。
	assert.Regexp(t,
		regexp.MustCompile(`(?s)membershipErr\s*!=\s*nil[^}]*g\.Error\(\s*"邀请确认 X-Space-ID[^"]*"[^}]*inviterSpaceID\s*=\s*""`),
		body,
		"addMembersTxWithSpace 的 CheckMembership 出错必须 Error + 降级，不抛 err")

	// E. 这些新断言不能推翻 YUJ-199 已保住的三段 switch 优先级顺序：
	//    inviterSpaceID > operatorMemberForSpace.SourceSpaceID > home。
	priIdx := strings.Index(body, "sourceSpaceMap[uid] = inviterSpaceID")
	opIdx := strings.Index(body, "operatorMemberForSpace.SourceSpaceID")
	homeIdx := strings.Index(body, "GetUserDefaultSpaceID(g.ctx, uid)")
	assert.NotEqual(t, -1, priIdx, "YUJ-199 inviterSpaceID 优先写入分支缺失 → 合并冲突")
	assert.NotEqual(t, -1, opIdx, "YUJ-58 operator 外部兜底分支缺失")
	assert.NotEqual(t, -1, homeIdx, "被邀请者 home 兜底缺失")
	assert.Less(t, priIdx, opIdx, "inviterSpaceID 优先级必须高于 operator 外部兜底")
	assert.Less(t, opIdx, homeIdx, "operator 外部兜底必须高于被邀请者 home")
}

// 防御性正交断言：api.go 必须仍 import spacepkg。grep 级。若未来有人
// 无意中把校验搬到别处、导致 spacepkg 只剩测试引用（甚至被 goimports 清理），
// 该断言会先失败，给 reviewer 一个醒目警报。
func TestAPI_StillImportsSpacepkg_YUJ201(t *testing.T) {
	src, err := os.ReadFile("api.go")
	assert.NoError(t, err)
	assert.Contains(t, string(src), `spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"`,
		"api.go 必须保留 spacepkg import；CheckMembership 的核心路径依赖它")
}
