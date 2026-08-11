# Token session v3 rollout runbook

这份 runbook 取代了 #725 的九步阶梯。旧流程要求 9 次滚动重启、11 次 CLI 调用、
两段各 ≥1h 的观察窗，以及一次对零存量环境也必须执行的空迁移——**而那整套仪式唯一的保护
对象是存量 legacy session**。没有存量时它保护不了任何东西。

现在 floor 由服务自己推进。**你只需要做一个决定：迁移的 cutoff 和策略，
也就是愿意让多少人提前重新登录。** 系统会自动走到那个决定面前停下来并说明差什么。

---

## 1. 它自己会做什么

```
全新部署        部署制品（配 EXPECT_WRITERS=<副本数>）。结束，floor 自动到 enforce。
有存量的部署    部署制品 → 自动推进，然后停在第一个有存量的门禁上：
                  有永久/超上限 legacy → 停在 revoke，报 persistent=N over_max=M
                  只剩有限 legacy      → 停在 bounded，报 v1=N v2=M
                → 你决定 cutoff 与 finite policy，跑一次 migrate
                → 到期后自动推进到 enforce
```

floor 单调不可逆，永远只前进。每次推进前都会写下触发它的证据快照
（存活 writer 数、build、扫描计数、Redis 实例指纹、时刻），用 `status` 可查。

## 2. 配置

只剩一个必须的开关，其余都可选：

| 配置 | 说明 |
|---|---|
| `OCTO_AUTH_SESSION_AUTO_ADVANCE` | `1` 开启自动推进。**首个引入本能力的 release 请保持关闭**，见 §6 |
| `OCTO_AUTH_SESSION_EXPECT_WRITERS` | 本部署计划运行的副本数。**首次建立 v3 floor 时必需**（无论 keyspace 是否为空——空只代表*此刻*没有存储，不代表没有未注册的旧副本），推过之后即可删除 |
| `OCTO_AUTH_SESSION_CANARY_AHEAD` | `1` 让该副本比 floor 高一阶，用于灰度。打在小流量副本上 |
| `OCTO_AUTH_SESSION_REDIS_POOL_SIZE` / `..._POOL_TIMEOUT` | 同 #725，未变 |
| `TS_CACHE_TOKENEXPIRE` | 同 #725，未变 |

**已废弃**（读到会告警，不会导致启动失败）：

- `OCTO_AUTH_SESSION_MODE` —— mode 现在由 floor 派生。为兼容保留一个 release：
  如果它高于 floor（#725 Phase D/E 的灰度姿态就是这样），会被继续尊重，
  否则升级会把 reader 从 `bounded` 悄悄放松回 `revoke`。floor 追上后即可删除。
- `OCTO_AUTH_SESSION_MAX_PER_UID` —— cap 已并入 floor 记录，首次启动时抄一次，之后可删。
- `OCTO_AUTH_SESSION_REQUIRED_FLOOR` —— **已完全失效**，可以直接删。

启动日志包含实际连接的 Redis 与实例指纹：

```text
Authentication session runtime: mode=... rollout_floor=... boot=... auto_advance=...
  redis=<addr> instance=<fingerprint> token_ttl=... redis_pool_size=... build=...
```

`boot=` 有四个取值，对应四种启动情形：`fresh`（全新）、`adopted`（从 #725 接管）、
`rollback-recovered`（**Redis 丢过 floor**，见 §7）、`normal`。

## 3. 唯一的命令

```bash
app session-rollout status     # 排查：floor 在哪、谁在跑、卡在什么上
app session-rollout observe    # 只读盘点，支持 --qps 限速
app session-rollout migrate    # 唯一的决策：cutoff + finite policy
app session-rollout pause      # 逃生门：秒级停下自动推进
app session-rollout resume

# 故障通道：仅当 reconciler 自身坏掉时使用。它绕过 reconciler，
# 但**不绕过谓词**——谓词不放行照样拒绝。
app session-rollout advance --force --yes [--expect-writers N] [--max-per-uid N]
```

每个子命令执行任何操作**之前**都会先打印它解析到的 Redis 地址和实例指纹：

```text
redis: <redis-service>:6379  db=0  instance=9f3e11b4a2c7d081  token_ttl=720h0m0s
```

> 这一行是有来历的。2026-08-11 一次实操把配置键写在了顶层 `redisAddr`（octo-lib 实际读
> `db.redisAddr`），键未命中导致工具回落到 `127.0.0.1:6379`，扫到本机测试残留并报
> `complete: true`。当时若带了 `--apply`，改的就是错误的 keyspace，而输出里没有任何线索。

`status` 的输出形如：

```json
{
  "floor": "revoke",
  "max_per_uid": 20,
  "reconciler_paused": false,
  "writers": [{"build": "<commit>", "applied_state": "revoke", "pod": "...<pod>"}],
  "tokens": {"total": 137, "v1": 0, "v2": 135, "v3": 2, "persistent": 0},
  "last_advance": {"from": "v3-write", "to": "revoke", "actor": "reconciler", "live_writers": 1},
  "next": {
    "target_floor": "bounded",
    "allowed": false,
    "blocked_by": ["persistent=0 over_max=0 (need 0)"],
    "options": ["wait: legacy records expire on their own", "migrate --finite-policy natural --cutoff <T>: ..."]
  }
}
```

**`blocked_by` 是排查的起点。** 卡住时读它，不要绕过去。

## 4. 那个决定：migration

只有 legacy token 还在时才需要。两条路：

| 做法 | 代价 |
|---|---|
| 什么都不做 | 最长等到 `TokenExpire`。活跃用户会被 `ReuseSession` 逐步提升为 v3，只有不活跃用户的 token 会拖满 |
| 跑一次 migration | 命中的用户下次访问需重新登录，换取立刻收口 |

```bash
# 先 dry-run，人工复核 shortened / would_delete
app session-rollout migrate --campaign prod-2026-09-01-a \
  --cutoff 2026-09-08T00:00:00Z --finite-policy natural \
  --batch-size 500 --qps 200 --lease 30s

# 参数完全一致，加 --apply
app session-rollout migrate ... --apply
```

`--finite-policy`：

- `natural` —— 只收敛永久与超上限记录，不动仍在 `TokenExpire` 内的有限 token；
- `cap` —— 有限 token 也压到 cutoff，重新登录面更大，需要批准。

migration 的全部正确性机制**与 #725 完全一致，未作任何改动**：不可变 cutoff、单占租约、
checkpoint 绑定 Redis `run_id`、只缩短不延长、cutoff 已过期时必须显式
`--confirm-elapsed-cutoff`。同一 campaign 的 cutoff/policy 永久锁定；新决策要用新的
campaign ID。

## 5. 从 #725 升级

带着进行中 rollout 的环境（floor 已是 `v3-write`/`revoke`/`bounded`）升级时：

1. 部署新制品。**唯一一次重启。**
2. 首启自动接管现有 floor；`MAX_PER_UID` 由 reconciler 在数秒内回写进 floor 记录
   （#725 的记录没有这个字段），MySQL 标记同样由 reconciler 补写——它在
   `module.Setup` 建表之后才可能成功，所以刻意不放在启动路径上。
   **不需要删除或修改任何 Redis key，也不会登出任何人。**
3. 确认 `status` 的 `floor` 与升级前一致、`writers` 数量与副本数一致。
4. **确认 `status` 的 `max_per_uid` 已经出现在 floor 记录里**，再删掉 configmap 里那三个
   已废弃的 key。删早了会让下一次重启找不到 cap（此时会退到保守默认值 20 并告警）。
5. 确认无误后再打开 `AUTO_ADVANCE`（见 §6）。

**注意灰度姿态**：如果升级前处在 #725 的 Phase D/E（`MODE` 比 floor 高一阶），
先保留 `OCTO_AUTH_SESSION_MODE` 直到 floor 追上；删早了 reader 会放松一阶。
启动日志会就此告警。

## 6. 首次开启自动推进

**首个引入本能力的 release 必须以 `AUTO_ADVANCE=0` 上线。**

原因是这套设计对自身的第一次升级有个盲区：writer registry 的作用正是发现"还有旧副本
在跑"，但升级到**带 registry 的版本**这一次，旧副本（#725 制品）根本不注册，registry
只看得见新副本。等到全部副本都是新制品、`status` 的 `writers` 数与副本数吻合之后，
再打开自动推进。

打开后：

- 全新环境：直接一路到 `enforce`；
- 有存量环境：自动到 `v3` 后停住，等你的 migration 决策。

首次为有存量环境建立 v3 floor 时需要 `OCTO_AUTH_SESSION_EXPECT_WRITERS=<副本数>`。
这不是判断题，是部署系统已经知道的一个数字：**只有新制品会注册，所以旧副本还在的话，
注册数就会不足**。它是过渡期配置，推过之后删掉即可；HPA 环境在这个窗口内临时固定副本数。

## 7. 出问题时

**先看 `status` 的 `blocked_by`。** 常见原因：

| blocked_by | 含义 |
|---|---|
| `no live writers registered` | 看不见任何副本。**空 registry 判失败，不是放行** |
| `N distinct builds are live` | 混部，等滚动更新完成 |
| `N of M writers have not applied floor X` | 有副本没跟上，通常几秒内自愈 |
| `expected N writers, registry has M` | 副本数对不上，多半还有旧制品在跑 |
| `v1=N v2=M (need 0)` | 还有 legacy，等自然过期或跑 migration（挡 `enforce`） |
| `persistent=N over_max=M (need 0)` | 有永久/超上限 legacy，进 `bounded` 会立刻登出这批人（挡 `bounded`） |
| `max_per_uid is not configured` | 首次建立 v3 floor 时缺 cap |
| `keyspace scan did not complete` | 扫描中断，通常伴随 Redis 异常 |

`blocked_by` 旁边会附 `options`，给出该阻塞的可执行选项（等待 vs 跑 migration）。

**reconciler 自身坏掉时**：`pause` 停掉它，然后用 `advance --force --yes` 人工推进。
谓词仍会执行——force 绕过的是 reconciler，不是门禁。

**停下自动推进**（按响应速度）：

1. `app session-rollout pause` —— 秒级，写持久标志，所有副本下一轮生效。**首选。**
2. `AUTO_ADVANCE=0` + 滚动 —— 分钟级，用于确认长期关闭。
3. 回滚制品 —— 仅当 reconciler 之外也出问题，且受 §8 的下界约束。

**Redis 不可达时**：副本会停止签发新 token（登录失败），但**保留在 LB 中、
已登录用户不受影响**。这是有意的——Redis 是全体副本同时失联，摘流会把一次认证降级
放大成整体不可用。

**Redis 丢了 floor**：启动日志出现 `boot=rollback-recovered`，说明这个部署曾经建立过
floor 但现在读不到了。系统会**向上取 `enforce`** 并重建记录：相关 legacy token 被拒绝，
那批用户重新登录。这是刻意的——丢失时"精确恢复"意味着继续接受一批来自不一致快照的
legacy token，而 session token 本就可丢弃。

## 8. 回滚

| 当前 floor | 可回滚到 |
|---|---|
| 未建立 | #723 或 #725 制品 |
| `v3-write` ~ `revoke` | 仅 #725 制品或更新（需能解析 v3 + generation） |
| `bounded` ~ `enforce` | 仅遵守该 floor 的兼容制品 |

**回滚到 #725 制品时，必须把 `OCTO_AUTH_SESSION_MODE` 与
`OCTO_AUTH_SESSION_REQUIRED_FLOOR` 加回 configmap**——#725 需要它们，而本 release 已
不再要求。Redis floor 记录在本 release 内保持 #725 可读（只增字段、不改名不删字段）。

**已经推进的 floor 不能撤。** 这是设计意图而非缺陷：它意味着"谓词有 bug"这一风险没有
事后补救，只能靠事前——所以谓词的每条分支都有独立测试，且首个 release 默认关闭自动推进。

migration 的回滚语义不变：可暂停续跑；不得删除 campaign/checkpoint、
不得延长已缩短的 TTL、不得删除 deny marker、不得换 campaign 复活已过期 token。

## 9. 监控

| 指标 | 关注点 |
|---|---|
| `dmwork_session_live_writers` | 与副本数不符 = 有副本失去租约或旧制品在跑 |
| `dmwork_session_writer_fence_total` | 持续增长 = 副本在拒绝新登录 |
| `dmwork_session_floor_advance_total{to,actor}` | `actor` 区分自动与人工 |
| `dmwork_session_reconcile_blocked_total{reason}` | 长期卡在非预期原因需要告警 |
| `dmwork_session_reconcile_scan_total` | 增速异常说明退避没生效 |
| `dmwork_session_rollout_boot_outcome{outcome}` | `rollback-recovered` 为 1 必须告警 |
| `dmwork_session_rollout_mode{mode}` | 全体副本应收敛到同一值（灰度副本除外） |
| `dmwork_session_undecodable_records` | 无法解码的记录数。**不阻塞任何一道 floor 门禁**（它们从来不是可用凭证，`observe` 的形态计数器只统计能解码的记录），但**没有任何东西会清掉它们**——migration 有意跳过,无 TTL 的键也不会自然过期。非 0 需要人看一眼 |

原有的 `validation_rejected_total`、`revocation_backlog`、`redis_pool_timeouts_total`
等指标保持不变。

## 10. Redis 预检

以下与 #725 一致，仍需在生产同型号/同版本/同 proxy 路径上验证：`EVALSHA` 与脚本缓存、
lease 所需的 `SET NX PX` / `PTTL` / compare-delete、`SCAN` cursor 语义、
`INFO server` 可返回稳定 `run_id`、主从切换后的行为。

**`noeviction` 仍然必须确认**：floor、generation、legacy deny marker 都是无 TTL 的安全
状态。floor 丢失现在可以自愈（§7），但 deny marker 丢失是**静默的安全回退**——
已撤销的 legacy token 会重新可用，且没有任何告警。
