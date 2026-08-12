---
type: Journal
title: "Journal: token-session-rollout-simplify"
description: Record of replacing the #725 rollout controls with one MySQL authority and bounded Redis evidence
tags: [auth, security, redis, mysql, session, rollout, operability]
timestamp: 2026-08-12T00:00:00+08:00
task: token-session-rollout-simplify
source: self
---

# Journal: token-session-rollout-simplify

## Current design

- `octo_session_rollout_state` is the only durable floor/cap/pause authority.
- `octo_session_rollout_advance` stores append-only evidence in the same MySQL transaction as the
  versioned floor CAS. Cap-only changes use the same stream with `transition_kind=set-cap` and
  old/new cap evidence.
- #725 Redis floor and legacy MODE/MAX are read only while the singleton is absent. The stricter
  floor and the existing Redis cap are adopted; Redis floor is never changed by this release.
- Session issuance is fenced before module migrations finish. A runtime mode change fences first,
  applies local reader state, atomically publishes registry state + lease, and only then unfences.
- Observe, migrate and reconciler share an instance-bound scanner and one scan-owner lease. A
  `run_id` change discards cursor and counters; lease loss cannot produce complete evidence.
- The reconciler ships disabled. Operators first validate takeover and shadow scans, then explicitly
  enable it. Migration cutoff and finite policy remain business decisions.

This replaces the earlier PR #733 revisions that combined a Redis floor with a MySQL write-once
marker and provisional recovery state. That state machine had separate interpretations in boot,
poller, predicate and registry paths; fixing one cell repeatedly opened another exit-path defect.

## Why one MySQL authority

The session floor does not sit on a latency-critical request write. Keeping a Redis mirror therefore
provided no data-plane benefit, while creating cross-store order, rollback and recovery questions.
MySQL already gates startup migrations and can atomically bind audit evidence to a versioned CAS.

Redis remains appropriate for short-lived writer/scan leases and for the session data being scanned.
Those facts are evidence, not authority: losing or restoring them can abort work, but cannot change
the floor.

## Load-bearing details

- Concurrent first starters may both observe an absent singleton. Duplicate-key losers rollback,
  reload under lock and monotonically adopt a stricter seed; normal startup contention is not fatal.
- `ApplyAndPublishRolloutState` publishes the state the process actually applied. Publication error
  keeps new credential creation fenced while existing-session reads remain available.
- `session-rollout set-cap --max-per-uid N --yes` is the only post-takeover cap mutation path. It
  performs a version/floor/old-cap CAS and audit insert in one transaction; Redis and deprecated env
  never regain authority. Runtime snapshots carry the MySQL version so a stale same-floor poll
  cannot undo a newer cap.
- Control writes have a five-second internal deadline. A blocked singleton update cannot hold an
  application goroutine until the database's much longer lock-wait timeout.
- The writer fingerprint is checked before and after each decision. A build/state/member change
  invalidates the scan even when token counters are green.
- `instanceBoundScanner` checks `run_id` before/after SCAN, after per-key work and before completion.
  The final checks matter: failover during the last returned batch otherwise has no next cursor on
  which to discover the change.
- Scan-owner and migration mutation lock have different jobs. The former bounds aggregate Redis
  load across status/observe/migrate/reconciler; the latter prevents concurrent token mutation.
- `paused=true` blocks predicate work before registry reads or a full scan, and the MySQL CAS still
  includes `paused=0` to close the race after evaluation.

## Structural learnings

**Enumerate ownership before enumerating states.** The old table listed Redis/marker/provisional
cells but still allowed several modules to interpret effective mode. Declaring one owner and one
publication path removed more risk than filling additional cells.

**Evidence should fail closed without becoming authority.** A missing writer lease, lost scan lease,
changed `run_id` or DB read error stops issuance/scan/advance as appropriate. None is converted into
a guessed floor.

**A completed SCAN needs a final-instance proof.** Checking only around `SCAN` is insufficient because
the returned keys are read or mutated afterwards. If failover occurs during the final batch, there is
no next iteration to detect it. Recheck after key work and immediately before recording completion.

**Characterize before designing.** Production-shaped fixtures exposed the incorrect v1 test format,
observe/migrate classification drift and per-UID distribution. Those facts belong before control-plane
design, not as post-implementation patches.

## Operational boundaries

- Before MySQL floor or cap changes beyond the adopted #725 posture, rollback to #725 remains
  possible if its required env is restored and the untouched Redis floor is present.
- After MySQL floor advances or cap changes, rollback to a Redis-floor-only artifact is forbidden. Pause and roll
  forward; writing the MySQL value back to Redis would recreate dual authority.
- Migration remains shorten-only and resumable. It must never extend TTLs, delete deny markers or
  revive expired sessions.
- A non-zero undecodable-record metric needs investigation but does not block floor movement: those
  records have never been valid credentials and migration deliberately leaves them untouched.

## Process notes

- Use `gofmt -w <specific files>`; repository-wide gofmt pulls unrelated legacy formatting into the
  diff.
- Keep RED and GREEN checkpoints distinct. A new test must execute and fail for the intended safety
  gap before production code changes.
- Report package/race/integration/E2E evidence separately; local Redis-only tests do not prove MySQL
  transaction or full login/revocation behavior.
