---
type: Task
title: "Task: bot-thread-archive"
description: 给 Bot API 增加子区归档端点 POST /v1/bot/groups/:group_no/threads/:short_id/archive，复用 thread.ArchiveThread（幂等）；失败按 botDeleteThread 同口径映射为 err.server.bot_api.store_failed。
tags: ["bot-api", "thread", "space", "isolation", "auth", "acl", "wire-contract", "testing", "commit"]
timestamp: 2026-09-17T15:47:17+08:00
# --- octospec extension fields ---
slug: bot-thread-archive
upstream: none (owner request — omp-channel-octo thread-archive 能力所需的服务端端点)
source: user
---

# Task: bot-thread-archive

## Goal

Bot（User Bot）具备与 Web 端「归档子区」一致的归档能力：

- 新增 `POST /v1/bot/groups/:group_no/threads/:short_id/archive`，挂在既有
  botAPI 组内（`authBot` + `requireBotIdentity` + per-bot 限流），与其它变更类
  子区路由一致带 `protectAIContainerMutation`（App Bot 在 `validateBotGroupAccess`
  即被拒）。
- Handler 复用既有 `threadService.ArchiveThread(groupNo, shortID, robotID)`：
  成功 `c.ResponseOK()`；失败日志 + `httperr.ResponseErrorL(c, ErrBotAPIStoreFailed)`，
  与 `botDeleteThread` / `botJoinThread` / `botLeaveThread` 同口径。
- 语义沿用 service：幂等（已归档返回成功）、线程不存在 / 已删除 / 无权限沿用
  `ArchiveThread` 的现有错误。不新增错误码、不改本地化、无数据库迁移
  （`thread.status=2` 已存在）。

消费方（另仓 `omp-channel-octo`）契约：无 body 的 POST，已归档返回成功；按
「瞬时失败（HTTP 5xx、或 bot API 把内部 500 包成 400 的 `store_failed`）可重试」
处理 —— 因此失败一律走 `ErrBotAPIStoreFailed`，不做更细的分类。

## Background

- Web 端归档走用户 API `POST /v1/groups/:group_no/threads/:short_id/archive`
  （`modules/thread/api.go` 的 `archiveThread`），Bot API 此前没有对应路由，
  客户端探测该路径得到 gin 默认 404。
- 服务端写路径已就绪：`modules/thread/service.go` 的
  `ArchiveThread`（幂等；权限 = 父群活跃成员 且 子区创建者或群管理员；
  父群已解散时 `canOperate` 经 `ensureGroupNotDisbanded` 拒绝）。
- 子区收到新消息会自动解档（`RecordMessageAndReactivate`），所以消费方把归档
  排到「回合最后一条回复送达之后」执行 —— 这不是本任务范围，但说明端点必须可
  重复调用（幂等）。

## Load-bearing list

- **子区写路径与幂等语义**（`modules/thread/service.go` `ArchiveThread`、
  `UpdateStatusFrom` CAS）：归档只切 `active→archived`；已归档再调用返回成功；
  `deleted` 返回错误。Bot 端点不得绕过或改写这些语义。
- **Bot 子区门禁**（`touches: bot-api, space, isolation, acl, auth`）：
  `validateBotThreadAccess` → `validateBotGroupAccess` 的 `ExistMemberActive`
  黑名单语义（被拉黑 / 被移出的 bot 一律拒绝）先于本端点执行；被拉黑的
  creator 由 `canOperate` 在 service 层再拒一次。App Bot 在
  `validateBotGroupAccess` 即被拒（DM-only）。
- **错误 wire 契约**（`touches: wire-contract`）：失败复用已注册的
  `ErrBotAPIStoreFailed`（D14：外层 400 / `error.http_status=500`，消息不泄露
  拒绝原因）；无新错误码，故无 i18n 提取 / 翻译变化。
- **路由表与中间件**（`modules/bot_api/bot_api.go`）：新路由必须挂在 botAPI
  组内、带 `protectAIContainerMutation`，与 `botDeleteThread` 一致；App Bot /
  AI 容器（`purpose=ai_session_container`）在这些门前即被拒。

## Out of scope

- 取消归档（`unarchive`）、批量归档、归档列表入口。
- Web / 用户 API 行为（含 `modules/thread` 下任何代码）。
- 数据库迁移、错误码与本地化文件。
- `omp-channel-octo` 侧实现与部署 / 真机验收（端点未部署前无法执行）。

## Acceptance

- `go test ./modules/bot_api/... ./modules/thread/...` 通过。
- 新增 handler 测试覆盖：
  - 成功归档：creator bot 归档自己创建的子区 → 200；DB `status=2`；
    `GET /v1/bot/groups/{g}/threads/{id}` 复查 `status=2`；重复调用仍 200（幂等）。
  - 权限 / 存储失败映射：非 creator / 非管理员 bot 归档 → 400 +
    `ErrBotAPIStoreFailed` 文案（不泄露原因），子区保持 `status=1`；
    不存在的子区 → 同一映射（400 store_failed，而非 404）。
  - 路由注册：无 token → 401（路由存在且 `authBot` 已挂载，缺路由会是 404）；
    AI 容器群 → `ErrAITeamContainerProtected`（证明
    `protectAIContainerMutation` 在路由上）。
- 真机验收（部署后，交回 owner 执行）：对一次性子区调用该端点，随后
  `GET /v1/bot/groups/{g}/threads/{id}` 复查 `status=2`。
