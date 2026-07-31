---
type: Task
title: "Task: bot-events-longpoll"
description: 给 POST /v1/bot/events 增加 opt-in long-poll（Redis 门铃唤醒），把卡片交互 bot 侧时延从轮询节奏压到秒级；ZSet 仍是唯一权威，门铃丢失只退化为等满超时。
tags: ["bot-api", "wire-contract", "rate-limit", "throttle", "test", "commit"]
timestamp: 2026-07-31T00:00:00+08:00
# --- octospec extension fields ---
slug: bot-events-longpoll
upstream: card-message-interaction D5（P3-2）
source: self
---

# Task: bot-events-longpoll

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

把 `POST /v1/bot/events` 从「一次 `ZRangeByScore` 立即返回」改为**可选的 long-poll**：
请求体新增可选 `wait`（秒）；队列当前为空时，服务端最多 hold `wait` 秒，一旦有新事件入队
就立即返回，否则超时返回**与今天完全相同的空响应**。

**解决的问题**：`card-message-interaction` brief D5 明确记录——bot 侧事件投递是游标轮询，
`modules/bot_api/events.go:103` 一次 `ZRangeByScore` 后立即返回，无 long-poll、无 push 通道，
因此**「点按钮 → bot 响应」的时延等于 bot 的轮询节奏**。客户端那一跳早已是实时的
（`message_extra` + CMD `/v1/message/extra/sync`），慢的只有 bot 这一跳。这是审批/表单类
交互卡体感的唯一瓶颈，D5 把它列为显式 P3 open item（P3-2）。

**唤醒机制（本任务的核心设计决策）**：在 bot 事件入队的两个 chokepoint
（`modules/robot/api.go` 的 `enqueueBotEventGeneric` 与 `enqueueBotTypedEventGeneric`，
后者正是 `card_action` 的入队路径，`api.go:247-277`）额外写一条**门铃**——
`LPUSH robotEventBell:{robotID}` + `LTRIM 0 0` + `Expire`；long-poll 侧用
`redis.Conn.BLPop(bellKey, chunk)` 阻塞等待。

选它的理由（对比另两条路）：

| 方案 | 多副本正确 | 新增依赖 | 连接成本 | 结论 |
|---|---|---|---|---|
| 进程内 ticker 反复 `ZRangeByScore` | ✅ | 无 | 低 | ❌ Redis 读放大：30s hold × 300ms tick = 100 次读，比现状更费 |
| **`BLPop` 门铃** | ✅（阻塞发生在 Redis 侧） | **无**——`redis.Conn` 已暴露 `BLPop`/`LPUSH`/`Ltrim`/`Expire` | 每个 hold 占 1 条池连接 | ✅ **本任务选用** |
| Redis pub/sub 单订阅连接扇出 | ✅ | 需自建 `rd.Client`（octo-lib `redis.Conn` **未暴露** `Subscribe`/`Publish`，仅暴露 `Options()`） | 每副本 1 条 | 📌 记为连接数撑不住时的升级路径，不在本任务 |

**安全性质（贯穿全设计）**：门铃只是**提示**，不是事件本身。醒来（或超时）后一律回到
`getEventsResult` 按调用方游标做权威 `ZRangeByScore`。因此**门铃丢失/被别的 waiter 抢走
只会退化成「等满 `wait` 后返回空」，永远不会丢事件**——ZSet 始终是唯一权威，与今天的语义完全一致。

## Background

- **现状核实**：`modules/bot_api/events.go:59` → `getEventsResult` → `events.go:103`
  单次 `ZRangeByScore(Min: "(eventID", Max: "+inf", Count: limit)`，同步返回。全仓无
  long-poll、无 push、无 bot 回调通道。
- **入队 chokepoint 已经是单一的**：`enqueueBotEventGeneric`（消息事件）与
  `enqueueBotTypedEventGeneric`（`card_action` 等类型化事件）两个 helper 统一了
  GenSeq / ZAdd / Expire 形状，注释明写「Centralizing … means the bot event consumer
  (/v1/bot/events) sees identical records regardless of which path produced them」。
  门铃写在这两处即可覆盖全部入队来源，不会漏。
- **本仓已有 long-poll 先例**：`modules/robot/api.go:855` `inlineQuery` 用
  `select { case <-resultChan; case <-time.After(20s) }` + 408
  （`errcode.ErrRobotInlineQueryTimeout`）。**但它是进程内 channel map，只在单副本正确**
  —— 本任务不沿用该形状（octo-server 是多副本部署，见 #627 的跨副本语义与 runtime-catalog
  的 multi-replica recovery 讨论）。
- **HTTP 层不会腰斩 hold**：`WKHttp.Run` 走 gin/`http.ListenAndServe` 的零值 `http.Server`
  ——无 `ReadTimeout`/`WriteTimeout`。反过来说**没有服务端兜底**，handler 必须自带 deadline。
- **`BLPop` 无 context**：octo-lib `redis.Conn.BLPop(key, timeout)` 是 go-redis v6 形状，
  不接受 `context`，无法被客户端断连取消。故实现必须把 hold **切成若干短 chunk**
  （每 chunk ≤ 5s），chunk 之间检查 `c.Request.Context().Done()` 与进程关停信号，
  把「客户端已走但连接仍被占住」的窗口压到一个 chunk 以内。
- **限流现状**：`botAPI := r.Group("/v1/bot", ba.authBot())`（`bot_api.go:262`）——**没有**
  `SharedUIDRateLimiter`（bot 是 token 鉴权，非登录用户维度），只受 `main.go` 全局挂载的
  per-IP `RateLimitMiddleware` 约束。long-poll **降低**请求频率，对限流是净利好；真正的新
  资源维度是**并发 hold 数**，需要自己设闸（见 Load-bearing）。

## Load-bearing list

- **`bot-api` / `wire-contract`** — `POST /v1/bot/events` 的请求/响应契约。必须保持：
  - **不传 `wait` 或 `wait<=0` → 行为与今天逐字节一致**（立即返回，含空队列时的
    `{"status":1,"results":[]}`）。这是「存量 bot 零适配」的硬保证，不是尽力而为。
  - 超时返回**沿用今天的空响应形状**（`status:1` + `results:[]`），**不引入 408、不新增
    errcode、不改 i18n** —— 超时不是错误，是「本轮没有事件」。
  - 响应结构 `eventResp`（`events.go:24-29`）一字不改；`event_type`/`event_data` 的
    additive-only 演进规则不受影响。
  - `wait` 上限由服务端 clamp（同 `limit` 的 `<=0→20 / >100→100` 既有纪律），
    非法值不报错、只夹紧。
- **`rate-limit` / `throttle`** — 新增的是**并发连接**维度，per-IP 令牌桶管不到：
  - 进程级并发 hold 上限（信号量）。**超限不报错**——降级为立即返回（等价今天的行为），
    fail-open 语义与既有限流中间件一致。
  - **每个 robotID 同时只允许一个 hold**：第二个并发 hold 立即返回，防止一个 bot
    反复开连接吃光 Redis 池（`BLPop` 每 hold 占用 1 条池连接）。
  - 必须记录并在 brief/journal 说明：并发 hold 上限与 Redis `PoolSize` 的关系
    （本仓限流类 client 的既有约定是显式 `PoolSize=10`；bot 事件走的是 `ctx.GetRedisConn()`
    主连接池，**不是**那些独立 client，需确认主池容量能覆盖设定的并发上限）。
- **事件不丢（队列语义）** — `ZRangeByScore` 保持唯一权威。门铃只影响「何时醒」，
  不影响「读到什么」。门铃 key 必须有 TTL 且**长度恒定**（`LTRIM 0 0`），
  不能因为无人监听而无界增长。
- **App Bot 过滤 + 自动 ACK**（`events.go:74-93`）—— DM-only 防御性过滤与 `ZRemRangeByScore`
  自动 ACK 必须同样作用于 long-poll 返回的结果，不得因为走了新路径而绕过。
  注意边界：过滤后可能**由非空变成空**，此时不应假装有事件返回——需明确是继续 hold 还是返回空。
- **优雅关停** — `main.go:362` `svc.Run(s)` 退出后会 `cardActionRuntime.Stop()`。
  在关停中的 hold 必须被唤醒并返回空，不能把 drain 拖长到一个完整的 `wait` 周期。
- **`test`** — 现有 bot events 测试是回归基线，不得破坏。

## Out of scope

- **不改事件入队的语义**：GenSeq/ZAdd/Expire 形状、`event_id` 单调性、`Expire` TTL 全不动；
  门铃是**旁路**新增 key，与 `robotEvent:{robotID}` ZSet 解耦。
- **不做真 push / WebSocket 通道**（D5 里 `(b)` 那条路）。本任务只做 D5 明确点名的
  「long-polling `getEvents`」这一半。
- **不引入 Redis pub/sub**、不自建 `rd.Client`。若并发 hold 撑不住主池，升级路径已在 Goal
  的表格里记录，另开任务。
- **不动 `modules/robot` 的 `inlineQuery` 长轮询**（进程内 channel 形状保持原样）。
- **不新增 errcode、不改 i18n locales、不新增/改端点、无 DB/迁移。**
- **不加 metrics**（G1 ingress 仪表是独立任务；本任务若顺手需要一两个计数器，
  留到 G1 统一按 bounded-label 纪律加）。
- **不改 bot 侧限流策略**（不新挂 `SharedUIDRateLimiter`——bot 无登录 uid）。
- **openclaw-channel-octo 侧的 `wait` 接入**：属另一仓库的 follow-up，本任务只交付服务端。

## Acceptance

机器可校验（`go test -race ./modules/bot_api/...`）：

**契约兼容（最高优先级）**
- 不传 `wait` → 响应与改动前**逐字段一致**（空队列 `{"status":1,"results":[]}`；
  有事件时同样的 `results` 投影）。以 golden/对拍断言，不靠人眼。
- `wait <= 0` 等同不传；`wait > maxWait` 被 clamp 到 `maxWait`（不报错）；
  非数字/非法 JSON 仍走既有 `respondBotAPIRequestInvalid`。

**long-poll 行为**
- 队列**已有**事件 + `wait>0` → **立即返回**，不进入等待（先读后等，不是先等后读）。
- 队列为空 + `wait>0` + 等待期间入队一条 `card_action` 事件 → 在入队后 **≤1s** 内返回该事件
  （断言时延上界，不只断言最终拿到）。
- 队列为空 + 全程无事件 → 约 `wait` 秒后返回 `{"status":1,"results":[]}`，**不是** 408/错误。
- **门铃丢失容错**：人为跳过门铃写入（或让别的 waiter 抢走）→ 请求仍在 `wait` 到期后
  正常返回空，且**下一次 poll 能读到该事件**（证明 ZSet 权威未被削弱、事件未丢）。

**资源与生命周期**
- 客户端断连 → hold 在 **≤1 个 chunk**（≤5s）内释放（以 goroutine/连接计数断言，不靠 sleep 猜）。
- 进程关停信号 → 在途 hold 被唤醒返回空，不阻塞 drain。
- 同一 robotID 的第二个并发 hold → 立即返回（等价今天的行为），不占第二条 Redis 连接。
- 并发 hold 达到进程上限 → 后续请求**立即返回而非报错**（fail-open）。

**既有行为不回归**
- App Bot 的非 DM 事件过滤 + 自动 ACK 在 long-poll 路径上同样生效（新增用例覆盖
  「long-poll 醒来后结果被过滤空」这一分支的既定行为）。
- 门铃 key 在无人监听时长度恒为 1 且带 TTL（`Llen` ≤1 断言 + TTL 存在）。
- `go build ./...`、`golangci-lint run ./modules/bot_api/... ./modules/robot/...` 干净。
- `make i18n-extract-check` + `make i18n-lint` **无变化**（预期未触 errcode）。

## 待确认（human confirm）

1. **`wait` 默认值与上限** —— 倾向 `默认 0（不等）/ 上限 30s`。默认 0 保证存量 bot
   零行为变化；上限 30s 是「远小于常见反代 60s 空闲超时」的保守值。若部署侧反代
   idle timeout 更短，需相应下调。
2. **「零适配」的准确含义** —— 本设计下存量 bot **完全不受影响但也拿不到收益**，
   要享受秒级时延必须主动传 `wait`。这与「同端点、同响应形状、SDK 无需改造即可继续工作」
   一致，但不等于「什么都不改就变快」。若期望后者（默认开启 long-poll），
   需接受存量 bot 的 HTTP client 超时风险，请明确拍板。
3. **并发 hold 上限取值** —— 需先确认 `ctx.GetRedisConn()` 主池的 `PoolSize`
   （本仓限流类独立 client 显式设 10，主池未见显式设置，可能是 go-redis 默认
   `10 × GOMAXPROCS`）。上限必须显著低于主池容量，避免 long-poll 饿死普通 Redis 调用。
