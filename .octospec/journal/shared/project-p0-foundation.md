---
type: Journal
title: "Learning: Project P0 foundation"
description: "A Space-internal collaboration layer built on three load-bearing disciplines: writes define their invariants inside the transaction that commits them, every 'the worker will come back' claim needs a test, and a label the handler does not control must not be printed."
tags: ["octospec-learning", "space", "project", "isolation", "i1", "member-epoch", "batch-semantics"]
timestamp: 2026-09-06T03:38:00Z
source: self
---

# Learning: Project P0 foundation

## What shipped

`modules/project` — P0 of the Project layer: `octo_project` + `octo_project_member`,
CRUD + membership under invariant I1 (an active project member is an active member of
the same Space, checked in the request transaction), `member_epoch` bumped in the same
transaction as every membership write, a reverse-registered Space-removal cascade step,
a read-only paged reconcile job, quotas, audit entries and per-entry-point rejection
metrics. No group code touched; revertible by dropping two tables and one blank import.

Four review rounds ran over the implementation. The recurring findings were not
random — they were the same three failure shapes, and each is worth remembering.

## 1. An invariant only exists where it is enforced, and every write path is a new enforcement point

I1 was checked synchronously from day one — but per path, and the paths were added one
review at a time. The check existed in `addOneMember` before it existed in
`createProject` (the owner seat); the successor/promotion path gained its Space-seat
recheck rounds after the direct role change did. The brief predicted exactly this
("eleven group write paths prove retrofits leak") and it still happened, because the
mistake is structural: adding a write path and remembering its invariants are two
separate mental steps.

The practical counter-move: **a source-level guard that enumerates the write paths** and
asserts each one contains the invariant call, so a new path without the check fails CI
instead of surviving until a reviewer with the whole history in mind reads it.

## 2. "A later pass will fix it" must be code, not a comment

Three separate defects were comments asserting properties the code did not have:

- the cascade step said the reconcile job was "the backstop" for seats past its first
  page — the reconcile job is read-only by design and never fixes anything;
- the page cap said "the next tick continues" — every cursor was a local, so ticks
  restarted from the top and rows past ~25k were never examined;
- the batch endpoints shared a label ("not_attempted") whose meaning was true on one
  path's execution model and false on the other's.

A comment describing a guarantee is a specification without a test. The fix each time
was to make the mechanism real (persistent cursors, service-layer batch termination) and
pin it with a test whose failure mode is the exact wording of the claim.

## 3. A report must only describe what the reporter controls

The partial-batch label (`not_attempted`) was printed by the handler for execution the
**service** layer had already performed. The honest division: the layer that decides
where execution stops is the layer whose labels describe what ran. Anything else is a
report about a world that no longer exists.

## Environment gotchas (local, repeated)

- `testutil.NewTestServer` runs with `Migration=false` against a shared `test` DB;
  per-package runs need `DROP DATABASE` + `CREATE DATABASE … COLLATE utf8mb4_general_ci`
  (mysql:8.0's `0900_ai_ci` default breaks space's migration JOINs with error 1267),
  plus a Redis FLUSHALL (rate-limit buckets survive `CleanAllTables`).
- Isolated per-test DBs (`octo_toctou_test`, `octo_msg_issue557_test`) are NOT reset by
  the CI shard script and accumulate schema from other worktrees' branches — stale
  foreign keys there fail whole packages with error 3730. Drop them before blaming code.
- Test-isolation bugs in other modules (a shared config mutated without restore) present
  as random failures in *their* package under `-shuffle=on`. Before attributing, run the
  failing package on a pristine worktree with the same seed.
