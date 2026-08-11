# Session rollout control state table

本表覆盖所有会决定副本运行 mode、是否允许签发、以及是否推进 floor 的路径。当前设计只有一个
持久权威：MySQL `octo_session_rollout_state`。#725 Redis floor 只在 MySQL singleton 尚未建立时
读取一次；marker、provisional mode 与 Redis floor recovery 的运行时路径均已删除。历史 marker
migration/table 仅为 `sql-migrate` 账本兼容保留，不参与任何判定。

## A. 启动与首次接管

启动顺序固定为：构造 session store（issuance fenced）→ module migration → 初始化 control →
应用本地 mode → writer registry 发布成功 → 解除 fence → 启动 poller/reconciler → HTTP serve。

| # | MySQL singleton | #725 Redis floor | legacy MODE | 结果 | 是否可签发 |
|---|---|---|---|---|---|
| A1 | present | 不读取 | 不再覆盖 | `normal`，应用 MySQL floor/cap | registry 发布成功后是 |
| A2 | absent | present | ≤ Redis floor | `adopted`，事务写入 Redis floor/cap | 同上 |
| A3 | absent | present | > Redis floor | `adopted`，事务写入较严格 MODE；优先保留 Redis cap | 同上 |
| A4 | absent | absent | absent/`expand` | `fresh`，事务 seed `expand` + 默认/legacy cap | 同上 |
| A5 | absent | absent | > `expand` | `adopted`，把 legacy MODE 持久化为兼容下限 | 同上 |
| A6 | absent | unreadable | 任意 | 启动失败，不猜测 floor | 否，保持 fenced |
| A7 | unreadable/invalid | 不读取 | 任意 | 启动失败，保持已存在数据不变 | 否，保持 fenced |

接管事务同时写 singleton 与一条 `bootstrap` / `legacy-redis` / `legacy-env` audit。并发首启若
都先读到 absent，只有一个 insert winner；duplicate-key loser rollback 后重新锁行，并在自己的 seed
更严格时单调抬高 floor。不会因为正常的多副本首启竞争让某个副本永久启动失败。

Redis `run_id` 只作为接管 audit 的辅助字段。读取不到该指纹不会改变已成功读取的 legacy floor；
读取 legacy floor 本身失败则必须停止接管。

## B. 本地应用与 registry 发布

所有运行期 mode 变化都走 `ApplyAndPublishRolloutState`：

1. 设置 `issuanceFenced=true`；
2. 原子替换本地 `{mode,max_per_uid}`，mode 不允许降低；
3. 用一个 Redis Lua 原子执行 `SADD roster` + `PSETEX writer entry`，发布 applied state 并续租；
4. 仅发布成功后解除 fence。

| # | MySQL load | 本地状态 | registry publish | 结果 |
|---|---|---|---|---|
| B1 | 更高 floor | 较低 | success | 抬高本地 mode，发布，解除 fence |
| B2 | 同 floor/cap | 相同 | 已 unfenced | no-op，heartbeat 负责续租 |
| B3 | 更低 floor | 较高 | success | 保持较高本地 mode，按实际 applied mode 发布 |
| B4 | 任意有效状态 | 任意 | error | reader strictness 保留，issuance 持续 fenced |
| B5 | DB read error | 当前已应用状态 | 不发布新状态 | 保持 mode；不从 Redis/env 推导，不推进 |

heartbeat 与 applied-state publication 共用 registry write mutex，旧 heartbeat 不能覆盖新状态。fence
只影响 credential create/reuse/promotion；既有 session 验证和 readiness 不受影响。

## C. Predicate

predicate 每次接收一个 MySQL `{floor,version,max_per_uid,paused}` 快照。

| current → target | 必须满足 |
|---|---|
| `expand → v3-write` | registry 非空；单一 build；所有 writer ≥ current；`EXPECT_WRITERS` 相等；writer set 稳定一个 lease TTL；完整 instance-bound scan |
| `v3-write → revoke` | registry 非空；单一 build；所有 writer ≥ current |
| `revoke → bounded` | 上述 writer 条件；完整扫描；`persistent=0 && over_max=0` |
| `bounded → enforce` | 上述 writer 条件；完整扫描；`v1=0 && v2=0` |
| `enforce` | terminal，不扫描 |

通用规则：

- `paused=true` 立即拒绝，不读取 registry、不扫描；
- 扫描前后 writer fingerprint 必须一致；
- complete evidence 必须绑定最后一个稳定 Redis `run_id`；
- 空 token keyspace 是有效的“没有 legacy”证据，但不能替代 writer presence；
- scan-owner lease 丢失、Redis failover、读错误均不生成 complete evidence。

## D. 推进事务

允许的 decision 通过 `RolloutControlStore.Advance` 执行：

1. 同一 MySQL transaction 插入 append-only evidence；
2. `UPDATE state ... WHERE version=? AND floor=? AND paused=0`；
3. affected rows 必须为 1；
4. commit。

| 竞争/故障 | 结果 |
|---|---|
| 单 winner | evidence + 新 floor 一起 commit |
| version/floor 变化 | `ErrRolloutControlChanged`，evidence rollback |
| pause 抢先提交 | version/paused CAS miss，evidence rollback |
| evidence insert / update / commit error | transaction rollback，不推进 |
| force advance | 只绕过 reconciler；仍需 predicate allowed，仍走同一 transaction |

## E. Redis failover 与 scan owner

observe、migrate、reconciler 共用 `<uidTokenPrefix>auth:rollout-scan-owner`：

- 每个 SCAN 前后、每批 key 处理后、complete 前读取 `INFO server run_id` 指纹；
- 指纹变化时丢弃 cursor 与累计计数，从 0 重扫；
- 每批与 complete 前同步确认 scan-owner；owner 不匹配立即失败；
- migrate apply 另持有 mutation lock。scan-owner 控制全量扫描并发与成本，mutation lock 保证只有
  一个 writer 修改 legacy token，二者不能互相替代。

## F. 回滚下界

- MySQL floor 尚未高于接管 seed：可回滚到 #725，但必须恢复 #725 所需配置，并确认旧 Redis floor
  仍在；本版本从不修改它。
- MySQL floor 已推进：禁止回滚到只认 Redis floor 的制品。先 `pause`，保存 state/audit，修复后
  roll forward。
- migration 只缩短 TTL；制品回滚不得延长 deadline、删除 deny marker 或复活 token。
