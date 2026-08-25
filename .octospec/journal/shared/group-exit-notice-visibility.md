---
type: Journal
title: "Journal: group-exit-notice-visibility"
description: 「某成员退出群聊」的系统提示改为全员可见 + RedDot:0。根因不是红点字段——IM 的未读是纯游标减法（latest_msg_seq − read_seq），与 red_dot、visibles 都无关；payload 里的 visibles 挡得住内容却挡不住 seq，于是非管理员看不到气泡、也永远推不动 read 游标，红点永久卡死。只改 red_dot 是无效修复，必须让消息可读。附带发现：可见性白名单还藏着一条早退，群里没有其他管理员时提示被整条静默吞掉。
tags: ["acl", "isolation", "wire-contract", "testing"]
timestamp: 2026-08-24T12:45:00Z
# --- octospec extension fields ---
task: group-exit-notice-visibility
upstream: Mininglamp-OSS/octo-server#group-member-exit-message-visibility
source: self
---

# Journal: group-exit-notice-visibility

## 做了什么

群成员自助退群时发的系统提示（`type=1021` GroupMemberQuit），原先带一个 `visibles`
白名单只给**一位**管理员可见，且 `RedDot:1`。改成**全员可见 + RedDot:0**，与既有的
bot 级联移除 Tip（`sendBotCascadeRemovedTip`）同一套语义。

实现放在 octo-server 侧：新增 `modules/group/group_exit_notice.go` 的
`sendGroupExitNotice`，经 `ctx.SendMessage` 直接发，**不动 octo-lib 的
`SendGroupExit`**（该函数原样保留，只是这两处不再调它）。两处调用点
（`api.go` 的 `groupExit`、`space_member_removal.go` 的 `sendGroupExitTip`）
同步去掉了那次只为挑 `visibles` 目标而存在的管理员查询。

渲染契约逐字节保持：`type=1021`、同一条 content 模板、退群者姓名仍放结构化 `extra`。

## 结构性认知：`visibles` 挡内容，挡不住 seq

这是本次最值得记住的一条，因为它让「改红点字段」这个直觉修复**完全无效**。

WuKongIM（octo-im）的未读是**纯游标减法**：

- `internal/app/build.go` `loadLatestConversationMessage` 直接取频道 `CommittedSeq`
  那一条，**不看 `red_dot`、不看 `visibles`**；
- `internal/usecase/conversation/sync.go` `unread = latest.MessageSeq − max(read_seq, delete_seq)`；
- 全仓 conversation/unread 逻辑里**没有任何一处分支于 `RedDot`**（唯一的分支是
  `SyncOnce`，且只在会话活跃度投影里，不在未读计算里）；
- octo-server `api_conversation.go` 只是 `Unread: resp.Unread` **原样透传**；
  `from()` 的 `visibles` 过滤只把 recents 的**内容**置 `is_deleted=1`，**不改未读数**。

于是这条持久频道消息推高了群的共享 committed seq → 所有还没读到它的成员 unread +1；
但内容被 `visibles` 锁给管理员 → 非管理员永远看不到这个气泡 → read 游标永远追不过
这条 seq → **未读卡死、红点消不掉**。

**推论（对以后所有系统消息都成立）**：在单条共享 channel log 里，
「只给部分人看的持久气泡」和「不给其他人产生未读」**不可兼得**。要安静就别持久
（`SyncOnce` 会被改写进 command channel，不参与未读），要持久就得人人可读。
本次选了后者——`RedDot:0` 只是顺带的「不打扰」，**真正修好未读的是去掉 `visibles`**。

## Gotcha 1：可见性白名单里藏着一条静默早退

`sendGroupExitTip` 原先在挑不到管理员（群里没有其他 creator/manager）时直接
`return`，`groupExit` 那侧则是 `len(visiblesUids) > 0` 的发送门槛。两处都是
「白名单为空 ⇒ 整条提示不发」——**不是**有意的产品规则，只是可见性实现的副作用。
白名单去掉后这两个门槛失去意义，现在只剩「群没解散」这一个条件，无管理员时同样照发。

`groupExit` 那侧还连带去掉了一个**失败即 500 中断整个退群**的错误分支
（`QueryGroupManagerOrCreatorUIDS` 查询失败）——它唯一的用途就是挑一个可见性目标。

## Gotcha 2：差分证据只回退一半 = 没证明

第一轮做「改动前必红」时，只把 helper 的 payload 回退成 `RedDot:1` + `visibles`，
两条新测确实红了——看起来证据齐了。**但那次回退没有还原 `sendGroupExitTip` 的早退
门槛**，所以 `require.Len(notices, 1)` 那条根本没红，等于「无管理员仍照发」这条
门槛**从未被真正钉住**。

补做第二段（把早退门槛整段还原）才跑出 `"[]" should have 1 item(s), but has 0`。

> 教训：一次改动若移除了 **N 个**行为门槛，差分验证就得**逐个**还原。
> 只回退最显眼的那个（payload 形状），会让另外几条断言在"看起来红了"的掩护下
> 从未被验证过。这与 `learnings/pending/mutation-testing-must-be-adversarial.md`
> 是同一族问题的另一个面：那条讲「变异由作者自选会失真」，这条讲
> **「变异只覆盖了改动面的一部分，也同样失真」**。

同时核实了该用例真的走「无管理员」路径：`seedGroupInSpace` 只插 `group` 表，而
`QueryGroupManagerOrCreatorUIDS` 查的是 `group_member` 的 creator/manager 行——
用例只种了两个 `MemberRoleCommon`，查询返回空。

## Gotcha 3：octo-lib 的文案里有个笔误，必须原样抄

content 模板是 `“{0}“退出群聊` —— `{0}` 两侧的引号**都是** U+201C（`0x201c`），
第二个并不是 U+201D。这是 octo-lib `config/msg_group.go` 里的既有笔误。

helper 里刻意逐字节保留，并写了注释挡住"顺手修正"的冲动：三端已经在渲染这个字符串，
改它属于范围外的可见变化，要改得另开任务并同步 octo-lib。Finish 前用脚本做了
字节比对确认两边一致（`0x201c 0x7b 0x30 0x7d 0x201c`）。

## 判断：i18n 源码守卫**不**登记新文件

CLAUDE.md 说新 handler 文件要加进 `TestGroupNoLegacyResponseError` 的文件列表。
查证后判定本次**不该加**：

- 该守卫是显式列表 `{api.go, api_manager.go, invite.go, api_welcome.go}`；
- 直系先例 `bot_cascade.go`（同样 `ctx.SendMessage` 发系统消息、同样非 handler）
  **不在**列表里；
- `group_exit_notice.go` 不接 `wkhttp.Context`，**结构上发不出** HTTP 响应。

按仓内先例不登记才是一致做法。

## 已知覆盖缺口（未解决）

`groupExit` handler 那处**发送门槛**的改动（`!disband && len(visiblesUids)>0`
→ `!disband`）**没有运行中的测试**。payload 与另一调用点共用同一 helper 故已覆盖，
缺的是门槛本身。原因：`api_test.go` 整片 HTTP handler 测试（19 处 `t.Skip`）卡在
issue #17 的路由重复注册——实测解除 skip 会
`panic: handlers are already registered for path '/v1/group/create'`。

补齐前置是先修 issue #17，独立任务。当前只有编译期保证 + grep 确认被删的
`QueryGroupManagerOrCreatorUIDS`/`visiblesUids` 计算确实只服务于 `visibles`
（该 DB 方法本身仍有 6 个其它调用者，未变死代码）。

## 范围外（同源但独立）

排查中确认了**第二个**问题，本任务未动：**跨端已读不同步**（web 读过 app 仍未读，
反之亦然）。根因是另外三处——`unreadClear` CMD 是 `NoPersist:true`（离线端永久丢失）、
`readed_to_msg_seq` 被 octo-lib 的 `SyncUserConversationResp` 丢弃（IM 明明回传了）、
iOS `WKUnreadStore.reconcileServerSnapshot` 默认走 `MAX(local, server)` 本地优先
（结构上不可能接受别端的已读）。独立任务。

## Review 后补的四件事（PR #807）

1. **存量通知不被本次修复补救。** 改的只是此后的发送。已经发出去的那些仍带
   `visibles`，而所有读门禁（`from()`、`visiblesAllows`、全局搜索门）会继续挡住它们
   —— 按本任务自己的根因分析，**那些会话在部署后依然卡着**。受影响用户仍可通过
   既有会话端点手动清除。别把绿部署读成"事故已关闭"，补救（回填 或 明示让用户清）
   要另行立项。

2. **可见性论证补上了"未来加入者"这一层。** 原论证只说"CMDGroupMemberUpdate 已广播
   给全群"，那只覆盖当时在群的人。消息是持久的、入群即得完整历史，所以后来加入的人
   会读到一条他从没收到过 CMD 的退群记录 —— `visibles` 此前是永久挡住这层的。结论
   仍然接受（bot 级联 Tip 已是同样的先例，Slack/Discord 亦然），但代价是展示名永久
   留在群历史里，因此展示名口径收紧（见下）。

3. **展示名统一，且绝不落到裸 UID。** 两个调用点此前一个用 `group_member.Remark`、
   另一个用全局 `user.Name`，同一个产品事件写出不同的名字；而后者在 `QueryByUID`
   失败或名字为空时会兜底成**裸 uid**，等于把内部标识符永久写进全群可读的历史。
   现在共用 `resolveExitShowName`：群内备注 → 全局名 → 中性兜底"该成员"。
   注意 `sendGroupExitTip` 跑在 `RemoveGroupMembers` **之后**，那时
   `QueryMemberWithUID`（`where is_deleted=0`）已经查不到人，所以备注必须由调用方
   在移除前读到的那一行传进来。

4. **"不可兼得"那条规律经实测核验成立。** Review 指出 octo-lib
   `SendGroupMemberBeRemove` 像是反例（持久 + `Subscribers` + `visibles`）。拿实际
   部署的 broker 二进制（`wukongim v2.2.4-20260313`）实测：该形状
   （`channel_id` + `subscribers` + `sync_once:0`）返回 **HTTP 400**，而本任务的形状
   （无 `subscribers`）返回 200。源码侧对得上 —— broker 在 `subscribers` 非空时清空
   `channel_id` 走 request-scoped 路径，而该路径头一句就要求 `SyncOnce`。
   **`Subscribers` 这条路对持久消息根本不可用**，故不构成反例。

   顺带两个超范围发现，应另行立项：
   - `SendGroupMemberBeRemove` 在这个 broker 上**发不出去**（400），「你被 X 移除群聊」
     实际从未投递。仓内 `TestGroupCascadeKickStillNotifies` 是绿的，因为它打的是返回
     `{}` 的 stub，不校验 broker 真实响应 —— 又一个"stub 让真实故障隐身"的例子。
   - broker 请求体**没有 `setting` 字段**，octo-lib 传的
     `Setting{NoUpdateConversation:true}` 被静默丢弃。

5. **测试不再拿生产常量自比。** 原来 `assert.Equal(t, groupExitNoticeContent, ...)`
   是用产物跟同一个常量比，常量被改照样绿。现在钉独立字面量
   `"\u201c{0}\u201c退出群聊"`，并额外断言生产常量本身没漂移。
