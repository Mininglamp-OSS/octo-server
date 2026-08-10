# Token session v3 rollout runbook

本 runbook 只描述 PR 2 的受控启用。合并或以默认 `expand` 部署不会关闭历史 Token 漏洞；
只有完成 migration、两次完整观察、`enforce` 灰度和安全复测后才能关闭漏洞。

## 1. 安全边界

- 运维工具必须使用与目标 API 制品相同的 commit 构建，并读取同一 Redis endpoint、DB、TLS
  配置和 `tokenCachePrefix` / `uidTokenCachePrefix`。不要用本地 standalone Redis 结果替代生产
  proxy/Cluster 结论。
- 所有阶段按环境独立执行。命令中的配置路径、namespace、Deployment 和 selector 必须替换为
  该环境的真实值。
- 不得并行运行 migration apply。observe、apply 也应错峰，避免两个独立的两连接池同时给 Redis
  增压。
- 不得输出或采集 Token、完整 Redis key、payload、generation、索引成员或 UID。工具的标准输出
  只保留聚合 JSON；原始 `SCAN` 结果不得进入工单或日志。
- rollout floor、migration campaign/checkpoint/evidence 和 legacy deny marker 均为无 TTL 安全状态。
  它们所在 Redis namespace 必须 `noeviction`；本版本没有可替代的 durable ledger。

## 2. 配置清单

| 配置 | 约束 |
| --- | --- |
| `TS_CACHE_TOKENEXPIRE` | 可缺失；显式配置必须是正数 Go duration，最大 `720h`。空值、`30d`、非正数或超过上限会启动失败。 |
| `OCTO_AUTH_SESSION_MODE` | 缺失时为 `expand`；合法值依次为 `expand`、`v3-write`、`revoke`、`bounded`、`enforce`。显式空值非法。 |
| `OCTO_AUTH_SESSION_MAX_PER_UID` | `v3-write` 及之后必须显式配置为 `1..10000`。数值和“满额拒绝新登录”行为须经产品/容量签字。 |
| `OCTO_AUTH_SESSION_REQUIRED_FLOOR` | floor 建立后必须设置为当前已批准 floor。control key 缺失、损坏或低于该值时实例拒绝启动。 |
| `OCTO_AUTH_SESSION_REDIS_POOL_SIZE` | 可选；默认 `10 * GOMAXPROCS`，最大通常为 4096。必须按副本数和 `maxSurge` 做连接预算。 |
| `OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT` | 可选；默认 `3s`，必须为 `(0,30s]`。 |

每次部署先检查最终渲染后的 Pod env，特别是 ConfigMap/Secret 中是否存在空字符串。启动日志必须
包含且符合预期：

```text
Authentication session runtime: mode=... rollout_floor=... token_ttl=... redis_pool_size=... redis_pool_timeout=... build=...
```

从第一个 `v3-write` floor 建立后，Deployment 配置与 Redis control record 是双重防线：不能只保留
control key，也不能仅靠环境变量声明 floor。

## 3. 发布前预检

### 3.1 Redis 命令与持久性

在生产同型号、同版本、同 TLS/proxy/故障切换路径执行以下检查：

1. 候选 API Pod 以 `expand` 启动并通过真实认证读取脚本的只读 startup probe。它验证
   `EVALSHA`，脚本未缓存时由客户端回退 `EVAL`；失败会阻止 Pod 服务流量。
2. 使用受控临时单 key 验证 lease 所需的 `SET NX PX`、`PTTL`、单 key `EVALSHA/EVAL` 续约和
   compare-delete。临时 key 必须有短 TTL，禁止对共享实例执行 `SCRIPT FLUSH`。
3. 先在隔离前缀验证 `SCAN` cursor，再以获批的低 QPS 运行生产 observe。proxy 若不支持完整
   cursor 语义，不得开始 migration。
4. 通过 `CONFIG GET maxmemory-policy` 或云厂商控制面确认 `noeviction`。若 proxy 禁止 `CONFIG`，
   由 Redis 平台负责人提供可审计配置证据，不能把“命令被拒绝”当作通过。
5. 验证主从切换后脚本缓存、lease owner 比较和无 TTL key 均保持预期；startup probe 只证明
   启动时刻，不证明 failover 路径。

### 3.2 连接数与性能

最坏连接预算至少包含：

```text
session_pool_size * (stable_replicas + maxSurge)
+ 2 * concurrent_observe_jobs
+ 2 * concurrent_migration_jobs
+ 现有 API/worker/限流/运维客户端峰值
+ Redis 平台要求的保留余量
```

用 `INFO clients` / 平台监控核对 `connected_clients`、`maxclients` 和历史峰值。实际 Pod 的 pool
size 以启动日志为准；只有 CPU request、没有 CPU limit 时，也要核实容器内 `GOMAXPROCS`，不能
按 request 值猜默认池大小。

在同拓扑压测中，v3 稳态认证为串行 `EVALSHA`（Token payload + PTTL）再 `GET` generation，零
写入；legacy 在迁移窗口还会读取 deny marker。按生产峰值认证 QPS 评估 Redis command rate、
CPU/network、auth p95/p99、pool wait/timeout 和 401 增幅，确认后再批准 pool 和 migration QPS。

至少监控：

- `dmwork_redis_pool_timeouts_total{client="session"}`、`total_connections`、`idle_connections`；
- `dmwork_session_validation_rejected_total{reason=...}`；
- `dmwork_session_operation_duration_seconds` / `operations_total`；
- `dmwork_session_rollout_mode{mode=...}`；
- `dmwork_session_revocation_backlog` / `revocation_retries_total`；
- `dmwork_dependency_duration_seconds{dependency="redis",op=...,status=...}`；
- HTTP 401、登录 QPS/失败率、Pod restart/readiness 和 Redis CPU/network/latency。

## 4. 工具准备

从已审核 commit 构建，记录二进制 SHA256；不要从开发机未提交工作区直接复制到生产：

```bash
go build -trimpath -o /tmp/token-session-admin ./tools/token-session-admin
go build -trimpath -o /tmp/token-session-observe ./tools/token-session-observe
shasum -a 256 /tmp/token-session-admin /tmp/token-session-observe
```

下文使用：

```bash
TOKEN_CONFIG=/etc/octo-server/tsdd.yaml
MIGRATION_CAMPAIGN=legacy-token-YYYYMMDD
MIGRATION_CUTOFF=YYYY-MM-DDTHH:MM:SSZ
OBSERVATION_MIN_GAP=1h
```

`MIGRATION_CUTOFF` 是不可变的绝对 UTC deadline。`--finite-policy` 必须经批准后显式选择：

- `natural`：已在 `TokenExpire` 内的有限 v1/v2 自然过期；永久和超过上限的记录仍被收敛。
- `cap`：有限 v1/v2 也压到 cutoff，可能使更多在线用户提前重新登录。

同一 campaign 的 cutoff、finite policy 和生效 `TokenExpire` 不可改变；续跑时可以下调
`--batch-size` / `--qps`。新的 campaign 使用新的 ID，不会复用旧 checkpoint。

`OBSERVATION_MIN_GAP` 必须由发布审批显式确定且不得低于 `1h`。首次推进 `v3-write` floor 时写入
无 TTL rollout control；后续阶段可经审批增大，但不得低于已持久化值，两次 observe 必须达到
本次传入的新间隔，否则命令 fail closed。若目标环境已有旧版 rollout control 且没有该字段，
不要删除或手改 key；读取仍兼容，下一次合法 `advance-floor` 会用本次传入值原子补齐。

完整演练要为证据窗口预留时间：进入 `bounded` 前的两次 observe 至少间隔 `1h`，进入
`enforce` 前还需要 bounded floor 下两次新的 observe，最短约 `2h`，且不含扫描、部署和审批耗时。
不要把这两段等待压进不足的生产变更窗口，也不要为缩短演练降低安全下界。

## 5. 分阶段启用

### Phase A：`expand`

1. 部署 PR 2，显式或默认使用 `OCTO_AUTH_SESSION_MODE=expand`。
2. 等待 rollout 完成，确认 `desired=current=ready`，所有 PR 2 之前的 ReplicaSet 副本为 0。
3. 核对每个 Pod 的 build、mode、floor、TTL 和 pool 日志；此时不得产生 v3、不得 apply。
4. 运行一次限速 observe 做基线盘点，但不要记录 floor 证据：

```bash
/tmp/token-session-observe --config "$TOKEN_CONFIG" --batch-size 200 --qps 50
```

结果必须检查 `complete`、`read_errors`、`invalid_ttl`、`decode_invalid`、`persistent`、
`over_max`、`v1/v2/v3`。任何不完整或歧义统计都不是放行证据。

### Phase B：`v3-write`

1. 产品/容量签字 session cap；部署 `MODE=v3-write` + `MAX_PER_UID=<approved>`。此时 floor 尚未
   建立，暂不设置 `REQUIRED_FLOOR`。
2. 清零所有 `expand` 副本，确认所有 Pod 只写 v3，验证登录、复用、资料更新和 session cap。
3. 推进不可逆 writer floor：

```bash
/tmp/token-session-admin advance-floor --config "$TOKEN_CONFIG" --to v3-write \
  --observation-min-gap "$OBSERVATION_MIN_GAP"
```

4. 立即把 Deployment 增加 `OCTO_AUTH_SESSION_REQUIRED_FLOOR=v3-write` 并完成一次同制品滚动，
   清零未带 required-floor 的副本。之后 control key 丢失会使新实例 fail closed。

不得在仍有 `expand` writer 时提前建立 v3-write floor；已运行的旧进程不会动态重读 floor。

### Phase C：`revoke`

1. 部署 `MODE=revoke`、`REQUIRED_FLOOR=v3-write`，清零 `v3-write` 副本。
2. 验证退出、改密/重置、禁用/注销、管理员删除的同步撤销和 durable retry；确认 backlog 收敛。
3. 推进 floor，再滚动更新 required floor：

```bash
/tmp/token-session-admin advance-floor --config "$TOKEN_CONFIG" --to revoke \
  --observation-min-gap "$OBSERVATION_MIN_GAP"
```

部署 `REQUIRED_FLOOR=revoke` 后清零旧配置副本。只有完成这一步才允许 apply 和 rollout evidence。

legacy deny marker 从本阶段开始按被全量撤销的 UID 写入且无 TTL。上线前按可能被撤销的唯一 UID
数做容量预算；在所有 legacy reader 退出并进入最终 enforce 前禁止人工删除。本版本不提供自动
cleanup，后续清理必须走单独审核的聚合盘点和受控工具。

### Phase D：migration 与 `bounded`

先 dry-run，人工复核聚合结果和预计提前失效量：

```bash
/tmp/token-session-admin migrate \
  --config "$TOKEN_CONFIG" \
  --campaign "$MIGRATION_CAMPAIGN" \
  --cutoff "$MIGRATION_CUTOFF" \
  --finite-policy natural \
  --batch-size 200 \
  --qps 50 \
  --lease 30s
```

批准后使用完全相同的 campaign/cutoff/policy 加 `--apply`：

```bash
/tmp/token-session-admin migrate \
  --config "$TOKEN_CONFIG" \
  --campaign "$MIGRATION_CAMPAIGN" \
  --cutoff "$MIGRATION_CUTOFF" \
  --finite-policy natural \
  --batch-size 200 \
  --qps 50 \
  --lease 30s \
  --apply
```

若取消、锁丢失或失败，保留原 campaign/cutoff/policy，按 checkpoint 续跑；可降低 batch/QPS。
只有输出 `complete=true` 才会生成 migration completion evidence。

如果 apply 启动时 cutoff 尚未过期、但在限速扫描中到期，未带确认的任务会在第一条需要立即删除
的记录上由 Lua 在 `DEL` 前停止，输出 `complete=false` 和明确错误；此前已经执行的 TTL 缩短不会
回滚，也不会生成 completion evidence。当前批次可能尚未写入 checkpoint，续跑会从最近已保存
的 cursor（没有则从 0）幂等重放。此时先以同一 campaign/cutoff/policy 重新 dry-run 评估
`would_delete`，获批后再加 `--confirm-elapsed-cutoff` 续跑；不得通过延后 cutoff 或更换 campaign
绕过确认。

若启动或恢复时 `MIGRATION_CUTOFF` 已经过期，不得改 cutoff 或换 campaign 来延长原安全
deadline。先用相同 cutoff/policy 再跑 dry-run：命中的剩余记录会计入 `would_delete`；获批 apply
后会立即删除并计入 `deleted`，可能触发批量重新登录。该行为是遵守已批准绝对截止时间，不得把
它解释成普通
`shortened`。过期 cutoff 的 apply 还必须显式传入 `--confirm-elapsed-cutoff`，仅传 `--apply` 会被
工具和 store 双重拒绝：

```bash
/tmp/token-session-admin migrate \
  --config "$TOKEN_CONFIG" \
  --campaign "$MIGRATION_CAMPAIGN" \
  --cutoff "$MIGRATION_CUTOFF" \
  --finite-policy natural \
  --batch-size 200 \
  --qps 50 \
  --lease 30s \
  --apply \
  --confirm-elapsed-cutoff
```

若影响不可接受，应停止 apply 并升级安全审批，不能由运维自行延后 deadline。

每次 apply 结束都必须同时核对 exit status、`complete`、`deleted` 和 `shortened`；`deleted>0`
表示本轮执行了经显式确认的立即删除，必须与审批中的预计重新登录影响一致。

apply 完成后执行两次独立完整 observe。两次应跨过一个经批准的间隔，并分别保存聚合输出：

```bash
/tmp/token-session-observe --config "$TOKEN_CONFIG" --batch-size 200 --qps 50 --record-rollout-evidence
/tmp/token-session-observe --config "$TOKEN_CONFIG" --batch-size 200 --qps 50 --record-rollout-evidence
```

两次均须 `complete=true`、`read_errors=invalid_ttl=decode_invalid=0`、`persistent=0`、
`over_max=0`。工具会在同一安全 key 中原子保留最近两份聚合证据，不保存 credential/UID。

先部署 `MODE=bounded`、`REQUIRED_FLOOR=revoke` 并灰度确认，再推进：

```bash
/tmp/token-session-admin advance-floor --config "$TOKEN_CONFIG" --to bounded \
  --observation-min-gap "$OBSERVATION_MIN_GAP"
```

命令会机器校验 apply completion/checkpoint 和两次 observe；缺失、过期、损坏或不达标均拒绝。
成功后部署 `REQUIRED_FLOOR=bounded` 并清零旧配置副本。

### Phase E：`enforce`

等待或继续获批 campaign，直到在 `bounded` floor 下两次新的完整 observe 均满足 Phase D 条件，
并额外满足 `v1=0`、`v2=0`。旧的 revoke-floor 证据不能复用。

以 `MODE=enforce`、`REQUIRED_FLOOR=bounded` 做小流量灰度，验证历史报告 Token、正常登录、退出、
高风险撤销和 Redis 故障场景。灰度可回到 `bounded`，因为 enforce floor 尚未建立。

所有副本稳定为 enforce、旧副本为 0 且复测通过后，才执行不可逆推进：

```bash
/tmp/token-session-admin advance-floor --config "$TOKEN_CONFIG" --to enforce \
  --observation-min-gap "$OBSERVATION_MIN_GAP"
```

然后部署 `REQUIRED_FLOOR=enforce` 并清零旧配置副本。enforce floor 建立后不能再启动 bounded
实例；只能回滚到仍支持 v3 generation 且遵守 enforce floor 的兼容制品。

## 6. Kubernetes 核对模板

不要只看一次 readiness。按实际 label 替换以下变量，并保存每阶段证据：

```bash
K8S_NAMESPACE=<namespace>
K8S_DEPLOYMENT=<deployment>
K8S_SELECTOR=<label-selector>

kubectl -n "$K8S_NAMESPACE" rollout status deployment/"$K8S_DEPLOYMENT" --timeout=10m
kubectl -n "$K8S_NAMESPACE" get deployment "$K8S_DEPLOYMENT" -o wide
kubectl -n "$K8S_NAMESPACE" get rs -l "$K8S_SELECTOR" \
  -o custom-columns='NAME:.metadata.name,DESIRED:.spec.replicas,CURRENT:.status.replicas,READY:.status.readyReplicas,IMAGE:.spec.template.spec.containers[*].image'
kubectl -n "$K8S_NAMESPACE" get pods -l "$K8S_SELECTOR" -o wide
```

门禁是目标副本全部 ready、上一阶段/旧 build ReplicaSet 的 desired/current/ready 均为 0，并且每个
新 Pod 的启动日志和 rollout-mode 指标一致。若 Pod 只有 CPU request 没有 limit，同时记录容器内
实际 `GOMAXPROCS` 与启动日志 pool size。

## 7. 中止与回滚

- `expand` 尚未签发 v3 且未建立 v3-write floor：可回滚 PR 1 制品。
- v3 已签发或 v3-write floor 已建立：不得回滚 PR 1，也不得恢复 v2 writer。回滚制品必须支持
  v3、generation、deny marker 和当前 floor。
- migration 可停止并按原 campaign 续跑；不得删除 campaign/checkpoint、延长已缩短 TTL、删除
  deny marker，或通过更换 campaign 恢复已过期 Token。
- rollout control 缺少 observation gap 的旧记录不需要 Redis 手术；保留原 key，由下一次合法
  floor 推进补齐。已持久化的 gap 可提高但不得降低。
- enforce 灰度期间、enforce floor 建立前可退到 bounded；enforce floor 建立后不可降级 floor。
- generation/outbox/Redis 故障时暂停新阶段和 migration，扩容或回滚兼容 v3 制品；禁止关闭
  generation 校验或临时恢复 v2 writer。

任何阶段的 Redis error、pool timeout、认证拒绝突增、revocation backlog 不收敛、observe 不完整、
旧副本未清零或客户端重登录风暴，均为 stop condition。中止不等于漏洞关闭。

## 8. 最终关闭条件

以下全部有证据后，才可把“认证 Token 生命周期过长”标记为已修复：

1. enforce floor 与所有副本 mode/build 一致，旧副本为 0；
2. migration apply 完成，最近两次完整 observe 为 `persistent=0, over_max=0, v1=0, v2=0`；
3. Redis non-eviction、连接容量、两读性能和告警门禁已签字；
4. 报告中的历史 Token 以及退出、改密/重置、禁用/注销、多设备、多副本竞态和 Redis 故障场景
   已在目标环境复测；
5. `/v1/user/pc/quit`、设备删除、OIDC sync `invalid_grant` 等未接线 scope 已有明确产品决定，
   未完成项不得被 PR 合并状态掩盖。
