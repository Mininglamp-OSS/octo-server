---
type: Task
title: "Task: bot-api-per-bot-ratelimit"
description: 把 bot 流量的限流维度从「出网 IP」改为「bot 身份」，并给 heartbeat 与 register（token 刷新）两条自愈通道各自的保命配额——否则限流会掐断"爬起来"的能力；配额定值先补 per-bot 观测，今天没有数据支撑。
tags: ["bot-api", "rate-limit", "throttle", "wire-contract", "error-response", "test", "commit"]
timestamp: 2026-08-05T00:00:00+08:00
# --- octospec extension fields ---
slug: bot-api-per-bot-ratelimit
upstream: Mininglamp-OSS/octo-server#696
source: self
---

# Task: bot-api-per-bot-ratelimit

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

`/v1/bot` 主组今天**没有任何 per-endpoint / per-bot 限流**——`bot_api.go:283` 是
`r.Group("/v1/bot", ba.authBot())`，唯一约束是 `main.go:222` 用 `route.Use` 挂载的
**全局 per-IP 令牌桶**。因为桶按 client IP 分片，**同一出网 IP 上的所有 bot 共享一份配额**：
一个 bot 打满，同 IP 的其它 bot 全部连坐 429，包括 `/v1/bot/heartbeat`，进而断联。

本任务把 bot 流量的限流维度从 IP 迁到 bot 身份，并让 heartbeat 拥有独立于业务流量的配额。

**核心设计约束（这条决定了方案形状，也是最容易做错的地方）**：

> 全局 per-IP 限流是 `route.Use` 挂的，对**所有**路由生效，**组级中间件无法绕过它**——
> 它跑在 `authBot` 之前。因此「给 bot 主组补一个 per-bot 桶」**并不能**解决 heartbeat
> 被饿死：per-IP 桶在更前面，先拒的是它。

要让 heartbeat 真正保命，只有把 `/v1/bot/heartbeat` 加进
`globalRateLimitExcludePaths()`（`main.go:64`，当前只有 `/v1/ping`、`/v1/health`；
lib 侧是 `excludeSet[c.Request.URL.Path]` **精确匹配**，heartbeat 是固定路径，可用）。
而一旦 exclude，该端点就失去 IP 层防护——未鉴权请求也能一路打到 `authBot`（它要查
Redis/DB），**所以 exclude 必须和一个自有的桶成对出现，不能单独做**。

因此本任务是四件不可拆的事 + 一件前置：

| # | 动作 | 解决 |
|---|---|---|
| 0（前置） | **影子模式**：用候选阈值跑 dry-run 判定 + 有界指标（见「观测方案」） | 直接回答「设成 X 会误伤谁」，跳过「先统计再猜阈值」这一步 |
| 1 | `/v1/bot` 主组挂 `botActorUID()` + **独立的** per-bot 令牌桶 | 一个 bot 异常不再连坐邻居 |
| 2 | `/v1/bot/heartbeat` **加入** `globalRateLimitExcludePaths()` + 配一个自有的桶 | 心跳不被同 IP 业务流量挤占；exclude ≠ 无限制，两者必须成对 |
| 3 | `/v1/bot/register`（token 刷新）同样需要保命通道 | **自愈链路自身被限流 ⇒ 永远无法恢复**，见 Background「二次事故」 |

> 第 (3) 项是 2026-08-05 二次事故后补入的，**原 brief 遗漏**。它比 (2) 更棘手：
> `register` 挂在 botAPI 组**之外**（`bot_api.go:280` 的 `r.POST`），跑在 `authBot` 之前，
> **拿不到 bot 身份**，因此 per-bot 桶这条路在它身上不成立。限流维度需另选，见「待人工确认」。

## Background

### issue #696 现场（2026-08-05 生产日志实测）

- 报告现象：`xiaow_king_bot` 心跳连续 429（`err.shared.rate.limited`, `retry_after: 1`），
  断联约 40 分钟。
- **归因与报告不同**：报告假设是「业务消息 API 与 heartbeat 共用同一限流桶」。共享是真的，
  但共享的轴是 **IP**，不是「业务 API vs heartbeat」。实测同一时段，**同一出网 IP 上另一个
  bot 以约 590 rps 持续冲刷 `/v1/bot/typing`**，单机就把 500 rps 的 per-IP 桶打满；
  该 IP 上所有端点（`heartbeat` / `sendMessage` / `card/profile`）无差别被拒。
  报告里的 bot 很可能**不是**自己饿死自己，而是被同 IP 邻居饿死。
- 该冲刷 bot 的请求速率**在限流放宽前后完全一致**（单副本 230 → 211 rps），说明它
  **完全无视 429、没有任何退避**，不是重试放大循环。这条对定值有直接影响：
  per-bot 桶拦住它之后，它不会自发降速，只会持续撞墙——所以桶必须能长期承受被撞。
- 该 bot 的 typing 请求里约 **19.5% 是 400**（`channel_id` 空或 `channel_type=0`，
  `typing.go:45-52`），即近两成配额消耗在客户端参数 bug 上。
- 部署侧已把全局 per-IP 上限从 `500 rps / burst 1000` 调到 `1500 / 3000` 止血，429 归零。
  **这是缓冲不是修复**：维度仍然错的，且冲刷源现在只是落在新天花板之下。

### 二次事故（同日，止血上线之后）—— 本 brief 的最大修订来源

止血（全局 per-IP 500→1500）滚动更新期间及之后，另一个 bot 报掉线且**自己起不来**。
复盘出两件原 brief 没覆盖的事：

**(a) 自愈链路自身被限流，形成死锁。** 完整链条：

```
IP 桶打满 → heartbeat 429 → 心跳 key（TTL 60s）过期 → 连接被判定失效
    → 客户端走重连 → 重连需要 POST /v1/bot/register 刷新 IM token
    → register 也在同一个全局 per-IP 桶下 → 429
    → 无法恢复，只能等人工干预
```

原 brief 只保了 heartbeat。**保住心跳但不保住 register，等于保住了"发现自己掉线"的能力，
却没保住"爬起来"的能力**——最终结果一样是断联。凡是"失败后用于恢复"的端点都必须在
保命集合里，这是本次得到的一般性教训，不止 register 一个点。

**(b) 认证失败的重试风暴。** 事故后统计 3 分钟窗口，`/v1/bot/register` 单一来源 IP 打了
**709 次 400** + 82 次 200（约 4 rps 的纯失败重试），另有两个 IP 各 104 / 55 次 400。
读 `register.go:48-58`，返回 400 且**不打日志**的分支只有「token 为空」和
「`queryRobotByBotToken` 查不到 robot」两条，日志中确认无对应 error 记录 ⇒ 即 **token 无效**。

**token 无效不会靠重试自愈**，但客户端把它和 429 用了同一套重试策略，于是无效 token 的 bot
持续以数 rps 冲刷 register。这直接消耗 IP 配额，是下一次限流事故的种子。根因之一在服务端：
`httperr.ResponseErrorL` 是**固定 400** 的门面（D14 兼容），**鉴权失败与参数错误在线上不可区分**，
客户端没有可靠依据把「该停下」和「该退避重试」分开。

**(c) 滚动更新窗口内限流行为会抖动（运维须知，非缺陷）。** 改 `rps/burst` 需滚动重启，
而新旧副本**共用同一个 Redis 桶 key**、却各自把自己的 `rps/burst` 作为参数传给 Lua 脚本
（`newKeyedLimiter` 持有值，脚本按传入参数计算填充与上限）。因此滚动期间同一个桶被两组参数
交替操作，旧副本会持续把 token 数压回旧 `burst` 上限。二次事故的最后一波 429 正落在这
93 秒窗口内。**将来调整 per-bot 桶参数时同样适用**，需要在变更窗口内接受一段抖动。

### 为什么观测必须先行

`/v1/bot` 的访问日志**不含 `robot_id`**，且**限流拒绝路径完全不写日志**
（`ratelimit.go` 的 `!allowed` 分支只设响应头后 `c.RenderError` + `c.Abort`）。
因此今天**无法**从任何现有数据推出「单个 bot 的真实 QPS 分布」，
而这正是 (1)(3) 定值所需的输入。本次事故只能靠人工翻 access log 复原，本身即证据。

### 为什么不能直接复用 `SharedUIDRateLimiter`

`pkg/wkhttp/ratelimit_helper.go:53` 的 `SharedUIDRateLimiter` 是**进程级单例**
（`uidRateLimitReady` 守卫），全进程共用一份 `rps/burst`（默认 `2.0 / 60`，
env `DM_API_UID_RATELIMIT_RPS`/`_BURST`）。它服务的是**登录用户**维度。
bot 的合理速率高于人类用户一个量级，改这个单例的值会**同时放宽所有用户路由**。
故本任务需要**另建一个 limiter 实例**（自有 env、自有 keyspace 前缀），
不得改动 `SharedUIDRateLimiter` 的默认值。

`modules/bot_api/search_route.go:51` 与 `incoming_webhook.go:26/34` 已经在 bot 路由上
挂了 `SharedUIDRateLimiter`——那是可接受的，因为搜索/webhook 本身低频；
但主组（sendMessage / typing / events）不能沿用 `2 rps`。

### `botActorUID()` 的语义风险（已核实，结论是安全的）

`botActorUID()`（`incoming_webhook.go:41`）做的是 `c.Set("uid", robotID)`，
而 lib 的 `Context.GetLoginUID()`（`http.go:140`）正是读 `c.Get("uid")`。
所以在主组挂它，等于让主组内任何读 `GetLoginUID()` 的 handler 拿到 **robotID 而非登录用户**。

已逐个核实：`modules/bot_api` 内共 7 处 `GetLoginUID()`，**全部**在
`oboCreateGrant` / `oboListGrants` / `oboDeleteGrant` / `oboUpdateGrant` /
`oboCreateScope` / `oboDeleteScope` / `oboListScopes`——这些挂在 **`/v1/obo/*`
user-token 组**，不在 `/v1/bot` 主组。主组内唯一的 obo handler `oboBotGetGrant`
（`obo_api.go:640`）**不读** uid。

⇒ **当前挂载是安全的**。但这是一条"今天为真"的性质，不是结构性保证：
主组将来任何新 handler 只要写 `c.GetLoginUID()` 就会静默拿到 robotID。
本任务需落一条源码守卫把它钉死（见 Acceptance）。

## Load-bearing list

- **`rate-limit` / `throttle`** — 本任务的主轴：
  - 新 limiter 必须走 lib 的 `UIDRateLimitMiddleware`（或同族），
    **不得手写 Redis INCR/TTL 计数器**（repo rule `rate-limit`）。
  - **fail-open 语义必须保持**：Redis 故障放行 + 告警，不得改成 fail-closed。
    限流层把基础设施抖动放大成业务不可用，是比超发更坏的失败模式。
  - `UIDRateLimitMiddleware` 在 `uid` 缺失时**fail-open 放行**（`ratelimit.go` 的
    `c.Get("uid")` 未命中即 `c.Next()`）。故挂载顺序**必须**是
    `authBot()` → `botActorUID()` → limiter，顺序错则静默失效、且测试很难发现。
  - keyspace：`UIDRateLimitMiddleware` 硬编码前缀 `ratelimit:uid:`。若新 limiter 复用该
    中间件，bot 与登录用户**共用同一前缀**（key 不撞，因为 robotID 形如 `xxx_bot`、
    用户 uid 是 32 位 hex）。若要独立前缀需 lib 侧支持，属跨仓改动——**取舍需在实现前定**。
  - 全局 per-IP 桶的 `rps/burst` **不在本任务内改动**（已由部署侧调整）。
- **全局 exclude 列表语义** — `globalRateLimitExcludePaths()`（`main.go:64`）+
  `main_test.go:14` 的既有断言。lib 侧是 `c.Request.URL.Path` **精确匹配**，
  不支持前缀/通配。加 `/v1/bot/heartbeat` 后 `main_test.go` 需同步更新。
  **每加一条 exclude 都是在全局 DDoS 底线上开洞**，必须逐条论证并配自有桶。
- **`bot-api` / `wire-contract`** — 429 的线上形状不得变：
  - 响应体沿用 `err.shared.rate.limited` 信封（`pkg/i18n/codes/shared.go:68`，
    `HTTPStatus: 429`），`details.retry_after` 语义不变。
  - `X-RateLimit-Limit` / `-Remaining` / `-Scope` / `Retry-After` 四个头必须继续下发。
    `Scope` 是客户端归因 429 来源的**唯一**手段（`ip` / `uid` / `strict:{tag}`），
    多层挂载时后写者覆盖前写者——新增一层会改变客户端观察到的 `Scope` 值，
    这是**可观察的契约变化**，需在 issue #696 同步给插件侧。
  - 正常速率下的 bot **不得**因本改动收到任何新的 4xx。
- **`error-response`** — 复用既有 `err.shared.rate.limited` 还是为 heartbeat 单独注册
  errcode，需在实现前定。倾向复用（客户端已按该 code 分支处理），
  但若复用则 `Scope` 头是唯一区分手段。任何新 code 都要过
  `make i18n-extract-check` + `make i18n-lint` + zh-CN 翻译。
- **`botActorUID()` 的 uid 副作用** — 见 Background。当前安全，需守卫钉死。
- **heartbeat 语义** — `bot:heartbeat:{robotID}`，TTL **60s**
  （`bot_api.go:28-29`，写入在 `typing.go:225-235`）。心跳桶的最小速率必须
  **显著**高于 `1/60s`，否则一次限流就可能让 key 过期。定值须以此为下界。
- **`/v1/bot/register` 是自愈链路的最后一环，且限流维度受限** —
  它挂在 `bot_api.go:280` 的 `r.POST("/v1/bot/register", ...)`，**不在 botAPI 组**：
  - 跑在 `authBot` **之前** ⇒ 无 bot 身份 ⇒ **per-bot 桶这条路在它身上不成立**。
  - 但它照样受全局 per-IP 桶约束 ⇒ 它是死锁链的闭合点（Background 二次事故 (a)）。
  - 它会调 `ctx.UpdateIMToken`（`register.go:61-67`，`DeviceFlag: APP` /
    `DeviceLevel: Master`），即**每次成功 register 都会改写 IM token**。因此它既是恢复
    通道、又是一个有副作用的写操作，**不能简单地整体豁免限流**——豁免它等于给一个
    无鉴权（authBot 之前）、能触发下游写的端点开无限额度。
  - 它是**未鉴权可达**的（token 在 body/header 里，由 handler 自己解析），
    所以任何限流方案都必须能在「拿不到合法身份」的前提下工作。
- **认证失败与限流在线上不可区分（`error-response` 的具体化）** —
  `httperr.ResponseErrorL` 固定返回 **400**（D14 兼容），所以 `register` 的
  「token 无效」和参数错误同为 400，而限流是 429。客户端据此**无法**把
  「该停下」（400 = 凭据失效，重试无意义）与「该退避重试」（429 = 瞬态）分开，
  实测后果是无效 token 以数 rps 冲刷 register（Background (b)）。
  本任务**不改** `ResponseErrorL` 的固定 400 契约（那是全仓级决策），
  但需确认：新增的限流层是否让这个区分**更难**（例如 429 被后写的 `Scope` 头覆盖）。
- **滚动更新窗口内桶行为会抖动** — 新旧副本共用同一 Redis 桶 key、各传各的
  `rps/burst`（Background (c)）。因此**任何桶参数变更都不是原子的**，
  验收/灰度不能在滚动窗口内取样，否则会把过渡态误判为新配置的行为。
- **typing 现有节流** — `typing.go:127-174` 的 `typing_start` / `typing_count`
  （90s 窗口 × 3 / 180s TTL）。它命中时 **返回 `200 OK`**，只抑制向 WuKongIM 的 CMD 派发，
  **不减少 HTTP 请求量**——所以它对令牌桶完全无效，本次事故中形同虚设。
  本任务**不改**它（见 Out of scope），但新 per-bot 桶会与它叠加，
  需确认两者不会互相掩盖（一个返回 200 一个返回 429，客户端观感不同）。
- **`test`** — 命中 UID 桶的集成测试必须在 setup 里 reset `ratelimit:uid:*`
  （repo rule `rate-limit` 的 testing note；`CleanAllTables` 不清 Redis）。
  新桶若用新前缀，同样要加进各测试的 reset 列表，否则**跨测试污染**会让
  整包 `go test ./modules/bot_api/...` 随执行顺序 flaky。
- **既有 bot API 测试是回归基线** — `modules/bot_api` 下的既有测试不得破坏。

## 观测方案（阶段 0）

### 目标不是「统计 QPS」，是回答「设成 X rps 会误伤谁」

先收集速率分布、再据此推阈值，中间隔着一次猜测。**影子模式（dry-run）直接跳过这次猜测**：
挂上 limiter、跑完整令牌桶判定、但**不拦截**，只记录「若拦截会拒多少、拒的是谁」。
候选值合不合适一眼可见，这也是限流上线的标准做法。

因为限流器是本仓自建的（见「配置与运行时可调性」选型），**dry-run 无需任何跨仓改动**，
就是判定之后的一个分支：

```go
// 形状示意，非最终签名
allowed, err := limiter.Allow(key)     // 完整执行，含 Redis
observe(class, robotID, allowed, dryRun)
if dryRun {                            // 影子：只观测，不改变任何可观察行为
    c.Next()
    return
}
if !allowed { /* 429 + X-RateLimit-* 头 */ }
```

`DryRun=true` 时**必须**：完整执行桶判定（含 Redis 写，成本要真实暴露）、
调用 `OnDecision`、但**不 abort、不设 `X-RateLimit-*` 头、不改响应**。
最后一条是硬要求：影子期若下发了限流头，客户端可能据此自行降频，观测到的就不再是真实流量。

### 两条硬约束（实测得出，决定了指标形状）

1. **活跃 bot 2903 个**（Redis `bot:heartbeat:*`，TTL 60s）。
   `robot_id` 作 Prometheus label ⇒ 2903 × class × decision ≈ 数万 series 且**随 bot 增长无上界**。
   ⇒ **禁止把 `robot_id` 放进 Prometheus label。**
2. **集群无日志采集**（已核实：无 Loki / Fluent / Filebeat / Logstash / Vector）。
   应用日志只存在于 pod 内，重启即丢。
   ⇒ access log 补 `robot_id` 仍要做，但它提供的是**现场排查**能力（`kubectl logs` 现捞），
   **不是**历史分析能力。定值不能依赖它。
   ⇒ 监控侧已有 Prometheus（`monitor` ns，两副本）+ Grafana，观测走这条。

### 三层设计（各司其职，基数全部有界）

| 层 | 载体 | 基数 | 回答什么 |
|---|---|---|---|
| 趋势/总量 | Prometheus counter | **12**（class×decision） | 「整体会拒多少」「拒绝率变化趋势」 |
| 定位 | Redis ZSet | **有界（top N）** | 「具体是哪几个 bot」 |
| 现场 | access log | — | 「这一刻发生了什么」（仅 pod 内） |

**第 1 层 — Prometheus（无身份）**
- `octo_bot_ratelimit_decisions_total{class, decision}`，
  `decision ∈ {allow, would_deny, deny}`，`class ∈ {business, heartbeat, register, events}`。
  用 `class` 粗分类而非具体 path（bot 端点 40+ 个，放进 label 就白省了 `robot_id` 的基数）。
- `would_deny` 与 `deny` **必须是两个不同的值**，否则影子期和执行期的数据无法对比。

**第 2 层 — Redis ZSet（有身份，有界）**
- `ratelimit:shadow:offenders:{class}`，member = robotID，score = would-deny 累计；
  写入后 `ZREMRANGEBYRANK` 只保留 top N（建议 50），**基数结构性有界**。
- 定位「谁会被误伤」只需一条 `ZREVRANGE ... WITHSCORES`。
  本次事故那种「单个 bot 打爆」的形态，这条命令一眼就能看出来。
- 该 key 必须有 TTL，且**只在 dry-run 或拒绝发生时写**，不能每请求都写。

**第 3 层 — access log**
- `/v1/bot/*` 的 access log 补 `robot_id`。**不得**记录 token 或任何凭据
  （参考 `#246` incoming-webhook 的 token-in-log 教训，`accesslog.Formatter` 已有脱敏先例）。

### 定值流程（把「没数据不敢定」这个死结解开）

1. dry-run 开启，参数设为候选值（保守偏紧）。
2. 看 `would_deny` 占比 + ZSet 里的 offender 名单。
3. **名单集中在少数已知异常 bot** ⇒ 阈值合适，可以关掉 dry-run 执行。
4. **名单散布在大量正常 bot** ⇒ 阈值过紧，热调调高（60s 生效），回到第 2 步。
5. 执行后继续盯 `deny`，异常立即用 kill switch 关闭整层。

这条流程是「风险取决于回退多快、而非初始值多准」的具体落地，
也是**阶段 0 与阶段 1 可以合并发布**的前提——dry-run 本身就是安全的灰度。

## 配置与运行时可调性

### 为什么这一节是硬要求，不是锦上添花

**本次事故的次生伤害直接来自「限流参数不能热调」**：改 500→1500 必须滚动重启，
93 秒窗口内新旧副本共用同一个 Redis 桶、却各传各的 `rps/burst`，行为抖动；
一个 bot 恰好在这个窗口里被踢下线且无法自愈（Background 二次事故）。
**如果参数当时能热调，二次事故不会发生。** 所以本任务引入的每一个新桶，
如果同样只能靠重启调整，等于把这个故障模式复制三份。

### 仓内先例：三级配置 + 每请求读取（incomingwebhook）

per-webhook 桶已经是目标形状，直接照抄即可：

- `SystemSettings.IncomingWebhookPerWebhookRPS()`（`modules/common/system_settings.go:760`）
  —— **DB(`system_setting`) → env → 代码默认** 三级。
- 限流器**每次请求读**（`modules/incomingwebhook/ratelimit.go:141`），
  所以改 `system_setting` 后最多 `defaultReloadTTL = 60s`（`system_settings.go:73`）生效，
  **无需重启**。
- **读侧防御（踩过的坑，必须照抄）**：NaN / ±Inf / ≤0 一律回退到 env/代码默认。
  原因写在那个 getter 的注释里——`rps<=0` 会让限流器**静默关闭**，
  NaN 会让 Redis Lua 脚本报错进而 **fail-open**；而 `ParseRPSFromEnv` 底层是
  `strconv.ParseFloat`，**会接受 `NaN` / `+Inf`**，所以 env 兜底本身也可能非有限值。
  写侧校验已存在，读侧是纵深防御，覆盖直接改库的旁路。

### 架构约束与选型（已定稿：本仓自建，零跨仓）

lib 的三个中间件在**构造时**固定 `rps/burst`（`newKeyedLimiter` 持有值，
middleware 只创建一次）⇒ **用 lib 中间件 = 改参数必须重启**。三条出路：

| | 方案 | 评价 |
|---|---|---|
| (a) | 接受重启生效，与全局桶一致 | ❌ 把本次事故的次生伤害模式复制三份 |
| (b) | 给 octo-lib 中间件加 `func() (float64, int)` 变体 | 🔻 **后备**：能力正确，但为一件本仓能做的事付跨仓成本 |
| (c) | **本仓自建（`incomingwebhook` 形状）** | ✅ **选定** |

**选 (c) 的依据是 `incomingwebhook` 已经把这条路走通并在生产验证过**，四件事全现成：

| 能力 | 现成实现 |
|---|---|
| 热调（每请求读配置） | `allowPerWebhook` 内 `w.settings.IncomingWebhookPerWebhookRPS()`（`ratelimit.go:141`） |
| **写侧校验** | schema `Type: settingTypeFloat, Positive: true`（`system_setting_schema.go:153`） |
| 读侧防御 | getter 内 NaN/±Inf/≤0 回退（`system_settings.go:760`） |
| 生效值可见 | schema 的 `Effective: func(s) string`，管理端显示 env fallback 后的实际值 |

且**令牌桶本身与 webhook 语义解耦**：`tokenBucketScript`（`ratelimit.go:19`）是通用 Lua，
`runBucketScript(script, key, args...)` 只耦合 `w.rateRedis` 与 `w.warnDegraded` 两个字段。

**dry-run 也因此不需要改 lib**——自建中间件里它就是一个分支：跑完判定 → `observe()` → `c.Next()`。

**repo rule `rate-limit` 的例外论证**（实现时需在代码注释中写明）：
rule 要求 authenticated routes 挂 `SharedUIDRateLimiter`，但它是**进程级单例**
（`ratelimit_helper.go:53` 的 `uidRateLimitReady` 守卫），bot 与登录用户**共用同一套
`rps/burst`**。本任务需要的是「独立配额值 + 运行时热调」，共享单例在结构上表达不了。
这与 per-webhook 桶同构——同样是「按业务身份的独立配额，共享中间件表达不了」，
落在 rule 的 Exception 条款内。**不是绕过 rule，是命中它已写明的例外。**

### 代码放置（一处取舍）

`tokenBucketScript` + `runBucketScript` 有两种去处：

- **(A) 新建本仓 `pkg/ratelimit`，只给 bot_api 用** —— 不动正在生产跑的 webhook 限流，
  代价是暂时存在两份令牌桶实现。**选 (A)**，并记 follow-up：择机把 incomingwebhook 收敛过来。
- (B) 一次性提取、两边共用 —— 更干净，但要改动生产中的 webhook 限流路径，
  收益（消除重复）不值当前的回归风险。

### 配置项清单（每一项都要有，缺一不可）

| 配置项 | 建议默认 | 生效方式 | 备注 |
|---|---|---|---|
| per-bot 层 `enabled` | **false** | 热调 | kill switch |
| per-bot `rps` / `burst` | 见待确认 1 | 热调 | **集群级**配额，非每副本 |
| heartbeat 层 `enabled` | **false** | 热调 | |
| heartbeat `rps` / `burst` | `rps` 下界 > `1/60` | 热调 | 下界见 Load-bearing |
| register 层 `enabled` | **false** | 热调 | ⚠️ 误配后果是**所有 bot 无法注册** |
| register 参数 | 见待确认 5 | 热调 | |
| 观测层 `enabled` | true | 热调 | |

- **每一层必须有独立的 `enabled` 开关，且默认 `false`**。这不是保守：
  register 层误配的后果是「全部 bot 无法注册」，而回滚发版比翻开关慢一个数量级。
- 所有数值项照抄 incomingwebhook 的**读侧防御**（NaN/Inf/≤0 → 回退）。
- `enabled=false` 时必须**完全旁路**：不读 Redis、不设 `X-RateLimit-*` 头，
  行为与改动前逐字节一致（否则 kill switch 本身成了新的故障面）。

### kill switch 的时效性（已定稿：统一 60s，不追求秒级）

`system_settings` 是内存快照 + 多实例 **60s** 收敛（`defaultReloadTTL`，`system_settings.go:73`）。

本仓历史上有过「60s 对灰度回滚不够快、要求秒级」的决策，并为此设计过独立的 Redis 缓存方案
（`featuregate`）。**但该方案从未落地**——已核实当前 main 上无 `pkg/featuregate` /
`modules/featuregate`、git 中无相关文件、无远程分支、全仓零 Go 引用。
而**真正上线并运行至今的 `incomingwebhook` 总开关用的就是 `system_settings` 的 60s**
（schema 注释原文：「其余三项实时调阈值无需重启（SystemSettings 快照 60s 内多实例收敛）」）。

⇒ **统一走 `system_settings`**，与既有生产实践一致，**不引入第二条配置读路径**，
不破坏「单一真源」。60s 的止损延迟是已被接受的水平；若日后确认不够，再单独论证。

### 这改变了发布策略（对原阶段划分的修正）

原 brief 论证「观测先行，收一周数据再定值」。**若热调能力 + kill switch 先落地，
该论证不再成立**：可以先上保守值并灰度开启，观测同时进行，
发现误伤时 60 秒内调宽或直接关闭。这比等一周更早拿到隔离收益，且风险可控——
**风险的大小取决于回退有多快，而不取决于初始值有多准。**

⚠️ **前提是 (b) 先落地**。若最终选 (a)（重启生效），则退回原方案「先收数据再定值」——
那时调错一次的代价是一轮滚动重启加一个抖动窗口，不再便宜。
**这两个决定是绑定的，不能分开拍。**

### 集群级 vs 进程级（别搞反）

lib 令牌桶状态存 Redis，**所以本任务所有桶都是集群级共享配额**：配置的 `rps` 是
**全站合计**，不是每副本。这与 `bot-events-longpoll` 的并发 hold 预算（进程级，
N 副本即 N 倍）语义相反，实现、注释、运维文档都不要混。

## Out of scope

- **不改全局 per-IP 桶的 `rps/burst` 值**——已由部署侧调整为 `1500/3000`，属运维配置。
- **不改 `SharedUIDRateLimiter` 的默认值**——它服务登录用户，动它会波及全部用户路由。
- **不修 typing 的应用层节流返回 `200` 的问题**。它是真问题（见 Load-bearing），
  但改成 429 是**可观察的行为变更**，会影响全部存量 bot，需要独立评估与灰度——
  另开任务，issue #696 已记录。
- **不修客户端参数 bug**（typing 的 19.5% 400、register 的失败重试策略）。
  那在 bot / 插件侧，本仓改不了。已在 issue #696 记录：**400 与 429 必须走不同的重试策略**
  ——429 退避重试，400（凭据失效）应停止并告警。
- **不改 `httperr.ResponseErrorL` 的固定 400 契约**。它让鉴权失败与参数错误在线上不可区分
  （Background (b) 的根因之一），但那是全仓级的 D14 兼容决策，改动面远超本任务，
  需独立评估。本任务只需保证不让这个区分**变得更难**。
- **不改滚动更新时的桶参数过渡行为**。新旧副本共用一个 Redis key 各传各的参数是
  lib 的既有设计，属运维须知而非缺陷（Background (c)）。
- **不做全站 bot 流量整体迁出 IP 维度**（即 exclude 整个 `/v1/bot/*`）。
  lib 侧 exclude 只支持精确匹配，需跨仓改动；且会让 bot 端点整体失去 IP 层 DDoS 防护。
  记为后续升级路径，不在本任务。
- **不改 `/v1/obo/*` user-token 组的限流**。
- **无 DB / 无迁移 / 不新增端点**。

## Acceptance

机器可校验（`go test -race ./modules/bot_api/... ./`）：

**阶段 0 — 观测（对应「观测方案」一节）**
- **dry-run 不改变任何可观察行为**：`DryRun=true` 时响应与未挂 limiter **逐字节一致**
  ——不 abort、**不设 `X-RateLimit-*` 头**、不改 body。以对拍断言。
  （下发限流头会诱导客户端自行降频，观测到的就不再是真实流量。）
- dry-run 仍**完整执行**桶判定并调用 `OnDecision`（断言 Redis 确实被调用，
  成本被真实暴露，不能因为"反正不拦"就跳过）。
- `would_deny` 与 `deny` 是**两个不同的 decision 值**，可分别累加。
- **Prometheus label 基数有界**：断言 `robot_id` **不出现在**任何 label 中；
  `class` 取值来自固定枚举而非 path 透传。以单测枚举全部 label 组合、断言上界。
- offender ZSet 结构性有界：写入后长度 **≤ N**（`ZREMRANGEBYRANK` 生效），
  且 key 带 TTL；仅在 would-deny / deny 时写，不是每请求写。
- access log 含 `robot_id`，且**不含 token 或任何凭据**（源码守卫或断言）。
- 拒绝路径**不产生每请求一条日志**（拒绝天然高频，日志会自我放大）。

**阶段 1 — per-bot 桶**
- 挂载顺序断言：`authBot` → `botActorUID` → limiter。**顺序颠倒时测试必须失败**
  （用「删掉修复看测试是否真的红」验证过，不接受只在正确顺序下变绿的测试）。
- 同一 bot 超配额 → 429 + 四个 `X-RateLimit-*` 头齐全，`Scope` 值符合预期。
- **隔离性（本任务的核心断言）**：bot A 打满自己的桶时，**bot B 的请求不受任何影响**——
  这是 issue #696 要的性质，必须直接断言，不能只断言"A 被限流了"。
- **heartbeat 不被业务流量挤占**：同一 bot 先用 `sendMessage`/`typing` 打满业务桶，
  随后 `heartbeat` **仍然 200**。
- Redis 不可达 → **放行**（fail-open）+ 告警，不返回 5xx、不返回 429。
- 正常速率的既有测试全部不回归（既有 bot API 测试零改动通过）。

**阶段 2/3 — 保命通道（heartbeat + register）**
- `globalRateLimitExcludePaths()` 含 `/v1/bot/heartbeat`；`main_test.go` 同步断言。
- heartbeat 专属桶存在且有上限：超过该上限仍返回 429（**exclude 不等于无限制**）。
- 桶速率下界断言：配置值必须 > `1/heartbeatTTL`（即 > 1/60 rps），
  以常量表达式或启动期校验钉死，防止将来有人把它调到"一次限流就断联"的水平。
- **`/v1/bot/register` 在业务流量打满时仍可用**（本任务新增的核心断言）。
- register 的限流层**不得**给未鉴权调用者开无限额度：无效 token 的连续调用最终被拒，
  且被拒时**不触发** `UpdateIMToken`（避免把恢复通道变成无鉴权的下游写放大器）。
- **死锁链不可复现（端到端，最有价值的一条）**：构造 Background (a) 的完整链条——
  同 IP 邻居打满业务配额 → 断言 `heartbeat` **与** `register` 均仍返回 2xx，
  即该 bot 具备完整的自愈能力。只断言其中一个不算通过。

**守卫**
- 源码守卫：`/v1/bot` 主组内的 handler **不得**调用 `c.GetLoginUID()`
  （挂 `botActorUID()` 后它返回 robotID，不是登录用户）。新增 handler 违反即测试失败。
- `Test<Module>NoLegacyResponseError` 既有守卫继续通过。

**配置与可回退（对应「配置与运行时可调性」一节）**
- 每一层 `enabled=false` 时**完全旁路**：不读 Redis、不设 `X-RateLimit-*` 头，
  响应与改动前**逐字节一致**（以对拍断言，不靠人眼）。
- `enabled` 由 `false` 翻到 `true` 后无需重启即生效；反向同理（**kill switch 可用性**
  必须被测到，不能只测开启路径）。
- 数值热调生效：改 `system_setting` 后限流行为随之改变，无需重启。
- **读侧防御**：`rps`/`burst` 取到 `NaN` / `±Inf` / `≤0` 时回退到 env/代码默认，
  且**限流仍然生效**（不得静默关闭、不得 fail-open）。逐值构造用例，
  含「env 本身就是 `NaN`」这一条（`ParseRPSFromEnv` 会原样接受）。
- heartbeat `rps` 下界校验：配置成 `≤ 1/60` 时被拒绝或夹紧，不允许落到
  「一次限流就断联」的水平。

**工程闸**
- `go build ./...`、`golangci-lint run ./modules/bot_api/... ./pkg/wkhttp/...` 干净。
- `make i18n-extract-check` + `make i18n-lint` 通过（若复用既有 code 则预期无变化）。

## 待人工确认（实现前需拍板）

0. ~~限流参数是否要做热调~~ **已定稿**：本仓自建限流器 + `system_settings` 热调，
   零跨仓改动（见「配置与运行时可调性」）。
1. **per-bot 桶的 rps/burst 初始值**。今天**没有**数据支撑（观测 gap 见 Background），
   本 brief 不给拍脑袋的数字。**因为第 0 项已定稿为可热调 + kill switch，
   路径是「先上保守值 + dry-run 观察 → 按数据收敛 → 翻开关执行」**，
   不必先等一周。**风险大小取决于回退多快，而非初始值多准。**
2. **keyspace 是否要与登录用户隔离**。复用 lib 的 `ratelimit:uid:` 前缀零跨仓改动
   但语义混杂；独立前缀更干净但需改 octo-lib。
3. **heartbeat 是复用 `err.shared.rate.limited` 还是单独 errcode**。
4. 阶段 0 的 metrics 命名空间是否与既有 `metrics.NewDependencyMetrics` 等对齐。
5. **`/v1/bot/register` 用什么维度限流**（二次事故后新增，本任务最大的未决项）。
   它在 `authBot` 之前、没有 bot 身份，per-bot 桶不适用。三条候选：
   - **(a) 独立的 `StrictIPRateLimitMiddleware(tag="bot_register", ...)`** —— 零跨仓改动、
     契合既有纪律（未鉴权敏感端点用 strict IP 桶，与 login/register/sms 同形）。
     但维度仍是 IP，**没有解决"同 IP 邻居互相影响"**，只是把影响面从全局收窄到该端点。
   - **(b) 按 token 指纹限流**（`extractBotToken` 已在 handler 内，可取其 hash 作 key）。
     维度正确——无效 token 的重试风暴被关进自己的桶，不影响同 IP 的健康 bot。
     代价：octo-lib 现有三个中间件的 key 分别写死为 IP / `c.Get("uid")`，
     **没有"按自定义 key 限流"的中间件**，需要跨仓新增（或本仓自建，但 `keyedLimiter` 是私有的）。
     另需注意 token 是凭据，**只能落 hash，不得把明文写进 Redis key 或日志**
     （参考 `#246` incoming-webhook 的 token-in-log 教训）。
   - **(c) 对连续认证失败做递增冷却**（形如 `LoginGuard` 的 `login:fail:*`）。
     直击 Background (b) 的重试风暴，且属 repo rule `rate-limit` 明示允许的
     「按业务身份的 per-resource cooldown」例外。可与 (a) 叠加。

   ~~倾向 (a) + (c)~~ —— **实现时改选 (b)，因为前提变了**。那份倾向写于
   「限流器用 octo-lib、只能按 IP 或 `c.Get("uid")` 分桶」的前提下；选型定稿为
   **本仓自建**之后，「按任意 key 分桶」变成零成本，(b) 唯一的障碍消失。
   已实现为 `botTokenFingerprint`（SHA-256 前 16 字节 hex，`modules/bot_api/ratelimit.go`）。
   维度正确性优于 (a)：token 失效的 bot 被关进它自己的桶，不再波及同 IP 的健康 bot。

   **(b) 的已知边界**：换 token 即可换桶。兜底是全局 per-IP 桶——`register`
   **未**被 exclude（与 heartbeat 不同）。这是刻意的：heartbeat 流量极小且必须保活，
   register 则是未鉴权可达、且会触发 `UpdateIMToken` 的写入口，不该同时失去 IP 层防护。
   (c)（认证失败递增冷却）仍是有效补充，留作 follow-up。
6. **(3) 是否与 (1)(2) 同批发布**。它独立于 per-bot 桶（维度不同、代码路径不同），
   拆成独立 PR 可以更快上线——而它恰恰是"掉线后能否自愈"的关键，
   优先级论证上应当**不低于** (1)。

## 实现后 code review 记录（2026-08-05）

独立 reviewer 三条发现，全部已修。记在这里是因为其中第 1 条**改变了设计**，
而另两条暴露的是"测试覆盖了主张、却没覆盖保护主张的那层"。

### 1. register 的 token 指纹桶有 keyspace 放大漏洞（HIGH，已修，设计变更）

原实现只按 token 指纹分桶。brief 原先把"换 token 即可换桶"记为已知取舍、
兜底交给全局 per-IP 桶——**这个取舍的后果被低估了**：

token 由客户端提供，且限流发生在有效性校验**之前**；而令牌桶 Lua 在首次判定时就
`HMSET` + `EXPIRE` 建 key（`pkg/ratelimit/bucket.go`）。于是攻击者每次换一个随机
token，不仅绕过限流，还**按请求速率线性创造 Redis key**：live key 数 ≈ 请求速率 ×
TTL(约 40s)。仅靠全局桶(1500 rps)兜底意味着最坏约 6 万个 key。

叠加上排查中发现的另一个事实——**生产 Redis 未设 `maxmemory`**（无 LRU 淘汰，
OOM 时被 OS kill）——这就从"限流被绕过"升级成"可被用来撑爆 Redis 内存"。

**修法**：register 前面再加一道 per-IP `StrictIPRateLimitMiddleware`
（tag `bot_register`，10 rps / burst 50，固定值不热调——IP 层是防滥用底线）。
**顺序载荷性**：必须排在 token 桶**之前**，否则 key 已经建好了。
这把 key 生成速率从 1500/s 压到 10/s（40s 窗口内约 400 个）。

即 brief 待确认项 5 的 (a) 与 (b) **两者都要**，不是二选一——(b) 解决"失效 token
重试不波及邻居"，(a) 解决"随机 token 洪水撑爆 keyspace"。回归用例
`TestRegisterIPLimitBoundsKeyspaceGrowth` 刻意每次换新 token，若 IP 层缺失则全部穿透。

### 2. `bucket.allow` 的降级路径零覆盖（MEDIUM-HIGH，已修）

Redis 故障与"Lua 返回形状异常"两条路径都 fail-open，此前**完全没有测试**。
危险在于它们的失效是静默的：症状是"限流装了但从未拒过任何请求"。
原注释写着这条分支的目的是"不让真实 bug 藏在总是放行后面"，
而那条分支自己恰恰没被验证过——将来改 `tokenBucketScript` 的返回形状不会有测试变红。

补 `pkg/ratelimit/bucket_test.go`：Redis 不可达、返回元素太少/太多/类型不对、
remaining 越界夹取、以及 `rate<=0` 在 Lua 里是**拒绝而非放行**（这正是读侧防御必须
回退到合法默认值、而不能"当作未启用"的原因）。

### 3. `main.go` 的 provider 闭包无任何保护（MEDIUM，已修）

那段闭包把 12 个 getter 映射到三条通道的 Params，而**所有**限流测试都经
`SetRateLimitParamsProvider` 直接注入、完全绕过它。若有人把 `Heartbeat.RPS` 接成
`s.BotRateLimitRegisterRPS()`，无测试会失败，后果是运维调心跳配额时实际改到 register。

补 `main_test.go` 的 `TestBotRateLimitWiringMapsMatchingGetters`：文本级守卫，
每条通道只允许引用带该通道名的 getter，并自证必须抓到 4 个 getter（防正则失配变恒真）。
已做负向验证（注入错位 → 守卫变红）。

### 顺带修掉的测试污染

新增 per-IP strict 桶后，`ratelimit:strict:bot_register:*` 也需要在 setup 里清理。
漏掉会造成跨用例污染：所有用例共享同一个测试 client IP，burst 50 被累积耗尽后，
后跑的用例随机拿到 429，且失败点落在与限流无关的断言上。同 `test_uid_ratelimit` 的坑。

### reviewer 未采纳的部分

reviewer 把"换 token 绕过限流"本身也算作缺陷。这部分**维持原判为已知取舍**：
任何基于客户端提供的凭据的维度都有此性质，真正的解法是认证失败递增冷却
（待确认项 5 的 (c)，留作 follow-up）。本次修的是它**被低估的后果**（keyspace 放大），
而不是这个性质本身。

## 两项遗留的处置（2026-08-05 定稿）

### access log 的 `robot_id`：**已做**（成本重估后改的决定）

原先估成"中等成本、价值有限"，查证后发现两点都不对：

- **成本远低于预估**：`pkg/accesslog/accesslog.go` 已有 `param.Keys` 机制和"行尾追加、
  不动既有列位"的模式（PR#479 的 trace_id），加一个字段只是同一处多读一个 key。
  且 `authBot` 本来就写了 `CtxKeyRobotID`，**零侵入**——不必改 bot_api 任何逻辑。
- **价值高于预估**：offenders ZSet 只记**被限流拒绝**的 bot，覆盖不到
  「未触发限流但异常」的请求。而事故里最想知道的恰恰是那类——那个 bot 的 typing 有
  455 次/18s 是参数错误的 400，当时无法定位到具体 bot，只能靠 Redis 残留的
  `typing_start:*` key 猜（`ytgeo_bot` 12 个 key），始终没有确证。
  限流整层关闭时（即默认状态）offenders 为空，access log 更是唯一线索。

实现要点与已知限制：

- 只有经过 `authBot` 的请求带该字段 ⇒ **普通用户路由的日志行逐字节不变**（无此 key）。
  刻意**不**记用户 uid：那会改变全部路由的日志行，且 uid 不属于 access log。
- `/v1/bot/register` **拿不到** bot 归因：它跑在 `authBot` 之前，token 无效时压根没有
  bot 身份。那类失败仍只能按 IP 归因——**已知限制，不是遗漏**。
- 记 `robotID` 而非 token：robotID 是公开标识，token 是凭据，永不入日志（#246 教训）。
- `pkg/` 不 import `modules/`，故 key 用字面量 + `TestCtxKeyRobotIDMatchesBotAPI`
  源码守卫钉住一致性。已做负向验证——改掉字面量后守卫变红。
  没有这条守卫，改名的症状是"日志里永远没有 bot 字段"，而它与"这个请求不是 bot 发的"
  在线上无法区分，属静默失效。
- **集群无日志采集**（无 Loki/Fluent/ELK，已核实），所以这是**现场排查**能力
  （`kubectl logs` 现捞），pod 重启即丢，不能用于历史分析或定配额。

### offenders 管理端点：**不做**（显式决定，非遗漏）

理由：
1. `redis-cli ZREVRANGE ratelimit:bot:offenders:{class} 0 49 WITHSCORES` 已够用，
   而事故排查者通常已有 kubectl 权限（本次排查全程如此）。
2. 现阶段三条通道默认全关，ZSet 里没有数据，端点上线也无可看。
3. 成本与价值不成比例：新增端点意味着新的 superadmin 鉴权面、路由、响应契约、
   i18n 与测试，而收益仅是省掉一次 `kubectl exec`。

**触发条件**：等限流真正开启、且运维明确提出"需要在管理台看"时再做。
在那之前把它记为已知的操作成本，而不是悬着的待办。

## Verify 阶段结果（2026-08-05）

逐条核对 `context.yaml` 里注入的 6 条规则、Acceptance、Out of scope。

### 规则合规

| 规则 | load_bearing | 结论 |
|---|---|---|
| `rate-limit` | ✓ | 合规,但**依赖 Exception 条款**——见下方「需人决定 #1」 |
| `space-isolation` | ✓ | 合规。heartbeat 移出主组后仍显式挂 `authBot`,只增不减中间件;未新增 handler,未绕过任何 ownership 检查;`/v1/bot/messages` 用 `r` 独立挂载,不在主组下,未受影响 |
| `error-handling` | ✓ | 复用已注册的 `err.shared.rate.limited`,无 raw response(i18n-lint 通过)。**但门面选择需 sign-off**——见「需人决定 #2」 |
| `trust-boundary` | ✓ | 转义条款不适用(不渲染外部内容);凭据条款已守:token 只落 SHA-256、access log 与 offenders 均记 robotID |
| `testing` | — | 合规。已 reset `ratelimit:bot:*` 与 `ratelimit:strict:bot_register:*`;`-race` 全绿 |
| `commit-style` | — | 尚未 commit,留到 Finish |

### Acceptance 缺口(5 处,已全部自修)

Verify 发现实现虽满足功能主张,但有 5 条 Acceptance 没有对应测试。都补齐了:

1. **Prometheus label 基数**——Acceptance 要求"以单测枚举全部 label 组合、断言上界",
   原先只在代码注释里声明。补 `pkg/metrics/ratelimit_test.go`:断言 label 集合恰为
   `{class,outcome}`,且不含 robot/uid/path/token 任一子串。另补"未注册时 Observe 是
   安全 no-op"——观测绝不该成为业务可用性的前置条件。
2. **offenders ZSet 有界性**——`offenders.go` 原先零测试。补
   `pkg/ratelimit/offenders_test.go`:灌入 3 倍上限后 `ZCard ≤ topN`、key 带 TTL。
   额外加了**「裁剪保留最高分」**一条:若 `ZREMRANGEBYRANK` 区间写反(删高分),
   集合大小同样有界、前一条断言照样通过,但名单只剩噪声——事故时看到的是
   "超限最少的 50 个 bot",且这种错误在真去看名单之前毫无症状。
3. **dry-run 仍完整执行判定**——原先只断言"不拦截/不设头",没断言判定真的跑了。
   补 `TestDryRunStillExecutesDecision`:检查 offenders ZSet 出现该 bot。
   若判定被跳过,影子模式就既拿不到观测数据、也没暴露 Redis 成本。
4. **heartbeat 自有桶真正生效**——补 `TestHeartbeatBucketStillEnforcesLimit`。
   这条钉的不是"限流能用",而是**exclude 让出的防护确实被补回来了**:
   心跳已移出全局 per-IP 桶,若自有桶不生效,它就成了未鉴权可达且无任何上限的端点。
5. **heartbeat 速率下界 + env 兜底消毒**——补
   `modules/common/system_settings_bot_ratelimit_test.go`(零基础设施,直接塞 snapshot):
   下界常量 > `1/60`、低于下界被夹紧、`0/-1/NaN/±Inf/abc` 全部回退、
   **env 本身为 `NaN`/`+Inf` 时也回退**(Acceptance 点名要求——`ParseRPSFromEnv` 底层
   `strconv.ParseFloat` 会原样接受它们)、以及 DB 压制 env 的优先级。

### Out of scope 核对

未触碰:全局 per-IP 的 `rps/burst` 值(只改 exclude 列表,那是任务第 2 项)、
客户端参数 bug、`ResponseErrorL` 本身的固定 400 契约、滚动更新的桶参数过渡行为、
"bot 流量整体迁出 IP 维度"(只 exclude 了 heartbeat 一条路径)。

### 门禁

`go build ./...` / `gofmt` / `go vet` / `make i18n-extract-check` / `make i18n-lint` 全绿。
测试:`pkg/ratelimit` `pkg/metrics` `pkg/accesslog` `main`(均 `-race`)+
`modules/bot_api`(整包 52s)+ `modules/common` + `modules/botfather`(跨模块)全部通过。

> `gofmt -l` 报 `modules/common/db_shortno.go` 与 `service.go`——已用 `git status` 确认
> **非本次改动**,属仓库既有 gofmt 债,不在本任务范围内修。
>
> 多包顺跑会撞 sql-migrate 的 unknown migration(每个测试二进制只 link 自己依赖的
> module init)。**分包跑、每包前重建 test 库**,这是已知环境约束而非本改动的问题。

### 需人决定(2 项,无法自修)

1. **`rate-limit` 规则的 Exception 依赖**。规则原文要求 authenticated route 挂
   `SharedUIDRateLimiter`;本任务自建了限流器。论证写在 `pkg/ratelimit` 包注释里
   （进程级单例无法表达"独立配额值 + 运行时热调",与 per-webhook 桶同构,
   落在规则已写明的 Exception 内）。**这个论证需要 maintainer 认可**,
   否则应改回 lib 中间件并放弃热调能力。

2. **`ResponseErrorLWithStatus` 需要 sign-off**。CLAUDE.md 明文:该门面限
   "new endpoints only ... diverging from D14 needs maintainer sign-off"。
   本任务用它是为了让限流返回**真正的 429** —— octo-lib 三个限流中间件都走
   `TransportStatus: 429`,客户端(含 issue #696 报告里的插件)按状态码识别限流并决定
   退避;若 bot 通道返回 400,同一系统里就出现两种"被限流"的表示法,客户端要么加分支、
   要么把限流误判成参数错误而**停止重试**。
   **我的判断是这属于"与既有限流层保持一致"而非"偏离 D14",但按流程仍需签字。**

3. （附带）**`X-RateLimit-Scope` 新增 `bot` 取值是可观察的契约变化**。
   brief load-bearing 已记"需在 issue #696 同步给插件侧",该同步动作尚未执行。

## 第二轮 code review：P1 —— 未鉴权 heartbeat 绕过全部限流（已修）

**这是 brief 自己写过、实现却没做到的一条约束。** Goal 一节原文：

> 而一旦 exclude，该端点就失去 IP 层防护——未鉴权请求也能一路打到 `authBot`（它要查
> Redis/DB），**所以 exclude 必须和一个自有的桶成对出现，不能单独做**。

实现确实配了自有的桶,但把它挂在了 `authBot` **之后**:

```
r.POST("/v1/bot/heartbeat", ba.authBot(), ba.botActorUID(), perBotLimiter, ba.heartbeat)
```

于是无效 token 的请求在 `authBot` 里就 abort 了,**永远走不到那道桶**;而 per-bot 桶
默认还是 `enabled=false`。净效果是:heartbeat 移出全局桶之后,未鉴权流量**完全无防护**,
攻击者可用任意无效 token 无限触发 `authBot` 的 Redis/DB 查询——
原本被全局桶挡住的 DDoS 面,反而因为 exclude 被打开了。**修复方向和事故本身相反:
这次是我们自己把面打开的。**

### 为什么测试没抓到（更值得记的部分）

`TestHeartbeatBucketStillEnforcesLimit` 的注释原文声称验证的正是
「exclude 的那道防护确实被补回来了」。但它用**有效 token**,走的是 authBot 通过之后的
per-bot 桶,所以在这个漏洞存在时**照样通过**。

**一个断言比它自称覆盖范围更弱的测试,比没有测试更危险**——它让 review 者认为该性质
已被验证。这类"假绿"在本任务里出现两次(另一次是守卫的空集合恒真断言,当时用自证断言
堵住),模式相同:**测试覆盖了 happy path,而声明覆盖了全部**。

### 修法

heartbeat 改为三层,顺序载荷性:

| 层 | 位置 | 职责 | 可关? |
|---|---|---|---|
| per-IP strict (`bot_heartbeat`, 100 rps / burst 300) | **`authBot` 之前** | 未鉴权洪水底线,exclude 的对价 | **否** |
| `authBot` + `botActorUID` | 中 | 鉴权 + 落 bot 身份 | 否 |
| per-bot 桶 | `authBot` 之后 | 防单个已鉴权 bot 滥用 | 是(默认关) |

第一层**刻意不接 `enabled` 开关**:per-bot 桶默认关闭(灰度需要),若这层也可关,
exclude 之后就存在一个完全无防护的未鉴权端点。参数按实测定——生产单 IP 的 heartbeat
合计峰值约 45 rps(一台机器多个 bot),100 rps 留两倍余量,相对原先全局桶的 1500 rps
是显著收敛。

`register` 无此问题:它的 IP 桶已在最前,且该端点没有 `authBot`(handler 自己解析 token)。
主组亦无此问题:它**未**被 exclude,未鉴权洪水仍由全局 1500 rps 兜底。

### 新增回归

- `TestHeartbeatIPLimitPrecedesAuth` —— 源码守卫,钉住 IP 桶在 `authBot` 之前,
  并断言它用固定常量而非 setting 开关。
- `TestHeartbeatUnauthenticatedFloodIsRateLimited` —— 行为回归,用**无效 token**:
  先断言桶未耗尽时不限流(不误伤),抽干桶后必须 429 且
  `X-RateLimit-Scope: strict:bot_heartbeat`(证明来自鉴权前那一层),
  另一个 IP 不受连坐。用预抽干 Redis 桶而非真打几百次——strict 桶 100 rps/burst 300,
  真打既慢(每次走 authBot 的 DB 查询)又不稳(打的过程中一直在回填)。
- 两条都做了**负向验证**:把 IP 桶挪到 `authBot` 之后,两条同时变红。
- `TestHeartbeatBucketStillEnforcesLimit` 的注释已改写,明确它只覆盖鉴权后那一半。

## 两道 per-IP 桶：用 lib 中间件 + env（定稿，经两次反转）

### 先分清目标与对价

**本任务的目标是 per-bot 限流**——按 bot 身份分配额，消除「同出网 IP 的 bot 共享一份
配额」这个事故成因。三条 per-bot 通道（business / heartbeat / register）是目标本体。

而两道鉴权前的 IP 桶**不是目标**，是两处不得不付的对价：

- `register` 跑在 `authBot` **之前**，没有 bot 身份。token 指纹能挡「失效 token 重试
  风暴」，但它是客户端提供的值——换 token 即换桶，而令牌桶首次判定就建 Redis key，
  于是轮换 token 会按请求速率造 key。需要一层不依赖客户端提供值的兜底。
- `heartbeat` 移出全局 per-IP 桶后（否则邻居饿死它），鉴权前完全无防护——per-bot 桶
  挂在 `authBot` 之后，无效 token 在鉴权里就 abort。

鉴权前唯一可用的维度是 IP。所以这**不是「回到 IP 维度」**，是「那个阶段只有 IP」。

### 两次反转的判据（记下来，因为每次的理由都不完整）

> 这一节记的是**设计过程**，不是提交历史——中间那两版都已 squash，`git log` 里找不到
> `clientip.go` 也找不到 4 项 IP 配置。保留它的理由:被推翻的那两个论证本身有信息量。

1. **第一版：lib 中间件 + env。** 理由是自建要复制 lib 未导出的 `getClientIP`，
   两份实现分叉会「让全局桶与这两道桶对同一请求算出不同 key」。
2. **第二版：自建 Limiter + system_setting。** 上面那个理由被推翻——两道桶与全局桶
   是**独立 keyspace**、不共享 token，分叉只导致本桶分片不精确（有界），
   而 env 要滚动重启、正是二次事故的抖动成因。
3. **定稿：回到 lib 中间件 + env。** 判据不是技术对错，而是**目标边界**：
   自建换来的是「参数热调 + 配置统一 + 观测统一」，代价是复制未导出逻辑、
   **偏离 `rate-limit` 规则明文的第二条**（未鉴权路由挂 `StrictIPRateLimitMiddleware`）、
   以及约 180 行新基础设施（`clientip.go` 及其测试）。
   **为一个非目标的对价引入新基础设施、并加重规则偏离，不划算。**
   IP 底线是防滥用阈值，不像 per-bot 配额需要按影子数据反复收敛，env 够用。

> 第 2 版曾引入 `pkg/ratelimit/clientip.go`（存在的唯一理由就是让 IP 层能自建）
> 与 `system_setting` 的 4 项 IP 配置,定稿时一并移除。后者尤其要移干净:
> **留着不生效的配置项比没有更糟**——管理台能改但不生效是最坏的一种。

净结果:规则偏离收窄到**只剩一条**（per-bot 不用 `SharedUIDRateLimiter`），
那一条论证干净、落在已有 Exception 内、有 incomingwebhook 先例。

### 顺带补上 octo-lib 的一个校验缺口

`ParseRPSFromEnv` 底层是 `strconv.ParseFloat`,**会原样接受 `"NaN"`/`"+Inf"`**;
而 lib 的 `newKeyedLimiter` 启动期只检查 `rps <= 0` 就 panic——**`NaN <= 0` 为 false**,
所以 `NaN` 会穿过那道校验直达令牌桶 Lua,让其中所有算术与比较静默失效。

故 env 值统一过 `ratelimit.SanitizeRPS/SanitizeBurst`（为此从 `pkg/ratelimit` 导出,
两层共用同一份消毒逻辑）。`TestIPLimitParamsSanitizesEnv` 覆盖
`NaN/±Inf/0/-1/abc` 全部回退 + 合法值仍生效（否则消毒会退化成「永远忽略 env」,
那么「改 configmap 就能调」的前提也不成立）。
守卫 `TestHeartbeatIPLimitPrecedesAuth` 追加断言:参数必须经 `ipLimitParams`,
不得直接把 `ParseRPSFromEnv` 的结果交给 lib。
