---
type: Task
title: "Task: cleanup-queue-durability"
description: Make the removal-cleanup queue's terminal state real and its failures visible — enforce the attempts budget at claim time, sweep over-budget jobs to abandoned out of process, add gauges, and stop swallowing the membership-cache DEL error.
tags: [space, isolation, acl, data-integrity, observability, testing]
timestamp: 2026-08-22T00:00:00Z
# --- octospec extension fields ---
slug: cleanup-queue-durability
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Task: cleanup-queue-durability

## Goal

Two P1 items from #797, both of which make a removed member's cleanup fail **silently**:

**A. The retry budget is not enforced where it matters, and `abandoned` is unreachable
after a hard kill.** `attempts` is incremented at claim time
(`db_member_removal.go:161`), but the claim `SELECT` (`:140-152`) has no
`attempts < removalCleanupMaxAttempts` predicate, and the only transition to
`abandoned` lives in `releaseCleanupJob` (`member_removal.go:423-431`) — a path
reached only when a job *ran and returned an error*. A `SIGKILL` / OOM / pod
eviction mid-job never reaches it. So a job that reliably kills the process is
re-claimed forever with `status` stuck `pending`, and because the claim orders
`BY id` it head-of-lines every job behind it.

**B. A transient Redis `DEL` failure leaves a removed member admitted for 60s with
nothing logged.** `InvalidateMembershipCache` (`pkg/space/middleware.go:59-61`) is
`_ = redisConn.Del(...)`. A *full* Redis outage fails safe (the middleware's `Get`
misses and falls through to the DB), but a **DEL-only** failure does not: the
positive `"1"` entry survives its full 60s TTL and `SpaceMiddleware` keeps letting
the removed member in. The handler has already committed and returned 200, and
re-issuing the removal does not help — `removeMemberLocked` returns `ok=false`, so
`afterMembersRemoved` is never reached for that uid (`api.go:883` says so in a
comment). Closing exactly this 60s window is #795's stated goal.

## Background

Both are the "silent" class: nothing crashes, no request fails, and the operator
sees a job marked `done` (A, after the sweep lands) or a 200 (B). The dangerous
shape of A is not one stuck job — an IM or DB degradation lasting past the ~70-minute
retry budget turns **every job pending in that window** into `abandoned` at once,
and nothing re-drives them.

Which is why observability is in scope rather than deferred: making the terminal
state reachable without making it visible just moves the silence.

The repo already uses Prometheus via `promauto` into the global default registry
(`modules/oidc/metrics.go`, `modules/sticker/metrics.go`); no `/metrics` endpoint is
mounted yet — that is a separate infrastructure concern and stays out of scope here.

## Load-bearing list

- `space` / `isolation` / `acl` — both items decide how long a removed member keeps
  access. A silently-abandoned job leaves them in the Space's groups and IM
  subscriptions indefinitely; a surviving positive cache entry keeps them past
  `SpaceMiddleware` for up to 60s.
- The claim path is concurrent across replicas (`FOR UPDATE SKIP LOCKED`, per-claim
  owner token). A new predicate must not break the claim's atomicity or let two
  replicas both believe they own a job.
- The abandon sweep must not abandon a job that is **currently executing** its last
  attempt — it has to respect the lease, not just the attempts count.
- `attempts` semantics are already load-bearing and documented at
  `db_member_removal.go:154-159`: counted at claim so a process-killing job
  self-converges. The new predicate must preserve exactly `removalCleanupMaxAttempts`
  real attempts, not one fewer or one more.
- `InvalidateMembershipCache` is exported from `pkg/space`; signature change touches
  its callers (1 production, 2 test).
- `test` — TDD; both failure modes are invisible without a test that asserts the
  silence is gone.

## Out of scope

- The **durable IM-pending outbox** (#797's own highest-value item, P0). Separate
  task, needs its own brief, and shares a design with #800 ①.
- Mounting a `/metrics` HTTP endpoint — the repo deliberately registers metrics
  without exposing them yet.
- The purge throughput item, the `(group_no, created_at)` index and lock ordering,
  the join-vs-disband root cause, and every other #797 batch.
- Re-driving `abandoned` jobs automatically. This task makes them terminal and
  *visible*; deciding whether a reconciler should retry them is a product call.
- Changing `removalCleanupMaxAttempts` or the backoff curve.

## Acceptance

New tests, each failing before the change and passing after:

1. **A job at the attempts ceiling is not claimed.** Seed a `pending` row with
   `attempts = removalCleanupMaxAttempts`; `claimMemberRemovalCleanup` returns nil.
2. **It does not head-of-line the queue.** With an over-budget row at a lower `id`
   and a healthy row behind it, the healthy row is claimed and runs.
3. **Exactly `removalCleanupMaxAttempts` real attempts happen** — a job one below the
   ceiling is still claimable; a job at it is not. (Off-by-one guard.)
4. **The sweep abandons an over-budget, lease-expired pending row** left behind by a
   simulated hard kill, and records `attempts` and a reason.
5. **The sweep does NOT abandon a row whose lease is still held** — a job on its last
   attempt that is currently executing must be left alone.
6. **A failed Redis `DEL` is returned and logged, and leaves no stale positive entry:**
   after `afterMembersRemoved` with a DEL that errors, `SpaceMiddleware`'s cache must
   not report the removed member as a member.
7. Gauges report pending count, oldest-pending age, and abandoned count off a seeded
   table.

Must stay green:

- `TestCleanupWorkerAbandonsAfterMaxAttempts`, `TestCleanupWorkerAbandonRecordsAttempts`,
  `TestClaimIncrementsAttempts`, `TestClaimCleanupRespectsLeaseAndSchedule`,
  `TestPurgeKeepsAbandonedJobs`, `TestRemoveMembersInvalidatesMembershipCache`
- the full CI E2E lane (44 packages)

Gates: `golangci-lint run ./...`, `make i18n-extract-check`, `make i18n-lint`,
`-race -shuffle=on` on `modules/space`.
