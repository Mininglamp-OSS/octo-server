---
type: Task
title: "Task: bot-event-score-monotonic"
description: 把 robotEvent:{robotID} 的 score 从 octo-lib GenSeq（进程内 HiLo 块）换成严格单调分配器，使 POST /v1/bot/events 的排他游标分页无损；生产上乱序与碰撞均已实测发生，碰撞还会让 ack 误删未投递事件。核心迁移风险是新号段必须整体高于所有客户端已持有的旧游标。
tags: ["bot-api", "wire-contract", "observability", "testing", "commit"]
timestamp: 2026-08-05T17:03:42+08:00
# --- octospec extension fields ---
slug: bot-event-score-monotonic
upstream: Mininglamp-OSS/octo-server#697
source: self
---

# Task: bot-event-score-monotonic

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

让 `robotEvent:{robotID}` 的 sorted-set score **严格单调、全局唯一**，从而使
`POST /v1/bot/events` 的排他游标分页（`ZRANGEBYSCORE key (cursor +inf`，
`modules/bot_api/events.go:251`）成为无损投递。

**当前的缺陷**：score 来自 octo-lib `GenSeq`（`config/seq.go`），这是**进程内**
HiLo 块分配器 —— `seqStep = 1000`，块缓存在包级 `seqMap`，只由进程内 mutex 保护，
`min_seq` 用无条件 `ON DUPLICATE KEY UPDATE` 写回。3 副本部署下同时存在多个 1000
宽的活跃块，`event_id` 顺序与真实入队顺序脱钩。**游标一旦推进到高块，之后从低块
发出的事件永久不可达** —— 排他 `Min` 把它排除，而游标下方没有任何重投机制。

`events.go:182-213` 已经记下了相邻的那条假设（"uniqueness is assumed and
unverified"，PR #685 review P2-3），但把后果限定在 score **碰撞**上。实测表明
**仅顺序倒挂就足够**，不需要碰撞。

**生产实测（只读）**：单个 bot 队列 644 条事件里 **19 处相邻顺序倒挂，全部精确落在
1000 块边界**（`…X007 → …(X+1)001`），**最大时间回退 6.7 天**。在 `card_action` 事件上
（`acted_at` 是服务端 `time.Now()`，`modules/message/api_card_action.go:253`，可直接
比较）测到 `…5009` 的 `acted_at` 比 `…4007/…4012/…4013` 都早 —— 两个块并发发号的
直接证据。

**碰撞也已发生，不只是乱序。** 全量只读扫描 **1948 个非平凡队列，3 个存在重复
score**：`70565/70395`（170 重复）、`25136/23932`（1204）、`16588/15526`（1062）。
重复数聚集在 `seqStep` 倍数附近 —— 整块被重复发放的指纹。机制在 `addOrUpdateSeq`：
写回的 `min_seq` 由**进程本地** `CurSeq + seqStep` 算出，且 `ON DUPLICATE KEY UPDATE
min_seq=VALUES(min_seq)` 是**无条件覆盖**，所以落后的副本能把 `min_seq` **改小**，
之后冷启动的副本读到已发放区间并重发一遍。

碰撞叠加出**第二条、更坏的丢失路径**：`ackEvent` / `eventAck` 按
`ZRemRangeByScore(key, id, id)` 删除（`events.go:160`、`:324`），会删掉**所有**共享该
score 的 member —— 于是 ack 一条已投递的事件会静默销毁一条从未投递给任何人的事件。
`events.go:182-213` 预言了这一点并记为「假设不会发生」；它已经发生了。

**本地已复现（`tools/genseq-repro/`）**：用真实 `config.Context.GenSeq` + 真实
`events.go` 的读/删形状，在容器 MySQL 8.0 + Redis 7 上复现出全部三条丢失路径 —— 碰撞
（确定性、全量重叠）、分页边界跳过、低块晚到跳过、ack 连带删除。详见 #697 的复现评论。

**用户可见症状**：点交互卡片没反应，且**同一张卡上部分人可用、部分人永久失效**，
取决于其点击落在哪个块。叠加 D4 幂等（键为 `(message_id, action_id, operator_uid)`，
TTL = `Robot.MessageExpire` 默认 7 天，刻意不含 `inputs`），被跳过的用户重试只会拿到
`{"accepted":true,"replay":true}` —— **D8「超时 re-tap 自愈」的假设在这里不成立**，
丢失不可自愈。

## Background

**这不是 bot 侧误用 API。** 服务端在自己的 long-poll hold 内部就是按「已观测到的
最大 `event_id`」推进游标的：

```go
for _, r := range raw {
    if r.EventID > page.cursor { page.cursor = r.EventID }   // events.go:235
}
```

一次 hold 里先读到高块事件、随后收到低块入队，服务端自己就会跳过它，与 bot 怎么管
游标无关。所以修复责任在服务端。

**与 #627 的关系**：同一根因家族（`GenSeq` 跨副本 HiLo），但**不同作用面**。#627 /
PR #644·#648 把 `message_extra.version` 换成了事务性 per-channel 序列
（`internal/msgextraseq`），只覆盖 `message_extra` 的写入方；`robotEvent` 的 score
从来不在那个范围内，**激活 #627 的 cutover 对本路径零影响**。本任务不依赖 #627 的
激活状态，也不改变它。

**分配器选型（待人类确认）**：

| 方案 | 单调唯一 | 成本 | 备注 |
|---|---|---|---|
| Redis `INCR`（per-bot key） | ✅ | 一次 Redis 往返，与 `ZAdd` 同一 pipeline/Lua 可合并 | **倾向选它**：队列本身就在 Redis，无新依赖；`saveRobotMessage` 是最热路径，不宜再加 DB 往返 |
| `internal/msgextraseq` 式 DB 事务序列 | ✅ | 每条事件一次 DB 写 + 行锁 | 语义最强，但对 bot 消息扇出这种量级的热路径过重 |
| 保留 `GenSeq` + 游标下方重投窗口 | ❌ | 低 | 只缩小窗口不封闭，仅作为过渡缓解；且要求消费方按 `event_id` 去重（改 bot 契约） |

若选 Redis `INCR`，必须处理 **Redis 持久化丢失导致计数器回退**：启动/首次使用时以
`max(该队列现存 max score, 旧 `GenSeq` `min_seq`)` 作为 floor 引导（与
`msgextraseq` 的 bootstrap 同思路），并在写入侧对「新 score ≤ 队列当前 max」计数告警。

## Load-bearing list

- **`POST /v1/bot/events` 的 wire 契约**：`event_id` 的语义、游标的排他语义、返回
  形状（`status`/`results`）。已有 bot 客户端持有旧号段的游标值。
- **切换时的号段单向性（本任务最大风险）**：新分配器的首发值必须 **> 所有客户端已
  持有的游标**，否则新事件被旧游标挡在 `Min` 之外 —— 症状与本 bug 完全相同，只是
  影响所有 bot。这是硬前置条件，不是 nice-to-have。
- **score == payload `event_id` 的等值不变量**：`ackEvent` /
  `eventAck` 用 `ZRemRangeByScore(key, id, id)` 定位 member（`events.go:160`、
  `:324`），`events.go:182-213` 明确记录了这个耦合。新分配器不得破坏它。
- **碰撞下的 ack 误删**：同一 score 的多个 member 会被一次 ack 全部删掉。修好分配器
  能阻止**新增**碰撞，但**现存队列里已有的重复 score 不会自愈** —— 处置方式见
  Open questions 3。
- **5 个 `ZADD` 生产点必须整体切换**（`modules/robot/api.go:224-235` 列举）：
  `enqueueBotEventGeneric`、`enqueueBotTypedEventGeneric`（`card_action`）、
  `saveRobotMessage`、以及 `modules/group` 的两个 `notifyBotJoinedGroup`。**部分
  切换会重新造出「两个活跃号源」这一正是本 issue 的条件。** 需要源码守卫，参照
  `pkg/botevent` 的 `TestEveryBotEventQueueWriterRingsTheDoorbell`。
- **long-poll 的前进保证**（PR #685）：`readEventPage` 的 `page.cursor`/`advanced`
  推进逻辑，及其「Redis 读成功而写失败时仍能前进」的设计（`ackEvent` 的注入缝）。
- **App Bot 的 DM-only 过滤 + auto-ack**（`filterAppBotEvents`）：依赖同一等值不变量。
- **doorbell**（`pkg/botevent`）：门铃只是提示，ZSet 是唯一权威 —— 本任务不得把
  分配器塞进门铃路径，也不得让分配失败阻塞生产者（`msgSem` 停摆风险，见
  `bot-events-longpoll` brief）。
- **`card_action` 的 D4 幂等 claim**：`modules/message/card_action_claims.go`。事件
  丢失不可自愈的那一半来自这里；本任务修上游丢失，**不改幂等语义**。
- **既有队列中的历史 member**：迁移期读路径必须同时能读旧号段和新号段的 member。

## Out of scope

- **#627 / PR #644·#648 的 `message_extra.version` 激活**（`tools/msgextra-version`
  的 cutover runbook）。独立事项，与本路径无因果关系。
- **过期事件回收**（`Mininglamp-OSS/octo-server#698`）：`robotEvent` 队列里过期
  member 永不清理是另一个 bug，单独处置。
- **bot 侧游标策略**。正确用法（`event_id=0` + 严格 ack，让 `ZREM` 而非游标定义进度）
  应写进文档并通知 bot 作者，但**不在本任务改任何 bot**。
- **重新设计 ack 协议 / 改成 stream（`XADD`）**。`XADD` 天然单调且有 consumer group，
  是更彻底的方向，但会改 wire 契约与所有 bot，属独立提案。
- **octo-lib `GenSeq` 本身的修复**，以及**已确认的两个同类实例**（不是"待审计"，是
  review 中查实的）：
  - `conversation_extra.version` — `GenSeq(common.SyncConversationExtraKey)`
    （`api_conversation.go:195`），读回 `Where("uid=? and version>?")`
    （`db_conversation_extra.go:29`）
  - `reminders.version` — `GenSeq(common.RemindersKey)`（`api_reminders.go:172`），
    读回 `reminders.version>?`（`db_reminders.go:93-102`）

  两者都是「排他游标 + `GenSeq` 号源」，且用**全局单一 seq key**（不带 uid/channel
  后缀），所以一次非单调分配由该功能的**所有用户**共担 —— 爆炸半径比 per-bot 队列更大，
  症状是静默丢提醒 / 丢会话扩展状态。**需单独立项**，已在 #697 的 Related 段记录。
  `pinned_message.version` 已走 `seqStore.ReserveTx`，属 #627 范围，不在此列。
  其余 `GenSeq` 调用点（`MessageReactionSeqKey`、`ProhibitWordKey`）尚未核对读回是否
  为排他游标。

## Acceptance

- **源码守卫**：新增测试断言 `robotEvent:` 的**每一个** `ZADD` 站点都经由新分配器，
  且 `GenSeq(RobotEventSeqKey…)` 在 `modules/` 下不再有调用点（参照
  `TestEveryBotEventQueueWriterRingsTheDoorbell` 与 `msgextraseq` 的 PR-2c 守卫形状）。
- **并发单调性（新分配器）**：并发 goroutine × 两个独立分配器实例发 N 号，断言结果
  **严格递增且无重复**。
- **对照必须失败（已本地验证可行，方法见下）**：同一条无损性断言用今天的 `GenSeq` 跑
  必须**失败**。**不能靠「实例化两个 `GenSeq`」** —— `seqMap` / `seqLock` 是 octo-lib 的
  **包级**变量，同一进程内拿不到两份独立块状态，那样写出来的测试会假绿。
  **可行方法（已跑通，见 `tools/genseq-repro/`）**：起两个子进程，各自冷启动真实
  `config.Context.GenSeq`，用一个共同的 wall-clock barrier 让两边的 `min_seq` 读都发生在
  任一写回之前 —— 这正是 N 副本滚动重启产生的条件。**不需要**手工操纵 `seq` 表。
  实测结果：两副本发出**完全相同**的 12 个 id（重叠是全量而非部分），`min_seq`
  5000 → 6000（两次无条件写回同值）。**碰撞是确定性的，不是要靠运气撞上的竞态。**
- **三条丢失路径都要各有一条测试**（本地已全部复现，`tools/genseq-repro/`）：
  - **分页边界落在共享 score 上** → 排他 `(cursor` 永久排除第二个 member。**不需要
    时间倒挂**；`getEvents` 的 limit 是 20..100，在有 1000+ 重复 score 的队列上是常态。
  - **低块晚到入队** → 事件出生即在游标下方，永不可投递（生产实测到的形状）。
  - **ack 连带删除** → `ZRemRangeByScore(id,id)` 删掉所有共享该 score 的 member，
    ack 一条已投递事件会销毁一条从未投递的事件。
- **潜伏性也要记进测试注释**：把整个碰撞队列**一次读完**是**不丢**的。丢失需要「分页
  边界命中」或「低块晚到」二者之一 —— 这解释了为什么症状是间歇的、同一张卡上因人而异，
  而不是一眼可见的整体故障。删掉这条注释的人会以为一次读完的绿测就证明了无损。
- **碰撞检测**：新增一个可只读运行于预发/生产的校验（及其测试形态等价物），断言任一
  `robotEvent:*` 队列的 member 数 == distinct score 数。当前生产该断言在 **3 个队列上
  失败**（见 Goal），修复后**新增**碰撞应恒为 0。
- **排他游标无损**：并发入队 + 按「最大 event_id」推进游标消费，断言零丢失。用今天
  的分配器跑必须复现丢失。
- **floor 引导**：分配器首发值 > `max(该队列现存 max score, 旧 GenSeq min_seq)`；构造
  「Redis 计数器被清空但队列仍有高 score member」的场景，断言不回退、不重号。
- **等值不变量**：`ZAdd` 的 score 与 payload `event_id` 相等；`ackEvent` 能按
  `event_id` 精确删到目标 member。
- **回归**：`go test -race ./modules/bot_api ./modules/robot ./pkg/botevent ./modules/message -count=1`
  全绿；`modules/group` 的 `notifyBotJoinedGroup` 相关测试全绿。
- **观测**：新增「score 非单调」计数器（写入时新 score ≤ 队列当前 max 即计数），
  低基数 label；上线后该计数应恒为 0。
- **迁移验证步骤**（写进 PR 描述，运维可执行）：切换前记录各队列 max score 与
  `seq` 表 `min_seq`，切换后断言新号段整体高于两者。

## Decisions（已定，附依据；无需再讨论除非依据被推翻）

**D1. 分配器用 Redis `INCR`，per-bot key（`botevent:seq:{robotID}`）。**

原先把它列为待决，是因为「Redis 持久化丢失导致计数器回退」这个风险的强度取决于生产
配置 —— 那是事实问题，不是偏好问题，已查清（生产 Redis，只读 `CONFIG GET` / `INFO`）：

| 配置 | 值 | 对本方案的影响 |
|---|---|---|
| `appendonly` | `no` | 无 AOF，只有 RDB |
| `save` | `3600 1 300 100 60 10000` | 崩溃最坏丢 60–300 秒 |
| `maxmemory` / `maxmemory-policy` | `0` / `noeviction` | **计数器不会被 LRU 淘汰** —— 这是 `INCR` 方案最大的隐性杀手，已排除 |
| `role` / `connected_slaves` | `master` / `0` | 单实例，无 failover 异步复制回退 |

**回退是自洽的，这是选 `INCR` 的核心理由**：计数器与 `robotEvent` ZSET 在**同一个 Redis
实例、同一个 RDB 持久化域**。设 RDB 落盘于 T0，计数器值 `C0`，队列 max score `S0 ≤ C0`；
T0 后发号至 `C1`、入队至 `S1`。崩溃恢复到 T0 → 计数器回到 `C0`，而 T0 之后入队的 member
**也一起没了**，队列 max 回到 `S0`。于是从 `C0+1` 继续发号，**不可能与存活 member 碰撞**。

> **硬约束（必须写进代码注释）**：计数器**必须**与队列同实例、同 db、同持久化域。任何
> 「为了性能把计数器挪到另一个 Redis / 另一个 db / 加独立 AOF」的优化都会立刻摧毁上面
> 这个自洽性论证。未来若引入主从，论证仍成立（计数器与队列一起滞后），但**跨实例不成立**。

选 per-bot 而非全局 key：单调性只需队列内成立；全局 key 的 floor 必须 ≥ **所有**队列的
max score（否则某个高 score 队列的客户端游标会把新号段整体挡在门外），且是热点。

**D2. 单阶段部署，不需要 #627 那样的 flip。** 号段单向性（新号恒高于旧号）+ D1 的同域
自洽性使新旧副本共存是安全的。前提是 floor 引导**幂等**：任何副本任意次引导结果相同，
不要求「引导必须在第一个新副本启动前完成」。

> **更正**：本 brief 早先写「用 `SET key <floor> GT`」是错的 —— Redis 的 `SET` 没有
> `GT` 选项（`GT`/`LT` 属于 `EXPIRE` 和 `ZADD`）。幂等取 max 必须用 Lua
> （`GET` → 比较 → 条件 `SET`，单脚本内原子），与本仓其他 Lua 站点同模式
> （`pkg/botevent/bell.go` 的 `EVALSHA` + `NOSCRIPT` 回退）。

**D3. 现存碰撞 member 不重编号**（详见 Open questions 1 —— 这是唯一需要人类签字的一条，
因为它是唯一涉及写生产数据的选项）。

**D4. `reminders` / `conversation_extra` 已单独立项**
（`Mininglamp-OSS/octo-server#700`，P1），不在本任务范围。

**D5. `tools/genseq-repro/` 保留在 `tools/`**，与 `msgextra-version`、`card-action-dlq`
同性质（排障/验证工具）。实现阶段把核心断言抽成 `pkg/botevent` 下的正式测试，工具保留
供运维复核。

## Open questions（需人类确认）

1. **现存 3 个碰撞队列（2624 个碰撞 member）要不要一次性重编号？**
   这是**唯一**涉及写生产数据的选项，所以只有你能签字。

   **我的建议：不动，只记基线。** 理由：
   - **无法区分哪一个未被投递** —— 游标在 bot 客户端手里，服务端没有投递记录。所以
     重编号只能对每对里的**两个**都重发，等于给已处理过的事件制造重复投递（bot 侧按
     `event_id` 幂等，而重编号恰恰换掉了 `event_id`，幂等失效）。
   - **多数已无消费价值** —— 同一队列里 470/647 的 member 级 `expire` 已过期数周
     （见 #698）。
   - **真正的损失已经发生且不可挽回**，重编号挽回的是「可能从未投递、且仍在有效期内」
     的一小部分，收益远低于写生产 ZSET 的风险。
   - 修好分配器即可**止血**（新增碰撞归零），这是主要收益。

   配套动作（只读，无需签字，我可以直接做）：把当前 3 个队列的碰撞计数与 score 区间记为
   基线，上线后验证增量归零。**当前碰撞正在增长**（`botfather` 一小时内 170 → 358），
   所以基线要在修复上线前后各取一次。

   如果你倾向重编号，我需要额外的授权范围（写哪个队列、是否停写窗口）和一个回滚快照方案。
