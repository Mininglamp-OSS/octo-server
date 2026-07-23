# Implementation plan — message-extra-transactional-version

> 配套 `brief.md`(spec)。本文件只讲**落地步骤 / 文件清单 / 顺序**,不含代码。
> 事实基线来自对现有写入方事务边界的源码勘察(见文末「勘察结论」)。

## 进度(2026-07-21)

- **PR-1 ✅ 完成**(未提交):迁移 `20260721000001_message_extra_version.sql` + 叶子包
  `internal/msgextraseq`(store/metrics)+ 15 个集成测试(全绿、-race 干净、覆盖 78%)。
  迁移经真实 sql-migrate 全量流程验证可 apply,建出的 `channel_id` collation =
  utf8mb4_general_ci(与 message_extra 一致)。API 采纳 `[]int64`(brief D3 已同步)。
- **PR-2a ✅ 代码+测试完成**(未提交):message 模块 6 处写入方全部经 `Store.ReserveTx`
  收口;两个 autocommit 路径(mutualDelete/无 robot)已事务化;pin/revoke 锁序死锁已修
  (seq 先于 pinned);批量路径(clearPinned/readed/manager)改为一次性区间预留 + 排序 key;
  manager `req.List` 加 ≤1000 守卫;删除两处 `genMessageExtraSeq` 与死代码 `insertOrUpdateDeleted`。
  新增 `modules/message/msgextra_writer_test.go`(3 测:分块 `reserveVersions`、legacy、
  `insertOrUpdateDeletedTx` upsert)。全量 build/vet 干净;各包单独跑绿(message 包既有测试无回归)。
  注:跨包 `go test ./...` 在本地共享未迁移 `test` 库下有 schema 耦合冲突(CI 全量迁移无此问题)。
- **待办**:PR-2b(bot_api+carddispatch)、PR-2c(robot+源码守卫)、PR-4(preflight/activation 工具)、
  PR-5(runbook);PR-2a/2b 的 writer 级 focused 测试(brief Acceptance)需在此后补齐。

---

## 0. 总体策略

- **代码全部可在 `mode=legacy` 下先合并**:allocator 内部按 DB 状态行分流,legacy 分支仍调用 `ctx.GenSeq`,生产行为不变。真正的「cut over」是**运维动作**(跑工具翻转状态行),不是代码发布。
- 按**叶子包 + 分流器**收口:所有写入方改成调用共享 `internal/msgextraseq.Store`,由它按 `mode` 决定走 legacy adapter 还是事务序列。源码守卫强制「除该 adapter 外无人再调 `GenSeq(MessageExtraSeqKey)`」。
- 分 5 个可独立 review 的 PR(PR-2 再拆 3 个子 PR),源码守卫随**最后一个**写入方一起落地(否则守卫无法通过)。

## 1. 落地顺序(PR 切分)

```
PR-1  schema + allocator 内核(inert)         ← 先行,生产无感
  └─ PR-2a  message 模块写入方(7 处中的 6 处)
       └─ PR-2b  bot_api + carddispatch(3 处,已近目标形态)
            └─ PR-2c  robot(1 处)+ 源码守卫(allowlist 收口)
  └─ PR-4  cutover preflight/activation 工具   ← 依赖 PR-1 的状态表
  └─ PR-5  rollout runbook 文档
────────────────────────────────────────────
[运维] 全部合并+部署(mode=legacy)后,drain→跑工具翻 mode=transactional
```

PR-4/PR-5 与 PR-2 无代码依赖,可并行推进,只依赖 PR-1 的表结构。

---

## 2. PR-1 — schema + allocator 内核(生产 inert)

### 2.1 迁移文件
`modules/message/sql/20260721NNNNNN_message_extra_version.sql`(HHMMSS 序号按目录内既有最大值 +1;`--go:embed sql` 已覆盖)
- `CREATE TABLE octo_message_extra_channel_seq`(per-channel 序列)
- `CREATE TABLE octo_message_extra_version_state`(单例状态)+ seed `(1,0,0,0)`
- **`message_extra` 上新增复合索引 `(channel_id, channel_type, version)`** —— 大表在线 DDL 项,单独一条 DDL 语句,便于运维用 OSC 工具单独处理。
- 风格:内联 `PRIMARY KEY`/`KEY` + 列级 `COMMENT` + `ENGINE=InnoDB DEFAULT CHARSET=... COLLATE=... COMMENT=...`,对齐 `octo_message_card_revision`。
- ⚠️ **开工前先实测** `message_extra.channel_id` 的**有效** charset/collation(它未显式声明,继承库默认),把 `octo_message_extra_channel_seq.channel_id` 钉成一致,不得假设 `utf8mb4_general_ci`。

### 2.2 新叶子包 `internal/msgextraseq/`
只依赖 octo-lib(`config.Context` / `GenSeq` / `DB`)+ `common`,**不 import 任何 `modules/*`**(勘察确认:message→robot、bot_api→carddispatch+robot、carddispatch 不 import 三者;叶子包对四者都安全)。文件建议:
- `store.go` — `Store` 结构 + 构造(持 `*config.Context`)。
- `reserve.go` — `ReserveTx(tx *dbr.Tx, channelID string, channelType uint8, count int) (first int64, err error)`:
  1. `count` 校验(>0、≤ 上限);
  2. state 行 `SELECT ... FOR SHARE`(见 `state.go`)→ 得 `(mode, epoch, floor)`;
  3. `mode=legacy` → 调 `legacyAdapter`;`mode=transactional` → 事务序列;
  4. 事务序列:init 竞争安全 upsert(`INSERT ... ON DUPLICATE KEY UPDATE last_version=last_version` 占位 → `SELECT ... FOR UPDATE` 复读)→ 首用 `last_version = max(floor, max(message_extra.version WHERE channel), legacy seq.min_seq)` → **每次分配先把 `last_version` 抬到 ≥ floor** → 加锁后判定接受再 `+count` → 返回 `old+1..old+count`;
  5. 溢出守卫:`last_version+count > 2^53-1`(或 int64)在变更前失败。
- `state.go` — 状态行读取 + fail-closed 规则(缺行/多行/未知 mode/读失败 → error;epoch 只读不校验);可选 expected-mode 断言。
- `legacy_adapter.go` — **唯一允许**的 `ctx.GenSeq(MessageExtraSeqKey:channelID)` 调用点(源码守卫 allowlist 目标)。
- `metrics.go` — D8 指标(reserve_total/seconds、lock_wait_seconds、state_lock_wait_seconds、batch_size、allocator_mode/epoch/cutover_floor gauge、invariant_violation_total)。

### 2.3 隔离级别
按仓库 ambient RR 设计;初始化竞争由 upsert 兜底。**不**在池化连接上裸 `SET TRANSACTION ISOLATION LEVEL`。

### 2.4 PR-1 测试(`internal/msgextraseq/*_test.go`,需真 MySQL)
两会话并发唯一递增 / 回滚不可见 / 异 channel 无全局锁 / 首用 bootstrap 四态 / 两并发 initializer(无 1062 外泄、无死锁)/ batch 一次预留连续区间 / 边界(count≤0、超上限、int64、2^53-1)。allocator 指标断言无内容泄漏。

---

## 3. PR-2 — 写入方 cutover(全 7 处走 Store)

三类改造(按难度):

**A. 已是「锁内分配」目标形态,最小改动(只换分配来源)** — 5a `cardSeqCASWrite`(send.go:1031)、5b `cardVersionInLockWrite`(send.go:1095)、6 `casWriteOnce`(carddispatch/mutation.go:263)。已在 tx 内、已持 `SELECT ... FOR UPDATE` 行锁、CAS 后才分配。改动:`GenSeq(...)` → `store.ReserveTx(tx, ...)`;并在 tx 顶部加 state 行 `FOR SHARE`(D4 锁序第 1)。

**B. tx 已存在但 version 在锁外前置分配(挪进事务序、调锁序)** — 1a `messageEdit`(api.go:806,tx 933/991)、1c `revoke`(api.go:2981,tx 3106/3182)、2a `pinnedMessage`(api_pinned.go:25,tx 177/246)。改动:把 `genMessageExtraSeq` 从 tx 外前置调用改为 tx 内 `store.ReserveTx`,置于 state `FOR SHARE` 之后、写 message_extra 之前。

**C. autocommit,需引入事务(工作量最大)** — 1b `mutualDelete`(api.go:2394,`insertOrUpdateDeleted` 用裸 session)、7 `botMessageEdit`(robot/api.go:1920,裸 `ctx.DB().InsertBySql`)。改动:`begin → state FOR SHARE → ReserveTx → mutate(tx) → commit → CMD`。需给 `insertOrUpdateDeleted` 增 tx 版本(`db_message_extra.go`),robot 裸 upsert 改成 tx 内。

**批量路径(循环内逐条分配)** — 2b `clearPinnedMessage`(api_pinned.go:336)、3 `handleReadedMessageCount`(event.go:59,双层循环)、4 `Manager.delete`(api_manager.go:118)。改动:**先对 key 排序**(D4:map 迭代序不得决定锁序)→ 每 channel **一次** `ReserveTx(count=N)` 取连续区间 → 循环内按序赋值,不再逐条 round-trip。注:这些路径今天已是「每消息一个 GenSeq」→ 每行 version 本就不同,批量化是**减少往返 + 修锁序**,非修重复。

**统一项**:
- 回滚风格统一为 `defer tx.RollbackUnlessCommitted()`(老 handler 目前逐处手动 `tx.Rollback()`)。
- 1213/1205 有界重试**只包整事务一层**(D4),不嵌套。
- CMD/EventCommit 仍在 commit 之后。

### 3.1 PR-2 子拆分
- **PR-2a**:message 模块 6 处(1a/1b/1c/2a/2b/3/4)。含 `db_message_extra.go` 加 `insertOrUpdateDeletedTx`。
- **PR-2b**:bot_api + carddispatch 3 处(5a/5b/6)。
- **PR-2c**:robot 1 处(7)+ **源码守卫测试**(禁止 `GenSeq(common.MessageExtraSeqKey)` 出现在 `internal/msgextraseq/legacy_adapter.go` 以外)。守卫必须随最后一个写入方落地。

### 3.2 PR-2 测试
每写入方 focused 测试(user edit / bot edit / callback / robot / revoke / delete / pin / manager delete / 批量读回执)走事务序列;卡片 CAS 回归(stale 拒绝、同帧 replay、CAS/LWW 混合、终态内容正确);#627 卡片跨副本回归(中间帧被 observe 后终态帧 version 更大且可被下次 delta sync 拉到);死锁/超时重试与「commit 前不发 CMD」。

---

## 4. PR-4 — cutover preflight/activation 工具

`tools/msgextra-version/`(新目录,读多写少,独立 main):
- 默认/只读 **preflight**:Redis `SCAN`/`HSCAN`(禁 `KEYS`)+ 有界 DB 查询,求三源最大值(`message_extra.version` / 匹配 `seq.min_seq` / `messageExtraVersion:*` hash),校验 operator 给的 floor(严格 > 观测最大值、≤ 2^53-1、有余量);输出脱敏(不打 UID/source/channelID/Redis 值)。
- **activation** 动作:短 `innodb_lock_wait_timeout` 会话 → state 行 `FOR UPDATE` → 断言 expected mode/epoch(CAS)→ 原子 set `mode=1, cutover_floor=F, epoch+1`。
- **emergency** 动作:合并同 channel_id 所有 channel_type 的 watermark(DB/Redis/legacy/`octo_message_extra_channel_seq.last_version`/floor 取 max)→ upsert legacy `seq.min_seq` ≥ watermark → state 记 `mode=0, epoch+1` → 要求 0 running replica。

### 4.1 PR-4 测试
DB/legacy-seq/Redis 最大值、`seq.min_seq` 陈旧/倒退、2^53 溢出拒绝、脱敏、expected mode/epoch CAS、原子转换、state 缺失/损坏时事务态拒写;emergency watermark 合并 + 单副本约束 + 再 cutover 用更高 floor/epoch。

---

## 5. PR-5 — rollout runbook

`.octospec/tasks/message-extra-transactional-version/runbook.md`(或 docs 目录):
Prepare(建表建索引/部署双能力二进制/确认镜像与就绪)→ drain(停 message-extra 写、等在途)→ preflight 权威复跑 → activation → 校验(每副本上报 mode/epoch/floor、无 pre-task 二进制、invariant_violation=0、无新行 version ≤ 该 channel 前值)→ 正常应用回滚(只回退到 transactional-capable 二进制)→ 目标 stale-client recovery → 应急单副本 legacy + 再 cutover。

---

## 6. 开工前需实测/确认(阻塞项)—— 探查结果

1. **`message_extra.channel_id` 有效 collation → `utf8mb4 / utf8mb4_general_ci`(已实测)。**
   本地 `octo-test-mysql`(mysql:8.0)实测:`channel_id varchar(100) utf8mb4 utf8mb4_general_ci`、`channel_type smallint`;`octo_message_card_revision.channel_id` 同为 `utf8mb4_general_ci`。CI 亦显式用 `CREATE DATABASE ... COLLATE utf8mb4_general_ci` 重建 test 库(mysql:8.0 服务端默认是 `utf8mb4_0900_ai_ci`,被特意改成 general_ci 以兼容 legacy 表)。
   → 新表 `channel_id` 钉 `VARCHAR(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci`。
   ⚠️ **仍需在生产库确认一次**:`SELECT collation_name FROM information_schema.columns WHERE table_name='message_extra' AND column_name='channel_id'`。若生产历史库不是 general_ci(如老 utf8mb3),以生产实测值为准对齐,不要照抄本值。

2. **MySQL 8.0(已确认)→ 复合索引原生在线,无需外部 OSC。**
   CI `mysql:8.0` + 本地容器 `mysql:8.0`;仓库约定「8.0 ADD INDEX 默认在线,别 pin `ALGORITHM=INPLACE,LOCK=NONE`」。`message_extra` 加 `(channel_id,channel_type,version)` 走朴素 `CREATE INDEX` 即 INPLACE/LOCK=NONE 在线,不阻塞 DML,无需 gh-ost/pt-osc。
   ⚠️ 大表 INPLACE 建索引仍会在负载下持续一段时间,运维应安排低峰窗口 + 监控;这是「时长」问题不是「阻塞」问题。

3. **`count` 上限 → 建议常量 `1000`(对齐本模块既有分块幅度 api_manager.go:763)。** 三批量路径实测:
   - **pin(2a/2b)**:每频道 ≤ `maxCount`(默认 **10**,`ChannelPinnedMessageMaxCount` 可配),天然远低于上限,无需额外处理。
   - **manager 删除(4)**:`req.List` **客户端可控、当前仅 `len==0` 守卫、无上限** → 需加显式上限校验:`len(req.List) > 1000` 用既有本地化错误契约拒绝(或分块)。
   - **读回执(3)**:每频道 group 大小 = 该 tick 内该频道待物化的消息数,**随流量累积、无硬上限**,且是内部路径不能丢 → **按 ≤1000 分块预留**(不 reject);同一频道多次 `ReserveTx` 仍单调。
   → allocator `ReserveTx` 对 `count > 上限` 返回错误;调用方按语义选择「拒绝」(客户端输入)或「分块」(内部批量)。

4. **叶子包命名 → 采用 `internal/msgextraseq`**(语义直白、与现有 `internal/carddispatch`/`internal/cardactiondispatch` 命名风格一致)。仅此一项为纯决策,无需探查。

### 阻塞项状态:1/2/3 已探明并给出取值,3 附带发现「manager 删除批量无上限」应顺带修;仅生产库 collation 需上线前再确认一次(非本地可解)。

## 7. 验收锚点(对齐 brief §Acceptance)

至少 `./modules/message/... ./modules/bot_api/... ./modules/robot/... ./internal/carddispatch/... ./internal/msgextraseq/...` focused 通过;有 MySQL/Redis/WuKongIM 环境时跑 `go test ./...`;`golangci-lint run ./...`;`make i18n-extract-check` + `make i18n-lint`(未新增错误码,但改动了 message 模块 handler 需过 D23 守卫)。

---

## 附:写入方事务边界勘察结论

| # | 函数 (文件:行) | 事务 | Begin/Commit | version 分配 | 改造类 |
|---|---|---|---|---|---|
| 1a | `messageEdit` api.go:806 | tx | 933/991 | 事务内(锁外前置) | B |
| 1b | `mutualDelete` api.go:2394 | **autocommit** | 无 | 裸调用前 | **C** |
| 1c | `revoke` api.go:2981 | tx | 3106/3182 | 事务内(锁外前置) | B |
| 2a | `pinnedMessage` api_pinned.go:25 | tx | 177/246 | 事务内(锁外前置) | B |
| 2b | `clearPinnedMessage` api_pinned.go:336 | tx | 463/507 | 事务内,循环逐条 | B+批量 |
| 3 | `handleReadedMessageCount` event.go:59 | tx | 122/221 | 事务内,双层循环 | B+批量 |
| 4 | `Manager.delete` api_manager.go:118 | tx | 184/274 | 事务内,循环逐条 | B+批量 |
| 5a | `cardSeqCASWrite` send.go:1031 | tx | 1039/1074 | **锁内**(目标形态) | A |
| 5b | `cardVersionInLockWrite` send.go:1095 | tx | 1096/1119 | **锁内** | A |
| 6 | `casWriteOnce` mutation.go:263 | tx | 264/297 | **锁内** | A |
| 7 | `botMessageEdit` robot/api.go:1920 | **autocommit** | 无 | 裸 insert 前 | **C** |

依赖方向:`message→robot`;`bot_api→carddispatch+robot`;`carddispatch→{botidentity,group,thread}`(不 import 三者)。→ 共享包必须是不 import `modules/*` 的叶子包。

dbr 事务范例:`ctx.DB().Begin()` / `session.Begin()` + `defer tx.RollbackUnlessCommitted()` + `tx.Commit()`(最贴目标形态:`internal/carddispatch/mutation.go:264-297`)。
