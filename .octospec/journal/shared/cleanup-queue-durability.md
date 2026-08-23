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

The damage was never one stuck job. The claim takes the queue head with
`ORDER BY id`, so one poison row sits in front of **everything behind it**.

Three changes:

- **`attempts < removalCleanupMaxAttempts` in the claim `SELECT`.** The poison row
  stops being claimed, and stops head-of-lining.
- **`abandonExhaustedMemberRemovalCleanups`**, on a 1-minute schedule — the
  out-of-process complement to `releaseCleanupJob`. Without it, the claim predicate
  alone would convert an infinite retry loop into a permanently-`pending` zombie
  that nothing ever looks at again.
- **Three gauges** (pending, oldest-pending age, abandoned), refreshed on the same
  tick as the sweep so no second timer is needed.

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
