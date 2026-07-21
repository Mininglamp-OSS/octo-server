---
type: Journal
title: "Journal: route-missing-retry"
description: Card-action dispatch now retries a route_missing (bounded) instead of dead-lettering it on the first attempt, so a config-divergence window across a restart self-heals.
tags: ["card", "dispatch", "reliability", "dlq", "testing"]
timestamp: 2026-07-20T16:30:00+08:00
# --- octospec extension fields ---
task: route-missing-retry
source: self
---

# Journal: route-missing-retry

## What was done

`internal/cardactiondispatch/dispatcher.go` — in `ProcessOne`, the `route_missing`
branch (route not found in the registry for `{sender_uid, owner, action_type}`) no
longer dead-letters on the first attempt. It now **defers** the event:

- Old: `d.nack(*lease, now, false, lease.Attempt, "route_missing")` — `retry=false`
  forces `maxAttempts = lease.Attempt`, so `queue.Nack` dead-letters immediately.
- New: within `routeMissingMaxWindow` (15m of `Event.ActedAt`), `d.queue.Defer(eventID,
  token, now+routeMissingDeferInterval)` — re-checks every `routeMissingDeferInterval`
  (5s) **without consuming an attempt**, mirroring the `max_in_flight` capacity-defer path.
  Past the window it dead-letters via `d.nack(..., false, ...)` (`reason=route_missing`).
- Added `routeMissingMaxWindow`, `routeMissingDeferInterval`, and `routeMissingExpired`.

**Why defer, not a bounded nack (the first cut):** the first implementation nacked with
a capped-exponential backoff up to a `routeMissingMaxAttempts=10` budget. An `xhigh`
code review caught that this shares the SAME `lease.Attempt` counter as delivery: after
more than `route.MaxAttempts` (default 5; configs use 3) route-missing retries, the event
would hit the `attempts_exhausted` gate the moment its route returned — dead-lettered
exactly when it became deliverable, and as `attempts_exhausted` not `route_missing`. So
the self-heal window was really `route.MaxAttempts` (~seconds), not the advertised
minutes. Deferring consumes no attempt, so the event delivers on its original budget
whenever the route returns — the fix the change was meant to be.

`internal/cardactiondispatch/route_missing_test.go` (new) — pins: a fresh route-missing event
**defers** and is NOT nacked (the guard against the attempt-budget bug); an event past
`routeMissingMaxWindow` dead-letters immediately with the reason preserved; `Deliver`/`Finalize`
never run while the route is missing; and `routeMissingExpired` covers the window boundary plus
the unset/negative `ActedAt` guard.

## Why (root cause)

Single-replica prod symptom: docs approve/deny cards never updated. DLQ held events with
reason `route_missing`, `attempt=1`, callback never invoked (no octo-docs-backend receipt,
no downstream log). An event only enters this queue when its route existed at *enqueue* time
(`Registry.Resolve` → `ResolutionCallback`); enqueue and dispatch share one in-process
registry — so `route_missing` at dispatch means the registry changed between the two, i.e.
a restart/redeploy came up before `OCTO_CARD_ACTION_ROUTES` had the route, while the durable
Redis queue carried the event across that window. Immediate, non-retryable DLQ turned that
transient window into permanent loss (and no self-heal), which read at the UI as "approve
has no response."

## Verification

- Reproduced the pre-fix behavior against the real package (two registries — with/without
  the route; enqueue under one, dispatch under the other) → `route_missing`, immediate DLQ,
  `Deliver`/`Finalize` never called.
- Tests (`route_missing_test.go`): `TestRouteMissingDefersWithoutConsumingAttempt` (fresh
  event defers, is NOT nacked — the guard for the attempt-budget bug), 
  `TestRouteMissingDeadLettersAfterWindow` (past the window → immediate DLQ, reason preserved),
  `TestRouteMissingExpired` (window boundary + unset-timestamp guard). The defer test fails
  against the nack-based first cut, so it pins the fix.
- `go test ./internal/cardactiondispatch/` green; `go build ./...` clean; `go vet` +
  `golangci-lint` clean.

## Scope / residual

- Forward-looking only: already-dead-lettered events are NOT resurrected by this change —
  they need a manual DLQ replay.
- Operational root cause (a run without `OCTO_CARD_ACTION_ROUTES` loaded) is config/deploy
  hygiene, out of scope for this code change.
- A genuinely unconfigured route still DLQs, just after `routeMissingMaxWindow` (15m) rather
  than on attempt 1 — deliberate trade of slightly-later DLQ visibility for self-healing a
  transient window.

## Learning

Staged in `.octospec/learnings/pending/durable-queue-registry-divergence.md`: a durable,
process-shared work queue combined with a per-process in-memory config table (built once at
startup from env) can dead-letter valid work across a restart/rollout that changes the config;
"transient config absence" should be a bounded retry, not a first-attempt DLQ.

## Follow-up: DLQ retention is now configurable (default 30d, opt into shorter)

Same branch, separate concern. The DLQ retention was a hardcoded `30 * 24 * time.Hour`
duplicated in `main.go` and `tools/card-action-dlq/main.go`. Replaced with a shared resolver
`cardactiondispatch.DLQRetentionFromEnv(os.Getenv)` (in `config.go`) reading
`OCTO_CARD_ACTION_DLQ_RETENTION_DAYS` (whole days, 1–365; empty/invalid → `DefaultDLQRetention`).
Both binaries share the one resolver so the CODE value cannot drift. `DefaultDLQRetention` stays
**30 days** — the value the code already shipped with — so an upgrade that does not set the
override keeps the existing recovery window and never silently prunes older DLQ entries on first
deploy. Opt into a shorter window (e.g. `7`) per deployment via the env var. Test:
`dlq_retention_test.go` (`TestDLQRetentionFromEnv`). Doc updated: `docs/card-action-callback-dispatch.md`.

## Review round (PR #621, 4 reviewers)

The PR drew four independent reviews (lml2468 APPROVE, then Jerry-Xin, mochashanyao/Octo-Q, and
yujiawei CHANGES_REQUESTED). Three blocking corrections, all folded into this branch as a
follow-up commit; the metric-noise nit was intentionally left as-is.

1. **route_missing with `ActedAt<=0` deferred forever** (Jerry-Xin Critical, yujiawei P2).
   `routeMissingExpired` returned "not-expired" for a non-positive `ActedAt`, so a legacy/malformed
   event with a missing route would re-defer every 5s indefinitely — never delivered, never
   dead-lettered — silently breaking the bounded-window guarantee. The wait is bounded by
   elapsed-since-`ActedAt`, and there is nothing to measure against when it is unset. **Fix:**
   non-positive `ActedAt` is now EXPIRED → immediate dead-letter (`reason=route_missing`, visible
   and replayable). Chose the runtime guard over an `Enqueue`-time guard because it also protects
   events already sitting in the durable queue and avoids churning many test event builders.
   New test: `TestRouteMissingZeroActedAtDeadLetters`; `TestRouteMissingExpired` flipped.

2. **DLQ default 30→7d silently prunes on deploy** (Octo-Q P1, yujiawei P1 follow-up). The
   configurability change had lowered the default to 7d; yujiawei traced that the *server itself*
   (`refreshDepthMetrics → Depths → pruneDLQScript`, ~every 250ms) would irreversibly prune
   8–30-day-old DLQ entries on the first event after deploy, no operator action needed. **Fix:**
   restored `DefaultDLQRetention = 30d`; 7d (or any 1–365) remains available via the env var. The
   user chose this over shipping 7d-with-a-warning.

3. **CLI `depth` deletes on a read** (yujiawei P1). The read-only `depth` command called
   `Depths()`, which prunes using retention resolved from the *CLI's* env — so inspecting the DLQ
   from a shell without the server's env var could delete recoverable entries. **Fix:** new
   `RedisQueue.DepthsNoPrune()` (ZCard only); `depth` uses it. The running server is now the sole
   pruning authority. `main.go` also logs the raw override value so an invalid-→-fallback is visible.

**Left as-is (documented, not a defect):** `observeError(route_missing)` fires once per 5s
re-check while an event waits. That is intentional and documented in the runbook's alerting
section; folding it to fire only on dead-letter would drop the "route currently missing" signal.
