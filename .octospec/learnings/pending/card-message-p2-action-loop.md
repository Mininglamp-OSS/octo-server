---
type: Learning
title: "A read-then-write CAS needs a concurrent -race test, not just a sequential one"
description: SELECT ... FOR UPDATE + INSERT ... ON DUPLICATE KEY UPDATE deadlocks on InnoDB insert-intention gap locks under concurrent first-inserts of the same unique key; only a concurrent test surfaces it, and the fix is a bounded retry on 1213/1205.
tags: ["mysql", "innodb", "concurrency", "deadlock", "cas", "testing", "review"]
timestamp: 2026-07-08T09:30:00Z
# --- octospec extension fields ---
source: self
origin_task: card-message-p2-action-loop
origin_pr: self
status: pending
candidate_rule: testing
---

# A read-then-write CAS needs a concurrent -race test, not just a sequential one

## Context

PR-B's D9 `card_seq` compare-and-set (optimistic ordering guard for card frames)
was implemented as, inside one transaction:

```sql
SELECT card_seq FROM message_extra WHERE message_id=? FOR UPDATE;   -- compare
INSERT INTO message_extra (...) VALUES (...) ON DUPLICATE KEY UPDATE ...;  -- set
```

The sequential test (`send seq=2 → edit seq=1 rejected`) passed cleanly. The
implementation looked obviously correct.

## What actually happened

A `-race` test firing N concurrent edits with increasing `card_seq` on a
freshly-sent card (no `message_extra` row yet) produced **ER_LOCK_DEADLOCK
(1213)**: two transactions each take an insert-intention gap lock on the same
non-existent unique key, then both attempt the INSERT → InnoDB deadlock-kills
one. The killed side returned a 500 to the bot and its frame was dropped, so the
card could end up stranded on a **stale** frame — the exact failure the CAS
existed to prevent. The bug was invisible to the sequential test because the
row-not-yet-exists gap-lock race only happens under concurrency.

## The rule

- Any **read-then-write CAS / optimistic-concurrency** path (`SELECT FOR UPDATE`
  then write, or check-then-upsert) MUST carry a **concurrent `-race` test** that
  asserts the post-condition invariant (e.g. "final stored value == max submitted",
  "exactly one winner"). A sequential test proves the logic, not the locking.
- The fix for the gap-lock deadlock is a **bounded retry on transient InnoDB lock
  errors** (1213 deadlock / 1205 lock-wait-timeout), detected via
  `errors.As(err, *mysql.MySQLError)` and `Number`. Deadlocks are transient:
  once any writer commits, the row exists and subsequent `SELECT FOR UPDATE` take
  record (not gap) locks, so a small bounded retry converges. Precedent:
  `modules/conversation_ext/service.go` `isRetriableMySQLLockErr`.
- Corollary: prefer the typed `*mysql.MySQLError.Number` check over string-matching
  the driver message.
