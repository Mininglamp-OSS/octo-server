---
type: Learning
title: "Quota re-scoping: narrow the count, not the lock"
description: When changing a DB count-gate quota from a coarse scope to a finer one, narrow only the COUNT(*) predicate. Do NOT narrow the FOR UPDATE serialization lock to the finer scope — an empty finer range takes a pure gap lock, and concurrent creators deadlock. Keep locking the always-present coarse parent row.
tags: ["db", "concurrency", "quota", "dbr", "review"]
timestamp: 2026-07-22T10:44:01Z
# --- octospec extension fields ---
source: self
origin_task: incoming-webhook-quota-per-thread
origin_pr: Mininglamp-OSS/octo-server (incoming-webhook per-thread quota)
status: pending
candidate_rule: testing
---
# Quota re-scoping: narrow the count, not the lock

A "count-gate" quota (count existing rows in a transaction, reject if
`count >= max`, else insert) is made race-safe by taking a `FOR UPDATE` lock
that serializes concurrent creators. When the quota granularity is later
refined — e.g. from *per group* to *per (group, thread)* — the natural but
**wrong** move is to narrow the lock to the finer scope too:

```go
// WRONG: locks the finer range
tx.SelectBySql("SELECT ... FROM child WHERE group_no=? AND thread_short_id=? ... FOR UPDATE", ...)
```

If that finer range is empty (first insert into a new thread), `FOR UPDATE`
matches 0 rows and takes a **pure gap lock**. Gap-X locks are mutually
compatible, so N concurrent creators all acquire the gap, all pass the
`count < max` check, then all try to `INSERT` and deadlock (InnoDB 1213)
contending for the insert-intention lock. With no retry, legitimate concurrent
creates fail with an opaque 500.

**The lock scope and the count scope are independent concerns:**

- The **lock** only has to *serialize* concurrent creators. Lock an
  always-present single row at any enclosing scope — e.g. the parent `group`
  record row (`SELECT id FROM \`group\` WHERE group_no=? FOR UPDATE`). A record
  lock on an existing row never degrades to a gap lock.
- The **count** predicate is what defines the quota bucket. Narrow *this*
  freely: `SELECT count(*) ... WHERE group_no=? AND thread_short_id=? ...`.

```go
// RIGHT: coarse lock (always-present parent row) + fine count
tx.SelectBySql("SELECT id FROM `group` WHERE group_no=? FOR UPDATE", groupNo)      // serialize
tx.SelectBySql("SELECT count(*) FROM child WHERE group_no=? AND thread_short_id=? AND status!=?", ...) // bucket
```

Over-serializing (whole group instead of per-thread) is usually a negligible
cost — child tables like this are small — and is strictly safe. The gap-lock
deadlock, by contrast, is silent until concurrency hits an empty bucket.

Origin: `modules/incomingwebhook/db.go insertWithQuota`, whose original design
comment already documented the gap-lock hazard; the per-thread quota change kept
the parent-group lock and only added `thread_short_id` to the two counts.
