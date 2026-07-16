---
type: Journal
title: "space-new-user-welcome-message: at-most-once Space welcome DM via notify ledger"
description: Deliver one configurable welcome DM from the notification bot on a human user's first join to a designated Space, backed by a persistent delivery ledger + reconciler + send worker.
tags: [notify, onboarding, space, isolation, i18n, system-setting, migration, idempotency, testing]
timestamp: 2026-07-16T00:00:00Z
---

# space-new-user-welcome-message

Branch `space-new-user-welcome-message`. Minimal implementation of the main
chain "first-join human → exactly one ledger row → member re-check before send".
Send semantics are at-most-once; transport-ambiguous outcomes become `unknown`
and are never auto-retried.

## What was done

1. **Delivery ledger** — new `octo_space_welcome_delivery` table (migration in
   `modules/notify/sql/`), the permanent dedupe source of truth. Status machine
   pending/claimed/dispatching/sent/failed/skipped/unknown. `notify/1module.go`
   gains `//go:embed sql` + `SQLDir` (it had none before — without it the
   migration would never run).
2. **Config** — five `system_setting` keys under category `onboarding`.
   `modules/common` gains `SpaceWelcomeConfig()` (reads all five keys from ONE
   snapshot in a single atomic access so a caller cannot straddle a background
   `Reload()`), `ValidateSpaceWelcomeCombination`, and prospective composite
   validation on the manager write path (validate `merge(current snapshot,
   incoming items)`, not the pre-write snapshot). New i18n error code
   `err.server.common.space_welcome_config_invalid`.
3. **notify service** — `space_welcome*.go`: event enqueue (extends the
   `SpaceMemberJoin` listener; never blocks/rolls back the join), a 60s
   reconciler (catches dropped events / restarts / the manager `addMembers`
   path), and a send worker (idle 5s poll / active drain / per-wake cap 20).
   Claim via `SELECT ... FOR UPDATE SKIP LOCKED`; every post-claim UPDATE is a
   CAS guarded by `status + claim_owner`. `attempts` grows ONLY on a definitive
   pre-IM failure; backoff {5s,30s,120s}, 4th failure → terminal failed. Any
   failure after the dispatching transition → `unknown` (no retry).
4. **notify-local sender** — a context-aware HTTP sender (`http.Client{Timeout:
   15s}` + `NewRequestWithContext`, explicit `Content-Type: application/json` +
   manager token) instead of octo-lib's `SendMessageWithResult` (which sets no
   timeout and takes no context). octo-lib is unmodified. 15s < 30s lease.
5. **Observability** — kept minimal per product steer: in-process atomic
   counters (also used by tests to assert enqueue-vs-dedup and send outcomes) +
   structured logs. No Prometheus wiring yet; promoting the counters later is a
   drop-in.

## Structural learnings / gotchas

- **DB writes NOW() in the deployment-local wall clock** (this deployment runs a
  uniform `+08:00`; overseas mounts a TZ env var to run UTC). Comparing a UTC
  `active_from` against `space_member.created_at` (written by `NOW()`) directly
  is off by the session offset. Fix mirrors the existing repo convention in
  `modules/opanalytics/etl_db.go`: `UNIX_TIMESTAMP(sm.created_at) >=
  activeFrom.Unix()` — epoch-to-epoch, TZ-agnostic, no CONVERT_TZ / no DSN
  `loc` dependency, valid because writer and reader connections share one global
  session zone. The ledger's own timestamps stay app-supplied UTC and only ever
  compare against each other, so they are self-consistent regardless.
- **`pkg/space.SystemBots` contains `notification`** (the sender identity). The
  human-filter `IsSystemBot` check is scoped strictly to the RECIPIENT uid; the
  send path / sender identity never runs through it, or the feature would
  self-exclude.
- **Prospective validation direction matters**: validating the current snapshot
  alone gets both directions wrong (accepts a patch that breaks the composite,
  rejects a patch that repairs it). Always validate the merged five-tuple.
- **A `package common` internal test cannot blank-import `modules/space`**
  (`modules/space` imports `modules/common` → cycle), so the write-path HTTP
  prospective test could not seed a real `space` row; that seam is covered by
  the validation function tests (both directions) plus the notify integration
  tests, which exercise the space-active check against a real `space` table.

## Verification

- `go build ./...`, `go vet`, `make i18n-extract-check`, `make i18n-lint`,
  `git diff --check` all clean.
- `go test -race` green for `modules/notify`, `modules/common`, `modules/space`
  (each on a fresh test DB — the shared `test` DB churns migrations across
  package binaries with different module sets; drop & recreate between them).
- Integration coverage: enqueue idempotency + concurrent single-insert; claim
  SKIP LOCKED single-winner; sweep claimed→pending / dispatching→unknown;
  cross-owner CAS blocked; pre-IM backoff→failed; precheck (member_left / robot
  / orphan / recipient system-bot); reconcile catch-up; active_from boundary;
  dispatch success/unknown/skip; event enqueue/dedup/exclusions/disabled.

## Not done (deliberate / out of scope)

- No octo-lib change, no client-supplied idempotency key (end-to-end
  exactly-once is a separate task); no auto-retry of `unknown`.
- No management API / CLI (operators use SQL + the ledger).
- Prometheus/Grafana wiring deferred.
- Three product/ops sign-off items gate flipping `enabled=true` in production
  (at-most-once acceptable; `skipped` is terminal; peak join-rate vs the
  single-replica ~240/min drain envelope). Code implements the recommended
  stance; enabling = accepting them.
