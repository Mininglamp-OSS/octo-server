---
type: Task
title: "Task: unread-clear-cmd-offline-delivery"
description: unreadClear 命令改为持久化并归属登录用户的命令频道，让离线端重连后能补拉到已读状态
tags: ["conversation", "unread", "cmd", "multi-device"]
timestamp: 2026-09-06T00:00:00Z
# --- octospec extension fields ---
slug: unread-clear-cmd-offline-delivery
upstream: n/a
source: self
---

# Task: unread-clear-cmd-offline-delivery

## Goal

web 端读过的会话，app 端也要能自动消掉红点，不需要用户在 app 上再读一次。

服务端的已读游标本来就是账号级的（WuKongIM `UserConversationState` 只有 uid + 频道，
没有设备维度），三端拉 `conversation/sync` 拿到的未读数一定一致。断的是「读了」这件事
怎么通知到另一台设备：`clearConversationUnread` 发的 `unreadClear` 命令带了
`NoPersist: true`，在 IM 侧走实时投递 —— 不落库、不进命令频道投影，端不在线就永久丢失，
重连后走 `/v1/message/sync`（客户端 pullCMDMessages）也补不回来。

## Background

链路三段：

1. 写状态：`PUT /v1/conversation/clearUnread` → IM `advanceReadSeq` 推进 `ReadSeq`。账号级，没问题。
2. 实时通知：`SendCMD(unreadClear)`，`NoPersist: true` → IM `sendRealtime`
   （`internal/usecase/message/send.go` 的 NoPersist 分支）：不落库；
   `internal/usecase/cmdsync/intent.go` 的 `isDurableCMDProjectionMessage` 又把
   NoPersist 直接判 false，命令频道投影里也没有。**本次修复的就是这一段。**
3. 兜底：客户端重连后的 conversation sync。Android 是 INSERT OR REPLACE，能自愈；
   iOS 的 `WKUnreadStore.reconcileServerSnapshot` 默认取 `MAX(local, server)`，
   结构性地不接受服务端把未读调低 —— 那是客户端侧的问题，不在本次范围。

顺带修掉第二半：`SendCMD` 不带 `FromUID` 时，IM 把命令归到全局的 `____system____cmd`，
而不是 `{uid}____cmd`。两条频道的 `message_seq` 各自独立递增，客户端用单一
`max_message_seq` 做水位就会漏拉。

## Load-bearing list

- `unreadClear` 命令的投递语义（实时 → 持久化）：命令开始占用命令频道的 seq 空间，
  客户端 `pullCMDMessages` 的水位推进随之变化。
- 命令频道归属：从 `____system____cmd` 改为 `{loginUID}____cmd`。
- 三端已有的 `unreadClear` 处理分支保持不变（web `packages/dmworkbase/src/module.tsx`、
  iOS `WKConstant.h` + `WKSystemMessageHandler.m`、Android `CMDManager.java`），
  收到命令后照旧把本地会话未读改写为 param.unread。
- 在线端行为不变：持久化命令同样经 delivery 实时投递。
- `Setting{NoUpdateConversation: true}` 保留，命令不会更新会话列表。

## Out of scope

- iOS `reconcileServerSnapshot` 的 `MAX(local, server)` 合并策略（octo-ios 仓库，只读权限）。
- 服务端 `conversation/sync` 透传 `readed_to_msg_seq`（IM 已下发，
  `SyncUserConversationResp` 未透传）——iOS 侧修复的前置，需要和客户端一起排。
- `modules/robot/event.go` / `modules/bot_api/send.go` 里清未读但完全不发命令的路径。
- 命令频道的容量/清理策略。清未读是高频操作，持久化后的写入量需要观察。

## Acceptance

- `go test ./modules/message/ -run TestClearUnreadCMDReachesOfflineDevice`
  （`conversation_unread_cmd_offline_test.go`）：真实 WuKongIM 下，清未读后以离线设备
  身份调 `/v1/message/sync` 必须拉到 `unreadClear`，且该命令落在 `{uid}____cmd`。
  回退任一半修复（`NoPersist: true` 或去掉 `FromUID`）该测试必须失败。
- `go test ./modules/message/`、`go build ./...`、`go vet ./modules/message/...` 通过。
- `make i18n-extract-check`、`make i18n-lint` 通过。
