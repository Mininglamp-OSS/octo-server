---
type: Task
title: "Task: bounded read receipt deadlock recovery"
description: "Share the user/Bot receipt write boundary and recover only known-aborted deadlocks."
tags: ["message", "bot-api", "database", "testing"]
timestamp: 2026-11-11T00:00:00Z
slug: read-receipt-deadlock
source: self
upstream: "https://github.com/Mininglamp-OSS/octo-server/issues/954"
---

## Goal

Normalize overlapping receipt batches at the final SQL boundary and recover
statement deadlocks without reporting an unknown commit as success.

## Load-bearing list

- Both user and Bot handlers use the same write service after their existing
  authorization, message search and single-chat channel normalization.
- Deduplicate and totally order the exact `(message_id, uid)` tuple. Reject
  conflicting channel payloads for a tuple instead of selecting by arrival order.
- Preserve `ON DUPLICATE KEY UPDATE message_id=VALUES(message_id)`.
- Retry only statement error 1213 identified with `errors.As`, after releasing
  the old transaction. At most three fresh transactions, exponential backoff
  plus jitter, a two-second write budget and the shorter request deadline.
- Rollback failure, begin failure, 1205, network failure and every commit error
  return failure. Commit errors are never retried, even if labelled 1213.
- Pin a database connection and execute START TRANSACTION, write, COMMIT and
  ROLLBACK with context. Native mysql v1.7.1 Tx.Commit/Rollback do not watch
  context. Discard the connection unless commit or rollback was confirmed;
  never put an uncertain transaction back into the pool.
- Only a definite commit enables the handler's one post-write Redis pass.
  Existing aggregation still recalculates the count from persisted receipts.

## Out of scope / known deviations

- No production DDL/index, isolation, deployment SHA or transaction-prelude
  verification is available. The existing message/user uniqueness assumption
  remains unverified; the test DDL is not production evidence.
- No DDL/isolation change or cross-replica queue/routing/coalescing.
- Bot still clears the unread badge before its receipt DB write; a DB failure
  can leave that badge cleared. Correct this separately with product agreement.
- Existing asynchronous Redis delivery can fail after commit. This change does
  not add a durable outbox or exactly-once delivery across HTTP requests.

## Acceptance

- SQL-boundary regression fails against the baseline unsorted duplicate batch.
- Two independent MySQL connections submit overlapping ascending/shuffled 6/7
  batches 50 times for each all/partial/no-existing-row state, preserving exactly
  seven unique records per pair.
- Driver injection proves rollback before new begin, at most three attempts,
  exhausted failure, 1205/non-deadlock failure, cancellation and unknown commit.
- Both handlers preserve DM/group normalization and send one cache pass only
  after a successful retry; exhausted/unknown-commit writes send none.
- Native formatting, focused race tests, vet/build and available repository
  checks pass; unavailable full-system and production evidence is reported.
