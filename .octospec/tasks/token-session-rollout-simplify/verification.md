# Verification: token-session-rollout-simplify

验证日期：2026-08-12（Asia/Singapore）

## 结论

当前工作树已经把 rollout control 收敛为 MySQL 单一权威，并通过核心包、race、覆盖率、
真实 MySQL/Redis/WuKongIM E2E 与旧库 migration 升级验证。

实现没有恢复旧的 Redis floor + MySQL marker 运行时状态机：

- `octo_session_rollout_state` 是唯一 floor/cap/paused/version 权威；
- `octo_session_rollout_advance` 与 floor CAS 在同一 transaction；
- Redis 只保留 session、writer/scan lease、migration checkpoint 与 `run_id` 证据；
- 历史 marker migration/table 仅为 `sql-migrate` 账本兼容保留，运行时无读写路径；
- PR 过程中曾执行的旧 control migration ID 保留为 no-op tombstone，真正建表由更晚 ID 完成。

未执行生产部署或真实 Redis 主从切换；这些仍属于上线前环境验证，不应从本地测试结果推断为已完成。

## 已关闭的 blocker

### 1. 并发首次接管 deadlock

`RolloutControlStore.Initialize` 对 MySQL 1062/1205/1213 做有界重试和指数退避，并响应 context
取消。并发首次启动最终都重新读取/锁定已存在 singleton，且只允许单调采纳更严格 seed。

覆盖：

- `TestRolloutControlInitializeRetriesConcurrentInsertAndAdoptsStricterSeed`
- `TestSessionRolloutConcurrentFirstTakeoverConvergesWithoutStartupFailure`

### 2. migration complete 前 mutation lock 丢失

完整扫描结束后、生成 complete evidence 前再次确认 mutation lock。最终批次后失锁会返回
`ErrMigrationLockLost`，不会留下可推进 evidence。

覆盖：`TestLegacyMigrationRejectsCompletionAfterFinalMutationLockLoss`。

### 3. 已发布 marker migration 被改名

RED checkpoint `d0f1e80f` 用隔离 MySQL 复现：数据库已记录
`20260811000001_session_rollout_marker.sql` 时，删除/改名该 source ID 会被 `sql-migrate`
拒绝为 unknown migration。

修复后保留：

- `20260811000001_session_rollout_marker.sql`：历史 ID 与表定义；运行时不使用；
- `20260811000001_session_rollout_control.sql`：已执行 PR 环境的 no-op tombstone；
- `20260812000001_session_rollout_control.sql`：创建 state + advance 两张权威表。

隔离 MySQL 同时验证两条前滚路径：

- 正式旧库：marker 已应用，tombstone + 新 control 待执行；
- PR 测试/预发布库：marker + 旧 control 已应用，仅新 control 待执行。

两条路径都不改写 `gorp_migrations`，且最终三张历史/现行表均存在。

### 4. 并发应用、初始化与 MySQL 读取边界

RED checkpoint `90e9af7d` 复现了并发初始化的 data race，并锁定了本地 rollout
state 非原子 `Load -> decide -> Store` 以及 control read 无 deadline 的结构性缺口。

修复后：

- `ApplyRolloutState` 用 atomic pointer CAS loop 实现 mode 单调；陈旧 floor 只能修复缺失
  cap，不能覆盖与更新 floor 配对的有效 cap；
- `sessionRuntime` 的初始化和 boot 读取共用实例级 mutex，并发 caller 返回同一
  control 实例和 boot snapshot；
- boot、poller 与 reconciler 的 MySQL control read 统一有 5 秒 deadline，同时保留
  caller 的更早取消/截止时间。

对应覆盖：

- `TestInitializeSessionRolloutSerializesConcurrentCallers`；
- `TestApplyRolloutStateIsMonotonicAndCarriesTheCap`；
- `TestRolloutStateSafetyMechanismsAreStructural`；
- `TestRolloutControlReadHelperAddsDeadlineAndPreservesCancellation`。

重审建议将 Redis cap 与废弃 env cap 取最小值，本实现不采用：#725 Redis control
中已持久化的 cap 是接管时的权威值，废弃 env 只用于旧 Redis 记录缺失 cap
的兼容兜底。`TestResolveRolloutSeedPreservesPersistedCapAndStrictestFloor` 覆盖了 Redis
cap=50、env cap=5 仍保留 50 的升级语义。

## 核心行为覆盖

`pkg/auth` 的测试覆盖以下安全边界：

- takeover seed、单调初始化、并发 winner、事务化 audit；
- floor 逐阶段 CAS、pause、CAS loser evidence rollback；
- registry state + lease 原子发布、publish failure issuance fence；
- scanner run_id 变化重扫、final batch 复核、scan-owner 丢失；
- observe/migrate/reconciler 共用 owner lease；
- writer fingerprint、expected writers、空 keyspace 与各阶段 predicate；
- MySQL read failure 保持当前 mode 且禁止推进；
- migration 只缩短 TTL、campaign/lock/checkpoint/failover/cutoff；
- v3 current-session revoke 删除 credential 与索引。

## E2E 核心流程

命令：

```bash
GIN_MODE=release go test ./modules/user \
  -run '^TestSessionRolloutAdoptedV3LoginAndAuthenticatedReadE2E$' \
  -count=3 -v
```

结果：连续 3 次 PASS，package `ok`（1.682s）。每次实际覆盖：

1. 隔离 MySQL 执行模块 migration；
2. Redis 写入 legacy rollout floor 并由 MySQL 一次性接管；
3. writer registry 发布后解除 issuance fence；
4. 走真实用户登录 handler；
5. WuKongIM 更新用户 token；
6. Redis 落 v3 token 与 UID 索引；
7. 生产 `CacheTokenParser` + `TokenValidator` 验证；
8. 带新 token 完成鉴权读取。

## 实测命令与结果

| 验证 | 结果 |
|---|---|
| `GIN_MODE=release go test ./modules/user -count=1` | PASS，143.185s |
| migration upgrade/source tests | PASS，marker-only 与旧 PR control 两条路径 |
| `go test ./pkg/auth -coverprofile=.context/pkg-auth-cover-final.out -count=1` | PASS，statements 80.3% |
| `go test -race ./pkg/auth -count=1` | PASS，5.590s |
| 核心 E2E `-count=3` | PASS，3/3，1.682s |
| `go test ./pkg/metrics . -count=1` | PASS |
| `go vet ./pkg/auth .` | PASS |
| `git diff --check` | PASS |

测试启动日志中的未配置可选集成告警（speech、notify、drive token 等）没有导致跳过或降级核心
session 流程；MySQL、Redis、WuKongIM 均由 E2E 实际访问。

## 未覆盖边界

- 未运行整个仓库的 `go test ./...`；已运行本改动直接涉及的完整 `modules/user`、`pkg/auth`、
  `pkg/metrics` 与根包。
- 未在生产同型号 Redis proxy/cluster 上执行真实 failover；本地测试覆盖 run_id 改变和 lease
  丢失后的 fail-closed 行为。
- 未部署到 staging/production，未验证真实副本数、HPA/maxSurge、告警阈值和容量。

上线前仍需按 runbook 保持 `AUTO_ADVANCE=0`，核对 MySQL seed、writer roster、完整 observe 与
业务 migration 影响后，再显式开启推进。
