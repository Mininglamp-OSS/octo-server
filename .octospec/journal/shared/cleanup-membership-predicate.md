---
type: Journal
title: "Journal: cleanup-membership-predicate"
description: The removal-cleanup pipeline had two rejoin guards asking the same question with different predicates, and each was wrong at one edge — a disbanded Space silently voided cleanup, a banned Space wrongly triggered it. Both now use one cleanup-only predicate; the authorization predicate is left untouched and pinned by a source guard.
tags: ["space", "isolation", "acl", "auth", "data-integrity", "testing"]
timestamp: 2026-08-22T13:40:00Z
# --- octospec extension fields ---
task: cleanup-membership-predicate
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Journal: cleanup-membership-predicate

## What was done

The member-removal cleanup pipeline has two rejoin guards — the worker gate that
decides whether a claimed job is still worth running, and the gate inside the
group cascade step itself. Both answer "did this person come back?", and they
were asking it with **different predicates**, each wrong at one edge:

| gate | was | wrong when |
|---|---|---|
| worker (`modules/space/member_removal.go`) | `queryMember` — `sm.status=1` only | Space **disbanded**: an orphan `sm.status=1` row voided the job, so cleanup never ran |
| cascade step (`modules/group/space_member_removal.go`) | `CheckMembership` — `sm.status=1 AND s.status=1` | Space **banned**: a fully active member was torn out of every group |

The worker's error is the reachable one. It composes with the join-vs-disband
orphan (still open, #797) which produces exactly `sm.status=1` inside a
`s.status=0` Space: the outer gate short-circuits the job to
`done/skipped_rejoined`, the inner gate's deliberately-chosen predicate never
runs, and the member keeps their `group_member` rows and IM group subscriptions
in a disbanded Space with nothing left to fix it.

Both gates now call one new predicate, `CheckMembershipForCleanup`
(`sm.status=1 AND s.status <> 0`), which answers *"does this person still hold
their seat, so cleanup must skip?"* — disbanded voids the seat, banned does not.

## The correction that shaped this

#797 originally proposed relaxing the shared `CheckMembership` helper to
`space.status <> 0`. That would have been a **security regression**, not a fix:
`pkg/space/membership.go` has 37 non-test call sites, one of which is
`pkg/space/middleware.go`'s `SpaceMiddleware` — the primary authorization gate
for the whole API. Relaxing it admits banned Spaces across every authenticated
endpoint.

A second #797 entry proposed the opposite — adopt `CheckMembership` in the
worker — which would have made every banned Space clean its members out of all
groups. The two proposed fixes contradicted each other; both were symptoms of
one root cause: **two different questions sharing one predicate**.

- *may this principal **access** the Space?* → `s.status = 1` — `CheckMembership`, unchanged
- *is this member still in place, should cleanup **skip**?* → `s.status <> 0` — the new one

Both #797 entries were corrected in place before any code was written.

## Learning

**When one predicate serves two callers that disagree about an edge case, the
bug is the sharing, not the predicate.** The instinct on both #797 entries was
to move the shared helper toward whichever caller was currently wrong — and each
move broke the other caller, one of them catastrophically. Counting the call
sites first (37, including the auth middleware) is what turned a one-line "fix"
into the right two-predicate shape.

Corollary for review: a proposed one-line predicate relaxation in a shared
`pkg/` helper deserves a call-site census before it is approved, not after.

## Verification

TDD, and every new test was mutation-verified rather than merely observed green:

| mutation applied to the new predicate | test that died |
|---|---|
| `s.status <> 0` → `s.status = 1` (behave like `CheckMembership`) | banned-Space test only |
| drop the `space` join condition (behave like `queryMember`) | disbanded-orphan test only |
| relax `CheckMembership` to `<> 0` (the rejected #797 fix) | `TestCheckMembershipStaysStrict` only |

Each mutation killed exactly the intended test and no other, so the two new
tests bind to opposite halves of `<> 0` — neither is passing by accident.

`TestCheckMembershipStaysStrict` is a source guard that pins the authorization
predicate strict and the cleanup predicate loose, so the merge #797 originally
proposed cannot be reintroduced quietly.

No-regression proof: `TestCleanupWorkerSkipsRejoinedMember` and
`TestGroupCascadeStillRunsAfterSpaceDisbanded` both stay green — the new
predicate preserves them by construction.

Gates: full `modules/space` (11.3s) and `modules/group` (38.2s) suites,
`pkg/space`, `golangci-lint` 0 issues, `i18n-extract-check`, `i18n-lint`.

## Environment gotchas worth remembering

Two things cost time bringing local test services up by hand, neither of which
is visible from CI config:

- The `test` database **must** be created `COLLATE utf8mb4_general_ci`.
  MySQL 8 defaults a fresh schema to `utf8mb4_0900_ai_ci`, and migration
  `20260308000002_space_legacy01.sql` then dies with
  `Error 1267: Illegal mix of collations`.
- `modules/group` tests that create real IM channels **panic with a nil-pointer
  dereference** when the WuKongIM broker is down, instead of failing cleanly —
  the rollback path returns a nil group that the test then dereferences. The
  real message (`dial tcp 127.0.0.1:5001: connection refused`) is buried above
  the stack trace.
- Packages share one `test` database but register different migration sets, so
  it must be dropped and recreated between package runs or the next package
  panics with `unknown migration in database`.

## Follow-ups (unchanged, still open in #797)

- The join-vs-disband **root cause** — lock the `space` row and re-check
  `status` inside `atomicAddMemberIfNotFull` / `atomicReactivateMemberIfNotFull`
  / `approveJoinApplyAtomic`. This task makes the pipeline handle the orphan
  state correctly; it does not stop the orphan being created.
- The membership epoch on `space_member` that would close the rejoin window to
  zero rather than to one query's width.
- Historical backlog remediation, which this task unblocks: a backfill was
  previously no-op'd by the worker's old gate.
