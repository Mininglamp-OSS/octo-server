---
type: Task
title: "Task: cloud-clone-per-user-limit"
description: Enforce one active Octo-hosted cloud clone per owner across all creation paths and Spaces.
tags: ["auth", "space", "isolation", "wire-contract", "error-response", "i18n", "test"]
timestamp: 2026-09-09T18:00:00+08:00
slug: cloud-clone-per-user-limit
upstream: user-request
source: user
---

# Task: cloud-clone-per-user-limit

## Goal

限制每个用户最多拥有一个有效的 AI 云端分身（`octo_hosted`），且该限制由
Server 在创建时执行，覆盖所有客户端和入口，不依赖「我的 Agent」页面或本地
Octo Bot store 的筛选结果。客户端在用户点击云端助理入口时幂等确保该分身存在，名称固定为
`<用户显示名>的 AI 分身`。

## Background

当前客户端调用通用的 `createOctoBot` 创建 User Bot，
创建成功后才异步上报 `agent_hosting=octo_hosted`。因此 Server 在创建时不知道它是
云端分身；不同客户端、不同 Space 或并发请求都可以绕过任何仅客户端/列表层的限制。
`robot.creator_uid` 是所有 User Bot 的全局归属事实，`agent_hosting=octo_hosted`
是 AI Team 将其归为 `cloud_clone` 的现有标记。

## Load-bearing list

- `auth` / `space` / `isolation`：配额键为真实登录用户 UID，**不按 Space 分桶**；
  请求仍须经现有授权及 Space 成员校验，不能用 body 的 `space_id` 或伪造 UID 影响他人。
- `wire-contract` / `error-response` / `i18n`：通用建 Bot 请求可显式声明受限的
  `octo_hosted` 创建意图；第二个有效云端分身返回稳定的本地化 4xx error envelope，
  不暴露已有 Bot ID、名称、Space 或其他敏感信息。
- Server 在同一 DB 事务/锁边界内检查并持久化 `octo_hosted` 身份，避免两个并发创建
  同时通过；失败或并发冲突不得留下 app、robot、user、friend 或 space_member 半成品。
- 全局计数只包含该用户当前有效、确认为 `octo_hosted` 的 User Bot；删除/失效后的
  云端分身不占名额。已有历史数据若已超过一个，不做自动删除或合并，只禁止继续创建。
- 普通自建助理（`self_hosted` / 未声明 hosting）、数字员工、App Bot 和其他第三方
  hosting 不受此「云端分身一个」限制。
- 云端分身入口随 Server 契约传递创建意图；仍保留创建后 runtime 上报用于状态展示，
  但它不再是限制正确性的唯一依据。
- 客户端云端助理入口不展示名称输入。点击时使用昵称、用户名的既有回退顺序，再拼接
  `的 AI 分身`；若已存在则查询并复用既有分身，重复点击或多窗口调用不产生第二个
  Bot。登录本身不得创建云端分身。

## Out of scope

- 不改变已有云端分身的运行时、OpenClaw 配置、AI Team 分组、二人会话或群聊逻辑。
- 不迁移、自动清理或删除已有重复的历史云端分身。
- 不限制普通本地助理、个人 Bot、数字员工或每个 Space 的普通 Bot 数量。
- 不更改用户手动「接入云端助理」流程以外的 UI 文案、OpenClaw 接入提示词或运行时行为。
- 不提交本地认证、密钥、LLM、端口或运行时配置。

## Acceptance

- 同一用户在 Space A 创建一个云端分身成功后，在 Space A 或 Space B 再次经任一
  受支持入口创建云端分身均得到确定 4xx 本地化错误；错误不泄露第一条分身信息。
- 两个并发的云端分身创建请求最多一个成功；失败方及其副作用均不落库。
- 删除或失效唯一云端分身后，用户可再次创建一个；同一用户的普通/local Bot 创建仍成功。
- 不同用户可各自创建一个云端分身，互不影响。
- 用户点击云端助理入口后，客户端幂等确保名为 `<显示名>的 AI 分身` 的云端分身；
  已有时查询并复用，重复点击不创建第二个 Bot，失败不影响其它助理初始化。
- 云端入口传递受限 hosting 创建意图；Server 的直接 API 测试覆盖跨 Space、并发、
  删除后重建、既有分身查询和普通 Bot 回归，且运行相关模块测试与 i18n 检查。
