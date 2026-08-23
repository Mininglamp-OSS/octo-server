---
type: Task
title: "Task: im-pending-outbox"
description: A failed IM unsubscribe is permanent and invisible — the removed member keeps full group access forever. Make every unsubscribe durable via an outbox consumed by the existing removal-cleanup worker.
tags: [space, isolation, acl, data-integrity, im, testing]
timestamp: 2026-08-23T00:00:00Z
# --- octospec extension fields ---
slug: im-pending-outbox
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Task: im-pending-outbox

## Goal

When octo-server removes someone from a group it deletes the `group_member` row and then
calls `IMRemoveSubscriber`. **If that call fails, the failure is dropped on the floor and
the job is marked `done`.** The person is no longer a member in the business database but
is still a subscriber in WuKongIM.

Make every group/sub-thread IM unsubscribe durable: record it so a failure is retried to
convergence instead of lost.

## Background — measured, not inferred

Probed against `wukongim v2.2.4-20260313` (the tag pinned in `ci.yml`) with a real client
(probe source and full results committed alongside this brief under `evidence/`):

| broker state | removed member can send | receives group messages |
|---|---|---|
| normal member (baseline) | ALLOWED | YES |
| **leaked (unsubscribe failed)** | **ALLOWED** | **YES** |
| correct (unsubscribe succeeded) | BLOCKED (`SubscriberNotExist`) | NO |

**A leaked subscription is indistinguishable from full membership.** Corroborated in
source at the same tag: `internal/service/permission.go:147-154` gates sending on
`Store.ExistSubscriber`, independent of anything octo-server knows.

**Nothing repairs it.** Four independent routes are all closed:

1. Re-running the cleanup job is a no-op — its retry scope is derived from live
   `group_member` rows, which are gone after the delete. It re-runs empty and launders a
   real failure into `done`.
2. The broker never reloads — the `IMDatasource` callbacks are dead code (#797, verified).
3. The user cannot fix it — the group is absent from their client (no `group_member` row),
   so there is no "leave group" to press.
4. An admin cannot fix it — the person is absent from the member list for the same reason.

So the state is **permanent**, and invisible to everyone who could act on it.

**This is not a #795 regression.** `git show fe9ddeb -- modules/group/service.go` shows the
PR did not touch a line of it; blame puts the code at this repo's history boundary
(≥ 2026-08-04). #795 added a third caller and raised call volume by orders of magnitude —
a disband is now one job per member, each walking every group — which is what makes a
single broker restart able to leak hundreds of subscriptions at once.

## The five leak sites

`IMRemoveSubscriber` has eight call sites. Three already survive a failure; **five drop it**:

| site | user action | today | |
|---|---|---|---|
| `modules/group/service.go:1912` | kick / Space-removal cascade / bot_api kick (3 callers) | log-only | ❌ |
| `modules/group/api.go:3481` | leaving a group, cascading the leaver's bots | log-only | ❌ |
| `modules/group/api.go:3644` | **blacklist** | log-only | ❌ |
| `modules/group/thread_cleanup.go:78` | sub-thread cleanup (every kick path reaches it) | log-only | ❌ |
| `modules/botfather/command.go:636` | deleting a bot | log-only | ❌ |
| `modules/group/api.go:3272` | user leaves a group (parent channel) | returns 500 | ✅ user retries |
| `modules/group/event.go:655` | org/department removes a person | `commit(err)` → event retry | ✅ |
| `modules/group/event.go:831` | org exit | `commit(err)` → event retry | ✅ |

Blacklist is the sharpest: the whole point is "this person must not see this any more", and
on failure they keep reading everything.

The three that survive matter for design — the repo already solves this correctly two
different ways, so the fix has precedent rather than inventing a mechanism.

## Load-bearing list

- `space` / `isolation` / `acl` — this is the enforcement point of group membership at the
  transport layer. Measured above: failing it grants full read **and** write.
- **Only two of the five sites have a transaction.** `RemoveGroupMembers` commits at
  `service.go:1899` with the IM call after; `groupExit`'s bot cascade commits at
  `api.go:3428`. `thread_cleanup.go`, `blacklist`, and the botfather delete path have **no
  transaction at all**. The design must not require wrapping unrelated code in transactions.
- Retry safety is **measured**: `subscriber_remove` returns 200 for an already-removed
  subscriber, a non-existent user, and a non-existent channel. Retry is a safe no-op.
- Sub-thread channels (`{groupNo}____{shortID}`, `ChannelTypeCommunityTopic`) are just
  another channel id — one uniform record shape covers parent groups and sub-threads.
- The existing `space_member_removal_cleanup` worker already provides claim/lease/backoff/
  abandon/purge/metrics. This task should **consume that machinery, not clone it**.
- `test` — TDD; the failure is invisible without a test that forces the IM call to fail.

## Design sketch (for confirmation before code)

A table of pending IM unsubscribes, drained by a step registered on the existing worker:

```
im_pending_subscriber_removal(
  id, channel_id, channel_type, uid,
  status, attempts, next_attempt_at, lease_owner, lease_until, last_error,
  created_at, finished_at)
```

Flow at each site: **record → attempt → delete the record on success**; on failure leave it
for the worker.

Two decisions worth a maintainer's opinion, both called out rather than assumed:

1. **Delete on success rather than marking `done`.** A disband of a 1,000-member Space with
   50 groups produces ~50k unsubscribes. Marking them `done` would put 50k rows through the
   retention purge for no benefit — the row's only purpose is to survive a failure. Deleting
   on success keeps the table sized to *in-flight + broken* rather than *all traffic ever*.
   Cost: one extra DELETE on the happy path.
2. **Record-always vs record-on-failure.** Record-always (the true outbox, written inside the
   transaction where one exists) also covers "process died after commit, before the IM call".
   Record-on-failure is nearly free but loses a crash in the microsecond between the IM error
   and the record write. **Recommendation: record-always at the two transactional sites**
   (that crash window is exactly the failure class being fixed), **record-on-failure at the
   three without a transaction**, since making them transactional is a much larger change
   than this task should carry.

## Out of scope

- Subscriber **add** failures (the join path). Same shape, different blast radius; separate task.
- The three sites that already survive failure — leave them as they are.
- Making `blacklist` / `thread_cleanup` / the botfather delete path transactional.
- **#800 ①** (owner removed → their bots keep `space_member.status=1`). It wants this same
  outbox for its group cleanup, so the mechanism must not assume a human uid — but the
  Space-membership half of #800 stays in #800.
- Every remaining #797 P2.

## Acceptance

New tests, each failing before the change and passing after:

1. **A failed unsubscribe leaves a pending record** rather than being logged and forgotten —
   with the IM call stubbed to error, at each of the five sites.
2. **The worker retries it to success**, and the record is gone afterwards.
3. **Retry does not depend on `group_member`** — the record must still drain after the
   member row is deleted. This is the property that makes today's job re-run useless, so it
   is the core regression test.
4. **The happy path leaves no row behind** (delete-on-success).
5. **Sub-thread channels drain through the same path** as parent groups.
6. A permanently failing unsubscribe reaches `abandoned` and surfaces on the existing gauges.

Must stay green: the three sites that already survive failure keep their current behaviour
(`api.go:3272` still returns 500; `event.go:655`/`:831` still `commit(err)`).

Gates: full CI E2E lane (44 packages), `-race -shuffle=on` on `modules/space` and
`modules/group`, `golangci-lint`, `make i18n-extract-check`, `make i18n-lint`.
