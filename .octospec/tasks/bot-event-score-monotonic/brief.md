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
