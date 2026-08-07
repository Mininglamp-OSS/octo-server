---
type: Task
title: "Task: robot-stream-authz-symmetry"
description: Bind stream/end to its caller and settle the group-vs-subarea membership predicate asymmetry in allowSendToChannel.
tags: ["robot", "auth", "stream", "group"]
timestamp: 2026-08-07T02:04:20+00:00
# --- octospec extension fields ---
slug: robot-stream-authz-symmetry
upstream: "Mininglamp-OSS/octo-server#706 round-8/9 评审的范围外发现"
source: review
---

# Task: robot-stream-authz-symmetry

## Goal

补上 `POST /v1/robots/:robot_id/:app_key/stream/end` 缺失的调用方绑定，并就
`allowSendToChannel` 群分支与子区分支的成员谓词不对称给出**明确决定**（收紧或保留），
让 `robotAuth` 组下四个端点的鉴权口径一致且有据可依。

不是新功能。两条都是 #706 期间被评审发现、当时判定为范围外而留下的既有缺口。

## Background

#706 把 `stream/start` 对齐到了同组的 `sendMessage` / `typing`：钉住 `FromUID`、调用
`allowSendToChannel`、拒绝卡片 payload。评审在确认这些之后指出，同一组里还剩两处不对称，
且都不该由那个「per-Bot 配置存储」任务顺手改掉。

**一、`stream/end` 没有任何调用方绑定。**
`config.MessageStreamEndReq` 是 `{stream_no, channel_id, channel_type}`——**不携带发送方
身份**。handler 只跑解散守卫，然后 `rb.ctx.IMStreamEnd(req)` 用管理端令牌发给 WuKongIM。
没有 `allowSendToChannel`，也没有任何东西把 `stream_no` 与调用它的 Bot 关联起来。

失败场景：Bot A 与 Bot B 同在频道 C。A 从收到的流式消息里观察到 B 的 `stream_no`，用
自己已鉴权的 `stream/end` 传 B 的 `stream_no` + C，终止 B 正在进行的流。对同频道内其它
Bot 的拒绝服务。

评审里对 `streamEnd` 有一条**正确但不完整**的结论需要在此澄清：它确实无法夹带卡片
（没有 payload 字段），#706 的 journal 也据此驳回过一个自动化误报——**但那只回答了卡片
问题，不构成对该端点归属问题的背书**。本任务处理的正是后者。

**二、群分支与子区分支用不同的成员谓词。**
`allowSendToChannel` 现在是：

| 频道类型 | 谓词 | 语义 |
|---|---|---|
| Person | 直接放行 | — |
| Group | `ExistMember` | 仅 `is_deleted=0`，**被拉黑成员通过** |
| CommunityTopic | `ExistMemberActive` | `is_deleted=0 AND status=Normal` |

子区那条是 #706 round-8 的 P1 修复（`group/service.go` 上 `ExistMemberActive` 的注释点名
了子区场景，YUJ-4185）。群那条是既有代码，评审判定收紧它是「更大范围的行为变更」，
建议单独决定。

于是当前语义是：**被拉黑的 Bot 能往父群发，但发不了该群的子区。** 这个不对称与全仓
既有口径一致（子区门禁在 YUJ-4185 被专门加固，群门禁没有），#706 已在调用点写明它是
刻意的——但评审明确说「it deserves a decision rather than inheritance」。

## Load-bearing list

- **`allowSendToChannel` 是三条 robot ingress 共用的唯一频道判定**（`sendMessage`、
  `typing`、`stream/start`）。改它 = 同时改三条路。#706 已经因为在这里补一个 case 而
  连带给 `sendMessage`/`typing` 加上了子区支持。
- **`ExistMember` 与 `ExistMemberActive` 的选择是安全边界**，不是风格。robot ingress 是
  服务端直发、不经 IM datasource，因此拿不到 thread 模块的子区拉黑继承
  （`thread/1module.go`），本地这道门是唯一防线——`group/db.go` 里「用于绕过 IM 层的
  接口」那句话指的就是这类调用方。收紧群分支会让被拉黑的 Bot 立即无法往父群发消息。
- **`stream_no` 由服务端签发**（`IMStreamStart` 返回），因此「绑定调用方」需要一处记录
  或一次校验；本仓目前没有 stream 归属表，方案选择本身是本任务的主要设计工作。
- **`streamEnd` 的解散守卫**在 #706 后已可达（`allowSendToChannel` 认了 type 5 之前那些
  子区分支是死代码）。改动不得让它重新失效。
- **错误码口径**：本 ingress 既有的单一泛化拒绝码（防枚举），拒绝原因只进日志。新增拒绝
  必须沿用 `ErrRobotChannelSendForbidden` 或既有码，不得新造一个会泄露原因的码。

## Out of scope

- **`modules/robot` 之外的入口。** `bot_api`、`message`、`notify` 的鉴权不在本任务。
- **字符串 `type` 在 `bot_api/sendMessage` 与 `message` ingress 上仍绕过卡片谓词**——
  同族问题，但跨三个模块、且严重度取决于「有没有客户端会把 `type` 强转」这个仓外事实，
  单独立项。
- **per-Bot 卡片开关与 `bot_setting` 存储本身**（#706 已合并），以及它的 owner 契约问题
  （见 `bot-setting-owner-contract`）。
- **子区目标的存在性/已删除校验**。`message/api.go` 同样没有，属全仓缺口而非本任务引入。
- **App Bot 的鉴权模型**。

## Acceptance

- `stream/end` 对不属于调用方的 `stream_no` 拒绝，且拒绝码与本 ingress 既有口径一致；
  测试构造「Bot A 传 Bot B 的 stream_no」并断言被拒。
- `stream/end` 对**属于**调用方的 `stream_no` 仍然成功，且既有的解散守卫行为不变；
  两个方向都有测试，缺任一条就无法区分「有校验」与「一律拒绝」。
- 群分支谓词的决定被**写进代码注释与本 brief**，无论结论是收紧还是保留：
  - 若收紧为 `ExistMemberActive`：新增测试覆盖「被拉黑成员发父群被拒」，并在 brief 与
    journal 记录这是对既有行为的收紧、影响面为「被拉黑 Bot 立即无法发父群」。
  - 若保留 `ExistMember`：调用点注释说明为什么群比子区松是刻意的，并加一条测试把这个
    不对称钉住（`member=true, activeMember=false` 时群放行、子区拒绝），使其成为契约
    而非巧合。
- `go test -race -shuffle=on ./modules/robot/ ./modules/group/` 通过。
- `go vet ./...`、`make i18n-extract-check`、`make i18n-lint`、
  `TestRobotNoLegacyResponseError` 通过。
- 每条修复用「只回退该修复、确认其测试失败」验证过。

## Open questions（需人确认，决定方案）

1. **`stream_no` 与 Bot 的绑定放在哪？** 候选：(a) 发起时写一张轻量归属表 / Redis 键，
   `stream/end` 查它；(b) 让 `IMStreamStart` 返回的 `stream_no` 携带可校验的签名；
   (c) 向 WuKongIM 查询该 stream 的 `from_uid`。各自的成本与 IM 侧支持情况需要确认，
   这决定本任务是小改还是需要迁移。
2. **群分支要不要收紧？** 收紧是安全改进，但会立刻改变线上行为（被拉黑 Bot 发父群从
   可发变为被拒）。需要产品/运维确认这是期望语义。
