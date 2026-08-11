---
type: Task
title: "Task: token-session-rollout-simplify"
description: Turn the #725 five-phase session rollout into a self-driving closed loop — floor loss resolving upward, writer registry, floor-derived mode, an absence-proving predicate, and a reconciler that advances the floor without operator commands
tags: [auth, security, redis, mysql, session, rollout, migration, operability]
timestamp: 2026-08-11T19:40:00+08:00
# --- octospec extension fields ---
slug: token-session-rollout-simplify
upstream: "follow-up to PR #725; test-env run 2026-08-11 (<cluster>/<namespace>)"
source: self
---

# Task: token-session-rollout-simplify

> #725 的阶段机是为**一次性迁移**设计的，但被固化成了常驻架构。本 brief 不推翻它的安全模型，
> 只把"保护存量 legacy session"这一目的与"每个新部署、每次 Redis 事故都要付的仪式成本"解耦。
> 全部结论以 2026-08-11 测试环境实操为依据，不是纸面推演。
>
> **rev 2**：主形态由"4 条 CLI 命令"改为"常驻 reconciler"。上一版把"运维工具"当成了前提；
> 但 `advance` 的谓词里没有任何一处需要人判断，让人去敲它是设计惰性。
> CLI 降级为排查 + 业务决策输入 + 逃生门。
>
> **rev 3（2026-08-11 19:40）**：三处修正 + 三处补齐。
> ① floor **不搬 MySQL**——rev2 的"权威 + 镜像"是两阶段写，Redis 侧失败无法回滚，
> 而 floor 单调没有撤销操作；改为 floor 留在 Redis，MySQL 只加一行 write-once 标记，
> 丢失时**向上收敛**（§1）。② 收回"floor + 证据必须同一事务"的主张，正确要求是顺序（§1）。
> ③ C6 降级为"可达性未验证"。补：`## Rollback`、`## PR 拆分`、reconciler 自升级 bootstrap 规则。
> 全部 6 条对现状的断言已实测，见 `verification.md`。
>
> **rev 3.1**：fence 不再失败 readiness——Redis 不可达是全体副本同时发生，
> 摘流会把认证降级放大成 fleet 级中断，比现状更差，而 readiness 那层对门禁毫无贡献
> （invariant 4）。同时关闭两个伪决策项：floor 丢失取 enforce、fence 下限。
>
> **rev 4（review 后）**：三位 reviewer 提出 10 条 blocking，全部复现并修复。
> 两处改规格而非硬做：① 谓词去掉"指纹匹配"项（§6 说明理由，指纹本身仍纳入实例身份）；
> ② greenfield 不再宣称"零配置"——`EXPECT_WRITERS` 无条件要求（§3）。
> 最严重的一条（首启丢弃 floor → 全员登出）是 rev3.1 那次"向上收敛"修复造成的：
> 把 marker 读取失败当成了 floor 丢失，而"读不到"和"不存在"是两回事。
>
> **rev 3.4**：交付形态定为**一个 PR**。前几版都在优化"什么能安全地先合"，
> 但交付物就是那个行为变更——新 PR 的定义是替换掉繁琐流程，不是零风险地挪一格。
> 同时发现 C6 被重设计自动消解，Decision #8 不再 gating。
>
> **rev 3.3**：Decision #1 定为"先落 `pkg/auth`，按可提取形状写"。
>
> **rev 3.2**：补 §3.5「从 #725 升级的兼容规则」。判定表原缺
> `标记缺失 + floor 存在` 一格，而那正是测试环境与所有在途部署的真实状态；
> 且 `mode = floor` 的朴素实现会让 Phase D 中 `MODE=bounded`+`floor=revoke` 的部署
> 在升级时静默放松到 `revoke`，必须取 `max(floor, 遗留 MODE)`。

## Goal

在**不削弱任何已有安全不变量**的前提下，把 session rollout 从"运维仪式"收敛成"自驱闭环"：

1. **floor 丢失时向上收敛，不再 panic。** floor 仍然只存 Redis（保留现有 Lua CAS）；
   MySQL 只增加**一行 write-once 标记**，用来区分"从未初始化"与"初始化过但 Redis 回滚丢了"。
   前者从 `expand` 开始，后者取 `enforce`。消除"Redis 丢 key + `mode ≥ revoke` → 启动 panic"。
   测试环境 Redis 实测 `appendonly no`，只有 RDB 快照。
1. **引入 writer registry（写租约）。** 把"所有旧副本已清零"从 runbook 里的人工断言变成
   机器门禁，并据此取消 `bounded`/`enforce` 前各 2 次、间隔 ≥1h 的观察仪式。
2. **mode 由 floor 派生，不再来自 env。** 取消 `OCTO_AUTH_SESSION_MODE` /
   `OCTO_AUTH_SESSION_REQUIRED_FLOOR`，滚动重启次数从 9 次降到 1 次，
   `mode < floor` 的 panic 结构性消失。
3. **修正证据谓词。** 从 `Total > 0`（`session_rollout_evidence.go:99`）改为
   `v1 = 0 ∧ v2 = 0 ∧ registry 已收敛`（rev4 去掉了"实例身份匹配"一项，理由见 §6）。
   greenfield 直通是这个正确谓词的**自然推论**，不需要单独的 greenfield 分支或
   `--greenfield` 标志——但它仍需 `EXPECT_WRITERS`（rev4，见 §3）。
4. **floor 推进由常驻 reconciler 完成，不由人敲命令。** 谓词里没有需要人判断的部分；
   人的唯一介入点是 migration 的业务参数，而系统会**自动停在**那个位置并说明差什么。
5. **工具并入 `/home/app`，且只保留三件事**：排查（`status`）、业务决策输入（`migrate`）、
   逃生门（`pause` / `advance --force`）。私有化交付当前必然漏带
   `token-session-admin` / `token-session-observe`（Dockerfile 只 build `./main.go`）。

目标终局：

```
greenfield ：部署新制品（带 EXPECT_WRITERS=<副本数>）。结束。（0 条命令，floor 自动到 enforce）
brownfield ：部署新制品 → reconciler 自动推进，停在第一个有存量的门禁上
             （有永久 legacy → 停在 revoke；只剩有限 legacy → 停在 bounded）
             → 人做一次业务决策：migrate --cutoff T --finite-policy P
             → 到期后 reconciler 自动推到 enforce
```

挂钟成本从"≥2h 安全税"变成"运维自选的 cutoff"——后者是业务决策（愿意让多少人提前重登），
不是安全下界。

## Background

### 已合并基线

`87118f6`（#725）交付了 `expand → v3-write → revoke → bounded → enforce` 单调阶段机、
持久 floor、v3 绝对到期、generation、有界索引、durable revocation intent、可续跑 migration。
runtime 默认 `expand`，合并/部署行为中立。运维流程见 `docs/token-session-rollout-runbook.md`。

### 2026-08-11 测试环境实操（本 brief 的事实依据）

环境 `<cluster>` / ns `<namespace>` / deploy `<deployment>`，
镜像 `<commit>`，Redis `<redis-service>`（`appendonly no`——只有 RDB 快照，无 AOF）。

| 指标 | 迁移前 | 迁移后（cutoff 生效） |
|---|---|---|
| total | 194 | 137 |
| **persistent (PTTL=-1)** | **57** | **0** |
| finite | 137 | 137 |
| v1 / v2 / v3 | 53 / 140 / 0 | **0** / 137 / 2 |
| decode_invalid | 1 | 0 |
| over_max | 0 | 0 |

campaign `test-2026-08-11-a`，cutoff `13:00 +08:00`，`--finite-policy natural`，
`shortened 57 / deleted 0 / complete true`。机制是 `PEXPIREAT` 而非 `DEL`。
54 个 v1 全部是永久 token，迁移后 v1 整代清零。当前 floor = `revoke`，
遗留 135 个 v2，最长 720h；`bounded`/`enforce` 未做，prod 未动。

实测成本：`v3-write → revoke` 两跳合计 **60 秒**，其中绝大部分是两次 pod 重启——因为
`validateRolloutAdvanceEvidence` 对非 `bounded`/`enforce` 直接返回 nil
（`session_rollout_evidence.go:153`）。**整套证据机的成本全部集中在最后两跳。**

那 2 个 v3 来自 `promoteLegacySession`（`session_v3.go:305`）而非新登录：`ReuseSession`
路径把仍有效的 legacy 提升为 v3 并继承剩余 TTL。因此遗留 v2 的自然消亡不只靠过期，
**活跃用户会被逐步提升**，只有不活跃用户的 token 会拖到 720h。

### 实操暴露的四个新问题（均不在 #725 的已知风险清单里）

> 以下每条都已在 HEAD 上实测复现，证据与复现步骤见同目录 `verification.md`，
> 原 tripwire 文件引用了本次删除的 `ValidateRolloutControl`，「翻红」会变成编译失败并
> 拖垮整包，因此按 brief 的说法把断言取反，并入 `session_rollout_boot_test.go` 与
> `session_rollout_wiring_test.go`；原始测量结果保留在 `verification.md`。

**① 观测/迁移目标可被静默错配，且现有指纹无法发现。**

配置键误写在顶层 `redisAddr`（octo-lib 实际读 `db.redisAddr`），键未命中 → 回落默认
`127.0.0.1:6379` → 工具连上**本机** Redis，扫出本地集成测试残留并报 `"complete": true`
（total 33）。工具不打印它连的是哪个地址。

`scope_fingerprint` 只覆盖 `tokenPrefix + uidTokenPrefix + maxTTL`
（`session_rollout_evidence.go:238`），三者全部来自同一份配置文件，**不含实例身份**——
两次连不同 Redis 得到的指纹完全相同（`<fingerprint>`）。

这条推翻了"指纹可以校验观测目标"的假设。修法是现成的：`currentRedisInstanceID()`
（`session_migration.go:503`，`INFO server` 的 `run_id`）migration checkpoint 已在用，
把它纳入指纹即可。若当时执行的是 `--apply`，后果是改错 keyspace。

**② `observe` 与 `migrate` 的版本口径不一致。**

- `observe` 走 `ReadToken` → `Decode()`，坏 payload 计入 `decode_invalid`；
- `migrate` 的 Lua 只看前 3 字节，`version` 默认值就是 `1`（`session_migration.go:22-27`），
  **坏 payload 被当作 v1 正常处理**（缩短 TTL / 删除），且不单独报告。

同一批 key 两者 v1 计数差 1（53 vs 54）。`enforce` 门禁依赖 observe 的 `v1=0`，
而实际收敛动作由 migrate 执行——两把尺子量同一件事。reconciler 依赖这个谓词自动推进，
口径不一致会直接变成自动化的错误决策，因此在本次范围内必须统一。

**③ `MAX_PER_UID` 与不复用 session 的客户端冲突。**

按 uid 聚合：115 个 uid，中位数 1，p95 = 2，max = **64**（管理台账号，64 个全
`deviceFlag=0`，管理台反复登录累积）。排除后 max = 3。本次取 `MAX_PER_UID=20`。
`bounded`/`enforce` 之后，这类反复登录的客户端会撞上 cap 并被拒绝新登录。

**④ 一条坏 payload 能把 floor 永久卡死（写验证 harness 时新发现；可达性未验证）。**

一条**有限 TTL 且在 `maxTTL` 内**的坏 payload 构成死锁：migrate 在 `natural` 下认为它是
普通 v1、TTL 合规 → `unchanged` 不动它；observe 每次报 `decode_invalid=1`；
而 `validateRecordableRolloutObservation` 拒绝任何 `decode_invalid != 0` 的观测
（`session_rollout_evidence.go:124`）。

**floor 卡死直到该 token 自然过期（最长 720h），没有任何工具能清掉它。**

测试环境侥幸躲过：那 1 条 `decode_invalid` 恰好是**永久** token，被 migration 压到 cutoff
后消失（迁移后归 0）。若它当时是有限 TTL，现在就已经卡住了。

**严重度待定——可达性未验证。** harness 里那条记录是人工注入的。生产中"有限 TTL 的坏
payload"由什么机制产生，目前不知道；若 payload 损坏只来自那条永远写永久 token 的旧路径，
本条实际不可达，优先级应下调。**实现前需要查一次这条 decode_invalid 的来源**
（见 Decisions #9）。机制未明之前，按"低概率但无自愈路径"对待。

这条对 reconciler 尤其要紧——人工流程卡住会有人去查，**自动流程卡住可能几周没人发现**。

### 成本账（零存量新部署）

一个从第一天起就没有任何 legacy token 的部署，要走完到 `enforce` 需要：
**9 次滚动重启 + 11 次 CLI 调用 + ≥2h 挂钟 + 1 次 scanned=0 的空迁移 + 1 个 canary 账号**。

其中 canary 是硬性的：`recordMigrationCompletion` 只要求 `result.Complete`
（`session_rollout_evidence.go:134`），空 keyspace 的 `--apply` 能通过；但
`validateRecordableRolloutObservation` 硬要求 `Total > 0`（`:99`），空扫描无法记证据。

整套阶段机的唯一保护对象是存量 legacy session。**没有存量，全是仪式。**

### 本仓第三次撞上同一个问题

`.octospec` 与 SQL 注释显示，"确认无旧副本"这一步在本仓已被三次推给人工：

- `internal/msgextraseq`（#627）：DB 排他锁 drain barrier，注释自述"runbook 仍要求显式
  write drain；这只是 fail-fast 兜底，**不是 drain 的替代**"（`activation.go:41-45`）；
- `pkg/botevent`（#697）：`modules/robot/sql/20260805000001_bot_event_seq_state.sql` 明写
  "本方案的 flip 前提是**运维先确认无旧副本**"，工具靠 `-yes` 确认；
- 本 rollout：runbook §6 的 kubectl 模板 + 1h×2 挂钟。

三者结构相同（纯 Redis 热路径写入，没有 commit 可持锁）。因此 writer registry 应做成
**共享能力**而非 `pkg/auth` 私有。

`octo_bot_event_seq_state` / `octo_message_extra_version_state` 还确立了
"MySQL 权威 + Redis 镜像 + 镜像只能抄近路不能推翻" 的形制。**本次不照抄**——
那个形制服务于承受不起 DB 往返的热路径分配器，session floor 没有同样的约束，
照抄会引入一个无法回滚的两阶段写。理由见 §1。

## Security invariants

以下不变量优先于本次的一切简化收益。任何一条无法保持，对应的简化项就要被砍掉而不是放宽：

1. **floor 单调不可逆。** 无论 mode 改为派生还是推进改为自动，floor 只能前进。
1. **不得在 legacy 未清零前拒绝 legacy。** `enforce` 的放行谓词只能变得**更严**，不能更松。
2. **registry 是写租约，不是状态上报。** 续不上租约的进程必须停止**新建** token；
   否则"不在 registry 里 ⟹ 没在写"不成立，门禁 fail-open。
3. **fence 只拒绝新建 token，绝不 panic、也绝不失败 readiness。**
   Redis 不可达是**全体副本同时**发生的——若 fence 失败 readiness，
   K8s 会把整个 fleet 摘出 LB，连不依赖 Redis 的路径和健康检查都没了，
   这比现状（认证降级、pod 留在 LB）更差。readiness 那一层对门禁毫无贡献：
   reconciler 看的是注册项在不在，与摘不摘流无关。
   **修掉一个总体故障模式，不能换来另一个。**
4. **空 registry 必须判失败。** 与 token 扫描相反：token 扫描证明*缺席*（空是最强证据），
   registry 证明*在场收敛*（空意味着看不见）。
5. **floor 丢失只能向上收敛，绝不向下。** 已初始化的部署丢了 floor，
   必须解析为 `enforce` 并重建，代价是相关用户重新登录；
   任何情况下不得因 floor 缺失而恢复 v2 writer 或重新接受已撤销 legacy。
   "从未初始化"与"丢失"必须由 Redis 之外的持久标记区分，不得靠推测。
6. **旧制品不注册。** registry 结构性看不见 #725 之前的副本，这一事实必须在
   `→ v3` 门禁上被显式处理，不得假装 registry 覆盖全体。
7. **自动推进只能收紧，且必须留证。** reconciler 每次推进强制写入触发它的证据快照
   （扫描计数、registry 明细、实例指纹、时刻）。**没有证据快照的推进不允许发生。**
9. **逃生门只能暂停，不能放行。** 运维可以停掉 reconciler，但不存在"跳过谓词继续推进"
   的常规入口；`advance --force` 是故障通道，需 `--yes` 且同样写审计。
10. Token、完整 Redis key、payload、generation、索引成员、UID 仍不得进入
    日志、指标 label、checkpoint、审计快照或命令输出。registry payload 只放
    build SHA / pod 名 / 已应用 floor / 启动时刻。

## Recommended design

### 1. floor 丢失向上收敛（不是 MySQL 权威 + Redis 镜像）

> **rev3 修正。** rev2 照抄了 `octo_bot_event_seq_state` 的"MySQL 权威 + Redis 镜像"，
> 但没检查它为什么需要镜像：那是因为分配器在 `saveRobotMessage` 的 msgSem 槽内，
> 每次分配承受不起一次 DB 往返，gate 必须与 INCR 同处一个 Lua。
> **session floor 没有这个约束**——mode 是进程内字段、按轮询刷新，不是每请求读一次。
> 镜像因此不必要，而它引入的两阶段写在 Redis 侧失败时**无法回滚**（floor 单调，没有撤销）。

floor **仍然只存 Redis**，沿用现有 `{uidTokenPrefix}auth:rollout-control` 与 Lua CAS，
这部分不动。`max_per_uid` 并入该记录；`observation_min_gap_ms` 不再使用，读取时容忍。

关键改变是**丢失时的反应方向**。floor 丢了不需要恢复原值，只需要取一个安全值，
而最安全的值是最大值——因为 session token 本就是可丢弃的，重新登录是可接受代价：

- Redis 整体回滚/清空 → token 也没了 → 本来就人人重登，`enforce` 不额外造成损失；
- Redis 回滚到 floor 建立之前但保留了老 token → 复活的永久 legacy 正是最不该接受的，
  拒绝它们让那批人重登，方向正确。

唯一会炸的是**首次采纳**：一个有大量存量 legacy 的老部署第一次上新制品，floor 从未建立，
此时"缺失 → enforce"就是全员立即登出。所以 `floor 缺失` 这一个状态对应两个相反语义，
必须靠一个 **Redis 之外、写一次不再改**的比特区分。这是 MySQL 唯一的职责：

```sql
-- 单行，write-once，无后续 UPDATE
octo_session_rollout_marker(singleton_id, initialized_at)
```

| 标记 | floor | 结论 | 动作 |
|---|---|---|---|
| 缺失 | 缺失 | 首次采纳 | 从 `expand` 开始；keyspace 空则按 §3 直达 `enforce` |
| **缺失** | **存在** | **从 #725 升级** | **采纳现有 floor，补写标记，不改变任何行为** |
| 存在 | 缺失 | Redis 回滚 | 取 `enforce`，重建 floor，大声告警 |
| 存在 | 存在 | 正常 | 按记录 |

第二行是**现实中最常见的一格**——测试环境此刻就在这里（floor=`revoke`，标记表尚不存在），
任何已经开始跑 #725 的部署也都在这里。它必须是纯粹的无声接管。

标记不参与一致性维护，也不需要与 floor 同步——两者语义不同，各写各的，
所以不存在 rev2 那个两阶段写问题。

**不引入新的启动依赖**：`main.go:437`（`RewriteLegacyMigrationIDs`）与 `main.go:456`
（`module.Setup` → sql-migrate）都在 `cn.Start()` 之前，**MySQL 本来就是启动硬依赖**，
多读一行标记不改变任何故障模式。这与"Redis 丢一个 key 就 panic"不同——后者是白送的。

**为什么向上取严格正确**（三种回滚场景，无一例外）：Redis 全清时 token 也没了，
enforce 零额外代价；回滚到 floor 建立前时，复活的永久 legacy 正是漏洞本身，
拒绝它们是对的；回滚到某个中间 floor 时，"精确恢复"意味着继续接受一个来自
不一致快照的 legacy 集合。**没有一种情况下精确恢复更好**，因此这里不存在权衡。

**关于原子性**：rev2 曾主张"floor 推进 + 证据快照必须同一事务"。**该主张已收回。**
正确要求是**顺序**而非原子性——先写快照，再 CAS floor。快照写失败则不推进；
CAS 失败留一个孤儿快照（无害）。Redis 两次写即可满足 invariant 8。

### 2. writer registry：写租约

```
{uidTokenPrefix}auth:writers            SET，成员是 writer id（花名册）
{uidTokenPrefix}auth:writer:{id}        STRING + PEXPIRE，值 {build, applied_floor, pod, started_at}
```

- **身份**：进程随机 ID（复用 `newSessionGeneration()`），**不用 Pod UID**。私有化不一定跑
  K8s；且容器原地重启 pod UID 不变会静默覆盖旧条目，掩盖重启。pod 名经 Downward API
  有则读、无则空，仅作 payload 标签。
- **时钟**：存活判定交给 Redis 自己的 key TTL，脚本内**不调用 `TIME`**、客户端**不传
  `nowMS`**（现有 `migrateLegacyToken` 的传时约定在这里会引入跨 pod 时钟漂移）。
- **枚举**：`SMEMBERS` + `MGET`，两条命令，不用 SCAN（runbook §3.1 对 proxy 的 cursor
  语义有明确担忧）。MGET 返回 nil 的成员顺手 `SREM`。
- **数值**：续租间隔 5s，租约 TTL 30s；gate 端额外要求连续观察 ≥1 个 TTL。
- **fence**：续租失败超过租约 TTL（30s）→ **拒绝新建 token**，已有 session 的校验读继续服务。
  **不失败 readiness**（invariant 4）。代价几乎为零：续不上租约意味着这个进程连不上 Redis，
  它本来也写不了 token；fence 只是把"我对门禁隐身却还在写"变成一个明确拒绝。
- **存哪**：registry **纯 Redis**，与它 gate 的对象（token 写入）同生共死。放 MySQL 会让
  一次 MySQL 抖动 fence 掉本可正常写 token 的进程——凭空造一个新失败模式。
  这与 §1 把 floor 放 MySQL **不矛盾**：两者的要求不同（floor 要扛 RDB 回滚，
  registry 要与被管辖操作对齐失效域）。
- **成本**：50 副本 × 每 5s 一次 ≈ 10 ops/s，复用现有 session pool，**新增连接 0**，
  不影响 runbook §3.2 的连接预算。

### 3. gate 谓词

```
live := SMEMBERS + MGET，过滤 nil
assert len(live) > 0                        -- 空 registry 是失败
assert 所有 entry.build 相同                 -- 自动抓混部
assert 所有 entry.applied_floor >= current   -- 全员已收敛
assert 指纹匹配 floor 记录中的实例身份
若目标是 v3 且 keyspace 非空：
    assert len(live) == EXPECT_WRITERS       -- 见下
若目标是 enforce：
    assert v1 == 0 且 v2 == 0
```

**`EXPECT_WRITERS` 解决 §Security invariants 第 7 条**：只有新制品会注册，
所以旧副本还在 → 注册数 < 期望数 → 拒绝。它**不是决策，是信息**——部署系统
（Helm values / Deployment `spec.replicas`）本来就知道这个数，只是 app 看不见。
因此它是部署侧配置 `OCTO_AUTH_SESSION_EXPECT_WRITERS`，不是 CLI flag，也不是人的记忆。

两点性质：

- **它是过渡期配置，不是常驻配置。** 只在 `→ v3` 这一跳被读；推过之后可以删除。
  HPA 环境在这个窗口内临时固定副本数即可。
- **多了少了都 fail-closed**（拒绝推进），配错不危险，只是卡住。

**keyspace 为空时跳过这一条**：没有任何 token 存在，意味着没有任何 writer 写过东西，
旧制品的威胁不成立。这是"greenfield 从正确谓词自然推出"的同一条原理，不是特例分支。

**残余风险（接受）**：若旧制品写的是永久 token，没有任何数据面探测器能发现它——
TTL 水位线一类办法对永久 token 失效。因此 `→ v3` 保留 `EXPECT_WRITERS` 是必要的；
能做的是让它可被工具证伪（count + build 交叉检查），而不是自我认证。
**这一跳之后 registry 完全可信**，之后所有跃迁全自动推导。

### 3.5 从 #725 升级的兼容规则

测试环境与所有已启动 #725 rollout 的部署，升级时必须**行为完全不变**。三条硬规则：

**① 采纳而非重建。** 见 §1 判定表第二行：读到现有 Redis floor 就采纳它，补写标记，
不推进、不重置、不要求删除或手改任何 Redis key。

**② `derived_mode = max(floor, 遗留 env MODE)`，不是 `= floor`。**
#725 的 Phase D/E 要求"先部署高一阶的 `MODE`、灰度确认，再推进 floor"，
所以处在那一步的部署是 `MODE=bounded` + `floor=revoke`。若直接取 floor，
reader 会从 `bounded` 掉回 `revoke`，**重新接受永久/超上限 legacy——升级即安全回退**。
兼容期内取二者较大值；`env MODE = floor+1` 与 `CANARY_AHEAD=1` 语义等价，按后者对待。
遗留 env 高于 floor 时打 warn，提示运维它正处在灰度姿态。

**③ `MAX_PER_UID` 一次性带入。** 现有 `SessionRolloutControl` 无此字段，采纳时从 env 抄一次。
`floor ≥ v3-write` 的部署必定配了它（`sessionPolicyFromEnv` 在 `writesV3()` 时强制要求），
所以这次抄取不会落空；抄完 env 即可删除。

**格式向后兼容**：给 Redis floor 记录**增加**字段是安全的——#725 的 `loadRolloutControl`
用 `json.Unmarshal` 填入结构体，未知字段被忽略。因此回滚窗口内 #725 制品仍能读懂
（配合 Rollback 一节要求把两个 env 加回去）。**本 release 不得删除或改名任何既有字段。**

### 4. mode 由 floor 派生

副本轮询 floor 并自抬 mode。`OCTO_AUTH_SESSION_MODE` 与
`OCTO_AUTH_SESSION_REQUIRED_FLOOR` 移除。

- `runtime.go:90-93` 的 `ValidateRolloutControl` + panic 删除。规则
  `mode ≤ floor+1`（`session_rollout.go:69`）的目的由 gate 在推进时**正面校验收敛**取代——
  更早、更准、且不杀进程。**同时消除"先推 floor 后改 env → 期间任何重启 panic"这个
  顺序陷阱**：新设计里 floor 和 mode 是同一个东西，没有顺序可以搞错。
- 保留唯一 env 开关 `OCTO_AUTH_SESSION_CANARY_AHEAD=1`：只允许比 floor 高一阶，
  打在小流量副本上。canary 上报 `applied_floor = floor+1`，gate 用 `>=` 判断天然通过。

**代价与取舍**：旧设计里 `advance-floor` 只改一条记录，行为要等下次部署才变，
这给了运维一个"推完再环顾"的缓冲。新设计里推进 ≤5s 内全局生效，缓冲没了。
这个交换是划算的：那个缓冲是假的安全感（floor 推进本来就不可逆，缓冲期能做的唯一
"回退"恰恰是制造 `mode < floor` 陷阱的动作）。真正的灰度手段是 `CANARY_AHEAD`——
先在小流量副本跑高一阶，确认后再推 floor，顺序正好相反。

### 5. reconciler：floor 由系统自己推

```
每 30s（owner lease，复用 revocation worker 的形制）：
  1. 读权威 floor；镜像不一致则重建镜像
  2. 廉价前置：registry 收敛？单一 build？实例指纹匹配？无进行中的 migration lock？
     —— 任一不满足则记录 blocked_by 并退出本轮，不扫描
  3. 目标是 v3 且 keyspace 非空 → 校验 EXPECT_WRITERS
  4. 目标是 enforce → 执行一次限速完整扫描（复用 ObserveRateLimited）
  5. 谓词成立 → CAS 推进 + 写审计快照；不成立 → 记录 blocked_by 并退避
```

- **扫描的退避是关键**：`blocked_by: v2>0` 意味着在等自然过期或人工决策，
  这种状态下每 30s 重扫毫无意义。退避到 ≥1h，并在 migration campaign 完成事件后
  立即重试一次。prod 上百万 key 的扫描成本必须由这条控制。
- **不需要选主做正确性**：推进是 CAS + 幂等，多副本同时尝试也只有一个成功。
  lease 只是为了避免多副本重复扫描，是**成本优化不是安全机制**。
  注意输的一方有**两种**失败形态（实测，见 `TestRolloutControlAdvanceIsSingleWinnerUnderConcurrency`）：
  读在赢家写之前撞 CAS（`ErrRolloutControlChanged`），读在之后撞乐观预检
  （`must advance exactly one phase`）。**两种都必须当良性 no-op**，
  不得计入错误率或触发告警——否则多副本 reconciler 会持续自己给自己报警。
- **审计快照**（invariant 8）随 floor 表一起持久化，回答"floor 为什么动了"。
  人工敲命令反而不留证据——2026-08-11 那次连错 Redis 就没有任何记录能事后发现。
- **开关与生效延迟**：`pause` 写一个持久标志，reconciler **每轮开头读它**，
  秒级生效——事故中停一个乱推进的 reconciler 不能要求一次 rollout，
  那既太慢也与"减少重启"的整体目标自相矛盾。
  `OCTO_AUTH_SESSION_AUTO_ADVANCE` 只作为**部署期初始值**，运行期以 `pause` 标志为准；
  两者冲突时**以更保守的一方为准**（任一为停即停）。
- **测试隔离**：reconciler **只从 `main.go` 接线启动，绝不从 `NewRedisSessionStore`**。
  否则全仓每个建 store 的测试都会起一个后台推 floor 的 goroutine，
  制造大批难以定位的串扰失败。这是一行约束，省掉后面一堆调试。
- **首次引入自身的 bootstrap 危险（必须写死）**：滚动升级到"有 registry 的版本"时，
  新 pod 会回填标记、注册 registry 并可能启动 reconciler，而存活的旧 pod（#725 制品）
  读 Redis floor 且 `mode < floor` 就 panic。**registry 正是用来防这个的，但升级到
  有 registry 的版本这一次，registry 还不存在**——它只看得见新 pod。
  规则：**首个引入 reconciler 的 release，reconciler 默认关闭**；
  或检测到"标记由本次启动新建"时强制静默一个完整的 `EXPECT_WRITERS` 确认周期后再工作。

**为什么自动推进一个不可逆开关是安全的**：谓词全部 fail-closed（#725 已有性质，
非新增假设）；推进只让 reader 更严，而谓词证明了被拒绝的是空集；能创造 legacy 的
writer 已被 floor + registry 排除，扫描到翻转之间不可能冒出新 legacy；最坏情况是
一批人被登出（可用性），不是有人拿到不该有的权限（安全）。

**一个自然落位的性质**：brownfield 会自动推到 `v3` 然后**卡在 enforce**（`v2>0`）。
而消除 legacy 只有两条路：等自然过期，或跑一次 migration——**migration 的参数就是那个
业务决策**。所以不需要单独设计审批机制，系统会自己走到"只差一个业务决策"的位置停下。

### 6. 证据谓词与 `bounded` 降级

- `rolloutObservationScopeFingerprint()` 纳入 `currentRedisInstanceID()`，
  修复 Background ① 的静默错配。所有子命令与 reconciler 启动时打印实际连接的 endpoint
  与实例指纹。
- 放行谓词从 `Total > 0` 改为 `v1 = 0 ∧ v2 = 0 ∧ registry 收敛`。

  **rev4 去掉了"指纹匹配"这一项，改为只把实例身份加进指纹本身。**
  原来的设想是让谓词校验"扫的 keyspace 属于持有 floor 的那个 Redis"，但 reconciler
  与 CLI 读 floor 和扫 keyspace 用的是**同一个 client**，结构上不可能不一致——这一项
  在实现里没有可校验的对象。真正会连错实例的是 `migrate --apply`（2026-08-11 那次），
  而那是另一条路径，由子命令启动时打印 endpoint + 实例指纹来覆盖。
  指纹本身仍然必须纳入实例身份：不纳入的话，同一份配置指向两个不同 Redis 会得到
  逐字节相同的指纹，那是真实缺陷（`TestWiringScopeFingerprintSeparatesRedisInstances`）。
  **空扫描成为最强证据而非被拒绝**，greenfield 与迁移完成后的 brownfield 落在同一谓词上。
- 取消 `bounded` / `enforce` 各自的 2 次观察、rollout observation evidence 整套 key
  与 `MinimumRolloutObservationGap`。
  `bounded` 的拒绝集（`TTL≤0 || TTL>maxTTL`，`session_v3.go:240`）是 `enforce` 拒绝集的
  **真子集**，且迁移完成后按构造为空；registry 闭合了"straggler v2 writer 造成 SCAN 漏扫"
  的担忧后，`bounded` 从强制 floor 降为**可选灰度姿态**（`CANARY_AHEAD` 可用）。
- 统一 observe 与 migrate 的版本口径（Background ②）：migrate 的 Lua 必须把非
  `v1:`/`v2:`/`v3:` 前缀的 payload 单独计为 `invalid` 并**跳过**，不再默认当 v1 处理；
  `LegacyMigrationResult` 单独报告。reconciler 用同一口径判断 `v1=0 ∧ v2=0`。
- **坏 payload 必须有清理路径**（Background ④）。一条无法 `Decode()` 的记录既不是有效
  credential（validator 本来就拒绝它），也不该无限期阻塞 floor。设计取向：
  migrate 识别出 `invalid` 后**按 campaign cutoff 收敛它**（与永久 legacy 同等待遇），
  因为它对任何人都不可用，删除不造成登出。`status.blocked_by` 必须能显式报出
  `decode_invalid=N`，且 reconciler 在这个原因下要单独告警——它不会自愈。

### 7. CLI：只剩三件事

并入 `/home/app`（Dockerfile 无需改动，仍只 build `./main.go`）：

| 子命令 | 用途 |
|---|---|
| `session-rollout status` | **排查**：floor（权威/镜像是否一致）、registry 明细、tokens 计数、`blocked_by`、最近一次审计快照 |
| `session-rollout migrate` | **业务决策输入**：cutoff + finite-policy。dry-run 默认，`--apply` 需批准。参数与机制原样保留 |
| `session-rollout pause` / `resume` | **逃生门**：暂停 reconciler |
| `session-rollout advance --force --yes` | 故障通道：reconciler 本身故障时人工推进。写审计，不跳过谓词 |

`status.blocked_by` 必须可执行，例如：

```json
"next": {"target": "enforce", "blocked_by": ["v2=135 (need 0)"],
         "options": ["wait: max 720h, decaying via ReuseSession promotion",
                     "migrate --finite-policy cap --cutoff <T>"]}
```

**注意逃生门的方向：只能暂停，不能放行。** 手动越过谓词就是把 fail-closed 拆掉，
那是这套东西存在的全部意义。卡住时正确动作是查为什么卡住，不是绕过去。

### 8. 退场

`checksLegacyDeny()` 在 `enforce` 时返回 false（`session_policy.go:102`）——代码里已有
"迁移结束"的语义锚点。到 enforce 后 deny marker 不再被读、4 个 rank 全是死代码。
本 brief **不执行**退场，但要求新代码不阻碍它：floor 表设计允许后续降级为布尔，
reconciler 在 floor=enforce 后进入静默（不再扫描）。

## Rollback

> rev2 **漏了这一节**，而本次把不可逆动作自动化了——回滚边界只会更需要写清楚。

**可回滚制品的下界。** floor 一旦越过某点，能启动的制品就被限死：

| 当前 floor | 可回滚到 |
|---|---|
| 未建立 | #723 或 #725 制品 |
| `v3-write` ~ `revoke` | 仅 #725 制品或更新（需能解析 v3 + generation） |
| `bounded` ~ `enforce` | 仅遵守该 floor 的兼容制品；**不能回到本次之前** |

新增约束：本次移除了 `OCTO_AUTH_SESSION_MODE` / `REQUIRED_FLOOR`，而 #725 制品**需要**它们。
因此 **Redis floor 记录必须在一个完整 release 内保持 #725 可读的格式**，
且回滚到 #725 时运维必须把那两个 env 重新加回去——这一步写进 runbook 的回滚清单。
下一个 release 才可以改格式。

**reconciler 异常时的三级处置**（按响应速度排序）：

1. `session-rollout pause` —— 秒级，写持久标志，所有副本下一轮生效。**首选。**
2. `AUTO_ADVANCE=off` + 滚动 —— 分钟级，用于确认要长期关闭。
3. 回滚制品 —— 仅当 reconciler 之外也出问题；受上表的下界约束。

**已自动推进的 floor 不能撤。** 这是设计意图（单调），不是缺陷。
它意味着"谓词有 bug"这一风险没有事后补救，只能靠事前：
谓词的每条分支都要有独立测试，且首个 release 默认关闭 reconciler（见 §5）。
**这是本次最需要 review 注意力的地方。**

**migration 的回滚语义不变**：可暂停续跑；不得删 campaign/checkpoint、
不得延长已缩短的 TTL、不得删 deny marker、不得换 campaign 复活已过期 token。

## 交付形态：一个 PR

> rev1 有依赖表，rev2 误删，rev3 拆成 10 个，rev3.3 归并为 3 个——**都不对。**
> 前面几版都在优化"什么能安全地先合入"，但**交付物就是那个行为变更**。
> 一个只打印 endpoint、加个计数器的 PR 什么流程都没替换掉，正是本 brief 在批的那种仪式。
> **新 PR 的定义是"替换掉繁琐流程"，不是"零风险地往前挪一格"。**

一个 PR，替换整套流程：

| 组成 | 替换掉了什么 |
|---|---|
| 标记表 + floor 丢失向上收敛（§1） | Redis 丢 key → 全站 panic |
| writer registry 写租约（§2、§3） | runbook §6 的 kubectl 人工核对 |
| mode 由 floor 派生，删两个 env（§4） | 9 次滚动重启 → 1 次；`mode < floor` 的顺序陷阱 |
| 谓词改 `v1=0 ∧ v2=0`（§6） | 2×1h 观察仪式；greenfield 的 canary + 空迁移死路 |
| §3.5 升级兼容 | 在途部署（含测试环境）无声接管 |
| reconciler，**本 release 默认关闭**（§5） | 剩余的 CLI 调用 |
| `app session-rollout` 子命令 + `status`（§7） | 私有化必然漏带工具 |
| **runbook 重写，删除旧的 9 步阶梯** | 两套互相矛盾的步骤 |
| 删除 `tools/token-session-admin`、`tools/token-session-observe` | 已核实无任何 CI/脚本引用 |

约 1600 行，其中 runbook 与测试占大头。#725 本身远大于此。

**reconciler 默认关闭不构成第二个 PR**：那是一个默认值，各环境按自己节奏用部署侧配置开启，
代码默认值等全网稳定后随手翻。§5 的 bootstrap 规则要求的是"上线时关闭"，不是"分两次上线"。

**没有阻塞项。** Decisions 1–7 均已有结论或默认值；#8（C6 可达性）也不再 gating——
见下。

### C6 被重设计自动消解

那个死锁的成因是 `validateRecordableRolloutObservation` 拒绝 `decode_invalid != 0`
（`session_rollout_evidence.go:124`）。本次删除整套 observation evidence 机器后，
谓词是 `v1 = 0 ∧ v2 = 0`，而坏 payload 在统一口径下计 `invalid`——**既不是 v1 也不是 v2，
不再阻塞推进**。这是正确的：`Decode()` 失败的记录本来就被 validator 拒绝，从来不是可用凭证。

死锁只存在于"证据校验比安全要求更严"这道缝隙里。缝隙没了，死锁也没了，
不需要为它单独设计清理路径。Decision #8 从阻塞项降为卫生问题（要不要顺手清掉坏记录），
可留待后续。


## Load-bearing list

- **`auth`** — `pkg/auth` rollout floor 读写路径（`session_rollout.go`、
  `session_rollout_evidence.go`、`session_policy.go`、`runtime.go`）；mode 的 5 个行为判定点
  （`writesV3` / `RevokesSessions` / `== bounded` / `== enforce` / `checksLegacyDeny`）
  语义必须逐一保持。
- **`auth`** — 启动路径 `main.go:188` 的 `SessionStoreAndClientForContext`：
  当前 panic 语义改为自愈，不得让任何一条 fail-closed 变成 fail-open。
- **`auth`** — 认证热路径命令数：v3 稳态仍必须是 Token 单 key Lua + generation 单 key read、
  零写入。registry 心跳与 reconciler 都是**后台 goroutine**，不得进入请求路径。
- **`auth`** — floor 推进从"人工、稀疏、可审阅"变为"自动、周期、可能在无人值守时发生"。
  这是本次最大的行为变更，审计快照与 `blocked_by` 是它的可观测性契约。
- Redis 单 key 原子性与 proxy/Cluster 兼容：registry 的所有脚本单 key；
  枚举不引入新的 SCAN 依赖；reconciler 的扫描复用现有 `ObserveRateLimited` 限速路径。
- Redis 连接预算：registry 与 reconciler 复用唯一 session pool，
  `OCTO_AUTH_SESSION_REDIS_POOL_SIZE` 语义与 runbook §3.2 的 `maxclients` 核算不变。
- Redis 命令率：reconciler 的周期扫描是**新增负载**，必须有退避且计入容量模型。
- MySQL：新增一行 write-once 标记表与 `pkg/db/migrate_compat.go` 迁移路径。
  **不复用** `octo_bot_event_seq_state` 的"权威 + 镜像"形制——那个形制服务于热路径分配器，
  session floor 没有同样的约束（§1）。
- migration 正确性机制**原样保留**：immutable cutoff、单占租约、checkpoint 绑 Redis
  `run_id` 指纹、TOCTOU 单 key Lua、只缩短不延长、`--confirm-elapsed-cutoff`。
- 已建立的测试环境 floor（`revoke`）与 campaign `test-2026-08-11-a`：
  新代码必须能读取**已存在的** Redis-only control 记录并回填到 MySQL，不得要求手工删 key；
  `MAX_PER_UID` 需从 env 一次性带入 control record（现有 `SessionRolloutControl` 无此字段）。
- 撤销矩阵、generation、有界索引、durable intent、WuKongIM device quit：全部不变。
- **`wire-contract`** — 客户端 opaque token 协议、登录响应、401 envelope 不变。
- **`testing`** — `testutil.NewTestServer()` 之间 registry 条目会残留（同 CLAUDE.md 记载的
  `ratelimit:uid:*` 问题，`CleanAllTables` 不清 Redis）；reconciler 在测试中默认必须关闭，
  否则会在测试间自行推进 floor。两者都需在 setup 处理。
- **`commit`** — Conventional Commits，英文。
- 运维文档：`docs/token-session-rollout-runbook.md` 必须与新流程同步重写，
  不能留下两套互相矛盾的步骤。

## Out of scope

- **不改 v3 payload、generation、issuance fence、有界索引、撤销矩阵。**
  本 brief 只动 rollout 控制面与运维面。
- **不改 migration 的任何正确性机制**（见 load-bearing）。要砍的是仪式，不是安全机制。
- **不自动决定 migration 参数。** cutoff 与 finite-policy 是业务决策，reconciler 永远
  不会替人选，只会停下并说明差什么。
- 不删除 `bounded` 阶段本身——降级为可选灰度姿态，保留其 reader 行为。
- 不执行阶段机退场（§8），只要求不阻碍。
- 不清理 legacy deny marker；不设计其自动过期。
- 不动 prod。本 brief 交付代码能力，生产推进仍是独立的 go/no-go。
- 不解决 `MAX_PER_UID` 与反复登录客户端的冲突（Background ③）——这是管理台的
  session 复用问题，单独立项，但它是 prod 推进 `enforce` 的前置。
- 不把 registry 接入 `#627` / `#697`。本次做成可共享的形状，实际接线由那两个 owner 决定。
- 不从 K8s API 读取副本数。`EXPECT_WRITERS` 走部署侧配置，保持与编排系统解耦。
- 不引入短 Access Token + Refresh Token、滑动空闲超时（沿用 #725 的 out of scope）。

## Acceptance

**floor 丢失向上收敛**

- 三态判定表（§1）逐格有独立测试：
  `标记缺失+floor缺失 → expand`、`标记存在+floor缺失 → enforce+告警`、`都在 → 按记录`。
- **从 #725 升级不得登出任何人、不得改变任何行为**：以
  `fixtures/test-env-2026-08-11.json` 为准搭出"有存量 legacy、floor 已是 revoke、
  env 带 MODE/MAX_PER_UID"的环境，新制品启动后：沿用 `revoke`、标记被补写、
  `max_per_uid=20` 被带入、**不要求删除或修改任何 Redis key**、已有 session 全部继续有效。
- **`MODE` 高于 floor 时不得放松**（§3.5 ②）：`MODE=bounded` + `floor=revoke` 的环境
  升级后 reader 仍拒绝永久/超上限 legacy。构造一条 persistent legacy 断言它仍被拒——
  取 `= floor` 的朴素实现会让它重新通过，这是该规则的负向测试。
- **Redis floor 记录格式向后兼容**：加字段后用 #725 的 `SessionRolloutControl`
  反序列化仍成功且语义不变。
- 删除 Redis floor key 后重启：进程**正常启动**并收敛到 `enforce`（不是 panic、不是 expand）；
  当前实现在同条件下 panic（`runtime.go:92`）——`TestTripwireMissingControlRefusesStartup` 翻红即证据。
- 标记表是 write-once：任何后续 UPDATE 被拒（表约束或应用层双保险）。
- floor 单调性不变，由现有 Redis CAS 保证——`session_rollout_invariants_test.go` 全部保持绿。

**writer registry**

- 租约到期后进程停止新建 token，**不 panic 且 readiness 仍然通过**；已有 session 的
  `Validate` 仍成功。故障注入覆盖：Redis 短暂不可达（<30s，不 fence）、
  持续不可达（>30s，fence）、租约 key 被外部删除（fence）。
- **全体 fence 不得造成总体不可用**：模拟所有副本同时失去 Redis，
  断言 readiness 仍为通过、非认证路径仍可服务——这是 invariant 4 的负向测试。
- 推进在下列每种情况下**拒绝**：registry 为空；存在两个不同 build；
  任一 entry 的 `applied_floor < current`；`EXPECT_WRITERS` 不匹配（keyspace 非空时）。
  每种拒绝有独立测试与明确 `blocked_by`。
- keyspace 为空时**跳过** `EXPECT_WRITERS` 校验，且该跳过有独立测试。
- 两个独立 store 模拟两副本：一个滞后未应用新 floor 时推进被拒；收敛后通过。
- 心跳不进入请求路径：命令计数测试证明 v3 稳态认证仍为 2 次 Redis 读、0 写。
- `SMEMBERS`+`MGET` 枚举不产生新的 SCAN 调用（源码守卫或命令计数断言）。

**mode 派生**

- 移除 `OCTO_AUTH_SESSION_MODE` / `..._REQUIRED_FLOOR` 后，副本在 floor 推进后
  **无需重启**即收敛到新 mode，收敛延迟有上界断言（≤ 一个轮询周期）。
- `CANARY_AHEAD=1` 的副本 mode = floor+1，且不能高于 floor+1。

**reconciler（本次核心）**

- **greenfield 全程零命令**：空 keyspace 部署后，floor 在有界时间内自动到达 `enforce`，
  期间无任何 CLI 调用、无 canary 账号、无空迁移。
- **brownfield 自动停位准确**：有 legacy 的环境自动推到 `v3` 后停止，
  `status.blocked_by` 精确报告 `v2=N (need 0)` 并给出两个可执行选项。
- 每次自动推进都写审计快照（扫描计数、registry 明细、实例指纹、时刻）；
  **构造一次"无快照的推进"必须失败**（invariant 8 的负向测试）。
- 退避生效：`blocked_by: v2>0` 时不得每周期重扫；命令计数断言扫描频率 ≤ 配置下界。
  migration campaign 完成后立即重试一次。
- 多副本并发 reconcile 只产生一次推进（CAS 幂等），其余为 no-op 且不重复扫描。
- `pause` 后不再推进；`resume` 后恢复。`AUTO_ADVANCE=off` 时完全不推进。
- `advance --force` 仍执行全部谓词校验（force 只绕过 reconciler，不绕过谓词），
  且写审计。
- floor 到达 `enforce` 后 reconciler 静默，不再产生周期扫描。

**证据谓词**

- `rolloutObservationScopeFingerprint()` 纳入实例身份后，连接两个不同 Redis
  产生**不同**指纹；以 Background ① 的场景（同配置、不同 endpoint）作为回归用例。
  当前实现在该场景下指纹相同——该断言即回归证据。
- 所有子命令与 reconciler 启动时打印实际 endpoint 与实例指纹。
- 非空 keyspace 在 `v1>0` 或 `v2>0` 时**拒绝** `enforce`。
- 迁移完成后的 brownfield 与 greenfield 走**同一段代码路径**（无 `if greenfield` 分支）。
- migrate 的 Lua 对非 `v1:`/`v2:`/`v3:` 前缀 payload 计入 `invalid` 并跳过，不再当 v1 处理；
  同一批 fixture 下 observe 的 `decode_invalid` 与 migrate 的 `invalid` **计数相等**
  （当前为 53 vs 54，见 `verification.md` C3）。
- **坏 payload 不再卡死 floor**（`verification.md` C6 的取反）：一条有限 TTL、在 `maxTTL`
  内的坏 payload 经一次 migration 后被收敛，floor 可继续推进。当前实现在同场景下
  `unchanged=1` 且证据被永久拒绝——该断言即回归证据。
- 卡在坏 payload 上时 `status.blocked_by` 报出 `decode_invalid=N` 而非泛化错误，
  且 reconciler 就此原因单独告警。

**回滚与拆分**

- `## Rollback` 的三级处置逐条可演练：`pause` 秒级生效（断言下一轮不推进）；
  `AUTO_ADVANCE=off` 生效；两者冲突时取更保守一方。
- 首个引入 reconciler 的 release，reconciler 默认关闭（配置断言 + 启动日志）。
- 混跑测试：新制品与 #725 制品同时在线时，新制品**不推进 floor**
  （模拟 registry 只看得见自己的 bootstrap 场景）。
- Redis floor 记录在本 release 内保持 #725 可读格式（用 #725 的解析逻辑做一次反序列化断言）。

**验证基线**

- 本 brief 对现状的 6 条断言均已在 `d68d0ad` 上实测，结果见 `verification.md`。
- 三组测试分工明确，PR 必须逐条说明结果：
  - `pkg/auth/session_rollout_invariants_test.go` —— **改完必须仍绿**（单调、逐阶、
    首个 floor、CAS 单赢、无 TTL、损坏 fail closed、apply 的 revoke 门禁）。
  - `pkg/auth/session_rollout_boot_test.go` —— 缺陷被移除后的行为，即原 tripwire 的
    取反形式。原 tripwire 文件引用了本次删除的 `ValidateRolloutControl`，保留它会让
    「翻红」变成编译失败并拖垮整包；原始测量结果保留在 `verification.md`。
  - `pkg/auth/session_rollout_wiring_test.go` —— **启动接线路径**（rev4 新增）。
    review 的 10 条 blocking 里有 9 条活在这一层，而当时所有测试都停在纯函数边界。

**不回归**

- `pkg/auth` 现有全部测试通过，特别是 `session_v3_test.go`、`session_migration_test.go`、
  `parser_test.go` 与 `modules/user/token_writer_guard_test.go`。
- migration 的 immutable cutoff / 单占租约 / 实例指纹 / `--confirm-elapsed-cutoff`
  相关测试逐条保持。
- 撤销矩阵集成测试不变。

**门禁命令**

- `go test -race ./pkg/auth/... -count=1`、`go test ./pkg/metrics/... ./tools/... -count=1`、
  `go build ./...`、`go vet ./...`、`golangci-lint run ./...`、
  `make i18n-extract-check`、`make i18n-lint`。
- `go test ./...` 若因 `internal/msgextraseq` 占用共享 `test` 库超时而未全绿，
  PR 必须显式列出未验证项，不得宣称全绿（沿用 #725 的既有约定）。

**可观测性**

- 新增低基数指标：`live_writers`、`writer_fence_total`、`floor_advance_total{from,to}`、
  `reconcile_blocked{reason}`、`reconcile_scan_total`。
- 告警至少覆盖：writer fence 持续发生、reconciler 连续 N 轮 blocked 于非预期原因、
  镜像与权威不一致、`advance --force` 被使用。

**文档**

- `docs/token-session-rollout-runbook.md` 重写为新流程；旧的 9 步阶梯、
  `REQUIRED_FLOOR` 配对、2×1h 观察窗全部移除，不留互相矛盾的两套步骤。
  新 runbook 的主体应是"如何读 `status`、如何做迁移决策、卡住了怎么查"，
  而不是"按顺序敲哪些命令"。

## Decisions required

以下需要在实现前拍板，代码不能自行猜测：

1. **`AUTO_ADVANCE` 的默认值。** 本 brief 默认 `on`（谓词安全性论证见 §5）。
   若 prod 治理要求人工放行不可逆变更，则默认应为 `off`，greenfield 的零命令收益随之减半。
2. **HPA / 副本数浮动环境下的 `EXPECT_WRITERS`。** 本 brief 的立场是"它是过渡期配置"——
   `→ v3` 窗口内临时固定副本数，推过后删除。若这不可接受，备选是改为
   "单一 build ID + 连续收敛 D 时长"，但那会重新打开旧制品的洞。
3. **旧制品写永久 token 的残余风险**（§3）：接受 `EXPECT_WRITERS` + 工具证伪，
   还是另设机制。本 brief 默认接受。
4. **是否保留 `bounded` 作为可选灰度姿态**，还是直接从 `revoke` 跳 `enforce`。
   本 brief 默认保留（reader 行为不删，只是不再是强制 floor）。
5. **测试环境已建立的 `revoke` floor 如何过渡**：写入标记后由 reconciler 接管，
   还是等新代码上线后重跑一遍。本 brief 默认接管，并以此作为升级兼容性的验收 fixture。
6. **prod 的 `MAX_PER_UID`**。测试环境 p95=2、排除 管理台账号 后 max=3，取 20；
   prod 需独立测量后签字，且要先解决 Background ③ 的管理台反复登录问题。
7. **Background ④ 的可达性**：需要查清测试环境那条 `decode_invalid` 的来源机制。
   若有限 TTL 的坏 payload 实际不可产生，该项优先级下调、清理路径可以简化。
   **实现前应完成这次调查**，否则会为一个可能不存在的场景设计清理逻辑。

### 已关闭的决策项（记录理由，避免重开）

- **~~registry 做成共享 `pkg/writerregistry` 还是先落 `pkg/auth`~~** —— 先落 `pkg/auth`。
  把已验证的实现提取出去，比预先为两个没细看的用例（#627 / #697）设计通用 API 容易得多。
  两条约束让以后 `git mv` 即可搬走：① registry 的 API 中不出现 auth 类型——
  `applied_state` 是不透明 string，比较函数由调用方传入，不是 `SessionMode`；
  ② key 前缀由调用方传入，不从 auth config 推导。

- **~~floor 丢失取 `enforce` 是否需要产品签字~~** —— 不是决策题。对照组是"全站起不来"，
  那是缺陷不是选项；且三种回滚场景下"向上取"都**严格优于**"精确恢复"（§1），
  后者会重新接受来自不一致快照的 legacy token。无需权衡。
- **~~fence 的下限~~** —— 由 invariant 4 定死：只拒绝新建 token，不失败 readiness。
  "整体 readiness 失败"这个备选被否决，因为 Redis 不可达是全体副本同时发生，
  它会把一个认证降级放大成 fleet 级摘流——**比现状更差**。
