---
type: Task
title: "Task: im-pending-outbox"
description: A failed IM unsubscribe is permanent and invisible — the removed member keeps full group access forever. Make every unsubscribe durable via an outbox consumed by the existing removal-cleanup worker.
tags: [space, isolation, acl, data-integrity, im, testing]
timestamp: 2026-08-23T00:00:00Z
# --- octospec extension fields ---
slug: im-pending-outbox
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Task: im-pending-outbox

## Goal

When octo-server removes someone from a group it deletes the `group_member` row and then
calls `IMRemoveSubscriber`. **If that call fails, the failure is dropped on the floor and
the job is marked `done`.** The person is no longer a member in the business database but
is still a subscriber in WuKongIM.

Make every group/sub-thread IM unsubscribe durable: record it so a failure is retried to
convergence instead of lost.

## Background — measured, not inferred

Probed against `wukongim v2.2.4-20260313` (the tag pinned in `ci.yml`) with a real client
(probe source and full results committed alongside this brief under `evidence/`):

| broker state | removed member can send | receives group messages |
|---|---|---|
| normal member (baseline) | ALLOWED | YES |
| **leaked (unsubscribe failed)** | **ALLOWED** | **YES** |
| correct (unsubscribe succeeded) | BLOCKED (`SubscriberNotExist`) | NO |

**A leaked subscription is indistinguishable from full membership.** Corroborated in
source at the same tag: `internal/service/permission.go:147-154` gates sending on
`Store.ExistSubscriber`, independent of anything octo-server knows.

**Nothing repairs it.** Four independent routes are all closed:

1. Re-running the cleanup job is a no-op — its retry scope is derived from live
   `group_member` rows, which are gone after the delete. It re-runs empty and launders a
   real failure into `done`.
2. The broker never reloads — the `IMDatasource` callbacks are dead code (#797, verified).
3. The user cannot fix it — the group is absent from their client (no `group_member` row),
   so there is no "leave group" to press.
4. An admin cannot fix it — the person is absent from the member list for the same reason.

So the state is **permanent**, and invisible to everyone who could act on it.

**This is not a #795 regression.** `git show fe9ddeb -- modules/group/service.go` shows the
PR did not touch a line of it; blame puts the code at this repo's history boundary
(≥ 2026-08-04). #795 added a third caller and raised call volume by orders of magnitude —
a disband is now one job per member, each walking every group — which is what makes a
single broker restart able to leak hundreds of subscriptions at once.

## The five leak sites

`IMRemoveSubscriber` has eight call sites. Three already survive a failure; **five drop it**:

| site | user action | today | |
|---|---|---|---|
| `modules/group/service.go:1912` | kick / Space-removal cascade / bot_api kick (3 callers) | log-only | ❌ |
| `modules/group/api.go:3481` | leaving a group, cascading the leaver's bots | log-only | ❌ |
| `modules/group/api.go:3644` | **blacklist** | log-only | ❌ |
| `modules/group/thread_cleanup.go:78` | sub-thread cleanup (every kick path reaches it) | log-only | ❌ |
| `modules/botfather/command.go:636` | deleting a bot | log-only | ❌ |
| `modules/group/api.go:3272` | user leaves a group (parent channel) | returns 500 | ✅ user retries |
| `modules/group/event.go:655` | org/department removes a person | `commit(err)` → event retry | ✅ |
| `modules/group/event.go:831` | org exit | `commit(err)` → event retry | ✅ |

Blacklist is the sharpest: the whole point is "this person must not see this any more", and
on failure they keep reading everything.

The three that survive matter for design — the repo already solves this correctly two
different ways, so the fix has precedent rather than inventing a mechanism.

## Load-bearing list

- `space` / `isolation` / `acl` — this is the enforcement point of group membership at the
  transport layer. Measured above: failing it grants full read **and** write.
- **Only two of the five sites have a transaction.** `RemoveGroupMembers` commits at
  `service.go:1899` with the IM call after; `groupExit`'s bot cascade commits at
  `api.go:3428`. `thread_cleanup.go`, `blacklist`, and the botfather delete path have **no
  transaction at all**. The design must not require wrapping unrelated code in transactions.
- Retry safety is **measured**: `subscriber_remove` returns 200 for an already-removed
  subscriber, a non-existent user, and a non-existent channel. Retry is a safe no-op.
- Sub-thread channels (`{groupNo}____{shortID}`, `ChannelTypeCommunityTopic`) are just
  another channel id — one uniform record shape covers parent groups and sub-threads.
- The existing `space_member_removal_cleanup` worker already provides claim/lease/backoff/
  abandon/purge/metrics. This task should **consume that machinery, not clone it**.
- `test` — TDD; the failure is invisible without a test that forces the IM call to fail.

## Design sketch (for confirmation before code)

A table of pending IM unsubscribes, drained by a step registered on the existing worker:

```
im_pending_subscriber_removal(
  id, channel_id, channel_type, uid,
  status, attempts, next_attempt_at, lease_owner, lease_until, last_error,
  created_at, finished_at)
```

Flow at each site: **record → attempt → delete the record on success**; on failure leave it
for the worker.

Two decisions worth a maintainer's opinion, both called out rather than assumed:

1. **Delete on success rather than marking `done`.** A disband of a 1,000-member Space with
   50 groups produces ~50k unsubscribes. Marking them `done` would put 50k rows through the
   retention purge for no benefit — the row's only purpose is to survive a failure. Deleting
   on success keeps the table sized to *in-flight + broken* rather than *all traffic ever*.
   Cost: one extra DELETE on the happy path.
2. **Record-always vs record-on-failure.** Record-always (the true outbox, written inside the
   transaction where one exists) also covers "process died after commit, before the IM call".
   Record-on-failure is nearly free but loses a crash in the microsecond between the IM error
   and the record write. **Recommendation: record-always at the two transactional sites**
   (that crash window is exactly the failure class being fixed), **record-on-failure at the
   three without a transaction**, since making them transactional is a much larger change
   than this task should carry.

## Out of scope

- Subscriber **add** failures (the join path). Same shape, different blast radius; separate task.
- The three sites that already survive failure — leave them as they are.
- Making `blacklist` / `thread_cleanup` / the botfather delete path transactional.
- **#800 ①** (owner removed → their bots keep `space_member.status=1`). It wants this same
  outbox for its group cleanup, so the mechanism must not assume a human uid — but the
  Space-membership half of #800 stays in #800.
- Every remaining #797 P2.

## Acceptance

New tests, each failing before the change and passing after:

1. **A failed unsubscribe leaves a pending record** rather than being logged and forgotten —
   with the IM call stubbed to error, at each of the five sites.
2. **The worker retries it to success**, and the record is gone afterwards.
3. **Retry does not depend on `group_member`** — the record must still drain after the
   member row is deleted. This is the property that makes today's job re-run useless, so it
   is the core regression test.
4. **The happy path leaves no row behind** (delete-on-success).
5. **Sub-thread channels drain through the same path** as parent groups.
6. A permanently failing unsubscribe reaches `abandoned` and surfaces on the existing gauges.

Must stay green: the three sites that already survive failure keep their current behaviour
(`api.go:3272` still returns 500; `event.go:655`/`:831` still `commit(err)`).

Gates: full CI E2E lane (44 packages), `-race -shuffle=on` on `modules/space` and
`modules/group`, `golangci-lint`, `make i18n-extract-check`, `make i18n-lint`.

---

# ⛔ 第一次实现已撤回（2026-08-23）

`eb74529` 实现、`78e46d3` 撤回。**问题陈述与实测证据依然成立，设计不成立。**

五路对抗式审查独立命中同一批缺陷，其中一路在真实 MySQL 8.0.46 上复现。重做前必须
把下面这些当作硬性需求，而不是"注意事项"。

## 第一次实现引入的、必须避免的缺陷

### D1 —— 唯一键 + `INSERT IGNORE` = 永久墓碑（三路独立命中）

`UNIQUE(channel_id, channel_type, uid)` + 无 purge ⇒ 某个目标一旦进入 `abandoned`，
此后**每一次**新入队都是静默 no-op 且返回 `nil`，调用方以为已持久化、日志还打
「已入队重试」。**把静默泄漏升级成会撒谎的泄漏。**

实测：两条 abandoned 行存在时，`INSERT IGNORE` 报成功影响 0 行，claim 可见行数 = 0。

**根因是把去重当成了需求。** worker 与 `AttemptIMUnsubscribe` 都不入队——入队只在
每次移除事件发生一次，所以**根本不存在重复入队的路径**。唯一键在解决一个不存在的
问题，代价是两个真实缺陷。

→ **重做要求：不要唯一键，用普通 INSERT。** 每次移除事件一行，与
`space_member_removal_cleanup` 同款（那张表刻意没有唯一键，见其迁移注释）。

### D2 —— 开火前不重新校验成员身份（两路命中）

成员移除 worker 有两道 rejoin 门，IM outbox 却无条件开火。拉黑 →（入队失败留行）→
解除拉黑（`IMAddSubscriber` 恢复）→ worker 几十秒后排掉陈旧待办 → **把一个
`group_member` 里活跃、成员列表里可见的合法成员永久切断收发**。

反向泄漏比正向更难发现：受害者**在**成员列表里，没人会怀疑传输层。

打脸之处：`20260821000001` 那张表的迁移注释第 20-23 行**已经写明**为什么刻意不对
目标建唯一键、以及 worker 为什么必须重新校验成员身份。新表两条都违反了。

→ **重做要求：开火前重新校验。** 建议复用 `RegisterMemberRemovalCleanupStep` 的
反向注册成例，由 `modules/group` 注册一个"该 uid 是否仍是该频道成员"的校验器，
避免 `modules/space` 反向 import。或者在所有 `IMAddSubscriber` 路径上取消待办
（七处，更易漏）。

### D3 —— fail-closed 入队 × sweep 锁扫描 = 踢人随机失败（实测复现）

sweep 的单条带 WHERE 的 UPDATE 因 `attempts` 无索引，会对整个 `status=0` 范围加
next-key 锁；而入队是 fail-closed、就在移除事务里。实测两张表都复现
`ERROR 1205 Lock wait timeout`。

sweep 侧已在 `3f22ac0` 修复（先只读选 id、再按主键更新）。但重做时要重新评估：
**入队失败是否真该让整个移除失败。**

### D4 —— 泄漏点是 7 个不是 5 个

`event.go:655` / `:831` 被归类为"有事件重试兜底"是**错的**。
`modules/base/event/api.go:61-62` 本仓库注释已写明：

> 监听方报错只会把事件行置为 Fail，而 QueryAllWait 只捞 Wait，**Fail 行永不重投**

而且失败时 `return`，后面的群**连尝试都没有**，成员行却已在同一个已提交事务里删除。
触发源是 HR 系统的「员工离职」事件。唯一真正安全的是 `groupExit`（先退订、失败返 500）。

### D5 —— 引用了不适用的批量上限，且丢掉了分块

代码注释写「批量踢人上限是 200」。`managerMaxBatchUIDs` **只在 modules/space 生效**；
`modules/group` 的 `blacklist` 与 `memberRemove` **没有任何上限**。实测 10000 uid →
一条 840KB 语句、128ms，在持有 `group_member` 锁的事务里执行。

隔壁 `enqueueMemberRemovalCleanupBatchTx` **按 200 分块**，正是为此。

→ **重做要求：分块，并且不要引用不适用的上限。**

### D6 —— 写放大

`thread_cleanup` 每个子区一次 INSERT + 一次 IM + 一次 DELETE，而
`queryThreadShortIDsForCleanup` 返回**所有**子区（不论用户是否加入过）。
1000 人 × 50 群 × 30 子区的解散 = 约 155 万行、310 万条语句、约 390MB binlog。

→ **重做要求：子区批量入队，一次而不是每子区一次。**

### D7 —— 认领查询的 `ORDER BY id` 陷阱

`ORDER BY id LIMIT 1` 让优化器放弃 pending 索引改走 PRIMARY（EXPLAIN 实证：
`type=range/key=idx_pending` → `type=index/key=PRIMARY`）。终态行永不删除且集中在
低 id 段，扫描长度随部署年龄单调增长。旧表已在 `3f22ac0` 修复；新表不要重蹈。

## 测试层面必须补上的

- **`channel_type` 从未被断言**。变异：worker 硬编码 `ChannelType: 2` → 8 条测试全过。
- **子区测试用错了类型**：写的是 `3`（`ChannelTypeCustomerService`），子区是
  `ChannelTypeCommunityTopic`（`= 5`）。所谓"子区走同一条路"测的是客服频道。
- **事务原子性未被测试**：把入队移出事务 → 测试仍然通过。而"与成员删除同事务"
  正是这个设计相对更便宜方案的**唯一**卖点。
- `releaseIMPending` / `abandonExhaustedIMPending` / `sweepExhaustedIMPending` 零覆盖。
- 新表零 gauge —— 而它的 abandoned 语义（"被移除者仍能收发"）比旧表更危险。
- **测试二进制里挂了活的 10s worker**：后台 goroutine 会改测试断言的同一张表。
  6400 次压测未复现，但窗口是真的。

## 方法论教训（写给重做的人，也写给我自己）

第一次实现声称"每条测试都做了变异验证"。**实际是挑了自己测试能抓到的变异。**
源码守卫用 `<> 0` 做变异——而测试正好 grep 这个字符串。对抗性地试，它
**该绿时红、该红时绿**。

→ 变异必须由**不知道测试怎么写的人（或视角）**来选，或者直接选"最像真实回归的
那个改动"。自己给自己出题，出的永远是自己会做的题。
