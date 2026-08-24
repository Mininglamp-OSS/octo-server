---
type: Task
title: "Task: oppo-push-rest-hardening"
description: Bring the OPPO notification unicast adapter onto the current domestic REST API with bounded I/O, token refresh, classification options, routing metadata, and deterministic tests.
tags: ["webhook", "push", "oppo", "adapter", "http", "testing"]
timestamp: 2026-08-24T00:00:00+08:00
# --- octospec extension fields ---
slug: oppo-push-rest-hardening
source: self
---

# Task: oppo-push-rest-hardening

## Goal

把 OPPO 离线通知单推切到当前国内 REST API，并补齐生产所需的表单编码、HTTP 超时、
鉴权 token 缓存及失效刷新、严格响应校验、通知去重、会话路由参数和新消息分类字段配置。

## Load-bearing list

- `wire-contract` — 鉴权与单推使用 `api-push-cn.heytapmobi.com`，请求为 UTF-8 form，
  `message` 为嵌套 JSON。
- `credential` — MasterSecret 和 auth token 不进入日志；token 按 AppKey 隔离并在本地、Redis
  同步提前过期。
- `retry/idempotency` — 仅在 OPPO 返回 code 11 时刷新 token 并重试一次；两次发送保持同一
  `app_message_id`，其他业务错误不自动重试。
- `routing` — 只发送服务端生成的 `space_id/channel_id/channel_type/message_seq`，不接受
  外部任意动作参数。
- `classification` — `category/notify_level/private_msg_template_id` 由部署配置显式开启；
  默认不把无法区分的人类、Bot、系统消息全部声明为 `IM`。
- `availability` — 出站 HTTP 总超时 5 秒，限制响应体大小，HTTP/JSON/协议异常均返回错误；
  正常推送使用进程内 token 快路径，不因 Redis RTT 串行化。
- `test` — 使用本地 HTTP server 和内存 token cache 覆盖编码、签名、缓存、刷新、错误码、
  payload 与超时配置，不依赖真实 OPPO 凭据。

## Design

1. 保留现有 `Push` 接口，在 OPPO adapter 内注入最小 HTTP doer、token cache 和 clock，生产
   默认使用带 5 秒总超时的 `http.Client` 与 Redis。
2. 用 `url.Values.Encode()` 生成 form body，严格要求 2xx、合法 JSON 和存在的数值 `code`。
3. token cache key 包含 AppKey 摘要，缓存值携带绝对过期时间并缓存 20 小时；正常路径无锁读取
   进程内 token，只有缓存 miss、过期或 code 11 才串行刷新。缓存故障时仍可直接鉴权，避免
   Redis 故障把 OPPO 推送完全阻断。
4. code 11 使旧 token 失效，强制重新鉴权后只重试一次。其他 OPPO 错误包装为带 code 和
   retryable 属性的类型，但本任务不新增异步重试队列。
5. 单推消息开启 `verify_registration_id`，使用稳定 `app_message_id` 去重，不发送会跨会话
   冲突的 `notify_id`；携带离线 TTL、notification channel、通知级别、私信模板和服务端
   生成的会话路由参数。标题/正文分别按 50/200 Unicode 字符截断，空值使用安全兜底，
   `action_parameters` 最大 4 KiB。
6. 新分类字段通过 `TS_PUSH_OPPO_*` 环境变量接入，保持未配置时的旧分类语义；配置非法时
   禁用 OPPO pusher 并记录错误，避免静默发送错误分类。

## Out of scope

- Android SDK 初始化、registration_id 上报和通知点击消费。
- 多设备 token 存储模型改造。
- 广播、批量单推、透传消息和回执回调消费。
- 为所有厂商推送统一重试队列或统一客户端抽象。
- 在没有测试凭据和测试设备时宣称真实设备已送达。

## Acceptance

- [x] 鉴权、单推均命中最新国内 host，form 特殊字符和中文可无损解析。
- [x] HTTP 总超时为 5 秒，非 2xx、超大/畸形 JSON、缺失或非法 code 均失败。
- [x] token 按 AppKey 隔离并在本地/Redis 同步 20 小时过期；正常路径不访问 Redis；并发
  cache miss 只鉴权一次；code 11 刷新并只重试一次，稳定去重 ID 不变。
- [x] code 41/54/10000 等参数或目标错误不自动重试，错误保留 OPPO code。
- [x] payload 包含校验 registration_id、去重 ID、离线 TTL、路由参数及已配置的新分类字段，
  不发送跨会话可能冲突的 `notify_id`，且字段长度符合 SDK 约束。
- [x] OPPO 业务拒绝记录 code/retryable 的 Warn；成功响应记录可用于对账的 messageId，日志
  不包含凭据或 registration_id。
- [x] 聚焦测试、race、10 次重复运行、OPPO adapter coverage 83.0%、`go vet ./modules/webhook`、
  `golangci-lint run ./modules/webhook/...` 与 `git diff --check` 通过。
- [x] `go test ./modules/webhook -count=1` 已通过：使用临时 octo-lib testutil overlay 指向独立的
  `test_oppo_santiago` 数据库运行，完成后已删除该临时库。默认共享 `test` 库仍存在 checkout
  未知的 `20260820000001_featuregate_init.sql`；未修改或清理共享测试库。
