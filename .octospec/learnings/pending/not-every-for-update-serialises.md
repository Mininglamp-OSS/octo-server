---
type: Learning
title: Not every FOR UPDATE serialises, and one that does not can deadlock
description: A locking read on a row that does not exist yet takes a gap lock, and gap locks are mutually compatible — so it serialises nothing and defers the conflict to the INSERT, where it becomes a cycle instead of a queue.
tags: ["mysql", "innodb", "concurrency", "deadlock", "locking", "review"]
status: pending
source: oidc-auto-join-initial-space
---

# Not every FOR UPDATE serialises, and one that does not can deadlock

## What happened

A new join path needed to be safe against a concurrent disband. The analysis
turned on one InnoDB fact, and it was written down explicitly in the code:

> `FOR UPDATE` on a row that does not exist yields a gap lock, and gap locks are
> mutually compatible — so "lock the member row first" serialises nothing; the
> only real rendezvous is the INSERT's insert-intention lock.

That reasoning was correct, and the disband ordering derived from it was correct.
The same function then opened with a `SELECT ... FOR UPDATE` on the member row —
a row that, for a brand-new account, never exists. Every concurrent joiner took a
compatible gap lock, and every joiner's INSERT then needed an insert-intention
lock conflicting with all the others. A cycle, and MySQL rolls one back.

Reviewers reproduced it at 19 failures in 20 concurrent joiners against a nearly
empty target, falling to zero once the table filled — worst exactly on rollout
day, and gone before anyone could investigate.

Fixing it exposed a second defect that the deadlocks had been masking: the
capacity `COUNT` was a plain read, so with the deadlocks gone, ten joiners all
read the same pre-insert total and all concluded there was room for two.

## The trap

The fact was known, written down, and applied to one adversary (disband) while
being missed for another (other joiners). Having explained why a lock does *not*
serialise in one direction, it is easy to keep reaching for it in another,
because the statement still *looks* like mutual exclusion.

Two locks that read identically in code behave oppositely:

- `FOR UPDATE` on **rows that exist** — an X range lock. Contenders **queue**.
- `FOR UPDATE` on a **key that does not exist yet** — a gap lock. Contenders all
  proceed, then **cycle** at the INSERT.

So the lock ended up on the statement where it could only cause deadlocks, and
was absent from the statement where it was the only thing standing between the
feature and over-admission.

## The rule

Before adding `FOR UPDATE`, ask which of the two it is: *is the row I am locking
already there?* If it may not be, the statement provides no mutual exclusion, and
placing a conflict-generating write after it manufactures a deadlock.

Put the lock on state that exists — the count, the parent row, the range being
counted — and let uniqueness handle the row that does not.

## What a test must do

A single-threaded test cannot see either defect: both are correct alone. The
covering test is N goroutines against **one** target, and it needs two cases,
because each fix hides the other's absence:

- unlimited target, near-empty (deadlocks peak when the table is small): every
  joiner must succeed;
- capped target with fewer seats than joiners: exactly the free seats admitted,
  everyone else told it is full.

Mutation-test both. Restoring the wrong lock must fail the first; removing the
right one must fail the second.
