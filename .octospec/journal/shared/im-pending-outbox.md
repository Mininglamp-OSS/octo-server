---
type: Journal
title: "Journal: im-pending-outbox"
description: A failed IM unsubscribe used to vanish, leaving the removed member with full group access forever and invisible to everyone who could fix it. Every unsubscribe is now durable — recorded before it is attempted, deleted on success, retried to convergence on failure.
tags: ["space", "isolation", "acl", "data-integrity", "im", "testing"]
timestamp: 2026-08-23T05:00:00Z
# --- octospec extension fields ---
task: im-pending-outbox
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Journal: im-pending-outbox

## What was done

Removing someone from a group deletes their `group_member` row, then calls
`IMRemoveSubscriber`. **If that call failed, the failure was logged and dropped**, and the
job was marked `done`. The person stopped being a member in the business database and
stayed a subscriber in WuKongIM.

Every group and sub-thread unsubscribe now goes through a durable record:
**enqueue → attempt → delete on success**; a failure leaves the row for a worker that
retries to convergence.

## Why this was worth a P0

The blast radius was **measured**, not argued (probe and results in `evidence/`):

| broker state | can send | receives messages |
|---|---|---|
| normal member | ALLOWED | YES |
| **leaked** | **ALLOWED** | **YES** |
| correctly removed | BLOCKED (`SubscriberNotExist`) | NO |

A leaked subscription is **indistinguishable from full membership**. Confirmed in WuKongIM
source at the pinned tag: `internal/service/permission.go:147-154` gates sending on
`Store.ExistSubscriber`, independent of anything octo-server knows.

And nothing repaired it. Four routes, all closed:

1. Re-running the cleanup job is a no-op — its retry scope comes from live `group_member`
   rows, which are gone. It re-runs empty and launders a real failure into `done`.
2. The broker never reloads — the datasource callbacks are dead code.
3. The user cannot fix it — the group is absent from their client, so there is no "leave".
4. An admin cannot fix it — the person is absent from the member list.

Permanent, and invisible to every party who could act.

**Not a #795 regression**: that PR did not touch a line of the code (`git show fe9ddeb --
modules/group/service.go`), and blame puts it at the repo's history boundary. #795 added a
third caller and raised call volume by orders of magnitude, which is what turns one broker
restart into hundreds of simultaneous leaks.

## Design notes

**Five of eight call sites dropped failures; three already survived them** — one returns
500 so the user retries, two use the event system's `commit(err)`. The three that worked
were left alone: the fix follows existing precedent instead of replacing it.

**Only two of the five had a transaction.** `thread_cleanup`, `blacklist` and the botfather
delete path have none. Rather than wrap unrelated code in transactions, the API has two
entry points with **identical behaviour** and different guarantee strength:

- `EnqueueIMUnsubscribeTx` — inside the caller's transaction, so "member deleted, work
  recorded" is atomic. Covers a crash after commit but before the IM call returns, which is
  a real window: the IM call is slowest exactly when the broker is under stress, and a
  rolling deploy evicts pods mid-flight.
- `EnqueueIMUnsubscribe` — immediately before the attempt where no transaction exists.

That is a difference in what the existing code shape can guarantee, not two policies.

**No `done` state; success deletes the row.** The row's only purpose is to survive a
failure — nobody queries which unsubscribes succeeded. A 1,000-member / 50-group disband is
~50k unsubscribes; retaining them all would feed the retention purge for no benefit. The
table stays sized to *in-flight + broken* rather than *all traffic ever*.

**One multi-row INSERT, not one per uid.** A batch kick is up to 200 uids and a disband
walks every group; 200 round-trips inside the removal transaction would multiply its lock
hold time on `group_member` by network latency.

## Learning

**"It retries" is not the same as "the retry can see the work."** The existing cleanup job
looked like a durable retry and had every mechanism — lease, backoff, attempts, abandon —
but its retry *scope* was derived from the very rows the operation had just deleted. It
re-ran, found nothing to do, and reported success. A retry loop whose input is destroyed by
the action it retries is worse than no retry: it converts a visible failure into a
confident false success.

The general check: for any retry, ask what the retry *reads* to decide its work, and
whether the failed operation destroyed it. If so, the record has to carry its own payload —
here, `(channel_id, channel_type, uid)` — rather than pointing at state.

## Verification

- Eight outbox tests: failure leaves a record; success leaves none; the worker drains after
  the broker recovers; **retry works with no `group_member` row anywhere** (the core
  property); sub-thread channels drain identically; exhaustion reaches `abandoned`; enqueue
  is idempotent; multi-uid batches enqueue and delete completely.
- A wiring test at the real call site (`RemoveGroupMembers` with the broker stubbed to 500),
  **mutation-verified**: deleting the in-transaction enqueue turns it red on exactly the
  assertion naming the bug.
- Retry safety is measured, not assumed: `subscriber_remove` returns 200 for an
  already-removed subscriber, an unknown user, and an unknown channel.

## Known coverage gap

The wiring is proven end-to-end at `RemoveGroupMembers` only. `blacklist`,
`thread_cleanup`, the `groupExit` bot cascade and the botfather delete path are the same
six-line shape but are not individually covered — their handlers need full authenticated
HTTP setup. A test that only called the enqueue API was written for `blacklist` and then
deleted rather than shipped: it would have read as coverage it did not provide.
