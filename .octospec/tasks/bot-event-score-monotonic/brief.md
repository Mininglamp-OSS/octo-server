---
type: Task
title: "Task: bot-event-score-monotonic"
description: 把 robotEvent:{robotID} 的 score 从 octo-lib GenSeq（进程内 HiLo 块）换成严格单调分配器，消除排他游标下的碰撞与跨重启倒挂两类永久丢失；生产上两者均已实测发生，碰撞还会让 ack 误删未投递事件。不含 ZADD 重排窗口（见 Out of scope，独立 issue），故不得称为「使排他游标无损」。核心迁移风险是新号段必须整体高于所有客户端已持有的旧游标。
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

让 `robotEvent:{robotID}` 的 sorted-set score **严格单调、全局唯一**，消除
`POST /v1/bot/events` 排他游标分页（`ZRANGEBYSCORE key (cursor +inf`，
`modules/bot_api/events.go:251`）下的两类永久丢失：**score 碰撞**与**跨重启的号序倒挂**。

> **范围声明（review 2.3/2.4 要求）**：这**不等于**「排他游标从此无损」。分配与发布是两次
> 操作，A 分配 `N` 后 stall、B 分配 `N+1` 并 ZADD 摇铃时，消费者的游标会越过 `N`。那个窗口
> 本任务不修，见 Out of scope，并已被 `TestKnownResidualZaddReorderingCanStillSkip` 钉成
> 测试事实。原文写「成为无损投递」是过度声称。

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

**分配器选型（已定，见 D1）**：

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
- **ZADD 重排窗口（review 2.3，已知 residual，需独立 issue）**。分配与发布是两次操作：
  A 分配 `N` 后 stall（marshal / GC / 慢往返），B 分配 `N+1` 并 ZADD **且摇铃** → 消费者被
  唤醒读到 `N+1`、游标推到 `N+1` → A 的 ZADD 才落在 `N`，永久不可达。**doorbell 让它更易
  发生，不是更难** —— 唤醒正由造成倒挂的那次 ZADD 触发。单调 id 消掉的是碰撞与跨重启倒挂，
  **不含这个窗口**；它需要游标下方的 re-delivery 窗口（`modules/bot_api/events.go:186-199`
  自己就写了 "a Redis-side allocator **or** a re-delivery window"，本任务只交付了前者），
  要改消费侧契约，属独立提案。已用 `TestKnownResidualZaddReorderingCanStillSkip` 把它钉成
  测试事实而不是 PR 描述里的一句话 —— 那个测试断言「今天仍会丢」，将来修好了它会失败并提醒
  改成回归测试。**因此本任务不得被描述为「使排他游标无损」。**
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

- **源码守卫**：`GenSeq(RobotEventSeqKey…)` 在 `modules/` 下不再有调用点（守卫改为断言
  **key 符号本身**不得出现在 allowlist 之外，见第四轮记录），且每个 `robotEvent:` 的
  `ZADD` 都摇门铃。
  > **DEFERRED（第六轮 review 达成一致）**：本条原文还要求断言每个 `ZADD` 的 **score 来源**
  > 是新分配器。已发的两个守卫都不约束 score —— 一个用 `time.Now().UnixNano()` 打分的
  > writer 能同时通过。已记入 `Mininglamp-OSS/octo-server#704` Gap 4，不在本任务交付。
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
- **观测**：见下方第三轮记录第 6 条 —— 原文要求的「写入时新 score ≤ 队列当前 max 即计数」
  **已修订**：那个比较做对了就等于原子 allocate-and-publish（属 Out of scope 的独立
  follow-up），这里改为交付 6 个自愈计数器 + `AuthorityReads()`，并把跨副本回退检测
  单独立项（P1-5/P1-6）。低基数、无 label 的要求保留。
- **迁移验证步骤**（写进 PR 描述，运维可执行）：切换前记录各队列 max score 与
  `seq` 表 `min_seq`，切换后断言新号段整体高于两者。

## Decisions（已定，附依据；无需再讨论除非依据被推翻）

**D1. 分配器用 Redis `INCR`，per-bot key `botEventSeq:counter:{robotID}`。**

> **更正**：本 brief 原写 `botevent:seq:{robotID}`，实现最初写成 `botEventSeq:{robotID}` ——
> 而 `ModeKey` 是 `botEventSeq:mode`，于是 `SeqKey("mode")` **就等于** `ModeKey`：robotID 为
> `mode` 的 bot 会把全局激活开关当自己的计数器 seed + INCR。UUID-hex 的 robotID 让它不可达，
> 但「一个合法输入能撞上全局键」的命名空间不该留着。已加 `counter:` 段并加回归测试
> （`TestCounterKeyCannotCollideWithModeKey`）。讽刺的是 brief 原来的拼写本来不会撞。

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

**D2.（已更正 —— 原判断是错的）需要显式 activation gate，两阶段，不是单阶段部署。**

原文写「号段单向性使新旧副本共存安全」，**方向搞反了，review 抓到**。legacy 副本是从它那个块的**底部往上**发号的：新分配器在 7001 发号时，legacy 副本还在发 5001、5002…… 一旦消费者游标到了 7001，legacy 后续发的每一个 id 都永久不可见 —— 正是本任务要消除的丢失，由本任务自己造成。而且 `seedSafetyMargin` 越大越糟：**margin 本身就是吞掉 legacy id 的那个 gap**。

改为：
- Redis key `botEventSeq:mode`，缺省 / 非 `incr` 即 legacy。**未激活时 `NextEventID` 委托给
  `GenSeq`**（与 `internal/msgextraseq` 的 `mode=legacy` 同构），所以部署本身行为中立。
- mode 与分配在**同一个 Lua 脚本内**读取（`gateSource`）—— 刻意不做进程内缓存：缓存哪怕
  1 秒，也意味着那 1 秒里部分副本用计数器、部分用 GenSeq，即两个活跃号源，正是缺陷本身。
  flip 后**下一次分配**即生效，无副本间分歧窗口。
- 残余窗口只剩「已读到 legacy、尚未 ZAdd」的在途请求（微秒级）；flip 前后短暂停写即可关闭。
- operator 工具 `tools/botevent-seq`（`-action preflight|activate`）。**它无法自动验证
  「所有旧副本已下线」**，这必须是人工前置步骤，工具用 `-yes` + 显式告警要求确认。
- 无 online deactivate：回退意味着 legacy id 落在消费者游标下方，是同一种丢失的镜像。

**D1 的「no fallback」与此不冲突，但必须分清**：未激活 → 走 legacy，这是**设计的模式**；
已激活但 Redis 失败 → 返回错误、拒绝入队，**不**偷偷回退 GenSeq。

**D6.（review 后新增）激活状态的权威在 MySQL，Redis 只是镜像 —— 对齐 #627，但不照搬。**

只把 mode 放 Redis 是错的：生产 `appendonly no`，RDB 回滚会让它退回，而**丢失后降级回
legacy 会发出低于计数器已发出号的 id**，落在活跃游标下方，正是 #697 的镜像。

- 权威：`octo_bot_event_seq_state`（singleton：`mode`/`epoch`/`cutover_floor`），
  `FOR UPDATE` 的 CAS flip + `ErrFloorTooLow` 的 floor 校验，形状与
  `octo_message_extra_version_state` 一致
- 镜像：Redis `botEventSeq:mode`，**唯一目的**是让 mode 能与 `INCR` 在同一个 Lua 里原子读取
  （热路径不能加 DB 往返）
- 镜像丢失 → 分配器读权威行；权威说已激活就**重建镜像 + 重新 seed 并继续**（自愈），说
  legacy 才走 legacy，读不到则按 expected-mode 决定 fail-closed 还是当作未迁移

**刻意不复用 #627 的 `FOR SHARE` drain barrier**：那个 barrier 要求每个写入方在业务事务内
持状态行锁到 commit，而 `robotEvent` 的写入是 `INCR` + `ZADD` 纯 Redis，**没有 commit 可持**；
为借用它而给每次分配包一个事务，等于把分配器要避免的「每条消息一次 DB 往返」原样请回来。
**代价必须写在纸上**：本方案在 flip 瞬间无法 drain 在途写入方，只能靠「运维先确认无旧副本」
+ flip 前后短暂停写。

**D3. 现存碰撞 member 不重编号**（详见 Open questions 1 —— 这是唯一需要人类签字的一条，
因为它是唯一涉及写生产数据的选项）。

**D4. `reminders` / `conversation_extra` 已单独立项**
（`Mininglamp-OSS/octo-server#700`，P1），不在本任务范围。

**D5. `tools/genseq-repro/` 保留在 `tools/`**，与 `msgextra-version`、`card-action-dlq`
同性质（排障/验证工具）。实现阶段把核心断言抽成 `pkg/botevent` 下的正式测试，工具保留
供运维复核。

## 实现记录（2026-08-05，commit b4fca040）

实现过程中三处偏离 / 补充了本 brief 的原始判断，记在此处而非默默改掉：

**①（已更正）站点是 6 个，第 6 个是 `addInlineQuery`，而它的事件**有**消费路径。**
新增守卫立刻抓到 `modules/robot/api.go` 的 `addInlineQuery` 也从 `RobotEventSeqKey+robotID`
取号 —— 本 brief 与 `modules/robot/api.go:232` 的注释都只说「five ZADD sites」，都漏了它。

第一版处置是「隔离到独立 `inlineQuerySeq:` key，因为 `inlineQueryEventsMap` 无人读取」。
**这个前提是错的，review 抓到**：`getEventsResult`（`modules/robot/api.go:1089`）会读该 map，
把 inline query 事件与 ZSET 事件**合并、按 `EventID` 排序、按同一个 cursor 过滤**。两个
id 空间共享一个游标。隔离后果比原缺陷更糟：新 GenSeq key 首号约 1000001，而计数器从低位
起，一条 inline event 就把客户端游标顶到百万，之后的普通事件全部被永久过滤。

**调查失误的具体原因值得记下来**：当时那次 `grep inlineQueryEventsMap` 加了 `| head`，
输出恰好 10 行被截断，漏掉了 1089 行的读取，然后据此断言「没有任何读取路径」。**截断过的
搜索结果不能用来证明「不存在」。**

最终处置：`addInlineQuery` 改用 `botevent.NextEventID` —— 与队列事件同一号源，未激活时
同为一条 GenSeq 序列，激活后同为一个计数器，任何时刻单源。

**② 队列 key 统一到 `botevent.QueueKey`。** 原先 5 处各自 `fmt.Sprintf("robotEvent:%s", ...)`。
seed 必须读 producer 写的同一个 key，硬编码分散是真实风险。统一后必须同步更新
`TestEveryBotEventQueueWriterRingsTheDoorbell` 的 `queueKey` regex —— 否则它 `matched=4 < 5`
直接 fatal（那个守卫的防盲下限按设计生效了，值得记一笔）。

> **更正（第三轮）**：这条当时写成已完成，实际只做了 4/5 —— `modules/robot/event.go:367`
> 仍从 `rb.robotEventPrefix` 拼 key，`api.go` 的读/删两处同样，消费侧
> `modules/bot_api/bot_api.go:32` 另有一份字面量。PR 描述当时是诚实的（"four of the five
> writers"），**brief 不是，而 brief 才是下一个人当记录看的东西**（review 抓到）。第三轮
> 已真正做完：`robotEventPrefix` 字段和 `bot_api` 的常量都删除，六个生产点与消费侧全部
> 走 `botevent.QueueKey`。

**③ 测试用独立库 `botevent_test`，不用共享 `test` 库。** `seq` 表由
`modules/common/sql/20211108000001_common_legacy01.sql` 管理；在 `test` 库裸建它会留下
`gorp_migrations` 无记录的表，下个包的 `NewTestServer` 就 `Table 'seq' already exists` ——
与 message 包大量 DB 测试被 skip 的根因同一个。已在实现中踩到并修正。

验证：`pkg/botevent` 全包 19 测试绿（`-race`）；`modules/robot`、`modules/group`、
`modules/bot_api` 整包绿；`go build ./...`、`go vet`、`gofmt`、`make i18n-extract-check`、
`make i18n-lint` 全过。

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


## 第二轮实现记录（review 之后，2026-08-05）

两位 reviewer 独立指出 co-recovery 论证「只证唯一性、不证相对游标的单调性」，据此加了 D6
与 durable high-water。实现过程中被测试推翻的、以及 review 抓到的，逐条记下：

1. **回退检测在并发下误判**。`INCR` 原子，但并发调用者**不按发号顺序观察结果** —— 拿到 100
   的可能比拿到 140 的更晚到达检查点。无容差比较把普通并发判成回退，并发测试暴露为「200 次
   分配跨度 6000」。加 `rollbackTolerance`（一个 legacy block：远高于真实在飞数，远低于真实
   回退幅度）+ CAS 维护最大值。**代价写明**：小于该容差的回退检测不到，靠下次 seed 修。
2. **并发首次分配各自 seed**。后到者在赢家已发号并推进高水位之后才算 floor，于是 floor 落在
   活跃 counter 之上，每个竞争者烧掉一个号段。改为 per-bot 单飞 + 双检。
3. **高水位写的是 `v + interval` 而非 `v`**，与 seed margin 叠加放大跳变。
4. **gate 哨兵用 `v < 0` 判断**，于是变负的 counter 会被读成「未激活」而静默降级。改为精确
   匹配；加第二个哨兵后又漏改这处校验，被测试再抓一次。
5. **缺 `EXISTS` 守卫**（review 2.2）。`INCR` 在缺失键上返回 **1**，而唯一的阻挡是 `seeded`
   —— **进程内状态守着 Redis 状态**。mirror 存活、counter 被删、进程不重启，该 bot 的事件会
   从 1 重新编号，比快照回滚更糟（回滚一个窗口后自愈，这个要重发完整个历史号段才有一个事件
   可见）。加 `-2` 哨兵，**发号前**失败而不是发完再检测。
6. **`ModeKey` 与合法 counter key 碰撞**（见 D1 更正）。
7. **写侧计数器接了 Prometheus，不留 follow-up**。原打算沿用 `bell.go` 把 metric 归给 G1
   的先例，但那条先例适用的是**门铃丢失**（代价有界：一个 chunk 的延迟）。这四个计数器
   （回退 / counter 丢失 / 镜像重建 / 高水位写失败）是四个**自愈**机制的唯一可见性 ——
   自愈意味着没有 error、没有失败入队、没有可告警的日志，不接就等于 RDB 安全边界静默退化。
   用 `promauto` 注册到 default registry（同 `pkg/i18n/details.go`、`modules/oidc/metrics.go`），
   无 label（brief 要求低基数），`healCounter` 同时保留一个 atomic 供测试断言，避免生产
   代码依赖 `prometheus/testutil`。
8. **`afterIssue` 与 `mirrorMissing` 互相递归无界**（自审发现，非 reviewer）。Redis 反复丢
   状态时会栈溢出 —— **进程崩溃**，比丢一个事件严重。加 `maxAllocAttempts = 3` 并把尝试
   计数穿过所有自愈路径。
9. **测试库判断反复了一次**：为避开污染改用独立库 `botevent_test`，**错得更隐蔽** —— CI 从不
   创建它，于是所有集成测试在 CI 里静默 `Skip`，与通过无法区分。已改回 `test` 库；CI 每包前
   `DROP/CREATE test` 才是让这里裸建 migration 表安全的前提。

**验证**（按 `.github/workflows/ci.yml` 的方式：`-race -shuffle=on` + 每包前重置库）：
`pkg/botevent`（28 测试）、`pkg/redis`、`modules/robot`、`modules/group`、`modules/bot_api`、
**`modules/message`**（brief 点名、上一轮漏跑，review 抓到）全绿；migration 经真实
sql-migrate 应用（`gorp_migrations` 有记录、状态表已建）。

## 第三轮实现记录（第四轮 review 之后，2026-08-06）

两位 reviewer 独立抓到同一条：**热路径信任正向 mirror，从不校验 MySQL 权威**。另一位还抓到
上一轮我自己引入的回归。八条 P1 里六条在本轮修，两条（回退检测的设计问题）单独立项 ——
理由见下第 7 条。

1. **mode 解析没有统一入口，于是三处给出三个不同答案**（P1-1/P1-2/P1-4 是同一个缺失）。
   新增 `pkg/botevent/mode.go`，不变量收敛成一句：**权威决定，mirror 只能加速、不能翻案**。
   - 正向信念（activated）对 legacy **终态**：本进程一旦解析出 incr，可以向上刷新 epoch，
     但永不再回 legacy —— 不因 DB 报错、不因行被回滚、不因表被 drop。这让 P1-4 从「不太可能」
     变成**结构上不可达**。
   - 负向信念只在 **mirror 也不声称 incr** 时可信。`mirror=incr` + 缓存 legacy 是**冲突**，
     强制现场重读。这一条是保住 D2 的关键：flip 的传播**不等 TTL**，因为第一次看到新 mirror
     的分配就会重读权威。TTL 只兜「operator 的 mirror 写失败」这一种情况。
   - 确认过的冲突（mirror 说 incr、权威说 legacy）= mirror 被伪造 → 走 legacy + 响亮计数。
     **这不会撕裂集群**：所有副本读同一行、得同一结论，一致性来自权威而非 mirror。这句话
     正是上一轮缺的那句。
2. **D2 需要修订而不是推翻**。D2 禁的是「用缓存判决 gate」，gate 仍每次在 Lua 里读未缓存的
   mirror；这里缓存的是「mirror 背后的权威」。分歧只可能来自两个副本对同一时刻的权威给出
   不同答案，而冲突强制重读移除了这种可能。
3. **mirror 值带 epoch（`incr:{epoch}`）**。gate 比对的是本进程向权威确认过的那个确切字符串，
   于是手写 `SET botEventSeq:mode incr` 连一次分配都开不了门 —— 它没有代次，只能触发一次
   权威读。这正是 migration 里 `epoch` 列注释**声称已有而实际没有**的机制（review 记为
   spec 偏离），现在注释成真。
4. **顺序倒了：先开全局门，再做 per-bot seed**（P1-3）。改成先 seed 后发布 mirror。并且
   **mirror 丢失会失效所有 bot 的 seeded 标记**，不只是当前这个 —— mode 键丢失就是 Redis
   丢数据的证据，本进程认为已 seed 过的每个 counter 都可疑。docstring 写明「mirror 是全局的、
   seed 是 per-bot」。
5. **`gateClosed` 与快路径互相打转，是测试抓出来的**：强制重读权威后 `seeded` 仍在，
   `allocate` 的快路径又撞同一个关闭的门，一直转到 `maxAllocAttempts` 才报错 —— 表现为
   「mirror 被反复丢失」，而实际上只丢了一次。修法是重读后丢掉该 bot 的 `seeded` 让重试走
   慢路径。`refreshAuthority` 因此需要一个 `force` 参数：正向信念的短路正是这条路径要绕开的。
6. **写侧「新 score ≤ 队列 max」检测器：修订 acceptance，不硬加**（原 Acceptance 观测项）。
   理由不是推脱：那个比较做**对**了就会收敛到我已经原型过又回滚的原子 allocate-and-publish。
   并发下 A 分配 100、B 分配 101 且先 ZADD，A 再比「100 ≤ 队列 max 101」就是假阳性，除非
   比较发生在 ZADD 那一刻的同一个 Lua 里 —— 而那与 `modules/bot_mention` 的原子性边界冲突，
   已在 Out of scope 立项。两位 reviewer 在这条上判断相反（一位判「实质满足」、一位判
   「未实现」），这个论证解释了两边各对一半：现有计数器覆盖了写侧非单调的**一个**来源，
   而规格要的那个比较**在当前架构里做不成无假阳性的**。
7. **P1-5/P1-6 单独立项，门禁定在「激活前」而不是「合并前」。**
   - P1-5：`rollbackTolerance = 1000` 以下的回退检测不到，而上一轮我写的「靠下次 seed 修」
     被代码否证 —— `seeded.Delete` 只有三处且**全在已检测分支**，未检测与自愈按构造互斥。
     那句错的理由同时写在 `seq.go` 和本 brief 第二轮记录第 1 条里，是**重申**而不是承认的
     取舍。而且生产 `save … 60 10000`，任何每分钟少于 ~1000 事件的 bot（1948 个队列里几乎
     全部）回退幅度**永远**在容差内 —— 这个机制在常见情形下从不触发。
   - P1-6：回退检测只比本进程 `lastIssued`，长寿进程冷 per-bot 状态会把跨副本回退当前进。
   - 两条是同一个设计问题（什么状态会回退 / 谁检测 / 什么重建 floor），需要一张表；而它们
     **只在 `mode=incr` 之后可达**，本轮把 merge 修成真正惰性（P1-1 去掉每次分配的 DB 往返，
     P1-2 让野键无法激活）之后，延后才站得住。已立项
     `Mininglamp-OSS/octo-server#704`，**门禁是「激活前」而不是「合并前」** ——
     `tools/botevent-seq` 的 `-yes` 拒绝文案也引用了它。
8. **激活证据结构性漏掉它要保护的 bot**（P1-8）。工具只从 `SCAN robotEvent:*` 发现 bot，
   队列已 drain 的 bot 一行 `seq` 都不读；`scalarSeq` 还把任何 DB error 变成 0。改为按前缀
   `SELECT MAX(min_seq)` 全表扫两个命名空间，并像拒绝 sampled 证据一样**拒绝部分失败的证据**。
9. **`payload.robot_id` 分支从来没做存在性校验**（review 要求人工确认 robotID 来源，查得出来
   就不该挂在那里）。另外三个分支（mention.uids / @username / ais）都查了 `existRobot`，只有
   这个把客户端 payload 里的值原样当 bot id。以前的代价只是一个带 TTL 的队列键加一行 `seq`；
   新分配器会为它建一个**无 TTL** 的 counter 键（Redis `noeviction`）加一行永不回收的
   `seq`。补上校验。
10. **测试能静默退化成只跑守卫**（P2-11，与上一轮 `botevent_test` 库同一个失效模式，我在这上面
    已经栽过一次）。加 `TestMain`：`CI=true` 下依赖缺失直接失败并说明缺什么，本地仍 skip。
    测试建表也补齐 migration 的 `CHECK` 与 `ON UPDATE`。

**验证**：`pkg/botevent` 42 测试全绿（`-race -shuffle=on`，`CI=true`，**零 skip**）。

## 第四轮实现记录（第五轮 review 之后，2026-08-06）

两位 reviewer 独立抓到同一处：**上一轮记为「已修」的 P1-7 其实没修**。加上另一位的三条，
六项在本轮修完，回退检测两条仍在 `#704`。逐条记下被推翻的判断：

1. **fail-closed 没生效，而且我的测试给了假信心。** 边界是「连续失败的 interval 数」，但
   `persistHighWater` 的节流早退发生在**查这个边界之前**，且 `noteHighWaterFailure` 顶上
   无条件重新武装了节流标记。于是过界后每 1000 次分配只有 1 次真的走到检查，其余 999 次
   `return nil` 成功 —— 正是「对着冻结的 mark 无界发号」本身。算术上还不只是声明没兑现：
   limit=3 时 id 跑到 `M+2999`，而 `seedSafetyMargin` 是 2000，从 `M` 重算的 seed 落在
   `M+2000`；发号进程会 `retried <= prev` 响亮拒绝，但**重启后的进程 `lastIssued` 为空，
   会静默从 `M+2001` 接着发**，落在已到 `M+2999` 的游标下方。
   我的测试直接调 helper、参数恰好按 interval 间隔，所以从没经过那个吞掉 999 的早退，
   断言的是 helper 的返回值而不是分配器的行为。**这条批评完全成立。**
2. **边界改成「距上次真正落盘的 mark 有多远」**，即安全论证真正依赖的那个量。`lastDurable`
   与节流标记分开，失败时绝不前进；在 `seedSafetyMargin` 处拒绝，使最大已发号恰好比恢复
   首号低 1。一条不等式取代了需要手工与 margin 对账的计数，顺带删掉那个非原子的计数器。
3. **测试逼出两件事**：①过界后按 id 距离节流会让恢复取决于流量（低流量 bot 无界等待），
   改成时间预算（200ms）；但**不能不节流** —— 300ms deadline 的失败 INSERT 占着 msgSem 槽，
   对全进程所有 bot 是吞吐悬崖，不因为这次入队注定失败而改变。②未节流路径上不能先查 span：
   durable 行还不存在的 bot base 为 0，先查会在还没尝试记录之前就拒掉它的第一次分配。
4. **`mode.go` 里那句核心不变量陈述是假的，而 follow-up 的拆分理由正引用它。** 我写
   「every replica reads the same authority row and reaches the same conclusion」，但正向
   belief 直接返回、不再读权威，mirror 完好时 gate 也永不关闭。于是激活后权威回滚会让
   在跑的副本继续用 counter、新起的副本走 GenSeq —— 两个活跃号源。
   **修法不需要 DB 读也不依赖 env**：读 mirror 的那次往返顺带返回该 bot 的 counter 是否存在，
   而 counter 存在只可能因为某个副本从它发过号，也就只可能发生在权威说 incr 之后。冷启动
   看到 legacy + 活的 counter 就拒绝而非降级。**折进同一次往返**，另开一次就是 P1-1 的缩小版。
   残余（counter 也一起丢）明确写成 accepted residual 并用测试钉住。
5. **被拒绝的 mirror 会让每次分配串行读一次权威且没有出口。** mirror 声称 incr 时刻意跳过
   负向 TTL（这才让 flip 不等 TTL），但没有任何代码会改写或清掉一个伪造的键，于是每次分配
   在 `beliefMu` 后各读一次、都在 msgSem 槽内 —— 与上一轮修掉的每次分配读权威同一类。改为
   按**被拒的那个 mirror 值**缓存否定结论，换一个值（包括真正的激活）仍算冲突仍强制重读；
   forced refresh 也加了指针检查，一次 mode 键丢失只花一次读而不是每个在飞分配各一次。
6. **队列 key 第三次才真正统一。** 还剩 4 个生产点自己拼（`api_manager.go` 撤 token、
   `botfather/command.go` 三处 teardown）加一个死字段，全是 `Del`、不影响分配 chokepoint，
   但「一种拼法」这句话就是假的。现在 `grep -rn '"robotEvent:' modules/` 在非测试代码里为空。
7. **`payload.robot_id` 的存在性校验改成查询出错时 fail-open。** review 指出照原样发布会把
   一次 DB/Redis 抖动变成静默丢事件，而它要挡的增长面是「攻击者给的、可证明不存在的 id」，
   且没有调用方能让这次查询报错。所以保留校验、去掉 fail-closed，改动收敛成「已知不存在才拒」。
8. **守卫的盲拼法**：`"robotEvent:%s"` 匹配得到，`"robotEvent:" + robotID` 匹配不到 —— 而后者
   正是 `modules/message`、`modules/bot_mention` 测试里通用的写法，第六个 writer 用它会同时
   躲过匹配和防盲下限。已补全并在 vacuity 测试里逐个断言。
9. **`TestMain` 的探针不够**：只探 `SELECT 1` 与 `PING`，而真正卡住测试的是两条 `CREATE TABLE`。
   CI 上一个权限/collation 问题仍会让整包静默 skip 而 `go test` 退 0 —— 正是这个 TestMain
   要关掉的盲点。DDL 已与 `seqTestCtx` 共用。
10. **expected-mode 改 atomic 指针**：测试钩子改写的是生产代码读的包级变量，当前调用点都是
    单 goroutine 所以 `-race` 干净，但「今天干净」不值得依赖。

**验证**：`pkg/botevent` 46 测试全绿（`-race -shuffle=on`，`CI=true`，**零 skip**）；
`pkg/redis`、`modules/robot`、`modules/bot_api`、`modules/message` 整包绿。
`modules/message` 的 `TestE2E_Issue557_*` 一度红过一次并被我误判成迁移文件所致 —— 重跑两次
均绿，是依赖 IM 的 flaky，**「revert 之后就好了」那一次是巧合**，记在这里以免下次又当因果。

## 第五轮实现记录（第六轮 review 之后，2026-08-06）

上一轮的两条修复又各自以新的方式出错，其中一条是我记为「已修」的。这一轮最重要的决定是
**停止在本 PR 里迭代 durable-mark 边界**。

1. **改主意：durable-mark 边界整块移交 `#704`，不再本地打第三次补丁。**
   review 指出上一轮的 span 边界在**每次 seed 后的第一次分配**就已过界，且是确定性的：
   `seedCounter` 存 `lastDurable = durableMax`，却把计数器 seed 到 `max(sources) + margin`，
   两者天生差一个 margin。后果是第一次 durable 写失败就拒绝并**丢事件**，探针变成风暴
   （每 bot 每 200ms 一次 300ms deadline 的 INSERT，都在 msgSem 槽内，约 67 个活跃 bot
   打满 100 槽），错误文案的算术也是错的。**我的测试第三次断言在旁边那个机制上** —— 它先做
   一次健康分配才打断写入，于是从没覆盖「seed 时写入就已经坏了」这个边界存在的状态。

   我先按计划试算「让 seed 持久化自己的 floor」，算完发现修不好：

       seed 把计数器设到 S = max(sources) + seedSafetyMargin
       恢复时的 seed **重放同一个计算**，落在同一个 S。seed 后发出的号是 S+1, S+2, …，全在 S 之上。

   也就是说 seed 后的**第一个 id** 就已经在「从未变的 mark 恢复」够不到的地方 —— 暴露不是
   在某段宽限之后开始，是立刻开始。要关掉它必须在 seed 时推进 mark，而记录了 seed 值的 mark
   会回灌进下一次 seed 的 floor，于是**每次 re-seed 复利一个 block**，并推翻
   `TestSeedIsIdempotentAndNeverLowers` 守的「再 seed 是 no-op」。这不是改一个比较式，是
   **改变 recovery floor 的定义** —— 正是 `#704` 那张表要做的事，两位 reviewer 也都建议移过去。

   所以现在：保留 mark、节流（失败也重新武装，这是 P1-7 真正修好的那半）、metric，**如实写出
   「写失败期间 mark 无界滞后」**，并用 `TestFailedDurableWriteIsThrottledAndDoesNotFailTheEnqueue`
   把这个 gap 钉成测试事实（断言它**仍在**，将来修好会失败并提醒改成回归测试）。
   算术与两次失败的原因都写进了 `#704` Gap 3，以免第三次重试同一条路。

2. **上一轮的「按被拒 mirror 值缓存否定结论」能吞掉真正的激活。** `Activate` 把 epoch 0→1，
   工具随后写的 mirror 就是 `incr:1` —— 与一个可能早已躺在 Redis 里的伪造 `incr:1`
   **字节相同**，缓存命中，那些副本在 TTL 内继续 legacy 而别的副本已用 counter。激活瞬间
   两个活跃号源。我 docstring 里写的「换一个值、包括真正的激活，仍算冲突」是错的。
   **改为删除**与权威矛盾的 mirror（按值 CAS 删）：权威说 legacy 时正确的 mirror 状态就是
   「不存在」，这与「权威说激活就重建 mirror」是对称的修复，于是没有缓存可过期，读风暴也
   不需要缓存来挡。只剩「删不掉时」的短冷却（1s，而那种状态下 counter 的 INCR 也在失败）。
   工具另加一道：检测到未授权 mirror 时**拒绝激活**，把前置条件在有人值守的那一步消掉。

3. **`invalidateSeeded` 的删除顺序**能让并发 reseed 留下「已 seeded 但配套状态被清」的 bot。
   改成 `seeded` 最后删 —— 最坏交错变成「未 seeded 但有残留簿记」，下次分配重 seed 即修。

4. **staleness 快捷路径**会用「针对另一次 mirror 观测解析出的 belief」作答：加 mirror 复核。

5. **那句运维在事故里唯一会读的文案是错的。** `counterExists` 拒绝里我写「restore the state
   row (or set `OCTO_BOTEVENT_EXPECTED_MODE=incr`)」，但设了这个 env 会走到
   `assertExpectedMode(false)` 报错，分配照样失败 —— 它是 fail-closed 断言，不是手动 override。
   已改成说清它到底买到什么。Rollout 第 6 步同样错（三种形态里两种已不再自愈），已拆成三个
   metric 并写明哪个不自愈。

6. **`payload.robot_id` 的 fail-open 路径加语法界**：长度 ≤ `robot.robot_id` 的列宽（40）+
   拒空白/控制字符。长度这条不需要判断力 —— 超过列宽的值不可能匹配任何行，所以采用它只可能
   为一个可证不存在的 bot 创建永久状态。**刻意不做字符集白名单**：生产 id 是 UUID-hex，但列里
   存什么都行，猜窄了会静默掉一个真 bot 的事件。另加一条测试把常量与列宽绑在一起。

7. **score-source 守卫标为 DEFERRED** 并记入 `#704` Gap 4 —— 上一轮说了「建或移」，结果两样
   都没做，于是 brief 挂着一条无人跟踪的验收项。

**验证**：`pkg/botevent` 45 测试全绿（`-race -shuffle=on`，`CI=true`，**零 skip**）；
`modules/robot` 新增 `plausible_robot_id_test.go`。

## 第六轮实现记录（第七轮 review 之后，2026-08-06）

上一轮我把 durable-mark 边界移交 #704 是对的（reviewer 明确认可），但**同一种复发转移到了
mirror repair** —— 它已被重写两次（缓存否定 → 删除键），第二次有了新的失效模式。这一轮
先复现、再修，且第三种做法是「不再自动修 mirror」。

1. **删除与权威矛盾的 mirror，在「权威才是回滚方」时是全 fleet 中断（review P1-1）。**
   `allocate` 在 `:596` 就拿到 `counterExists`，`:604` 删 mirror，`:611` 才用同一个
   `counterExists` 断定「权威曾激活过、现在回滚了」并拒绝 —— **代码在确认权威不可靠之前，
   已经按权威的说法销毁了 mirror**。一个新起的副本就能删掉每个健康已激活副本 gate 比对的
   那个 artifact，于是它们 gate 返回 -1 → 强制读权威 → legacy vs 已激活 belief → 拒绝
   每一次入队，直到有人恢复状态行。**一次滚动重启把零投递影响的簿记回滚放大成全面中断。**
   与第四轮被接受的 P1-3 同族：推翻操作前提的证据在操作之前算出、在操作之后才被使用。

   **我先写了复现测试并看它失败**（`TestRegressedAuthorityDoesNotDestroyTheLegitimateMirror`），
   报错正是「the legitimate mirror "incr:0" was destroyed」。上一轮随修复发的测试第三次
   断言在旁边的性质上 —— 它 `Del` 掉了 counter，所以 `counterExists` 全程为 false。

2. **修法不是加 `!counterExists`，是不再删。** reviewer 给的最小修法能收窄窗口但不闭合：
   mirror 是全局的、counter 是 per-bot 的，一个从未收过事件的 bot 的分配仍然没有 counter
   可以反对。而删除的**唯一收益**是压住读风暴，而冷却也能压住 —— 所以删除整体去掉，不是
   加条件。现在**分配器在 legacy 路径上永不写 mirror**，只有 operator 工具写它，那是唯一
   有人能看见自己在覆盖什么的地方。
   代价如实写出：按值缓存的否定结论有 ≤1s 的窗口，期间「与被拒值字节相同的真激活」不被
   察觉。这个窗口不可能为 0（区分二者只能读权威），且只在「已有 mirror 声称激活而权威说
   legacy」时进入 —— 而工具现在拒绝在这个前置条件下激活。冷却**只在无 counter 时武装**：
   有 counter 时分配一律拒绝，读风暴伤不到正常流量，且权威一恢复立刻生效。

3. **工具不幂等，且它的拒绝断言了一个它没检查过的事实（P2-1，两位 reviewer 独立发现）。**
   `refuseUnauthorizedMirror` 跑在 `Activate` 之前且**从不读状态行**，于是在「已成功激活 +
   mirror 正确」这个正常状态下重跑 `activate -yes` 会死掉，并告诉运维「…while the authority
   says legacy…then DEL the key」—— 每一句在常见情形下都是假的，还指示删掉活的 mirror。
   已改为**先读权威**。并且把判定抽成纯函数 `judgeMirror`，给这个工具补了**第一个测试文件**
   —— 它承载整个激活流程、是切换时唯一由人手跑的组件，两轮出两个问题而零测试。

4. `belief.confirmed` 只写不读（docstring 承诺了一个没人 surface 的区分）→ **删掉字段**，
   两个 warn 调用点本身就是那个区分的记录。`install` 永不返回错误而三处都在检查 → 去掉。
   `plausibleRobotID` 卡字节而理由说的是字符（`VARCHAR(40)` 在 utf8mb4 下按字符）→ 写清它
   **故意是更严的那个**，并说明为何今天不可达（两条产 id 的路径都是纯 ASCII）。
   `invalidateSeeded` 的 docstring 夸大了顺序的重要性（span 边界移走后 `lastPersisted` 只是
   节流标记）→ 改成真实的赌注。
   botfather 保留 per-bot counter 的注释补上「为什么 bot-count 尺度可接受、payload 尺度不行」
   —— 滥用单位不同：payload 字段每条消息免费且无界，bot 是限流、审计、有配额的对象。

5. **「behaviour-neutral … exactly as before」第三轮被提，这次改在声明本身**：未激活时每次
   分配确实多一次 Redis EVALSHA（`probeAllocatorState`），而 GenSeq 999/1000 次零 I/O。
   披露从 Rollout 第 3 步（写成激活的后果）移到 Summary 里那句声明旁边，并点名
   `addInlineQuery` 是唯一真正新增 Redis 依赖的地方。

6. **#704 加 Gap 5**：mirror repair 没有安全的自动形式（两次尝试及各自失效原因），加上它依赖
   却无人审计的两件事（`counterExists` 只是 EXISTS、无 provenance 无 TTL；每进程重启把每个
   活跃 bot 的 counter 抬 ~2000 的 id 空间消耗），以及保留状态的回收 sweep。

**验证**：`pkg/botevent` 46 测试、`tools/botevent-seq` 首个测试文件、`pkg/redis`、
`modules/robot`、`modules/bot_api`、`modules/message` 全绿（`-race -shuffle=on`，`CI=true`，
零 skip）。`modules/message` 的 `TestE2E_Issue557_*` 一度连续红，查明是**本地 WuKongIM 的
raft 超时**（`propose batch until applied timeout` / `store message failed: context deadline
exceeded`）—— 在 HEAD 上同样红，重启 IM 容器后即绿。

## 第七轮实现记录（head `6a5cd9a6` 重审之后，2026-08-09）

最新重审确认两处合并即暴露的边界，均先加失败测试再修复：

1. **expected-mode 拒绝没有保存已解析的权威结果。** 当权威为 legacy 而
   `OCTO_BOTEVENT_EXPECTED_MODE=incr` 时，原实现先返回 mismatch error、后 `install`，所以每条
   被拒消息都在 `msgSem` 槽内重新读取 MySQL；拼错值（如 `inrc`）也先 probe Redis、再读 MySQL
   才失败。现在 malformed guard 在任何依赖 I/O 前拒绝；已成功解析的权威 belief 先保存、再执行
   assertion，缓存 belief 每次使用仍重新 assertion。结果是 mismatch 首次最多一次权威读、之后
   快速拒绝，不会因缓存 negative belief 而放行 legacy；mirror 真正翻到新 epoch 时仍会触发刷新。
2. **`payload.robot_id` 的 40 字节形状界只限制单键大小，不限制键数量。** 原来的 lookup-error
   fail-open 允许客户端在 DB/Redis 故障窗口提交无限多个不同但合法形状的 id，并为每个值留下无
   TTL counter 与永久 `seq` 行。现在只有 `err == nil && exists == true` 才采用客户端 id；查询失败
   会记录错误并忽略这条仅靠 `payload.robot_id` 路由的事件。这个可用性取舍在**合并部署后立即
   生效**，与 allocator activation 无关；它避免把一次依赖故障放大成永久、无界状态增长。

同时更正 rollout 残余的描述：需要 fail-closed assertion 覆盖的真实证据空洞是 **mirror 缺失或
非法 + authority 不可读 + 当前 bot 尚无 counter（新建或空闲 bot）**，不只限于「mirror 和
counter 被同一次 Redis 恢复一起丢失」。因此激活验证完成后仍必须最后滚动设置
`OCTO_BOTEVENT_EXPECTED_MODE=incr`；它是断言，不是激活开关。

## 第八轮实现记录（head `c175ca6e` 重审之后，2026-08-09）

最新重审确认上一轮两条 blocker 已修，但又找出四处会误导激活操作或评审边界的声明，以及
一处激活 floor 风险。本轮继续先写失败测试，再做最小修复：

1. **MySQL 已翻转、Redis mirror 写失败不能算成功。** 原工具只打印 warning 并退出 0，还说
   下一次 allocation 会立即修复；实际已有 negative belief 的副本最长 5 秒仍可走 GenSeq。
   `writeMirror` 现在返回 error，两条 activate 路径都 fatal 非零，并明确要求保持 bot-event
   停写，直到 `botEventSeq:mode` 出现期望的 `incr:{epoch}`。测试
   `TestMirrorWriteFailureStopsActivationAndExplainsPause` 覆盖退出契约依赖的错误内容。
2. **`-yes` 不再隐藏前置条件。** 三项前置条件每次 activate 都输出；`-yes` 只表示 operator
   已核实。清单补上 cutover floor 不能继承已回退的 legacy `min_seq` 并落到现存 consumer
   cursor 下方；完整算术交由 #704。
3. **unauthorized mirror 不会被删除，也不会自动自愈。** allocator 无法判断回退的是 mirror
   还是 authority，因此保留冲突键；日志、metric 注释和 operator 文案全部改成要求人工诊断，
   只有确认 mirror 非法后才可删除。allocator 仅在已验证激活且当前 bot 完成 re-seed 后重建
   丢失的 mirror；不是 legacy 路径上的自愈。
4. **证据空洞比上一轮写得更宽。** 精确条件是「任意 negative belief + 当前 bot 无 counter」：
   包括 authority 可读但已回退为 legacy/missing，也包括 authority 不可读且 mirror 不声称激活。
   `OCTO_BOTEVENT_EXPECTED_MODE=incr` 仍是激活验证后的最终 fail-closed assertion。
5. **守卫边界保持 DEFERRED。** 已有 guard 证明每个 queue ZADD 会响 doorbell；它不证明 score
   一定来自 allocator。score-source guard 仍由 #704 Gap 4 承接，PR 描述不得再把两者等同。

**验证**：新增两条工具测试转绿；`pkg/botevent`、`tools/botevent-seq`、`pkg/redis`、
`modules/robot`、`modules/bot_api`、`modules/group`、`modules/message` 均以
`-race -shuffle=on` 通过（模块包按 CI 方式逐包重建 MySQL test 库并清 Redis）；另通过
`go build ./...`、`go vet ./...`、i18n extract/lint 与 `git diff --check`。

## 第九轮实现记录（head `d96efa38` 后续复核，2026-08-09）

本轮先为七个可复现缺口提交 RED checkpoint（`795675f0`），逐项确认失败来自目标逻辑，
再提交生产修复（`2c3a0d18`）：

1. **已验证的正向 belief 丢失 cutover floor。** floor 现在与 `activated` / `epoch` 一起保存在
   不可变 belief 中，后续 per-bot seed 直接使用同一次权威读取验证过的值；不会再单独重读状态行
   并把读取失败折成 0。`TestValidatedBeliefRetainsTheCutoverFloor` 覆盖权威行随后不可读的形状。
2. **一次 seed 做两次同表 MySQL 查询。** legacy ceiling 与 durable high-water 合并成一条带
   deadline 的条件聚合查询；mirror 丢失导致全进程 seed 失效时，每个活跃 bot 只产生一次 DB
   往返。既有 I/O-shape guard 与真实行值集成测试同时覆盖。
3. **preflight 对单队列做 `ZRANGE 0 -1` 并保留 score-sized map。** 改为每页 500 条的 rank
   分页；利用 ZRANGE 按 score/member 排序的性质，仅保留上一条 score，就能跨分页统计重复且
   内存为 O(1)。激活仍要求写暂停，因此 rank 视图不会在证据采集中漂移。
4. **两个 recovery retry 对 gate sentinel 解释不一致。** `gateNotActivated(-1)` 统一重新进入
   权威恢复；`gateCounterMissing(-2)` 统一按 counter 再次丢失 fail closed，不再把 -2 误报为
   数字 floor。两个 race fixture 分别在 retry 前删除 mirror / counter。
5. **正向 belief 会覆盖更高 mirror epoch。** 观察到高于本地 belief 的 `incr:N` 时强制刷新
   MySQL 权威；权威不能确认该代次就拒绝，确认后才允许继续，旧进程不能把新 mirror 写回旧代次。
6. **运行时 counter 没有 float64 score 精度上界。** `runGate` 在 Redis `INCR` 后拒绝
   `>= 2^53` 的 id；否则相邻 int64 会映射为同一 ZSET score，重新制造 #697 的碰撞、游标跳过
   与按 score ack 连带删除。operator floor 的 2^50 上界仍保留更大的提前余量。
7. **未知 `payload.robot_id` 既重复查 MySQL，又吞掉 sibling mention 路由。** 可证不存在的 bot
   以 `robot:exist:{id}=0` 负缓存 30 秒；仅当 payload id 正向验证成功才停止后续路由，否则继续
   解析 `mention` / 文本 @。短 TTL 明确承担“刚创建 bot 最多延迟 30 秒被该缓存看见”的取舍，
   避免客户端控制的重复未知 id 把 listener 变成逐消息 DB 查询。

**完整验证**：按 CI 脚本逐个 `go list ./...` 包执行 `-race -shuffle=on -count=1 -timeout=5m`，
每包前重建 `test` 库并 `FLUSHALL`；本地漏设 CI 的 `max_connections=1000` 时
`modules/common` 首次以 `Error 1040` 失败，对齐 CI 后单独重跑通过，其余包一次通过。
另通过 `CGO_ENABLED=0 go build ./...`、`go vet ./...`、`golangci-lint run ./...`、
`make i18n-extract-check`、`make i18n-lint` 与 `git diff --check`。#704 的 activation-only
Gap 1–7 仍是独立门禁；本轮没有把“合并/部署”描述成“已激活”。

## 第十轮实现记录（head `60c2dd0e` 重审之后，2026-08-09）

重审确认第九轮 preflight 的内存修复引入了一条 cutover 证据回退：停写不等于停 ACK，
低端 `ZREM` 会在两次 rank 分页之间把后续成员左移，使分页提前结束并静默低估队列最大 score。
该值直接参与不可逆激活的 floor 校验，因此属于 blocker。

本轮先提交编译期 RED（`ad187e12`），用 2000 个 score 模拟第一页后 ACK 掉前 1600 条；旧实现
只观察到最大值 500，而真实最大值 2000 仍在队列。修复将两类证据拆开：重复 score 数量继续用
500 条 rank 分页与 O(1) 状态做诊断；激活所依赖的最大 score 单独用一次原子的
`ZREVRANGE 0 0 WITHSCORES` 获取。ACK 只会缩小队列，所以预先捕获的最大值可能保守偏高，
不会低估 cutover floor；新写入仍由激活前停写前置条件排除。
