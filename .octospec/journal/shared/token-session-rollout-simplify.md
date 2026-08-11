---
type: Journal
title: "Journal: token-session-rollout-simplify"
description: Record of collapsing the #725 five-phase session rollout into a self-driving loop, and the measured defects that shaped it.
tags: ["auth", "security", "redis", "mysql", "session", "rollout", "operability"]
timestamp: 2026-08-11T09:10:00+08:00
# --- octospec extension fields ---
task: token-session-rollout-simplify
source: self
---

# Journal: token-session-rollout-simplify

## What was done

- Boot no longer panics. #725 paired an environment mode against the persisted
  floor and panicked on a mismatch or a missing record, so one lost Redis key
  stopped the fleet from starting. A single write-once MySQL marker row now
  separates "never initialised" from "initialised then lost to an RDB
  rollback", and a lost floor resolves **upward** to enforce.
- The mode is derived from the floor and polled, removing eight of nine rolling
  restarts and the ordering trap where advancing the floor before deploying the
  matching mode made every pod restart fatal.
- Added a writer registry modelled as a write **lease**. Losing it refuses new
  credentials only — it does not fail readiness and does not panic.
- Replaced the two-observation / one-hour-gap evidence ritual with a predicate
  evaluated at decision time, and added a reconciler that advances the floor
  when the predicate holds. Ships disabled.
- Folded the two standalone tools into `app session-rollout`, which prints the
  resolved Redis endpoint and instance fingerprint before acting.
- Rewrote the runbook; deleted the nine-step ladder.

Migration correctness is untouched: immutable cutoff, single-owner lease,
`run_id`-bound checkpoints, shorten-never-extend, elapsed-cutoff confirmation.
So are the floor's monotonicity, one-phase and single-winner rules —
`AdvanceRolloutControl` stayed a bare CAS with the predicate split out, and the
invariant tests covering it needed no edits.

## Structural learnings

**Measure before designing, not after.** The brief was drafted from code
reading and verified afterwards. The verification found a defect (a corrupt
payload wedging the floor) that changed a design decision, so that decision
arrived as a patch instead of being designed in. For a load-bearing change to a
live system, a characterization of current behaviour belongs in the Plan phase
as an input. Candidate rule filed.

**Enforcement was inverted relative to risk.** #725 machine-enforced the
*least* critical check — that two scans were an hour apart — while the *most*
critical one, that no pre-fix replica remained, was a kubectl template a human
copied. This is the third subsystem in this repo to stall on exactly that
question: `internal/msgextraseq` (#627) documents that its lock "is not a
substitute" for an operator drain, and `pkg/botevent` (#697) states the flip's
precondition is that operations confirmed no old replica remains. The registry
is the shared shape of an answer; it was kept auth-local but written to be
extractable (opaque applied-state string, caller-supplied comparison, injected
key prefix).

**Empirical evidence where a deductive argument exists.** "No legacy remains"
was proved by scanning twice an hour apart. It follows deductively from: no
legacy writer can exist, every legacy record has a bounded deadline, that
deadline has passed. The floor already enforced the first term for new
processes; the hole was processes already running, which is precisely what the
wall clock was standing in for. Answering it by query rather than by waiting
removed two hours and four commands.

**Ask what a borrowed pattern is for before copying it.** The first revision
copied "MySQL authority + Redis mirror" from `octo_bot_event_seq_state` without
checking why that shape exists — the bot event allocator runs inside a msgSem
slot and cannot afford a DB round trip, so its gate must share a Lua script
with the INCR. The session floor has no such constraint, and copying it
introduced a two-phase write whose Redis half cannot be rolled back, because
the floor is monotonic and has no undo.

**The safe direction on lost state is not always "restore".** Session tokens
are disposable, so a lost floor does not need its old value, only a safe one —
and the safest is the strictest. Resolving upward is strictly better than
restoring exactly in every rollback shape.

## Gotchas worth remembering

- **`gofmt -w .` reformatted ~120 unrelated files.** The repo contains files
  that were already non-gofmt-clean, and a repo-wide format pulls them all into
  the diff. Use `gofmt -w <file>`.
- **A tripwire that pins a defect must still compile.** The pre-change harness
  referenced a method the change deletes, so "going red" would have been a
  build failure taking the whole package with it. Tripwires belong as
  compiled-but-skipped tests, and their inverted forms become the acceptance
  suite.
- **The v1 fixtures in `pkg/auth` were wrong.** They used JSON, but the v1 wire
  format is `uid@name[@role]`. The old migration Lua hid it by treating
  anything without a v2/v3 prefix as v1. Production corroborates the real
  format: observe parsed all 53 v1 records in the test environment with
  `decodeLegacy`.
- **A losing racer on `AdvanceRolloutControl` surfaces two different errors.**
  It reaches the CAS if it read before the winner's write, and the optimistic
  pre-check if it read after. A multi-replica reconciler must treat both as
  benign or it will alert on itself.
- **"Conservative" is not the same as "stricter".** Falling back to `expand`
  when the floor is unreadable reads as cautious and is a fail-open, because
  `expand` stops consulting legacy deny markers.

## Not done / still open

- The reconciler ships with `AUTO_ADVANCE` off. The bootstrap rule requires it:
  the registry cannot see pre-registry replicas, so the release that introduces
  it must not act on its own view during its own upgrade.
- `ErrWriterLeaseLost` joins the existing internal-error path at the handlers.
  Whether it deserves a dedicated i18n code is an open product question.
- The per-UID cap still collides with clients that re-login without reusing a
  session; 管理台账号 held 64 sessions in the test environment. Separate task,
  and a prerequisite for taking production past `bounded`.
