package group

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentExitNotices 从录到的 /message/send 里挑出「退出群聊」那条（可能有多条系统
// 消息同批发出，例如 bot 级联 Tip），返回 (header, payload) 已解码的对子。
//
// payload 在 MsgSendReq 里是 []byte，JSON 编码后是 base64 —— 与 payloadsContain
// 的解码方式保持一致。
func sentExitNotices(t *testing.T, payloads []map[string]interface{}) []struct {
	Header  map[string]interface{}
	Payload map[string]interface{}
} {
	t.Helper()
	var out []struct {
		Header  map[string]interface{}
		Payload map[string]interface{}
	}
	for _, p := range payloads {
		raw, ok := p["payload"]
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(decoded, &payload); err != nil {
			continue
		}
		// GroupMemberQuit(1021) 才是退群提示；同批的 bot 级联 Tip 是 Tip(2000)。
		typ, ok := payload["type"].(float64)
		if !ok || int(typ) != 1021 {
			continue
		}
		header, _ := p["header"].(map[string]interface{})
		out = append(out, struct {
			Header  map[string]interface{}
			Payload map[string]interface{}
		}{Header: header, Payload: payload})
	}
	return out
}

// TestGroupExitNoticeIsEveryoneVisibleAndSilent 钉住本次修复的核心 wire 形状。
//
// 缺陷是「不可见却计未读」：IM 的未读是纯游标减法
// （unread = latest_msg_seq − read_seq），与 red_dot / visibles 都无关。payload 里
// 的 `visibles` 只挡得住内容（octo-server from() 把未命中的用户那条置 is_deleted=1），
// 挡不住它推高频道 committed seq —— 非管理员既看不到气泡、又永远没法把 read 游标
// 推过它，红点就永久卡住。
//
// 所以这里的两条断言各自独立、缺一不可：
//   - **没有 visibles** 才真正修好未读（消息可见 ⇒ 可读 ⇒ 可清）；
//   - red_dot=0 只是附带的「不打扰」，单独改它对未读毫无作用。
//
// 同时锁住渲染契约（type=1021 / content 模板 / extra），三端按它渲染系统气泡。
func TestGroupExitNoticeIsEveryoneVisibleAndSilent(t *testing.T) {
	ctx, _ := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)

	require.NoError(t, sendGroupExitNotice(ctx, "g-notice", "u-leaver", "张三"))

	notices := sentExitNotices(t, stub.sentPayloads())
	require.Len(t, notices, 1, "应发出且只发出一条退群提示")
	notice := notices[0]

	// 1) 全员可见：payload 不得带 visibles 白名单。这一条才是未读修复本身。
	_, hasVisibles := notice.Payload["visibles"]
	assert.False(t, hasVisibles,
		"退群提示不得带 visibles —— 带了就会「非管理员看不到内容却仍被计未读」")

	// 2) header 三元组一次钉死，不能只钉 red_dot（PR #807 review nit）。
	//
	// no_persist / sync_once 是**承重项**，不是陪衬：本次可见性论证明确接受了
	// "后来入群的人也读得到这条退群记录"，而那正建立在"消息持久、入群即得完整
	// 历史"之上。谁把 NoPersist 翻成 1，这条契约就静默漂移了，而只断言 red_dot
	// 的测试照样绿。
	require.NotNil(t, notice.Header, "header 必须存在")
	assert.EqualValues(t, 0, notice.Header["red_dot"], "退群提示不应点亮红点")
	assert.EqualValues(t, 0, notice.Header["no_persist"],
		"必须持久化 —— 可见性论证接受了『后来入群者也读得到』，靠的就是这一条")
	assert.EqualValues(t, 0, notice.Header["sync_once"],
		"不得走 sync_once —— 那会被改写进 command channel，变成一次性提示")

	// 3) 渲染契约：type / content 模板 / extra 结构一律不变。
	assert.EqualValues(t, 1021, notice.Payload["type"], "必须是 GroupMemberQuit(1021)")
	// 独立字面量，**不能**拿 groupExitNoticeContent 自比 —— 那样常量被改动时
	// 测试照样绿，测不出对 octo-lib 既有 wire 文案的漂移（PR #807 review）。
	// 用 \u 转义把两个引号钉死成 U+201C：第二个**不是** U+201D，那是 octo-lib
	// config/msg_group.go 里的既有笔误，三端已按它渲染，不能"顺手修正"。
	const wantContent = "\u201c{0}\u201c退出群聊"
	assert.Equal(t, wantContent, notice.Payload["content"],
		"content 必须与 octo-lib 原文案逐字节一致（含 {0} 两侧的 U+201C 笔误）")
	assert.Equal(t, wantContent, groupExitNoticeContent,
		"生产常量本身也不得漂移 —— 它就是客户端渲染依赖的那个字符串")

	extra, ok := notice.Payload["extra"].([]interface{})
	require.True(t, ok, "extra 必须是数组")
	require.Len(t, extra, 1)
	first, ok := extra[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "u-leaver", first["uid"])
	assert.Equal(t, "张三", first["name"],
		"名字走结构化 extra，不插值进 content —— 不可信文本不进渲染敏感串")
}

// TestGroupExitTipSentWhenNoOtherAdmin 覆盖本次去掉的隐性门槛。
//
// 改动前 sendGroupExitTip 要先查管理员挑 visibles，挑不到（群里没有其他
// 管理员/群主）就直接 return，提示被静默吞掉。可见性白名单去掉后该门槛失去
// 意义 —— 这里造一个成员全是普通角色、压根没有管理员的群，退群提示仍须发出。
func TestGroupExitTipSentWhenNoOtherAdmin(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)
	const spaceID, leaver = "sp-noadmin", "u-leaver"

	// 群里只有两名普通成员，没有 creator / manager 的 group_member 行 ——
	// QueryGroupManagerOrCreatorUIDS 返回空，正是老代码静默跳过的场景。
	seedGroupInSpace(t, ctx, "g-noadmin", spaceID, "u-absent-creator")
	seedGroupMember(t, ctx, "g-noadmin", "u-other", MemberRoleCommon)
	seedGroupMember(t, ctx, "g-noadmin", leaver, MemberRoleCommon)

	require.NoError(t, g.cleanupSpaceMemberGroups(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: leaver,
		OperatorUID: leaver, // 自助退出：操作者就是本人
		Reason:      spacemod.MemberRemoveReasonLeft,
	}))

	_, stillIn := liveMemberRole(t, ctx, "g-noadmin", leaver)
	assert.False(t, stillIn, "清理本身照做")

	notices := sentExitNotices(t, stub.sentPayloads())
	require.Len(t, notices, 1,
		"群里没有其他管理员时，退群提示同样要发（改动前会被静默吞掉）")
	_, hasVisibles := notices[0].Payload["visibles"]
	assert.False(t, hasVisibles, "退群提示不得带 visibles")
	assert.EqualValues(t, 0, notices[0].Header["red_dot"], "退群提示不应点亮红点")
}

// TestResolveExitShowName 钉住两个调用点共用的展示名口径。
//
// 关键那条是最后一个 case：兜底**绝不能**是裸 uid。提示现在全员可见且持久，
// 后来入群的人也读得到——把内部 UID 写进群历史等于永久泄露一个不该出现在
// 聊天记录里的标识符（PR #807 review）。
func TestResolveExitShowName(t *testing.T) {
	cases := []struct {
		name       string
		remark     string
		globalName string
		want       string
	}{
		{"群内备注优先", "群里的老王", "王五", "群里的老王"},
		{"无备注则用全局名", "", "王五", "王五"},
		{"两者皆空走中性兜底", "", "", exitShowNameFallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveExitShowName(c.remark, func() string { return c.globalName })
			assert.Equal(t, c.want, got)
			assert.NotEqual(t, "u-leaver", got, "兜底绝不能是裸 UID")
		})
	}

	// globalName 为 nil（调用方没有第二来源）时同样不能 panic、不能吐裸值。
	assert.Equal(t, exitShowNameFallback, resolveExitShowName("", nil))

	// 备注命中时不该为了兜底再打一次 DB —— 闭包必须没被调用。
	called := false
	got := resolveExitShowName("有备注", func() string { called = true; return "不该用到" })
	assert.Equal(t, "有备注", got)
	assert.False(t, called, "备注命中时不应触发全局用户名查询")
}

// TestGroupExitNoticeSanitizesShowName 退群提示的 extra 名字同样要过净化。
//
// 与交接通告是同一条渲染面：showName 来自成员可自设的 group_member.remark，
// 而客户端逐次替换 `{N}` 并重新扫描替换后的文本。#807 把这条提示改成了**全员可见
// 且持久**，后来入群的人也读得到，所以一条被构造过的名字会永久留在群历史里。
//
// 净化函数本身由 TestSanitizeSystemMessageName 覆盖；这条钉的是**发送点调用了它**
// —— 漏掉调用是这类修复最常见的失败形态，而单元测试看不见。
func TestGroupExitNoticeSanitizesShowName(t *testing.T) {
	ctx, _ := cascadeSetup(t)
	stub := newGroupIMStub(t, ctx)

	require.NoError(t, sendGroupExitNotice(ctx, "g-sanitize-exit", "u-leaver", "张三{0}伪造‮反转"))

	notices := sentExitNotices(t, stub.sentPayloads())
	require.Len(t, notices, 1, "应当发出一条退群提示")
	extra, ok := notices[0].Payload["extra"].([]interface{})
	require.True(t, ok)
	require.Len(t, extra, 1)
	name := fmt.Sprint(extra[0].(map[string]interface{})["name"])

	assert.NotContains(t, name, "{", "不得残留半角左花括号：%q", name)
	assert.NotContains(t, name, "}", "不得残留半角右花括号：%q", name)
	assert.NotContains(t, name, "‮", "不得残留 RLO：%q", name)
	assert.Contains(t, name, "张三", "净化不能把名字吃光")
}
