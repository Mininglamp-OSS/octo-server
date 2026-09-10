---
type: Task
title: "Task: group-exit-notice-visibility"
description: 群成员自助退出的系统消息改为「全员可见 + RedDot:0」（对齐 bot 级联 Tip），修复非管理员被 visibles 白名单挡住内容却仍被推高未读、红点消不掉的幽灵未读。改动全部落在 octo-server 侧，不改 octo-lib 的 SendGroupExit——两处调用点改为调用 octo-server 本地 helper 直接经 ctx.SendMessage 发送。不含跨端已读同步（问题二，独立任务）。
tags: ["wire-contract", "acl", "test", "commit"]
timestamp: 2026-08-24T06:10:36Z
# --- octospec extension fields ---
slug: group-exit-notice-visibility
upstream: Mininglamp-OSS/octo-server#group-member-exit-message-visibility
source: self
---

# Task: group-exit-notice-visibility

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

把**群成员自助退出**的系统消息，从当前的「`visibles` 白名单只给一位管理员看 +
`RedDot:1`」，改成「**全员可见 + `RedDot:0`**」，与已有的 bot 级联移除 Tip
（`modules/group/bot_cascade.go:179-191`，`RedDot:0`、无 `visibles`、人人可见）
**同一套语义**。

**要修的现象**：普通成员看不到「X 退出群聊」这条消息（`visibles` 把内容挡在
管理员之外），但这条消息仍然给每个成员**推高了一格未读 / 红点，且消不掉**。

**根因（已核到底，纯读代码）**：`visibles` 挡的是**内容**，挡不住 channel 的
**seq**。octo-im 的未读是纯游标减法，与 `red_dot`、`visibles` **都无关**：

- `octo-im internal/app/build.go:1544-1577` — `loadLatestConversationMessage`
  直接取 `CommittedSeq` 那条，不看 `red_dot`、不看 `visibles`。
- `octo-im internal/usecase/conversation/sync.go:155-158` — `unread =
  latest.MessageSeq − max(read_seq, delete_seq)`。
- `octo-im` 全仓 conversation/unread 逻辑**没有任何一处分支于 `RedDot`**。
- `octo-server modules/message/api_conversation.go:1602` — `Unread: resp.Unread`
  **原样透传** IM 的未读数；`from()` 的 `visibles` 过滤（`api.go:3839-3846`）只把
  recents 的**内容**置 `is_deleted=1`，**不改未读数**。

于是：这条退群消息作为**持久频道消息**推高了群的共享 `committed seq` → 所有还没
读到它的成员 `unread = latest − read = 1`；但内容被 `visibles` 锁给管理员 → 非管理员
永远看不到这个气泡 → read 游标永远追不到这条 seq → 未读卡死。

**为什么改 `RedDot` 一个字段不够、必须同时去掉 `visibles`**：单把 `RedDot:1→0`
是无效的（未读与 red_dot 无关，见上）。要真正消除幽灵未读，就必须让这条消息**可读**
——即去掉 `visibles`，让全员能看到、能读、能正常清掉。`RedDot:0` 是附带项：让它
不再实时点亮红点，与 bot 级联 Tip 一致。两者一起改，才等价于「一条人人可见、不打扰
badge、可被正常已读」的系统提示。

**为什么落在 octo-server 侧**：真正发消息的 `SendGroupExit` 在
`octo-lib config/msg_group.go:348`（当前仓库只读，不改）。octo-server 有两处调用它
（见 Load-bearing）。本任务在 `modules/group` 内新增一个本地 helper，直接经
`ctx.SendMessage`（与 `bot_cascade.go` 同一路径）构造并发送退群提示，两处调用点改为
调用该 helper，`octo-lib.SendGroupExit` 保持原样、只是不再被这两处调用。

## Background

- 复现自生产（截图）：某群聊对普通成员显示一条「"{退群者}"退出群聊」的未读，
  `payload` 里 `type:1021`、`red_dot:1`、`visibles:["…"]`、`is_deleted:1`，未读无法消除。
  （群名与退群者姓名此处脱敏 —— 本仓库是公开仓，生产复现细节不落公开产物。）
- 三家 IM（Slack `last_read` / Discord `read_state` / Feishu 已读游标）的通行做法是
  「服务端只存单调已读游标、未读客户端 derive」。本任务不动这套架构，只消除
  「不可见却计未读」这一具体缺陷（最小改动，对齐仓内既有的 bot 级联 Tip 先例）。
- 跨端已读同步（`unreadClear` CMD 的 `NoPersist:true` 离线丢失、`readed_to_msg_seq`
  中间层丢弃、iOS `MAX` 本地优先）是**另一个独立问题**，见 Out of scope。

## Load-bearing list

- **调用点 1 — 主动退群 handler**：`modules/group/api.go:3608-3614`。当前
  `if groupInfo.Status != GroupStatusDisband && len(visiblesUids) > 0 { SendGroupExit(...) }`。
  其中 `visiblesUids`（`api.go:3445-3453`）挑一位「非退群者的管理员/群主」。改为全员
  可见后，`len(visiblesUids) > 0` 这个**发送门槛作废** —— 需改成仅按解散态判定，否则
  「群里已无其他管理员」时不再发提示（行为回退）。`visiblesUids` 的计算若不再被别处
  使用则一并移除。`acl`/`wire-contract`
- **调用点 2 — Space 清理路径的自助退群 Tip**：`modules/group/space_member_removal.go:198-221`
  `sendGroupExitTip`。当前查管理员、`visibles` 只给一位、无管理员则 `return` 不发。
  改为全员可见后：不再需要 `QueryGroupManagerOrCreatorUIDS` 挑 `visibles`，且**无管理员
  时也应照发**。`acl`/`wire-contract`
- **消息 payload 契约（客户端渲染依赖）**：三端按 `type == GroupMemberQuit(1021)`
  （octo-lib `common/msg.go:80`）渲染「"{0}"退出群聊」系统气泡，`extra:[{uid,name}]`
  提供占位名。**必须保持** `type=1021`、`content="“{0}“退出群聊"`、
  `extra` 结构不变，只去掉 payload 里的 `visibles` 字段、并把 header `RedDot` 置 0。
  `wire-contract`
- **发送路径**：新 helper 经 `ctx.SendMessage(&config.MsgSendReq{...})`，与
  `bot_cascade.go:179` 同一路径与 header 形状（`NoPersist:0, RedDot:0, SyncOnce:0`），
  `ChannelID=groupNo`、`ChannelType=ChannelTypeGroup`。`wire-contract`
- **`visibles` 作为读 ACL 的移除**：`visibles` 非空时 octo-server 的 `from()` /
  `visiblesAllows`（`modules/message/api.go:3839-3846`、`api_message_get.go:41`）把它
  当读白名单强制执行。去掉后退群提示对全体群成员可读。**需论证无信息泄漏**：内容仅
  含退群者名 +「退出群聊」，而成员变更本就通过 `CMDGroupMemberUpdate`（`api.go:3591`）
  广播给全群、成员列表随之更新，全员可见此事实不引入新信息面。`acl`
- **既有测试断言**（`test`）：
  - `modules/group/space_member_removal_test.go:307-312`：自助退出必须出现「退出群聊」、
    不得出现「移除群聊」文案 —— **改后仍须成立**（文案不变）。
  - `space_member_removal_test.go:334`：解散场景不得逐成员发「移除群聊」 —— 不受影响。
  - `space_member_removal_test.go:354`：正常踢人仍发「移除群聊」 —— 不受影响（本任务
    不碰 `SendGroupMemberBeRemove`）。
  - `modules/group/api_test.go:689` `TestGroupExit` —— 主动退群主流程须仍绿。
  - IM stub 拦 `/message/send` 原始 body（`space_member_removal_test.go:53-62`），
    `red_dot` 与 payload 内 `visibles` 均可断言。

## Out of scope

- **不改 `octo-lib config/msg_group.go` 的 `SendGroupExit`**：函数保留原样，仅这两处
  调用点不再调它。其它仓/其它调用方（若有）行为不变。
- **不改 octo-im 的未读计算**（`sync.go` / `build.go`）。
- **不碰被踢/被移除提示** `SendGroupMemberBeRemove`（「你被{0}移除群聊」）—— 仍保持
  现有可见性语义。
- **不碰 bot 级联移除 Tip** `sendBotCascadeRemovedTip`（`bot_cascade.go`）—— 已是全员
  可见 + `RedDot:0`，本任务只是向它对齐。
- **不碰群主退群转让/邀请入群等其它系统消息**。
- **跨端已读同步（问题二）**：`unreadClear` CMD 的 `NoPersist:true`、
  `readed_to_msg_seq` 全链路透出、iOS `WKUnreadStore` 的 `MAX` 本地优先 —— 独立任务，
  不在本次。
- **不改 `conversation/clearUnread`、`setUnread`、`sync` 端点**。

## Acceptance

- **单测（新增/改，`modules/group`）**：捕获自助退群 `/message/send` payload，断言：
  - header `red_dot == 0`；
  - payload **不含** `visibles` 字段（或为空）；
  - payload `type == 1021`（GroupMemberQuit）；
  - `content` 含「退出群聊」，`extra[0]` 含退群者 `uid`/`name`。
- **差分证据（改前必红）**：把 helper 临时回退成改动前语义（`RedDot:1` + `visibles`
  白名单）后，上述两条新测**必须失败**，且失败点正是 `visibles` 与 `red_dot` 两条断言。
  没有这一步，「测试通过」不足以证明它咬得住这个 bug。
- **回归**：`space_member_removal_test.go` 现有三条断言（自助退出出现「退出群聊」且无
  「移除群聊」、解散不逐发、踢人仍发「移除群聊」）全绿。
  > `api_test.go:689 TestGroupExit` **不在**此列：它带着**既有的**
  > `t.Skip("OCTO migration TODO: issues/17")`，本任务不解除。已实测确认解除后会
  > `panic: handlers are already registered for path '/v1/group/create'` —— `api_test.go`
  > 里整片 HTTP handler 测试（19 处 skip）都卡在该路由重复注册问题上，属 issue #17
  > 范围。修它超出本任务，见「已知覆盖缺口」。
- **行为**：群内已无其他管理员时，主动退群/清理路径**仍会发出**「X 退出群聊」提示
  （去掉 `len(visiblesUids)>0` / 无管理员 return 的门槛后），新增一条测试覆盖此场景。
- **调用面**：`modules/group/api.go` 与 `modules/group/space_member_removal.go` 两处
  **不再调用** `g.ctx.SendGroupExit`（grep 归零），改调 `modules/group` 内新 helper。
- **构建/静态**：`go build ./...` 绿；`go test ./modules/group/...` 绿；
  `golangci-lint run ./modules/group/...` 无新增告警。
- **i18n**：本改动不新增/修改 `pkg/errcode` 错误码（退群提示是系统消息 payload、非
  错误信封），`make i18n-extract-check` / `i18n-lint` 不受影响；content 中文串沿用
  octo-lib 现有文案，与 `bot_cascade.go` 同类（系统消息内容硬编码，非错误码）。

## 已知覆盖缺口

诚实记账 —— 本任务**没有**做到全路径测试覆盖：

| 改动面 | 覆盖状态 |
|---|---|
| 消息 payload 契约（本次真正的修复） | ✅ helper 直测，且已验证改动前必红 |
| 调用点 2 `sendGroupExitTip`（`space_member_removal.go`） | ✅ 端到端覆盖，含「无其他管理员仍发」场景 |
| 调用点 1 `groupExit` handler 的**发送门槛**改动 | ⚠️ **无运行中的测试** |

调用点 1 的 payload 与调用点 2 共用同一个 helper，故 payload 形状已被覆盖；未被覆盖的
是它那处门槛改动本身（`!disband && len(visiblesUids)>0` → `!disband`）。原因是
`api_test.go` 的 HTTP handler 测试整片 skip（issue #17，见上）。当前只有编译期保证 +
人工 grep 确认「被删除的 `QueryGroupManagerOrCreatorUIDS` / `visiblesUids` 计算确实只
服务于 visibles、无其它消费者」。

**补齐前置**：先修 issue #17 的路由重复注册，让 `TestGroupExit` 能跑。独立任务。

## 验证记录（本地集成环境实测）

环境：MySQL 8.0.46 + Redis 7 + WuKongIM `v2.2.4-20260313`（`WK_TOKENAUTHON=false`）。

- 两条新测：改动后 **PASS**。差分证据分两段做（回退均已还原，无 TEMP 残留）：
  1. helper 回退成改动前 payload 语义（`RedDot:1` + `visibles`）→ 两条新测 **双双 FAIL**，
     失败点即 `不得带 visibles` / `不应点亮红点`。
  2. 再把 `sendGroupExitTip` 的「查管理员 + 无管理员则 return」早退门槛整段还原 →
     `TestGroupExitTipSentWhenNoOtherAdmin` 失败于 `"[]" should have 1 item(s), but has 0`，
     证明改动前该提示确实被**静默吞掉**。
     （第 1 段只动 helper payload，覆盖不到这条门槛；补第 2 段才算把门槛也钉住。）
- 已核实 `TestGroupExitTipSentWhenNoOtherAdmin` 真的走「无管理员」路径：
  `seedGroupInSpace` 只插 `group` 表，而 `QueryGroupManagerOrCreatorUIDS` 查的是
  `group_member` 的 creator/manager 行 —— 该用例只种了两个 `MemberRoleCommon`，
  故查询返回空，正是老代码静默跳过的场景。
- **wire-contract 逐字节核实**：`groupExitNoticeContent` 与 octo-lib
  `config/msg_group.go:357` 的 content **完全一致**（`{0}` 两侧均为 U+201C `0x201c`，
  即原有笔误已原样保留），渲染文案零变化。
- `go test ./modules/group/` → **ok 28.255s**（含上述三条既有回归，全 PASS）。
- `go test ./modules/message/` → **ok**（未读透传所在模块，无连带影响）。
