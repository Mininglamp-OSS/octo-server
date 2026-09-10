---
type: Learning
title: "Learning: 差分验证必须覆盖改动面的每一个门槛，不只是最显眼的那个"
description: 一次改动移除了两个行为门槛（payload 里的可见性白名单、以及白名单为空时的静默早退）。做「改动前必红」时只回退了 payload，两条测试确实红了——但另一条断言从未被验证过。回退整段早退门槛后才发现它本来就没被钉住。
tags: ["testing", "verification", "review"]
timestamp: 2026-08-24T12:45:00Z
# --- octospec extension fields ---
status: pending
source: self
upstream: Mininglamp-OSS/octo-server#group-member-exit-message-visibility
candidate_rule_id: differential-must-cover-every-gate
---

# 差分验证必须覆盖改动面的每一个门槛，不只是最显眼的那个

## 观察到的事

一个改动同时移除了**两个**行为门槛：

1. 消息 payload 里的 `visibles` 可见性白名单（显眼：wire 形状变了）；
2. 「白名单为空就直接 `return`」的静默早退（不显眼：藏在可见性实现里，
   删掉后行为从"有时不发"变成"总是发"）。

新增了两条测试，然后按惯例做「改动前必红」：把 helper 回退成改动前的 payload
语义（`RedDot:1` + `visibles`），跑测试——**两条都红了**。证据看起来齐了。

但那次回退**只动了 payload**，没有还原早退门槛。于是：

- 断言 `不得带 visibles` / `不应点亮红点` → 红了 ✅
- 断言 `require.Len(notices, 1)`（无管理员时提示仍须发出）→ **没红**

因为提示照发（早退门槛还没被还原回去），Len 自然是 1。**那条断言在"整体看起来
红了"的掩护下，从未被验证过。**

补做第二段回退（把查管理员 + 早退整段还原）后才跑出：

```
Error: "[]" should have 1 item(s), but has 0
Messages: 群里没有其他管理员时，退群提示同样要发（改动前会被静默吞掉）
```

## 为什么会漏

「改动前必红」在心里被当成一个**布尔**：跑一次、看到红、打勾。
但改动面不是布尔——它是**一组**被移除/修改的门槛。红色只证明
「至少有一条断言咬住了你回退的那一部分」，不证明每条断言都有对应的保护对象。

而且越是不显眼的门槛越危险：显眼的（wire 形状）改错了 code review 一眼能看出来；
静默早退这种"删掉后只是变得更常发生"的行为，除了测试没有第二道防线。

## 候选规则

做差分/变异验证时，先**列出这次改动移除或改写的每一个行为门槛**，再逐个还原、
逐个确认有断言变红。

- 一次回退只还原一个门槛，确认**具体是哪条断言**红了；
- 若某个门槛还原后没有任何断言变红 → 该门槛**缺测试**，不是"顺带就覆盖了"；
- 尤其针对「删掉后行为只是变得更宽松」的门槛（早退、`if len(x) > 0` 发送条件、
  静默 `return`）——它们不改 wire 形状，review 看不出来。

与既有的 `mutation-testing-must-be-adversarial` 是同一族问题的另一个面：
那条讲**变异由作者自选会失真**，这条讲**变异只覆盖改动面的一部分也会失真**。

## 附：一条相关规律的反例核验（PR #807 review 要求）

Review 指出本任务顺带记下的另一条规律 ——「单条共享 channel log 里，『只给部分人
看的持久气泡』与『不给其他人产生未读』不可兼得」—— 在仓内似乎有反例：
octo-lib `SendGroupMemberBeRemove`（`config/msg_group.go:203-238`）正是
`NoPersist:0` + `SyncOnce:0` + `Subscribers` + `Setting{NoUpdateConversation:true}`
+ `visibles` 的组合。

**已用实际部署的 broker 二进制实测（`wukongim v2.2.4-20260313`），规律成立、反例不成立：**

| 请求形状 | 结果 |
|---|---|
| `channel_id` + `subscribers` + `sync_once:0`（`SendGroupMemberBeRemove` 的形状） | **HTTP 400** `无法处理发送消息请求！` |
| `channel_id`，无 `subscribers`（本任务退群提示的形状） | HTTP 200，正常投递 |

源码侧对得上（octo-im）：`internal/access/api/message_send.go` 在
`len(subscribers) > 0` 时把 `channel_id` 清空并走 request-scoped 路径，而
`internal/usecase/message/send.go` 的 `sendRequestScoped` 头一句就是
`if !cmd.Framer.SyncOnce { return ErrRequestSubscribersRequireSyncOnce }`。
也就是说 **`Subscribers` 这条路对持久消息根本不可用**，不构成反例。

顺带两个发现（均超出本任务范围，应另行立项）：

1. `SendGroupMemberBeRemove` 在这个 broker 上**发不出去**（400）——「你被 X 移除群聊」
   实际从未投递。仓内 `TestGroupCascadeKickStillNotifies` 之所以是绿的，是因为它打的是
   返回 `{}` 的 HTTP stub，不校验 broker 的真实响应。
2. broker 的 `sendMessageRequest` **没有 `setting` 字段**，octo-lib 传的
   `Setting{NoUpdateConversation:true}` 被静默丢弃。

**对本 learning 的影响**：主规律（差分验证要覆盖每个门槛）与此无关，不受影响；
上面这条附带规律经此核验可以照写，但必须带上「`Subscribers` 路线对持久消息不可用」
这个前提，否则读者会以为 `SendGroupMemberBeRemove` 是可行的反例做法。
