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
- **入队点有 5 个，不是 2 个（本条最初写错，PR#685 review 更正）**：

  | 生产者 | ZADD 位置 | 投递内容 |
  |---|---|---|
  | `enqueueBotEventGeneric` | `modules/robot/api.go:233` | OBO 扇出等合成事件 |
  | `enqueueBotTypedEventGeneric` | `modules/robot/api.go:274` | `card_action` |
  | **`saveRobotMessage`** | **`modules/robot/event.go:365`** | **普通 DM / @提及（量最大）** |
  | `Group.notifyBotJoinedGroup` | `modules/group/api.go:1982` | `bot_joined_group` |
  | `Service.notifyBotJoinedGroup` | `modules/group/service.go:2082` | `bot_joined_group` |

  本条原文写的是「两个 helper 统一了 GenSeq/ZAdd/Expire 形状，门铃写在这两处即可覆盖
  全部入队来源，不会漏」。**这是错的**，而错误来源值得记：`enqueueBotEventGeneric` 的
  docstring 当时声称自己被 `saveRobotMessage` 使用，实际从未被调用过。我信了注释、没有
  追调用图，实现继承了这个错误前提，结果量最大的那条路径没有摇铃——真上线后，一旦调用方
  按契约把轮询间隔归零，普通消息时延反而会退化到 chunk 边界（≤5s）。

  **纪律**：门铃必须挂在**每一个** ZADD 成功之后。这条不变量现在由
  `pkg/botevent` 的 `TestEveryBotEventQueueWriterRingsTheDoorbell` 源码守卫强制，
  不再依赖任何注释——注释正是这次出错的载体。
- **本仓已有 long-poll 先例**：`modules/robot/api.go:855` `inlineQuery` 用
  `select { case <-resultChan; case <-time.After(20s) }` + 408
  （`errcode.ErrRobotInlineQueryTimeout`）。**但它是进程内 channel map，只在单副本正确**
  —— 本任务不沿用该形状（octo-server 是多副本部署，见 #627 的跨副本语义与 runtime-catalog
  的 multi-replica recovery 讨论）。
- **HTTP 层不会腰斩 hold**：`WKHttp.Run` 走 gin/`http.ListenAndServe` 的零值 `http.Server`
  ——无 `ReadTimeout`/`WriteTimeout`。反过来说**没有服务端兜底**，handler 必须自带 deadline。
- **`BLPop` 无 context**：octo-lib `redis.Conn.BLPop(key, timeout)`（`pkg/redis/redis.go:445`）
  直通 go-redis v6 `client.BLPop`，不接受 `context`，无法被客户端断连取消。故实现必须把
  hold **切成若干短 chunk**（每 chunk ≤ 5s），chunk 之间检查 `c.Request.Context().Done()`
  与进程关停信号，把「客户端已走但连接仍被占住」的窗口压到一个 chunk 以内。
- **阻塞命令不会被 socket 读超时腰斩（已核实）**：go-redis v6 `commands.go:1009-1020`
  的 `BLPop` 显式 `cmd.setReadTimeout(timeout)`，而 `redis.go:224-230` 对带 readTimeout 的
  命令返回 `t + 10*time.Second` 作为连接读 deadline。故 `BLPop(key, 5s)` 的实际 socket
  deadline 是 15s，**默认 `ReadTimeout` 不会打断它**——门铃方案在这套 client 上成立。
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
  - **long-poll 必须用独立 Redis client，不得占用 `ctx.GetRedisConn()` 主池**（已核实：
    octo-lib `config.Context.NewRedisCache()` 建主池时只设 `Addr`/`Password`(+TLS)，
    **没有显式 `PoolSize`** → 取 go-redis v6 默认 `10 × runtime.NumCPU()`，且该池被全进程
    共享。`BLPop` 每个 hold 独占 1 条连接，若走主池，几十个 bot 同时 long-poll 就能把
    普通 Redis 调用饿死）。按本仓既有约定用 `redis.NewWithOptions` + 显式 `PoolSize`
    建专用 client（先例：`modules/user/api.go:198`、`modules/incomingwebhook/api.go:152`、
    `modules/file/api.go:100`、`modules/usersecret/api.go:53`、`modules/group/api.go:166`
    均显式 `o.PoolSize = 10`）。**专用池容量 = 并发 hold 上限 + 余量**，使 long-poll
    的最坏情况被结构性地关在自己的池里。
  - 进程级并发 hold 上限（信号量），取值 ≤ 专用池容量。**超限不报错**——降级为立即返回
    （等价今天的行为），fail-open 语义与既有限流中间件一致。
  - **每个 robotID 同时只允许一个 hold**：第二个并发 hold 立即返回，防止一个 bot
    反复开连接吃光专用池。
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
- **openclaw-channel-octo 侧的 `wait` 接入**：见下方「跨仓依赖」——不在本仓 acceptance 内，
  但**不是可有可无的 follow-up**：不改插件就拿不到任何收益。

## 跨仓依赖（openclaw-channel-octo — 不在本仓 acceptance 内）

服务端单独上线**不产生任何收益**，也**不造成任何损害**（opt-in 默认保证）。收益要插件侧接入，
已逐个核实改动面：

**为什么必须 opt-in（硬证据）**：插件 `src/api-fetch.ts:16` 定义
`EVENTS_POLL_TIMEOUT_MS = 10_000`，并在 `fetchBotEvents`（`api-fetch.ts:787`）对每次
`/v1/bot/events` 请求挂 `AbortSignal.timeout(...)`。**若服务端把 long-poll 设为默认开启且
hold > 10s，现存插件会每 10 秒 abort 一次并打一条 error 日志**，空闲请求量反升至今天的 3 倍。
这独立证明了「默认 `wait=0`」不是保守，是必需。

插件侧改动面（4 处，均已定位）：

| # | 改动 | 位置 |
|---|---|---|
| 1 | 请求体带 `wait` | `src/api-fetch.ts` `fetchBotEvents` body（一行） |
| 2 | **客户端超时抬到 hold 之上**（hold 25s → 超时 ≥35s） | `src/api-fetch.ts:16` 常量 + :14-16 的「Cap each request well under any reasonable poll cadence」注释（前提已变） |
| 3 | long-poll 模式下轮询间隔归零 | `src/events-poll.ts:88` `schedule()` 现无条件等 `intervalMs`（默认 2000ms），不改则每次返回后白等 2 秒，吃掉一半收益 |
| 4 | 配置开关 + 文案 | `src/config-schema.ts:137/162` + `openclaw.plugin.json:51/119`（`pollIntervalMs` 已在此四处，新开关需同步）；`src/events-poll.ts:78` 的 "short-poll loop" 文档注释 |

**插件侧副作用（需一并处理）**：`api-fetch.ts:14` 注释指出这是**单条顺序循环**，一次挂住的
请求会推迟该账号的后续处理。今天上限 10s，改为 25s hold 后 `stop()` 的响应性最坏要等 25s
——插件停止时必须用 `AbortController` 主动打断，不能只靠超时自然到期。

**收益的诚实量化**：插件当前轮询间隔默认 **2s**（`events-poll.ts:10`，可配、下限 500ms），
所以时延收益约为**平均 1 秒**，并非「秒级 vs 分钟级」。真正的大头是**空闲请求量**：
每 bot 30 次/分钟 → long-poll 后约 2 次/分钟，bot 越多差距越大。

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

## 已知偏差与盲点（实现后 review 记录）

1. **关停唤醒是近似的，不是精确的。** Load-bearing 里要求「关停中的 hold 必须被唤醒」，
   实现用 chunk 上界（≤5s）逼近：`register.Module` 没有 Stop 钩子，`main.go` 也没有对
   API server 做 graceful shutdown，加一个无人调用的 `Stop()` 只会是死代码。净效果是
   drain 最坏被拖长 **5s**（而非一个完整的 30s hold），满足「不得拖长一个完整 wait 周期」
   的原意，但不等于零延迟。真要精确，需先给模块引入统一的关停信号，属独立改动。
2. **门铃失败无信号。** 生产者 `_ = botevent.Ring(...)` 丢弃错误且不记日志（与相邻的
   best-effort `Expire` 刷新同一约定）。持续失败会让所有 hold 静默退化成等满超时，且**没有
   任何可观测信号**。这条盲点归 G1（card ingress 仪表）收口，它拥有指标命名空间；
   在叶子包里临时加 logger 会把第一个计数器放错位置。
3. **并发预算是进程级，非集群级。** N 个副本下同一 bot 可各占一个 hold，全局上限为
   `maxEventHolds × N`。有界且符合预期（被保护的资源——本副本的专用 Redis 池——本身就是
   进程级的），但不要当成分布式不变量。
4. **hold 时长会超出请求值 <1s。** chunk 向上取整到整秒（BLPOP 参数是整秒，向下取整会
   退化成 0 = 永久阻塞）。宁可超出不到一秒，也不能像最初实现那样把 `wait=2` 截短成 ~1s。

## 已定稿（2026-07-31，maintainer 拍板）

1. **`wait` 默认 0、上限 30s。** 默认 0 保证存量 bot 零行为变化；30s 远小于常见反代 60s
   空闲超时。若部署侧反代 idle timeout 更短，下调该上限即可，无契约影响。
2. **opt-in，不默认开启。** 存量 bot 完全不受影响、但也拿不到收益；要低时延必须主动传
   `wait`。理由不止「保守」：插件侧 `EVENTS_POLL_TIMEOUT_MS = 10_000` 的硬超时使默认开启
   会直接制造错误日志风暴（见「跨仓依赖」）。
3. **专用 Redis client + 显式 `PoolSize`，不占主池。** 主池无显式 `PoolSize`
   （= `10 × NumCPU` 默认）且全进程共享，`BLPop` 每 hold 独占一条连接，必须隔离。
   并发 hold 上限 ≤ 专用池容量，两者同处一份配置、不得各自漂移。
