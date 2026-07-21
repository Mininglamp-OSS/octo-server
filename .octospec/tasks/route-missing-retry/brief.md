---
type: Task
title: "Task: route-missing-retry"
description: Treat a route_missing at card-action dispatch as a bounded retry instead of an immediate dead-letter, so a transient config-divergence window self-heals.
tags: ["card", "dispatch", "reliability", "dlq", "testing"]
timestamp: 2026-07-20T16:30:00+08:00
# --- octospec extension fields ---
slug: route-missing-retry
upstream: none (diagnosed from a prod DLQ investigation — docs approve/deny cards never updating)
source: self
---

# Task: route-missing-retry

## Goal
When the card-action dispatcher (`internal/cardactiondispatch`) claims a queued
event whose route is absent from the current process's registry (`route_missing`),
retry it with a bounded, capped-exponential backoff instead of dead-lettering it on
the first attempt. A route miss at dispatch is transient far more often than it is a
permanent misconfiguration, and the durable Redis queue outlives that window — so the
event should ride it out and dispatch once the route returns, instead of being lost.

## Background
Production single-replica symptom: docs access-request approve/deny cards never
updated after clicking. DLQ inspection showed events (approve `1003002`, deny
`1001002/1001003`) dead-lettered with reason `route_missing`, `attempt=1`, and the
callback never invoked (no octo-docs-backend receipt, no downstream log).

Mechanism (pinned by an invariant): an event only enters the dispatch queue when its
route existed at *enqueue* time — `Registry.Resolve` must return `ResolutionCallback`,
else the event goes to bot-pull or is rejected and never reaches this queue. Within one
process, enqueue and dispatch share the same in-memory registry, so a `route_missing`
at dispatch means the registry differed between enqueue and dispatch: a restart/redeploy
changed `OCTO_CARD_ACTION_ROUTES` (e.g. came up before the route loaded) while the
durable queue carried the event across the window. The old code
(`d.nack(*lease, now, false, lease.Attempt, "route_missing")` → `retry=false` forces
`maxAttempts=lease.Attempt`) dead-lettered on the first such attempt, permanently losing
a decision whose route existed moments later — and, being non-retryable, never self-healed.

Reproduced against the real package (two registries, one with / one without the route;
event enqueued under the first, dispatched under the second) → `route_missing`, immediate
DLQ, `Deliver`/`Finalize` never called.

## Load-bearing list
- **Card-action dispatch retry/DLQ semantics** (`internal/cardactiondispatch/dispatcher.go`,
  `ProcessOne`): which nack retries vs dead-letters, and the attempt/backoff accounting.
  (No existing `.octospec/rules` tag covers dispatch-queue reliability — captured as a
  pending learning.)
- **At-least-once + idempotency contract**: retrying `route_missing` re-delivers later;
  the consumer (octo-docs-backend) is idempotent by `event_id` (receipt CAS) and the whole
  path is already at-least-once, so a delayed re-delivery is safe.
- **DLQ as an operational signal**: a *genuinely* unconfigured route must still land in the
  DLQ (visible/alertable) — now after a bounded budget rather than on attempt 1.
- `touches: testing` — new package unit tests.
- `touches: commit` — Conventional Commit.

## Out of scope
- The operational root cause (a run coming up without `OCTO_CARD_ACTION_ROUTES` loaded) —
  config/deploy hygiene, not code.
- Replaying already-dead-lettered events (manual DLQ replay; this change is forward-looking).
- Enqueue-time resolution, route-config schema, signature/secret handling, and the existing
  delivery-error retry path — all untouched.
- octo-web; and the two other diagnosed docs-card issues (applicant deny-reason omitted;
  applicant name empty) — separate work.

## Acceptance
- `route_missing` within `routeMissingMaxWindow` **defers** (no attempt consumed) — it is
  NOT nacked, so the delivery attempt budget is untouched and a returning route delivers on
  the original attempt (never trips `attempts_exhausted`).
- The bounded window is anchored on the **first observed route-miss** (a durable per-event
  marker, `RouteMissingSeenAt`), NOT on `Event.ActedAt`. An event that dwelt in the queue past
  the window before its first dispatch (long restart/outage/backlog) still **defers** on its
  first transient miss; only after waiting past `routeMissingMaxWindow` *since that first miss*
  does it dead-letter (reason preserved). This also removes the `ActedAt<=0` edge entirely — the
  marker is always a real stamp, so there is no unset-timestamp case and no permanent-defer wedge.
  (Review-driven: anchoring on `ActedAt` gave a backlogged event zero self-heal window; earlier
  cuts anchored on `ActedAt` and special-cased `ActedAt<=0`.)
- `Deliver` and `Finalize` are never invoked while the route is missing.
- The DLQ `replay` path is **non-destructive**: an entry past the CLI's resolved retention is
  refused (returns false) but NOT deleted — the running server stays the single pruning authority.
  (Review-driven: the CLI resolves retention from its own env; a shorter window could otherwise
  silently delete a server-retained entry.) A successful replay clears the first-miss marker so
  the re-queued event starts a fresh window.
- The first-miss marker has a **bounded lifecycle**: it is HDEL'd on every exit transition —
  `ackScript` (delivery), `nackScript` (both requeue and terminal dead-letter), and `replayDLQScript`
  — so the shared `route_missing_since` hash holds markers only for events currently waiting in the
  defer loop and cannot grow unbounded. (Review-driven: a whole-hash `PEXPIRE` cannot expire
  individual fields, and every miss refreshes it, so relying on TTL alone leaked a field per
  completed event under sustained route-missing traffic.)
- Green: `go test ./internal/cardactiondispatch/`; clean: `go build ./...`, `go vet`, `golangci-lint`.
- Tests: `internal/cardactiondispatch/route_missing_test.go`
  (`TestRouteMissingDefersWithoutConsumingAttempt`, `TestRouteMissingDeadLettersAfterWindow`,
  `TestRouteMissingOldActedAtDefersOnFirstMiss`, `TestRouteMissingExpired`) and
  `route_missing_queue_test.go` (`TestReplayDLQPastRetentionIsNonDestructive`,
  `TestRouteMissingSeenAtAnchorsOnFirstMiss`, `TestRouteMissingMarkerClearedOnTerminalTransitions`,
  Redis-backed).
