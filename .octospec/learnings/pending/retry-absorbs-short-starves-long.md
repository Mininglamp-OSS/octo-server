---
type: Learning
title: "A deadlock retry absorbs short-lived counterparties and starves against long-lived ones"
description: Bounded retry is not a general substitute for lock ordering. It closes cycles against a counterparty that finishes quickly, and loses deterministically against one that outlives the retry budget — the fresh, zero-undo transaction is elected victim every time.
tags: ["mysql", "concurrency", "deadlock", "review"]
timestamp: 2026-08-24T14:20:00Z
# --- octospec extension fields ---
source: review
origin_task: space-removal-creator-handover-notice
origin_pr: 804
status: pending
candidate_rule: concurrency
---

# A deadlock retry absorbs short-lived counterparties and starves against long-lived ones

## Context

A batch `space_member` removal acquired row locks in index order. Several other
writers acquire in their own orders, and one of them — owner transfer — is
two-step non-monotonic, so no single call site can unify the *other* side's
order. The chosen fix was a bounded retry on MySQL 1213, which is correct for
that pair: measured 0 deadlocks surfaced, down from 40/40.

The same wrapper was then applied to `upsertMembers`, a plain
`INSERT … ON DUPLICATE KEY UPDATE` loop over up to 200 uids in caller order,
and declared closed. A reviewer measured it instead:

| `upsertMembers` treatment | deadlocks surfaced to the removal side, 60 rounds |
|---|---|
| wrapped in the retry (5 attempts) | 48/60 |
| wrapped in the retry (3 attempts + 5/20ms backoff) | **60/60** |
| **uids sorted ascending**, matching the batch's order | **0/60** |

More attempts helped slightly. Backoff helped not at all. Ordering closed it
completely.

## Why

The failure is **victim starvation**, not an unlucky collision.

InnoDB picks as deadlock victim the transaction with the least work to undo. A
retrying transaction is *always* the cheapest one: it just started and has
modified nothing. So against a counterparty that is still running, the retrier
is elected victim, rolls back, immediately re-collides with that same live
transaction, and is elected again — until its attempts are spent. A 200-row
upsert easily outlives 5ms + 20ms of backoff.

The retry works against owner transfer for exactly the opposite reason: that
transaction is short, so by the second attempt the obstacle is gone.

## The rule

**Retry absorbs cycles against short-lived counterparties. Ordering is the only
thing that closes them against long-lived ones.** Retry is not a substitute for
ordering — it is the fallback for where ordering cannot be arranged.

Before reaching for a retry wrapper, ask which case you are in:

- **Can I control both sides' acquisition order?** Then order them. A plain loop
  you own is always orderable — `sort` the keys. This is structural: the cycle
  cannot form, so there is nothing to recover from.
- **Is the other side non-monotonic, or in code I don't control?** Then retry,
  and size the budget against that counterparty's *lifetime*, not against a
  guess.

The trap is that a retry wrapper, once written, looks like it applies
everywhere. It does not, and the case where it silently fails is the one where
the other side is a big batch — precisely the high-contention case you built it
for.

## Corollary — zero is not evidence until you remove the absorber

A retry hides the very errors you are counting. After ordering fixed the pair,
the 0/60 was re-run with **both sides unwrapped**, and InnoDB's
`LATEST DETECTED DEADLOCK` timestamp checked for advancement. Both confirmed the
cycles were *absent*, not *absorbed*.

Report the distinction explicitly. "0 deadlocks surfaced" through a retry and "0
deadlocks occurred" are different claims, and only the second one means the
lock order is right.
