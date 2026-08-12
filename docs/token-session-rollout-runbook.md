# Token session v3 rollout runbook

本版本把 session rollout control 收敛为一个 MySQL 权威状态，删除 Redis floor + MySQL marker
恢复状态机的运行时路径。Redis 只承载 session 数据、writer lease、scan-owner lease 与 `run_id`
证据。历史 marker migration/table 仅为 `sql-migrate` 账本兼容保留，服务不再读写它。

默认 `OCTO_AUTH_SESSION_AUTO_ADVANCE=0`。首次部署先验证接管、registry、扫描和 migration，
再显式开启自动推进。

## 1. 权威状态与启动顺序

MySQL migration 创建：

- `octo_session_rollout_state`：单例 floor、`max_per_uid`、version、paused；
- `octo_session_rollout_advance`：append-only 推进 evidence/audit。

升级不会删除或改名已记录的 `20260811000001_session_rollout_marker.sql`；这样旧库的
`gorp_migrations` 仍可校验通过。真正的 control 表由更晚 migration 创建。

服务启动顺序：

1. 构造共享 Redis session store，立即 fence 新 credential；
2. `module.Setup` 执行 MySQL migration；
3. 加载 MySQL singleton；
4. singleton 不存在时，一次性读取 #725 Redis floor 与遗留 MODE，取较严格 floor；cap 优先
   保留 Redis 记录，其次遗留 `MAX_PER_UID`，最后默认 20；
5. MySQL transaction 写 singleton + bootstrap/takeover audit；
6. 应用本地 mode，加入 writer registry，并原子发布 applied state + lease；
7. 发布成功才解除 issuance fence，然后启动 poller/reconciler 和 HTTP serve。

singleton 已存在时，后续启动不再读取 Redis floor。Redis 恢复旧快照、清空或 failover 都不能
改变 rollout mode。

首次接管若 MySQL 或 legacy Redis floor 不可读，服务启动失败并保持 fenced。不要手工 seed
`expand` 或 `enforce`；两种猜测分别可能 fail-open 或无证据登出用户。

## 2. 配置

| 配置 | 说明 |
|---|---|
| `OCTO_AUTH_SESSION_AUTO_ADVANCE` | `1` 开启自动推进；本版本首次部署必须先保持 `0` |
| `OCTO_AUTH_SESSION_EXPECT_WRITERS` | 首次 `expand → v3-write` 所需计划副本数；通过后可删除 |
| `OCTO_AUTH_SESSION_CANARY_AHEAD` | `1` 让该副本比 MySQL floor 高一阶；只用于受控灰度 |
| `OCTO_AUTH_SESSION_REDIS_POOL_SIZE` / `..._POOL_TIMEOUT` | session 共享 Redis pool 参数 |
| `TS_CACHE_TOKENEXPIRE` | session 最大 TTL，沿用 #725 |

仅在首次接管兼容读取：

- `OCTO_AUTH_SESSION_MODE`：若高于 Redis floor，会作为 MySQL seed 下限；
- `OCTO_AUTH_SESSION_MAX_PER_UID`：legacy Redis control 未带 cap 时作为 seed；
- `OCTO_AUTH_SESSION_REQUIRED_FLOOR`：无效，启动只告警。

确认 MySQL singleton 已正确 seed 后，删除这些遗留项。之后更改它们不会改变 floor 或 cap；
takeover 后修改 cap 必须使用下方 `set-cap`。

启动日志示例：

```text
Authentication session runtime: mode=revoke rollout_floor=revoke boot=adopted
  auto_advance=false mysql_versioned=true redis=<addr> instance=<fingerprint>
  token_ttl=720h0m0s redis_pool_size=... redis_pool_timeout=... build=<commit>
```

`boot` 只有：`fresh`、`adopted`、`normal`。旧的 `rollback-recovered` / `unknown` 已删除。

## 3. Operator 命令

```bash
/home/app session-rollout status
/home/app session-rollout observe --batch-size 200 --qps 100
/home/app session-rollout migrate --campaign <id> --cutoff <RFC3339> \
  --finite-policy natural --batch-size 200 --qps 100 --lease 30s
/home/app session-rollout set-cap --max-per-uid <1..10000> --yes
/home/app session-rollout pause
/home/app session-rollout resume

# 仅 reconciler 故障时使用；仍执行完整 predicate 与 MySQL CAS。
/home/app session-rollout advance --force --yes \
  [--expect-writers N] [--observe-window 90s]
```

每个命令都会先打印 Redis endpoint、DB、实例指纹和 Token TTL。命令同时直连 MySQL control，
所以 config 必须同时指向正确的 Redis 与 MySQL。

`advance` 不再接受 `--max-per-uid`；cap 来自 MySQL authority。`set-cap` 在一个 transaction
内写 `transition_kind=set-cap` 的旧/新 cap audit，并执行 version/floor/旧 cap CAS。CAS loser
不会留下 audit，重新执行 `status` 后再重试。

cap 可收紧或提高。副本通过 5 秒 poller 应用新 version，不需要滚动重启；传播窗口内仍可能有
副本短暂使用旧 cap。降低 cap 是 create-gate：它不会踢出已经存在的 session，只会在当前有效
session 数不低于新 cap 时拒绝后续签发，因此不能把 `set-cap` 当作紧急全量撤销命令。
`status.last_advance` 沿用历史字段名，最近一次 transition 是 cap 修改时会显示 `set-cap`。

`status` 关键字段：

```json
{
  "floor": "revoke",
  "version": 3,
  "max_per_uid": 20,
  "reconciler_paused": false,
  "writers": [{"build": "<commit>", "applied_state": "revoke", "pod": "<pod>"}],
  "tokens": {"complete": true, "redis_instance_id": "...", "persistent": 0, "v2": 12},
  "last_advance": {
    "from": "revoke",
    "to": "revoke",
    "actor": "operator",
    "transition_kind": "set-cap",
    "cap_change": {"from_max_per_uid": 20, "to_max_per_uid": 5}
  },
  "next": {"target_floor": "bounded", "allowed": true}
}
```

`blocked_by` 是排障起点，禁止绕过。`status` 本身可能触发一次限速全量扫描；若另一个
observe/migrate/reconciler 已持有 scan-owner，命令会明确报错，等待当前扫描结束后重试。

## 4. 推进门禁

floor 只允许：

```text
expand -> v3-write -> revoke -> bounded -> enforce
```

| 目标 | 门禁 |
|---|---|
| `v3-write` | live writer 非空、同一 build、均应用 current floor、数量等于 `EXPECT_WRITERS`、集合稳定 30s、完整扫描 |
| `revoke` | writer 非空、同一 build、均应用 current floor |
| `bounded` | 上述 writer 条件；完整扫描；`persistent=0 && over_max=0` |
| `enforce` | 上述 writer 条件；完整扫描；`v1=0 && v2=0` |

任何需要扫描的 decision 都绑定：

- 扫描完成时的 Redis `run_id` 指纹；
- 扫描前后完全一致的 writer fingerprint；
- scan-owner lease。

Redis 实例在扫描中变化时，cursor 和累计计数全部丢弃，从 0 重扫。scan-owner 丢失、续租失败
或 Redis 读错误均不会生成 complete evidence。

推进在一个 MySQL transaction 中插入 evidence 并执行 `version + floor + paused=0` CAS。
多副本并发只有一个 winner，loser 的 evidence 会 rollback。

## 5. 首次部署与启用

1. 部署本制品，保持 `AUTO_ADVANCE=0`；不要同时删除 legacy 配置。
2. 确认 migration 成功，所有副本启动，启动日志没有 MySQL/registry publication 错误。
3. 执行 `status`，核对：
   - MySQL floor 等于升级前 Redis floor 与 legacy MODE 中较严格的姿态；cap 优先保留 Redis
     已持久化值，仅在旧记录未带 cap 时使用 legacy `MAX_PER_UID`；
   - `version >= 1`，`paused=false`；
   - writers 数量等于计划副本数，build 一致，applied state 不低于 floor；
   - Redis endpoint 与 instance fingerprint 正确。
4. 执行一次 `observe`，确认 complete、`run_id`、计数和 scan-owner 行为。
5. 对 migration 做 dry-run；核对 `shortened` / `would_delete` 与业务影响。
6. 固定副本数或暂停 HPA，设置 `EXPECT_WRITERS`。
7. 显式设置 `AUTO_ADVANCE=1` 并滚动；观察 MySQL audit、writers 与 blocked reason。
8. floor 通过 `v3-write` 后可删除 `EXPECT_WRITERS`；确认 seed 后可删除 legacy MODE/MAX 配置。

首个带 registry 的版本不能在自身滚动过程中自动推进：旧制品不会注册，只有部署系统提供的
预期副本数与稳定窗口能排除尚未退出的旧 writer。

## 6. Migration

只有 legacy token 仍存在时才需要：

```bash
# dry-run
/home/app session-rollout migrate --campaign prod-2026-09-01-a \
  --cutoff 2026-09-08T00:00:00Z --finite-policy natural \
  --batch-size 500 --qps 200 --lease 30s

# 参数完全一致后 apply
/home/app session-rollout migrate ... --apply
```

`natural` 只收敛永久与超上限 legacy；`cap` 还会把有限 legacy 压到 cutoff，影响面更大，
需要业务批准。cutoff/policy 对同一 campaign 不可变；cutoff 已过时必须显式
`--confirm-elapsed-cutoff`。

migrate apply 只有本副本已应用 `revoke` 或更高 mode 才允许，并同时持有：

- scan-owner：避免多个全量扫描叠加；
- migration mutation lock：保证单 writer；
- `run_id`-bound checkpoint：failover 后从 0 重扫。

迁移只缩短 TTL，绝不延长 deadline；可安全重跑。不要删除 campaign/checkpoint 或换 campaign
复活已过期 Token。

## 7. 故障处理

常见 blocker：

| blocker | 处理 |
|---|---|
| `rollout is paused` | 确认事故已处理后 `resume` |
| `no live writers registered` | 查 Redis、registry lease 与启动 publication |
| `N distinct builds are live` | 等滚动完成，不推进 |
| `N of M writers have not applied floor X` | 查 DB poller/registry publish，失败副本会保持 fenced |
| `expected N writers, registry has M` | 查旧制品、HPA/maxSurge、计划副本数 |
| `writer set changed during evaluation` | 等滚动/重启完成后重试 |
| `persistent=N over_max=M` | 等自然过期或审批 migration |
| `v1=N v2=M` | 等 promotion/过期或审批 migration |
| `keyspace scan did not complete` | 查 Redis、run_id 变化、scan-owner/lock |

故障时先执行：

```bash
/home/app session-rollout pause
/home/app session-rollout status
```

`pause` 只阻止推进，不阻止副本应用已经存在的更高 floor。

- MySQL 短暂不可读：保留当前 mode，不推进，不从 Redis/env 猜测；
- registry 发布失败：保持新 credential fenced，已有 session 验证继续；
- Redis 不可达：新登录/签发失败，已有 session 在可读范围内继续；不要同时摘除所有副本；
- scan-owner 被占：等待当前 scan 完成，不要删除 lease key；
- run_id 变化：当前扫描自动丢弃并重扫；反复变化时先稳定 Redis 拓扑。

## 8. 回滚与 roll-forward

先读取 MySQL state 与最后 audit，按以下下界处理：

| 状态 | 允许动作 |
|---|---|
| singleton 仅完成接管、MySQL floor/cap 均未偏离原 #725 posture | 可回滚 #725；恢复 #725 所需 MODE/REQUIRED_FLOOR，并确认旧 Redis floor 仍在 |
| MySQL floor 已推进或 cap 已通过 `set-cap` 修改 | 禁止回滚到只认 Redis floor/cap 的制品；`pause` 后 roll forward |
| migration 已 apply | 可暂停/续跑；不得延长 TTL、删除 deny marker 或恢复旧 checkpoint |

本版本不写、不修复、不删除 #725 Redis floor，以保留“floor/cap 均未偏离接管值”阶段的回滚窗口。
MySQL floor 或 cap 一旦改变，回写 Redis 作为“同步”会重新引入双状态源，禁止操作。

SQL migration 的 Down 仅用于未启用、无推进的开发环境，不是生产回滚 SOP。

## 9. 监控

| 指标 | 告警方向 |
|---|---|
| `dmwork_session_live_writers` | 与副本数不符或持续波动 |
| `dmwork_session_writer_fence_total` | 持续增长表示登录签发被 fence |
| `dmwork_session_floor_advance_total{to,actor}` | 与 MySQL audit 对账 |
| `dmwork_session_reconcile_blocked_total{reason}` | 非预期 blocker 长期增长 |
| `dmwork_session_reconcile_scan_total` | 增速异常表示退避/scan-owner 未生效 |
| `dmwork_session_rollout_boot_outcome{outcome}` | 仅应见 fresh/adopted/normal |
| `dmwork_session_rollout_mode{mode}` | 除 canary 外全副本应收敛 |
| `dmwork_session_undecodable_records` | 非 0 需调查；不阻塞 floor，但不会由 migration 清理 |

额外对 MySQL 建议监控：control Load/Advance 错误率、singleton version 变化、paused 持续时间、
advance audit 与 floor version 是否一致。

## 10. Redis / MySQL 预检

在生产同型号、同版本、同 proxy 路径验证：

- Redis：`EVALSHA`、`SET NX PX`、compare-renew/delete、`SCAN` cursor、稳定 `INFO server run_id`、
  failover 后 lease/checkpoint 行为；
- MySQL：migration 权限、InnoDB transaction、JSON 列、CHECK 兼容性、连接池与读写超时；
- 容量：全量扫描 QPS、writer/scan lease key 数、advance audit 增长。

Redis 仍建议 `noeviction`：generation、legacy deny marker、migration campaign/checkpoint 与 lease
是安全状态。Redis 不再保存权威 floor，但 deny marker 被驱逐仍会静默重新接受已撤销 legacy token。
