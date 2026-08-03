---
type: Task
title: "Task: doc-comment-bot-mention-mvp"
description: MVP — @User Bot in a doc comment wakes an OpenClaw bot that edits the doc via octo-cli and replies in the comment thread
tags: [bot-api, robot, internal-api, event-queue, openclaw-plugin]
timestamp: 2026-07-30T00:00:00Z
# --- octospec extension fields ---
slug: doc-comment-bot-mention-mvp
upstream: 可执行评论产品原型（Octo Docs · 可执行评论 v2）；技术方案 bot-task-platform-solution v2.6（slim 化执行）
source: self
---

# Task: doc-comment-bot-mention-mvp

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

用户在 octo-web 文档页评论区 @ 一个**已具有该文档权限的 User Bot**（`robot`
表中 `status=1`、通过 Bot Token 接入 OpenClaw），bot 被唤醒后通过 octo-cli
（docs API，bot 自身 token）编辑文档正文；完成后在该评论串下回复一条 bot 消息，
文档版本记录中出现该次 bot 修改的 diff。

本 MVP **只支持 User Bot，不支持 `app_bot`**。

octo-server 在链路中只做「业务 ↔ Bot」的桥梁：接收 docs-backend 上报的 @bot 事件，
校验内部调用、灰度范围、User Bot 状态和幂等性后，投进目标 bot 的既有事件队列。
**不做**临时授权票据、平台任务状态机、终态对账或通知卡片；文档访问权限始终由
docs-backend 在评论写入和实际编辑/回复时判定，`from_uid` 等上报字段不得成为授权依据。

链路（六步，★ 为本 brief 范围）：

```
1. octo-web 评论 @User Bot                  [octo-web]
2. docs-backend 存评论 → 上报 octo-server     [docs-backend]
3. ★octo-server 校验、幂等 claim、入队 bot 事件 [本仓库]
4. ★openclaw 插件收事件 → 隔离会话 → 派发 agent [openclaw-channel-octo]
5. agent 经 octo-cli 改文档、回复评论          [bot 运行时 + docs-backend]
6. 页面展示 bot 回复 + 版本 diff              [docs 侧既有能力]
```

## Background

- 原始诉求：文档评论 @bot 的操作流量不能复用 DM 私聊（会话与串行队列同键，
  混入私聊历史；`openclaw-channel-octo/src/inbound.ts` sessionId 派生已证实）。
- 经五仓库分析与业界对标（Linear/GitHub/飞书/Slack），不新增 IM channel 类型；
  完整平台化设计见 bot-task-platform-solution.md v2.6，本 MVP 是其 slim 子集。
  开工前须把产品原型、技术方案及外仓证据补成可访问的 `repo@commit:path`，避免实现期
  依赖漂移。
- 关键存量（octo-server 当前实现已复核，直接复用）：
  - 类型化 bot 事件是队列一等公民：`robotEvent{EventType, EventData}`
    （`modules/robot/model.go:9-16`）；普通调用使用 `EnqueueBotTypedEvent`，本 ingress 使用
    同一序列化路径的 `PrepareBotTypedEvent` 后原子提交（card_action 仍走普通路径）。
  - 内部服务鉴权可复用 `X-Internal-Token` header、常量时间比较和未配置 fail-closed
    模式；本能力使用独立 token，不复用 notify token。
  - `ExistRobot` 只查询 `robot` 表中 `status=1` 的 User Bot，正好匹配本 MVP；
    `app_bot` 明确拒绝。
  - `/v1/bot/events` 会原样透传类型化事件；App Bot 的非 Person 过滤只针对
    message-shaped 事件，但本 MVP 不以 App Bot 为目标。
  - 插件缝合点：`synthesizeCardActionMessage` + `routeOverride`
    （`src/card-action.ts:110-129`、`src/channel.ts:1516-1536`；开工前固定 commit）。

## Delivery semantics

- docs-backend 为每一次“评论中的某个 bot mention”生成稳定且唯一的
  `idempotency_key`；HTTP 重试必须复用该 key，不得按请求次数生成新 key。
- octo-server 在正常请求和并发重放下保证同一 `(bot_uid, idempotency_key)` 只入队一次。
  幂等流程为 `claim(pending) → prepare event → atomic enqueue+confirm(event_id)`：
  - pending TTL 使用 60 秒；事件序列号分配或序列化失败时，以 CAS 方式只释放仍为 pending
    的 claim；因此这类失败可安全重试；
  - 事件准备阶段不写可见队列；最终通过同一 Redis Lua 原子校验 lease、`ZADD` bot 队列、
    刷新队列 TTL，并把 claim 更新为 done。旧 lease 即使在慢请求后恢复也不能写入队列；
  - atomic enqueue+confirm 的 claim TTL 与队列 TTL 均使用 `Robot.MessageExpire`；序列号可因
    lease 失效而出现空洞，bot 消费游标不得假设 event_id 连续；
  - claim 仍存在时，同 key、同规范化请求体回放原 `event_id`，同 key、不同请求体返回冲突；
    docs-backend 对同一 key 的所有重试必须保持规范化请求体稳定，不能把 key 当作可复用业务 ID；
  - Redis 提交结果不明确时先读取 claim：done 则按 replay 返回；无法确认则 fail-closed，
    不释放 pending claim，避免已提交事件被另一请求重复入队。
- bot 事件记录的 `expire` 和队列 key TTL 均由 `Robot.MessageExpire` 生成。现有队列在新事件
  入队时会刷新整个 key 的 TTL，因此繁忙 bot 的旧成员可能存活更久；离线恢复承诺至少覆盖
  `Robot.MessageExpire`，但插件的持久去重不能只依赖该 TTL，应保存到明确终态/ACK，避免旧事件
  与已过期 claim 重叠时重复产生文档副作用。
- 插件不得在事件仅被解析时立即 ACK：只有事件已进入可恢复的执行/幂等记录，或任务已经
  到达明确终态后才能 ACK。游标只有在 ACK 成功或事件已被持久 claim 后才能越过该事件。
- 跨 docs-backend、octo-server、OpenClaw 的分布式 exactly-once 事务不在 MVP 范围；bot
  拉取/执行仍采用 at-least-once delivery。用户可见效果通过稳定 `idempotency_key`、插件持久去重，以及 docs 编辑/回复 API 的
  幂等能力实现 effect-once。若 docs API/现有 octo-cli 不支持幂等 key，必须在开工前确认
  插件侧持久去重足以覆盖进程重启，否则不得声称“一次编辑、一次回复”。

## Load-bearing list

### octo-server

- 新增独立模块 `modules/bot_mention`，并在 `internal/modules.go` 加 blank import；模块通过
  `robot.IService` 复用 `ExistRobot` 和 `PrepareBotTypedEvent`。
- `modules/robot` 事件队列 wire format（`robotEvent` 结构、score/expiry 语义）只新增
  `event_type` 值，不改结构；`robot/model.go` 与 `bot_api/events.go` 的两处定义保持
  lockstep。
- 内部入口使用独立环境变量 `OCTO_DOCS_BOT_MENTION_TOKEN`：
  - header 为 `X-Internal-Token`，常量时间比较；
  - 未配置时入口 fail-closed 并记录启动错误日志；
  - 不复用 `NOTIFY_INTERNAL_TOKEN` 或 `OCTO_DOCS_NOTIFY_TOKEN`；
  - 这是 service-to-service 鉴权，明确不挂用户 `AuthMiddleware`、Space middleware 或
    UID limiter；全局 IP 防护仍由现有全局 middleware 承担。
- 新端点无固定 400 的历史客户端，错误使用 `ResponseErrorLWithStatus` 返回真实 HTTP
  status；合并前取得 maintainer 对偏离 D14 的确认。所有错误仍注册 errcode/i18n，并加入
  `TestBotMentionNoLegacyResponseError` guard。
- 幂等 key 必须包含 bot 维度并保存规范化请求指纹；Redis 故障 fail-closed，不允许绕过去重
  直接入队。
- 灰度配置采用环境变量，启动时读取：
  - `OCTO_DOCS_BOT_MENTION_ENABLED`，默认 `false`；
  - `OCTO_DOCS_BOT_MENTION_SPACE_ALLOWLIST`；
  - `OCTO_DOCS_BOT_MENTION_DOC_ALLOWLIST`。
  开关为 true 后，doc 或非空 space 命中任一 allowlist 才放行；两份 allowlist 都为空时
  仍 fail-closed。doc allowlist 的 `*` 表示所有文档；space allowlist 的 `*` 表示任意**非空**
  space，不能让空 `space_id` 命中。配置变更通过重启生效。

### openclaw-channel-octo

- `events-poll.ts` 轮询器改为配置开启后常驻，不依赖先发卡片；
  `doc_comment_mention` 必须显式解析、派发和 ACK，未知事件仍按既有兼容策略处理。
- `inbound-queue.ts` 的队列键必须随会话键覆写，禁止合成 DM 形状后落入用户 DM 队列。
- 服务端下发规范化 `thread_id = parent_id != "" ? parent_id : comment_id`；插件不得自行
  产生另一套规则。
- session/queue key 使用同一个 canonical key：
  `octo:doctask:{account_id}:{bot_uid}:{space_or_global}:{doc_id}:{thread_id}`。
  每段须做无歧义编码或哈希，避免分隔符注入；同评论串串行、跨评论串可并行，但仍受
  插件既有的 per-account 总并发上限约束，不得按 thread 无界创建执行 goroutine。
- 插件持久去重使用 `idempotency_key`，而不是仅用可能因崩溃重投而变化的 `event_id`；
  去重状态必须覆盖进程重启，TTL 不短于 `Robot.MessageExpire`。
- ClawHub 发版与存量 bot 升级必须纳入发布计划；旧插件不识别新事件时不能开启服务端灰度。

## Ingress contract

`POST /v1/internal/bot-mentions`

```json
{
  "idempotency_key": "stable-id-for-this-comment-mention",
  "doc_id": "doc-id",
  "comment_id": "comment-id",
  "parent_id": "optional-root-comment-id",
  "from_uid": "human-user-id",
  "bot_uid": "active-user-bot-id",
  "text": "comment text",
  "url": "optional https://docs/...",
  "space_id": "optional-space-id"
}
```

字段规则：

- 整个 request body 最大 32 KiB。
- `idempotency_key`、`doc_id`、`comment_id`、`from_uid`、`bot_uid` 必填，trim 后
  1–256 bytes；`parent_id`、`space_id` 非空时不超过 256 bytes。
- `text` 必填且不能全为空白，保留原文，最大 10 KiB；它是用户指令内容，不是可信代码或
  授权信息。
- `url` 可选，最大 2048 bytes，只接受绝对 `http/https` URL；仅作展示/定位元数据，
  agent 工具调用以 `doc_id/comment_id/thread_id` 为准，不得把 URL 拼进 shell 命令。
- `thread_id` 由 octo-server 按 `parent_id`/`comment_id` 派生，不接受调用方直接指定。
- `from_uid`、`space_id` 和 `url` 均来自受信 docs-backend，但只能作为事件元数据、灰度输入
  或审计线索，不能替代 docs 权限校验。

成功与灰度响应：

```json
{"accepted": true, "replay": false, "event_id": 1000001}
{"accepted": true, "replay": true,  "event_id": 1000001}
{"accepted": false, "replay": false, "reason": "disabled"}
```

- 首次有效请求：200，`accepted=true, replay=false`。
- 同 key、同请求体且 claim 已为 done：200，`accepted=true, replay=true`，返回原 `event_id`；
  replay 检查先于可变的灰度和 bot 状态检查，已受理事件不会因后来关灰度或停用 bot 而
  改写历史结果。
- 同 key 尚在 pending：409，返回幂等处理中错误并带 `Retry-After`。
- 同 key、不同规范化请求体：409，绝不复用或覆盖旧事件。
- 灰度未命中：200，`accepted=false, reason=disabled`，不写幂等终值、不入队，避免
  docs-backend 把有意关闭当瞬时故障无限重试。
- 无/错 token：401；坏 JSON、缺字段、越界字段：400；目标不是 active User Bot（包括
  `app_bot`）：统一 404，防止区分不存在与停用；存储或依赖失败：500。

入队事件：

```json
{
  "event_type": "doc_comment_mention",
  "event_data": {
    "idempotency_key": "stable-id-for-this-comment-mention",
    "doc_id": "doc-id",
    "comment_id": "comment-id",
    "thread_id": "root-comment-id",
    "parent_id": "optional-root-comment-id",
    "from_uid": "human-user-id",
    "bot_uid": "active-user-bot-id",
    "text": "comment text",
    "url": "optional https://docs/...",
    "space_id": "optional-space-id",
    "enqueued_at": 1785460000
  }
}
```

可选字段为空时省略；`enqueued_at` 由 octo-server 生成。上述字段只许向后兼容地增补，
不得改名或改变语义。

## Failure handling

- docs 编辑/回复返回网络错误、429 或 5xx：按带 jitter 的指数退避重试，不 ACK，不得热循环；
  重试不得超过事件保留窗口。
- docs 返回 401/403/404 等永久错误：禁止继续编辑；若评论回复权限仍可用，向原线程写一条
  安全、非敏感的失败说明，然后作为 terminal failure ACK。
- agent/tool 返回确定的参数或业务错误：回复安全失败说明并 ACK；无法分类的错误按可重试
  处理，但必须有次数/时间上界。
- terminal failure 的评论回复本身也永久失败时，记录结构化日志和指标后 ACK，避免 poison
  event 永久占队；平台级终态对账仍属 out of scope。
- 日志和 prompt 不得包含 bot token、内部 token 或其他凭证；生产日志不得记录完整评论正文
  或 URL。

## Observability

- octo-server 至少提供低基数指标：
  - `dmwork_doc_bot_mention_ingress_total{result}`，result 包含 accepted/replay/disabled/
    invalid/not_found/unauthorized/conflict/error；
  - `dmwork_doc_bot_mention_enqueue_total{result}`；
  - ingress/enqueue latency。
- 插件至少提供 poll、claim、dispatch、retry、terminal、ack 指标，并能从
  `event_id + idempotency_key hash + bot_uid` 关联一次执行；不得把 doc/comment/user ID 放进
  metrics label。
- 结构化日志记录 `event_id`、bot uid、结果和 idempotency key 的不可逆短 hash；不记录
  token、完整 text 或 URL。

## Out of scope

- `app_bot` 支持。
- 临时授权票据（task_grant）、平台任务状态机/SLA/终态对账、任务流 UI、通知卡片、
  出站事件订阅——完整版方案内容，按需另立任务。
- 跨系统 exactly-once 事务；MVP 的 bot 拉取/执行采用 at-least-once delivery + effect
  idempotency，但 octo-server ingress 的 claim 与队列写入保持原子。
- octo-im、octo-cli 的代码改动；若现有 octo-cli 能力不能满足前置条件，必须回到 brief
  重新定范围，不能在实现时静默扩项。
- docs-backend / octo-web 内部实现（上报调用、@ 选择器、版本/diff/权限模型均自理；
  octo-server 只冻结上报和事件契约）。
- bot 对文档的权限授予流程（docs 侧产品能力）。

## Acceptance

### octo-server

- `POST /v1/internal/bot-mentions` 按 Ingress contract 返回固定响应；有效首请求只生成一条
  `doc_comment_mention` 事件，event_data 字段完整。
- 集成测试覆盖：
  - token 未配置、无 token、错 token、正确 token；
  - 坏 JSON、缺字段、越界 body/text/URL、非法 URL scheme；
  - active User Bot 成功；不存在、inactive User Bot 和 `app_bot` 统一拒绝且不入队；
  - 同 key 顺序重放与并发重放只有一个正常入队；replay 返回原 event_id；
  - 同 key 不同 body 409；pending 409 + Retry-After；
  - event 准备失败释放 pending 后可重试；旧 lease 在另一请求接管后恢复时不能入队；
  - atomic enqueue+confirm 结果不明确时，done 可恢复为 replay，其余情况 fail-closed；
  - 灰度全关、doc 命中、space 命中、空 allowlist、`*` 全量；
  - Redis/DB 故障 fail-closed；日志不包含 token 或完整正文。
- 事件可被 `POST /v1/bot/events` 拉到并 ACK；队列 TTL 和幂等 done TTL 均来自
  `Robot.MessageExpire`。
- 指标按 accepted/replay/disabled/conflict/error 等结果递增，无高基数 label。
- `make i18n-extract`、`make i18n-extract-check`、`make i18n-lint`、
  `TestBotMentionNoLegacyResponseError` 和目标模块测试全部通过。

### openclaw-channel-octo

- 配置开启后轮询常驻；`doc_comment_mention` 被解析、持久 claim、派发并按 ACK 规则确认。
- vitest 断言：
  1. 同评论串两事件串行且同 session；
  2. 不同评论串可并行且 session 互不可见，同时受总并发上限约束；
  3. bot 的 DM session、DM history 和 DM queue 零污染；
  4. 同 `idempotency_key` 即使以不同 event_id 双到，也只产生一次文档副作用；
  5. 插件重启后仍能识别已 claim/完成事件；
  6. retryable failure 不 ACK，terminal failure 按规则回复并 ACK。
- agent prompt 含评论文本与 doc/comment/thread 定位，明确文本为不可信用户指令；prompt 中
  不出现票据或 credential，bot token 只走既有 credential 链。

### 端到端（灰度环境，人工/脚本各跑一次）

- 评论 @User Bot → 评论串出现 bot 回复、版本记录出现 bot diff。
- bot 进程离线期间触发 @，在 `Robot.MessageExpire` 窗口内恢复后事件送达并完成。
- 同一 mention 顺序/并发重复上报，仅一次编辑、一次回复。
- 灰度未命中时不入队，docs-backend 能识别 `accepted=false/reason=disabled` 且不重试。
- 执行前撤销 bot 文档权限时不发生编辑，评论串出现安全失败说明；若回复也不可用，日志和
  指标可定位 terminal failure。
- 注入一次 docs 429/5xx，验证退避重试、无热循环、成功前不 ACK，恢复后只产生一次最终效果。

## 前置确认（开工前拿到答复）

1. docs-backend：User Bot 作为“有文档权限的协作者”的授权模型；评论落库时验证触发用户
   可评论，编辑/回复执行时再次验证 bot 当前权限。
2. docs-backend：为每个 `(comment mention, bot_uid)` 生成稳定唯一 idempotency key，明确
   上报超时、409 pending、4xx 和 5xx 的重试口径。
3. docs-backend / octo-cli：现有 bot token credential 链已支持文档编辑、指定评论串回复，
   并确认写操作的幂等 key 能力；不满足时回到 brief 决定插件持久 ledger 或扩展 API。
4. openclaw-channel-octo：可复用的持久状态/去重存储、per-account 总并发上限，以及 ACK/
   游标的现有语义已经核实。
5. 产品原型、bot-task-platform-solution v2.6、插件缝合点均补充 `repo@commit:path`。
6. 插件 ClawHub 发版窗口、最低兼容版本和存量 User Bot 升级计划。
