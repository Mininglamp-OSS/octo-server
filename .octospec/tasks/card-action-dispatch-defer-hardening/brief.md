---
type: Task
title: "Task: card-action-dispatch-defer-hardening"
description: Close two defensive gaps in the card-action dispatch queue's defer path — the claim→Defer attempt-increment leak on reclaim, and the missing LiveTTL lower bound relative to the defer interval.
tags: ["card", "dispatch", "reliability", "queue", "testing"]
timestamp: 2026-07-24T13:15:45+00:00
# --- octospec extension fields ---
slug: card-action-dispatch-defer-hardening
upstream: Mininglamp-OSS/octo-server#623, Mininglamp-OSS/octo-server#624
source: self
---

# Task: card-action-dispatch-defer-hardening

> Follow-up hardening for the `route-missing-retry` task (PR #621). Both items were
> raised in that PR's review (yujiawei, P2/P3) and filed as non-blocking issues so they
> would not be lost.

## Goal
Two independent reliability fixes in `internal/cardactiondispatch/queue.go`, both about
the queue's *defer* path (the route-missing self-heal loop PR #621 added):

1. **#623 (P2) — attempt-increment leak on reclaim.** `claimScript` increments the
   per-event attempt on every claim; only a completed `Defer` restores it, so a
   claim→Defer re-check cycle is net-zero. If the worker crashes / Redis errors / the
   lease expires in the window *between* claim and `Defer`, `ReclaimExpired` requeued the
   event without undoing the `+1`. Across many cycles the counter creeps up until the
   event reads `attempt > MaxAttempts` and dead-letters as `attempts_exhausted` on a route
   it never got to try. `reclaimScript` now refunds the increment (floored at 0) for a
   reclaimed lease that still carries a `route_missing_since` marker — the durable signal
   that the event is in the defer loop — making a reclaimed defer cycle net-zero too.

2. **#624 (P3) — missing LiveTTL floor.** `NewRedisQueue` validated only `LiveTTL > 0`.
   A deferred event is rescheduled up to `routeMissingDeferInterval` (5s) ahead and every
   queue op `PEXPIRE`s its ready/payload keys to `LiveTTL`; if `LiveTTL` did not comfortably
   exceed that interval a deferred key could expire before the event became claimable,
   silently dropping it. Add a `minLiveTTL` floor (`4 × routeMissingDeferInterval`) so a
   pathological config fails fast at construction (server boot) instead of at runtime.

## Background
Both are follow-ups from the PR #621 review at head `d0b0a879`:
- #623: the claim-then-transition shape *predates* #621 (the capacity-defer path shares
  it); #621 only added more re-check cycles, marginally widening exposure. Worst case is a
  **recoverable** DLQ entry (reason preserved, replayable), not silent loss, and the path is
  already at-least-once with an idempotent consumer — hence P2, not P0/P1.
- #624: `LiveTTL` is `Robot.MessageExpire`, which defaults to **7 days**. A sub-5-second
  value would break card messaging broadly long before this dispatch path mattered, so this
  is defensive hardening (a missing invariant), essentially zero live risk — hence P3.

## Load-bearing list
- **Reclaim requeue semantics** (`reclaimScript` in `queue.go`): what a reclaimed lease
  does to the attempts hash. Previously it touched only `ready`/`leased`/`tokens`; it now
  conditionally `HINCRBY -1` on `attempts`, gated on the `route_missing_since` marker.
- **Attempt accounting as the retry bound** (`claimScript` `+1`, `deferScript` `-1`,
  `nackScript`'s `attempt >= maxAttempts` DLQ gate, dispatcher's `lease.Attempt >
  route.MaxAttempts` pre-delivery gate): the refund must NOT weaken the bound on real
  delivery retries. Delivery leases carry no marker, so their accounting is untouched —
  pinned by the existing `TestRedisQueueLeaseTokenRetryDLQAndReplay` (markerless reclaim →
  attempt 2).
- **`route_missing_since` marker lifecycle** (set by `RouteMissingSeenAt`, HDEL'd on
  `ack`/terminal `nack`/`replay`): reused here as the "in the defer loop" discriminator.
- **Dispatch ordering around the marker clear** (`dispatcher.go`, review-driven): once
  `Route()` resolves, `ClearRouteMissing` (token-protected) runs BEFORE the `MaxAttempts`
  gate and `Deliver`, so a delivery lease is markerless and its reclaim is not refunded. The
  route-absent branch returns earlier and never reaches it, keeping the marker the defer loop
  needs. The `!cleared` result means the lease was reclaimed + re-leased, so the dispatcher
  bails with `ack_lost` before `Deliver`/`Finalize` rather than doing a delivery whose terminal
  `Ack` would fail — mirroring the existing `!deferred` lease-loss handling.
- **`NewRedisQueue` construction contract** (boot-time; `main.go` returns its error): the
  new floor rejects only a pathological `LiveTTL`; all real/test configs (7d, 1h, 10m) clear it.
- `touches: testing` — Redis-backed package unit tests.
- `touches: commit` — Conventional Commits, English, `Fixes #62x` footer.

## Out of scope
- The **capacity-defer** path's identical claim→Defer shape (`dispatcher.go`): pre-existing
  and NOT covered by this change. The marker-gated refund cannot reach it — `ClearRouteMissing`
  runs before the capacity check, so a capacity-deferred lease is always markerless — and a
  crash or lease expiry between claim and that `Defer` still leaks the `+1`. Note the window is
  no longer the purely in-memory channel check it was when this task was drafted: the
  `ClearRouteMissing` round-trip now sits between claim and the capacity `Defer`. Exposure is
  still bounded (worst case a recoverable `attempts_exhausted` DLQ entry, replayable), but the
  honest framing is deferred-to-follow-up, not negligible-by-construction. Closing it properly
  means moving the speculative `+1` off `claim` — see the note below.
- Relocating the attempt increment from `claimScript` to just before `Deliver`, which would
  make BOTH defer paths net-zero without a discriminator and retire the marker's accounting
  role entirely (subsuming this refund, the capacity-path gap above, and #680). It is the
  cleaner shape but it moves a load-bearing off-by-one — the `lease.Attempt > MaxAttempts`
  pre-delivery gate, `nackScript`'s `attempt >= maxAttempts` DLQ gate, and the replay reset all
  shift together — so it needs its own review round rather than riding along with this P2.
- Folding `RouteMissingSeenAt` + `Defer` into one atomic Lua transition (the other option
  the issue lists): it does not improve leak coverage over the marker-gated reclaim refund
  (the `+1` is at *claim*, before either call), so it was not pursued for this P2.
- A cross-config `LiveTTL` guard covering route-level `MaxBackoff` (up to 10m), the
  dispatcher's `PollInterval` (no upper bound) and `LeaseDuration` (30s default). Each can
  exceed `minLiveTTL`, so the floor does not make deferred-key expiry impossible in general —
  only for the route-missing defer interval it is derived from. The values live on
  `DispatcherConfig`, not `QueueConfig`, so the check belongs at dispatcher construction; the
  `minLiveTTL` doc comment and the constructor error now state only what they enforce.
- The delivery-error retry path, enqueue resolution, and the DLQ-retention/replay logic —
  all untouched. `dispatcher.go` IS touched (see Load-bearing list) for the review-driven
  marker clear and the pre-delivery lease-loss bail.

## Acceptance
- #623: a reclaimed lease that carries a `route_missing_since` marker has its attempt
  refunded (floored at 0), so its next claim returns to the SAME attempt (net-zero across a
  crash), not a higher one. A reclaimed lease WITHOUT a marker keeps its attempt (delivery
  retries stay bounded). Both pinned by
  `TestReclaimRefundsRouteMissingDeferAttemptLeak` (two subtests) and the unchanged
  `TestRedisQueueLeaseTokenRetryDLQAndReplay` (markerless reclaim still → attempt 2).
- #623 (review-driven, #662): once the route RESOLVES at dispatch, the marker is dropped
  (token-protected `ClearRouteMissing`) BEFORE delivery, so a hard crash during the ensuing
  delivery is reclaimed as a real attempt (bound preserved), not refunded as a defer. Pinned by
  `TestClearRouteMissingRestoresDeliveryReclaimBound` (Redis: token-mismatch cannot clear;
  cleared marker → reclaim advances to attempt 2) and
  `TestRoutePresentClearsRouteMissingMarkerBeforeDelivery` (dispatcher wiring).
- #624: `NewRedisQueue` rejects `LiveTTL` at/below the floor with a descriptive error and
  accepts values at/above it. Pinned by `TestNewRedisQueueRejectsLiveTTLBelowDeferFloor`
  (rejects `routeMissingDeferInterval` and `minLiveTTL-1ns`; accepts `minLiveTTL` and `1h`).
- Green: `go test -tags integration ./internal/cardactiondispatch/` (unit + the 3 e2e
  orchestration tests, run against live MySQL+Redis+WuKongIM); clean: `go build ./...`, `go vet`.
- Tests: `internal/cardactiondispatch/route_missing_queue_test.go`
  (`TestReclaimRefundsRouteMissingDeferAttemptLeak`, Redis-backed) and `coverage_test.go`
  (`TestNewRedisQueueRejectsLiveTTLBelowDeferFloor`).
