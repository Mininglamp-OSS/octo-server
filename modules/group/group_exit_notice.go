package group

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
)

// groupExitNoticeContent 是「某成员退出群聊」系统提示的文案模板，逐字节复刻
// octo-lib `config.(*Context).SendGroupExit` 原有的 content，客户端据此渲染。
//
// 注意：`{0}` 两侧的引号**都是** U+201C（LEFT DOUBLE QUOTATION MARK，UTF-8
// e2 80 9c），第二个并不是 U+201D。这是 octo-lib 里的既有笔误
// （config/msg_group.go:357）。这里刻意原样保留 —— 本任务只修「不可见却计未读」
// 这一缺陷，不改渲染文案；把第二个引号「修正」成 ”会让三端已经在渲染的字符串
// 发生可见变化，属于本任务范围外的改动。要改请另开任务，并同步 octo-lib。
const groupExitNoticeContent = "“{0}“退出群聊"

// exitShowNameFallback 是两个调用点共用的兜底展示名。
//
// 绝不能兜到裸 uid：提示现在全员可见且是持久历史，后来入群的人也读得到，
// 把内部 UID 永久写进群历史等于泄露一个不该出现在聊天记录里的标识符
// （PR #807 review）。宁可显示一个中性词。
const exitShowNameFallback = "该成员"

// resolveExitShowName 统一两个调用点的展示名口径：群内备注优先 → 全局用户名 →
// 中性兜底。groupExit 与 sendGroupExitTip 是同一个产品事件，此前一个用
// group_member.Remark、另一个用 user.Name，同一件事在两条路径上会写出不同的名字。
//
// globalName 用闭包传入而不是直接取值：备注命中时就不该为了兜底再打一次 DB。
func resolveExitShowName(groupRemark string, globalName func() string) string {
	if groupRemark != "" {
		return groupRemark
	}
	if globalName != nil {
		if n := globalName(); n != "" {
			return n
		}
	}
	return exitShowNameFallback
}

// sendGroupExitNotice 发送「某成员自助退出群聊」的系统提示。
//
// 与 octo-lib `SendGroupExit` 的差异（这正是本 helper 存在的理由）：
//
//	octo-lib: Header{RedDot: 1}，payload 带 `visibles`（只给一位管理员可见）
//	本 helper: Header{RedDot: 0}，payload **不带** `visibles`（全员可见）
//
// 为什么必须去掉 `visibles`：IM 的未读是纯游标减法
// （unread = latest_msg_seq − read_seq），与 `red_dot`、`visibles` 都无关。
// `visibles` 挡得住消息的**内容**（octo-server `from()` 会对未命中白名单的用户把
// 该条置 is_deleted=1），却挡不住它推高频道的 committed seq。于是非管理员既看不到
// 这条气泡、又永远没法把 read 游标推过它 —— 未读卡死、红点消不掉。
// 让它全员可见 ⇒ 可读 ⇒ 可正常清除。RedDot: 0 则让它不实时点亮红点，
// 与 bot 级联移除 Tip（sendBotCascadeRemovedTip）保持同一套语义。
//
// 可见性收敛（space-isolation / acl）：消息只投递到 groupNo 这个 group channel，
// 订阅者就是群成员本身 —— 群成员资格即边界，不跨 Space、不外溢。
//
// 受众分两层，必须分开论证（PR #807 review 指出原论证只覆盖了第一层）：
//
//  1. **当时在群的成员**：成员变更本就通过 CMDGroupMemberUpdate 广播给全群、
//     成员列表随之更新，"某人退群"对他们并非新信息，全员可见不增信息面。
//
//  2. **此后才入群的人**：这一条是真正的扩大。消息是持久的（NoPersist:0 /
//     SyncOnce:0），而入群即获得完整历史，所以半年后加入的人会读到一条他既没
//     见过成员列表变化、也没收到过 CMD 的退群记录。`visibles` 此前是永久挡住
//     这一层的。
//
// 第 2 层是有意接受的，理由有二：同仓的 bot 级联移除 Tip
// （sendBotCascadeRemovedTip）本就是全员可见的持久消息，已经建立了这个先例；
// Slack / Discord 的频道离开事件同样对后来加入者可见。代价是展示名会永久留在
// 群历史里，因此展示名口径收紧为"群内备注 → 全局名 → 中性兜底"，绝不落到裸 UID
// （见 resolveExitShowName）。
//
// 不可信输入（trust-boundary）：showName 来自成员备注 / 登录名，是用户可控的。
// 这里**不做**任何字符串插值 —— content 保持模板原样，名字放进结构化的
// `extra[].name`，由客户端按 `{0}` 占位符填充。因此不存在把不可信文本拼进
// 渲染敏感串的路径。
//
// 返回 error 供调用方按各自策略处理；两个调用点都是 best-effort（只记日志，
// 不让提示发不出去影响退群/清理本身的成败）。
func sendGroupExitNotice(ctx *config.Context, groupNo, uid, showName string) error {
	return ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    0,
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": groupExitNoticeContent,
			"type":    common.GroupMemberQuit,
			"extra": []config.UserBaseVo{
				{
					UID:  uid,
					Name: showName,
				},
			},
		})),
	})
}
