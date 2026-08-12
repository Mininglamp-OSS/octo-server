---
type: Task
title: "Task: token-session-rollout-simplify"
description: Replace the runtime Redis floor and MySQL marker state machine with one MySQL-authoritative rollout state, fenced writer publication, and instance-bound scans
tags: [auth, security, redis, mysql, session, rollout, migration, operability]
timestamp: 2026-08-12T00:00:00+08:00
slug: token-session-rollout-simplify
upstream: "follow-up to PR #725"
source: self
---

# Task: token-session-rollout-simplify

## Conclusion

PR #733 的前一版把 Redis floor 与 MySQL write-once marker 组合成恢复状态机，导致 boot、
poller、predicate、registry 各自解释一部分 effective mode。多轮修补仍出现跨存储写序、
provisional 降级、registry 发布错误被吞、Redis failover cursor 复用等 blocker。

本版不再逐格修补：

1. **MySQL 是 rollout control 的唯一权威。** Redis 不再保存、推进或恢复 floor。
2. **Redis 只承担 session 数据、writer lease、scan owner lease 和 run_id 证据。** Redis
   回滚、清空或 failover 都不能改变 floor。
3. **mode 变更只有一个运行时入口。** 先 fence issuance，再应用本地 atomic state，随后用
   一次 Redis Lua 原子发布 registry state + lease；发布失败保持 fenced。
4. **observe、migrate、reconciler 共用 instance-bound scanner。** 完整扫描绑定一个 Redis
   run_id；实例变化即丢弃旧 cursor/聚合计数并从 0 重扫。
5. **全量扫描共用 owner lease。** 同一 session namespace 同时只允许一个完整扫描，避免
   status、observe、migrate 和多副本 reconciler 叠加压 Redis。
6. **自动推进默认关闭。** 先以 shadow/status 验证，再显式开启。

## Goal

- 消除 Redis floor + MySQL marker 双状态源及其恢复分支。
- 保持 `expand -> v3-write -> revoke -> bounded -> enforce` 单调逐阶段语义。
- 升级时一次性采纳 #725 Redis floor，并把遗留 `OCTO_AUTH_SESSION_MODE` 持久化为兼容
  下限，避免升级后从 `bounded` 降回 `revoke`。
- 将推进 evidence 与 floor CAS 放在同一个 MySQL transaction；并发推进只有一个 winner。
- MySQL 短暂不可读时保持当前已应用 mode，不从 Redis/env 猜测，不自动推进。
- registry 发布失败时继续验证已有 session，但拒绝新建 credential，直到成功发布。

## Data model

### `octo_session_rollout_state`

单例 `singleton_id=1`：

| field | purpose |
|---|---|
| `floor` | 当前权威阶段 |
| `max_per_uid` | v3 session 上限，与 floor 一起持久化 |
| `version` | optimistic CAS version |
| `paused` | 暂停自动推进；不阻止副本应用已存在 floor |
| `updated_at` | 运维审计时间 |

### `octo_session_rollout_advance`

append-only evidence：`from_floor`、`to_floor`、`actor`、`evidence_json`、
`redis_run_id`、`writer_fingerprint`、`transition_kind`、`created_at`。

floor advance 与 cap-only update 共用该审计流：`transition_kind=advance` 保存扫描/写副本证据；
`transition_kind=set-cap` 保持 `from_floor=to_floor`，在 `evidence_json` 保存旧/新 cap。两者都与
对应的 versioned state CAS 在同一个 MySQL transaction 中提交。

推进 transaction：

1. 插入 evidence；
2. `UPDATE ... WHERE version=? AND floor=? AND paused=0`；
3. affected rows 必须为 1；
4. 任一步失败整体 rollback。

因此不存在“有 evidence 无 advance”或“advance 无 evidence”的跨存储中间态。

### Legacy migration compatibility

`20260811000001_session_rollout_marker.sql` 及其表仅作为已发布 migration ID 的兼容 artifact
保留，运行时不再读写；删除该文件会让已升级数据库被 `sql-migrate` 判为 unknown migration。
PR 过程中曾使用的 `20260811000001_session_rollout_control.sql` 同样保留为无副作用 tombstone，
真正的 control 表由更晚的 `20260812000001_session_rollout_control.sql` 创建。历史表存在不代表
marker 仍是状态源，唯一权威仍是 `octo_session_rollout_state`。

## Startup and takeover

初始化顺序是 load-bearing：

1. 构造共享 Redis session store，但保持 issuance fenced；
2. `module.Setup` 执行 migration，创建 MySQL control tables；
3. 读取 MySQL singleton；
4. 仅当 singleton 不存在时，读取一次 #725 Redis floor 和遗留 MODE，floor 取更严格者；cap
   优先保留 Redis 已持久化值，仅在旧记录未带 cap 时使用遗留 `MAX_PER_UID`；
5. MySQL transaction 内写 singleton + bootstrap/takeover audit；
6. 应用本地 mode，绑定 writer registry，成功发布 state + lease 后解除 fence；
7. 启动 poller/reconciler，最后才进入 HTTP serve。

MySQL singleton 一旦存在，后续启动不再读取 Redis floor。Redis rollback 只影响 session data，
不能降低或提高 rollout mode。

首次接管若 Redis floor 不可读，启动失败并保留 issuance fence。此时 MySQL 尚无权威状态，
猜 `expand` 会 fail-open，猜 `enforce` 会无证据登出用户；显式失败比两种猜测都安全。

## Runtime mode publication

`ApplyAndPublishRolloutState` 是唯一运行期变更入口：

1. `issuanceFenced=true`；
2. CAS loop 原子更新 `{mode,max_per_uid}`，并发调用也不允许降低；
3. registry Lua 在一个原子执行中 `SADD roster` + `PSETEX entry`，同时发布 applied state
   和续租；
4. 成功才清 fence；失败返回 error 且保持 fenced。

heartbeat 与 state publication 由同一进程 mutex 串行化，旧 heartbeat 不得覆盖新 state。
fence 只影响 credential create/reuse/promotion，不影响已有 session 验证，也不失败 readiness。

## Advance predicate

每次基于一个 MySQL `RolloutState{floor,version}` 快照计算下一阶段：

- 所有 live writer 必须来自同一 build 集合，并至少应用当前 floor；
- 第一次进入 `v3-write` 仍要求 `EXPECT_WRITERS` 和 writer set 稳定窗口；
- 进入 `bounded` 前必须 `persistent=0 && over_max=0`；
- 进入 `enforce` 前必须 `v1=0 && v2=0`；
- 扫描前后 writer fingerprint 必须一致；
- 扫描 evidence 必须绑定完成时的 Redis run_id；
- MySQL `paused=true` 或 control read error 时禁止推进。

空 token keyspace 是“没有 legacy”的有效证据，不需要 canary token。

## Scanner and load control

`instanceBoundScanner` 在每个 SCAN batch 前后读取 Redis `INFO server run_id` 指纹：

- run_id 不变：cursor 与聚合继续；
- run_id 变化：丢弃旧 cursor 和旧聚合，从 cursor 0 重扫；
- complete evidence 只属于最后一个稳定实例。

observe、migrate 和 reconciler 进入全量扫描前必须获得同一 namespace 的
`auth:rollout-scan-owner` lease。lease 续租失败立即中止，不生成 complete evidence。
migrate apply 仍额外保留 campaign mutation lock；两者职责不同：scan lease 控制成本，
campaign lock 保证写迁移单 owner。

## Operator surface

`/home/app session-rollout` 保留：

- `status`：MySQL floor/version/paused、writer roster、live scan、last advance；
- `observe`：只读、限速、instance-bound 全量扫描；
- `migrate`：显式 campaign/cutoff/finite-policy；
- `pause|resume`：更新 MySQL pause；
- `advance --force --yes`：只绕过 reconciler，不绕过 predicate，最终仍走 MySQL CAS。

`max_per_uid` 已是 MySQL authority，不再由 advance 命令临时传入。
`set-cap --max-per-uid N --yes` 是 takeover 后唯一受支持的 cap 修改通路；它不修改 floor，
也不写回 Redis/env。降低 cap 只拒绝后续超限签发，不主动撤销已经存在的 session。

## Rollout

1. 部署本制品，保持 `OCTO_AUTH_SESSION_AUTO_ADVANCE=0`。
2. 确认 migration 成功、所有副本 registry lease 正常；MySQL floor seed 取原 #725 floor 与
   legacy MODE 中更严格者，cap 优先保留 #725 Redis 已持久化值，仅在旧记录缺 cap 时使用
   legacy `MAX_PER_UID`。
3. 运行 `status`/`observe` 做 shadow 验证；核对 run_id、writer fingerprint 和 scan owner。
4. 确认业务 cutoff/finite policy，必要时执行 migrate。
5. 显式开启 auto advance；任何异常先 `pause`，再看 MySQL state/audit 与 Redis roster。

## Rollback

- **MySQL floor/cap 均未偏离接管值时**：可回滚到 #725 兼容制品；旧 Redis floor 仍保留且
  未被本版修改。
- **MySQL floor 已推进或 cap 已通过 `set-cap` 修改后**：不得回滚到只认 Redis floor/cap 的
  旧制品。先 `pause`，保留 MySQL
  state/audit，修复后 roll forward。回写 Redis floor 会重新引入双状态源，禁止作为回滚手段。
- 数据 migration 仍只缩短 TTL、不延长；不得删除 deny marker 或复活过期 token。
- MySQL control migration 的 Down 只用于未启用/无推进的开发环境，不是生产 rollback SOP。
- 不删除、改名或复用已进入 `gorp_migrations` 的 marker/control migration ID。

## Security invariants

1. floor 只能逐阶段上升，CAS 单 winner。
2. Redis 内容不能推翻 MySQL floor。
3. evidence、writer fingerprint、Redis run_id 与 floor advance 同 transaction。
4. registry 不可发布时禁止新 credential；已有 session read 不受影响。
5. scan failover 或 scan lease loss 不得生成 complete evidence。
6. migration apply 只在当前已应用 mode 至少为 `revoke` 时允许。
7. pause/read failure 一律阻止推进。
8. cap 修改必须走 MySQL version CAS，并与旧/新 cap audit 同 transaction；陈旧 snapshot 不得
   覆盖本地已应用的新版本 cap。

## Out of scope

- 不改变 v3 token wire format、generation、deny marker 或 revocation 语义。
- 不修改用户登录 API/error envelope/i18n。
- 不删除 #725 Redis floor key；本 release 只读接管，保留旧制品回滚窗口。
- 不删除历史 marker migration/table；它仅用于 migration 账本兼容，运行时不得重新依赖。
- 不自动选择 migration cutoff/finite policy。
- 不把 writer registry 提取为跨模块共享包。

## Acceptance

- 新 control migration 定义 MySQL state + append-only advance 两张权威表；历史 marker migration
  与 PR 旧 control ID 保留为兼容 artifact，且运行时无 marker 读写路径。
- 已记录 marker migration 的旧库可直接前滚并创建两张 control 表，不出现 unknown migration。
- fresh、#725 Redis floor、遗留 MODE 三种接管路径有测试；floor 取 Redis floor 与遗留 MODE
  中更严格者，cap 保留 Redis 已持久化值，仅在旧记录缺 cap 时使用遗留 `MAX_PER_UID`。
- MySQL CAS loser 回滚 evidence；单调与逐阶段检查有测试。
- `set-cap` 通过 version/floor/旧 cap CAS 与 append-only audit 原子提交；旧版本 snapshot 不能
  覆盖新 cap，降低 cap 不被描述为主动 session revoke。
- 删除/回滚 Redis floor 后，poller 仍保持 MySQL mode。
- MySQL read failure 保持当前 mode 且不调用 advance。
- registry publish error 后 `CanIssue()` 返回 fence error，成功重试后可恢复。
- observe 在 run_id 变化时从 0 重扫且不混入旧实例计数。
- observe/migrate/reconciler 争用同一 scan owner lease。
- `go test ./pkg/auth -race`、相关 `modules/user` 测试、`go vet`、`git diff --check` 通过。
- 自动推进默认关闭；runbook 明确启用、pause、rollback/roll-forward 边界。

## COMPREHENSION

1. **最 load-bearing 的状态是什么？** MySQL singleton 的 `floor/version/max_per_uid/paused`；
   Redis floor 只在 singleton 缺失时作为一次性升级输入。
2. **推进为何可信？** predicate 的 writer/scan evidence 与 versioned floor CAS 在同一个 MySQL
   transaction；scan 又绑定一个 Redis run_id 和稳定 writer fingerprint。
3. **失败时系统怎样退化？** DB read error 不推进；registry/lease error fence 新 credential；
   scan failover/lease loss 中止并重扫；已有 session read 保持服务。
