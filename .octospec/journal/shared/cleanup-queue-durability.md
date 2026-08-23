---
type: Journal
title: "Journal: cleanup-queue-durability"
description: The removal-cleanup queue could never actually give up after a hard kill, and a failed membership-cache DEL was invisible. Both failures were silent by construction; the fix makes the terminal state reachable, visible, and the cache fail closed.
tags: ["space", "isolation", "acl", "data-integrity", "observability", "testing"]
timestamp: 2026-08-22T15:20:00Z
# --- octospec extension fields ---
task: cleanup-queue-durability
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Journal: cleanup-queue-durability

## What was done

Two P1 items from #797, both of the same species: **a real isolation failure that
produces no error anywhere**.

### A. The retry budget was enforced in the one place a dying process never reaches

`attempts` was already incremented at claim time, and `releaseCleanupJob` already
abandoned a job past the ceiling. But that function only runs when a job **ran and
returned an error**. A `SIGKILL` / OOM / pod eviction mid-job never reaches it, so
the row stayed `pending` with `attempts` frozen. The claim `SELECT` had no
`attempts` predicate, so the lease would expire, the same row would be claimed
again, and the process would die again — forever.

The damage was never one stuck job. Every time the poison row is claimed it
burns one of the round's `removalCleanupBatchSize` slots, so healthy jobs that
should have been handled in the same round get squeezed out.

> **Corrected 2026-08-23.** This paragraph originally read "the claim takes the
> queue head with `ORDER BY id`, so one poison row sits in front of everything
> behind it" — contradicting the *same document* further down, which records
> removing that ordering and the EXPLAIN evidence for it. Written against an
> earlier draft and not re-read after the change landed. The conclusion is
> unaffected: burning a batch slot starves other jobs regardless of pick order,
> which is why the claim-time gate is needed either way.

Three changes:

- **`attempts < removalCleanupMaxAttempts` in the claim `SELECT`.** The poison row
  stops being claimed, and stops head-of-lining.
- **`abandonExhaustedMemberRemovalCleanups`**, on a 1-minute schedule — the
  out-of-process complement to `releaseCleanupJob`. Without it, the claim predicate
  alone would convert an infinite retry loop into a permanently-`pending` zombie
  that nothing ever looks at again.
- **Three gauges** (pending, oldest-pending age, abandoned), on their own
  5-minute schedule (`removalMetricsInterval`) rather than the sweep's 1-minute
  tick — the stats query is a full-table aggregate, and running it every minute
  buys nothing for a signal read at minute-or-longer granularity. (Corrected
  2026-08-23: this line originally said they shared the sweep's tick so no
  second timer was needed. The second timer is right there in the same commit,
  with a comment explaining why.)

The gauges are not gold-plating, and this is the part worth remembering: making a
failure terminal without making it visible just **relocates the silence**.
`abandoned` has no automatic re-drive — a removed member stays in their groups until
a human intervenes. `oldest_pending_age_seconds` is the only signal that moves
*before* the damage: backlog age rises well before jobs burn a ~70-minute budget.

### B. A failed membership-cache DEL was the one Redis failure that fails open

`InvalidateMembershipCache` was `_ = redisConn.Del(...)`.

The counter-intuitive part: a **total** Redis outage is safe here. The middleware's
`Get` misses and falls through to the database, so the removed member is refused
immediately. It is the **partial** failure — DEL alone erroring — that is dangerous:
the positive `"1"` entry lives out its full 60s TTL, `SpaceMiddleware` keeps
admitting someone who was just removed, the handler has already committed and
returned 200, and nothing is logged. Re-issuing the removal does not help either:
`removeMemberLocked` returns `ok=false`, so `afterMembersRemoved` never runs again
for that uid — the code says so in a comment at `modules/space/api.go:883`.

So the fix is not only to return and log the error, but to **actively overwrite**:
on DEL failure, write a negative entry at the shorter `negativeCacheTTL`. The
middleware then reads `"0"` and refuses. Waiting out the TTL was the old behaviour;
that is exactly the window #795 exists to close.

## Learning

**"Fail-safe on outage" is not the same as "fail-safe on error."** The membership
cache was reasoned about as if losing Redis were the risk, and losing Redis really is
safe — which is probably why the `_ =` looked acceptable. The dangerous case is the
one where the dependency is *up* and a single operation fails, because that leaves
stale state behind instead of no state. When auditing a swallowed error on a cache,
ask what survives the failure, not what is lost.

**A backstop that only runs in the process it is protecting is not a backstop.**
`releaseCleanupJob` looked like complete retry-budget enforcement and passed review as
such; it was complete only for jobs that live long enough to report their own failure.
Anything that must hold across a `SIGKILL` has to live outside the process — in the
claim predicate, or in a sweep.

## Verification

TDD, every test mutation-verified:

| mutation | test that died |
|---|---|
| drop `attempts<?` from the claim | the three claim tests only (ceiling, boundary, head-of-line) |
| drop the lease guard from the sweep | `TestSweepLeavesLiveLeaseAlone` only |

Each mutation killed exactly the intended tests and left the others green, so the
claim predicate and the sweep's lease guard are independently pinned.

`TestClaimAllowsExactlyMaxAttempts` is the off-by-one guard: because `attempts` is
counted at claim, a wrong comparison silently buys one attempt too few or too many.

`TestSweepLeavesLiveLeaseAlone` covers the boundary that makes the lease condition
non-optional: a job on its **last** attempt has `attempts == max` while still holding
its lease. Judging on `attempts` alone would steal the terminal state from a job that
might be one second from succeeding, and the executor's own write would then land
nowhere.

The sweep needs no locking, and that is provable rather than assumed: claim requires
`attempts < max`, the sweep requires `attempts >= max`. The predicates are disjoint,
so no row can be selected by both.

The DEL-failure branch is the only dangerous one and cannot be produced against a real
Redis, so the invalidation path grew an injectable seam
(`invalidateMembershipCacheIn`) following the repo's existing
`enforceKeySpaceWithChecker` pattern.

Gates: the full CI E2E lane (44 packages) against real MySQL 8 / Redis / WuKongIM
v2.2.4-20260313, `modules/space` under `-race -shuffle=on`, `golangci-lint` 0 issues,
`i18n-extract-check`, `i18n-lint`.

## Deliberately not done

- **Automatic re-drive of `abandoned` jobs.** This task makes them terminal and
  visible. Whether a reconciler should retry them is a product decision, not a
  refactor — retrying a job that has already killed a process twenty times needs a
  reason.
- **A `/metrics` endpoint.** The repo registers metrics via `promauto` into the
  global registry and exposes nothing yet (`modules/oidc`, `modules/sticker` do the
  same); mounting the endpoint is separate infrastructure work.
- The durable IM-pending outbox (#797's P0), the purge throughput item, the
  `(group_no, created_at)` index, and the join-vs-disband root cause.

---

## 更正（2026-08-23，`3f22ac0`）

上面描述的 sweep 与 metrics 是**第一版**。对抗式审查发现三处缺陷，均已修正；
记录在这里而不是改写原文，因为"当时怎么想的"和"后来错在哪"都有价值。

### 扫描会锁住整个待办队列

第一版是一条带 WHERE 的 UPDATE。`attempts` 不在任何索引里，所以 REPEATABLE READ 下
它对**整个 `status=0` 范围**加 next-key 锁 —— `LIMIT` 限制的是"改了几行"，不是
"锁了几行"。

实测复现：扫描在途时，另一个连接插入一条**全新、不冲突**的行会得到
`ERROR 1205 Lock wait timeout`。而那条 INSERT 正是移除清理的入队，发生在移除事务内部。
净效果：**队列一积压，踢人就随机失败** —— 一个此前永远成功的操作。

改成先用非锁定读选出 id、再按主键 UPDATE。同一复现下入队成功。

**教训**:`LIMIT` 在 UPDATE 上给人一种"影响面已经被限制住了"的错觉。它限制的是写，
不是锁。判断锁范围要看**可用的访问路径**，而不是看 LIMIT。

### 扫描会把跑成功的作业判死

第一版只要求"租约已过期"。但本文件自己写着大空间的级联"几十个群就能跑上几分钟"——
**跑过 10 分钟租约是预期内的，不是进程死了**。那种情况下作业还在正常推进、随后会
成功，却被扫描判成 `abandoned` 并触发"需人工介入"告警；而它自己的 finish 因为 status
已变而落空，只留下一句误导的"租约已易主"。**完成的工作被永久记成放弃。**

现在要求 `lease_until <= now - removalCleanupLease`（一整个租约周期的宽限），外加
`lease_until IS NOT NULL` 以免碰到从没被认领过的行。

### 认领查询的 `ORDER BY id` 让索引失效

EXPLAIN 实证：带 `ORDER BY id LIMIT 1` 时 `type=index / key=PRIMARY`，去掉后
`type=range / key=idx_pending`。优化器认为"按主键顺序扫、取第一条命中"更划算，于是
放弃 pending 索引。而 `abandoned` 行永不删除且集中在低 id 段，**扫描长度随部署年龄
单调增长**。FIFO 本来也不是保证——`SKIP LOCKED` 已经让多副本的实际取件顺序不确定。

### 两条本以为在保护什么的注释，是错的

- "本 PR 不引入 /metrics 端点" —— **假的**。`pkg/metrics` 早就在服务 /metrics，
  `main.go` 里启动。这句是从 `modules/oidc/metrics.go` 照抄的，那句话在 oidc 写下时
  是真的，现在不是了。**照抄注释而不核实，等于把一条过期断言复制到新地方。**
- `oldest_pending_age_seconds` 拿 `UTC_TIMESTAMP(3)` 去减 `created_at`，而后者从不由
  Go 写入、走列默认值（MySQL 会话时区）。`TZ=Asia/Shanghai` + `loc=Local` 的部署下读
  出 `-28799`，要等积压满 8 小时才转正 —— 而这个 gauge 恰恰是用来在大规模放弃**发生
  之前**报警的，在那个点上它是坏的。改成 `NOW(3)`，两边同为会话时区。

同时把指标挪到独立的 5 分钟节奏：那条查询是全表聚合（`MIN(created_at)` 无索引可用），
而它恰恰会在最需要它的那一刻——一次大规模放弃之后——变得更慢。
