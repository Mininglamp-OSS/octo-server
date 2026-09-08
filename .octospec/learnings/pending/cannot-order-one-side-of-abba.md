---
type: Learning
title: "You can't fix an AB-BA deadlock by ordering only your own side"
description: A cross-path deadlock is a property of two transactions' combined acquisition orders. Pinning one path's lock order (a FORCE INDEX, a sorted batch) does nothing about the row the other path grabbed out of band. When you don't control the other side's order from your call site, a bounded deadlock-retry is the fix that works regardless of what it does.
tags: ["concurrency", "database", "deadlock", "review"]
timestamp: 2026-08-24T09:00:00Z
# --- octospec extension fields ---
source: self
origin_task: space-removal-creator-handover-notice
origin_pr: Mininglamp-OSS/octo-server#804
status: pending
candidate_rule: none
---

# You can't fix an AB-BA deadlock by ordering only your own side

## Context

A batch member-removal held many `space_member` row locks in one transaction. It
deadlocked against the owner-transfer path. I "fixed" it twice by making the batch's
acquisition order deterministic:

- round 6: switch from a per-uid `FOR UPDATE` loop to one `uid IN (…) FOR UPDATE`.
- round 7: add `FORCE INDEX (space_id, uid)` so the plan locks in uid order.

Both were measured and refuted by reviewers. The transfer path acquires
**non-monotonically**: it single-row-locks its target first, *then* range-scans.
When the target is inside the batch, batch-holds-low-uid-waits-on-target and
transfer-holds-target-waits-on-low-uid is a deterministic cycle — no matter how
cleanly the batch itself is ordered. Aligning A's order does nothing about the row B
grabbed before it started scanning.

## The rule

A deadlock is a property of the **pair** of transactions, not of either one alone.
Ordering only your own path closes a cycle **only if every other path that touches
the overlapping rows also acquires in that same order** — which you generally cannot
guarantee from one call site, especially against a path with a two-step
lock-target-then-scan shape.

When you don't control the other side's acquisition order, the fix that is robust
regardless of it is a **bounded retry on the deadlock error** (MySQL 1213 / 1205).
A deadlock rolls the victim back with nothing half-committed, so for an all-or-nothing
transaction a retry is trivially safe and almost always succeeds on the next attempt,
because the survivor has completed and the contention is gone. One shared retry
wrapper over all the mutators settles the whole class at once; per-path ordering
settles one pair and invites the next round.

## The second lesson, about evidence

I could not reproduce the deadlock in my own harness (two goroutines, sequential
rounds — low overlap probability), and I let that null result quietly support the
"it's fixed" claim. It wasn't evidence of anything. Two reviewers with higher-
concurrency harnesses and a control (the old per-uid shape at 0 deadlocks) measured
it at 35–100%. **Absence of a repro in a weak harness is not evidence of closure**,
and a reviewer's positive measurement with a control outranks your null result. If
you can't make the failure fire, you cannot claim you closed it — guard the recovery
mechanism directly instead (here: inject the 1213 into the retry wrapper and assert
it recovers), and don't assert the race is gone.
