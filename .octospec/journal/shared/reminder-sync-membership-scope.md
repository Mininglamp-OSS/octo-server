---
type: Journal
title: "Journal: reminder-sync-membership-scope"
description: Scoping channel-level reminders to real membership, and why the pentest report's root-cause attribution would have produced a false fix.
tags: ["space", "isolation", "acl", "sql", "collation"]
timestamp: 2026-08-07T16:19:08Z
slug: reminder-sync-membership-scope
---

# reminder-sync-membership-scope

## What was wrong

`POST /v1/message/reminder/sync` returned **every** channel-level (`@所有人`)
reminder in the system to any authenticated caller — the `channel_id`,
`publisher`, `message_id`, `message_seq` and timestamp of channels the caller
had never joined. Cross-tenant metadata leak; enough to enumerate the group
graph and who broadcasts in it.

`remindersDB.sync`'s `uid=''` branch carried no channel-membership predicate.
With `channel_ids: []` there was not even a `channel_id IN (?)` constraint, so
the query returned the whole table minus the caller's own broadcasts and
already-read rows.

## Why the report's attribution was wrong

The 2026-07-30 retest filed this as §4.11 "逻辑漏洞垂直越权", reproduced by
deleting the `X-Space-Id` header: with the header → `403 无权访问该 Space`,
without it → `200` and a 26 KB dump.

That reproduction is real but the diagnosis is not, and following it would have
shipped a false fix:

- `reminderSync` never reads `space.GetSpaceID(c)`. `SpaceMiddleware` was the
  only Space gate on the route, and it fails open when no `space_id` is present
  (`pkg/space/middleware.go:112`). Deleting the header only walks past that
  gate.
- The leak itself has nothing to do with Space. A caller who *is* a legitimate
  member of any one Space keeps the header, passes the middleware, and gets the
  identical full dump.
- `reminders` has no `space_id` column at all — only `channel_id`. A
  space-scoped fix was never possible here.

Had the middleware been made fail-closed, the report's exact reproduction would
have turned green while the vulnerability stayed fully open. **The primary
acceptance test therefore keeps a valid `X-Space-Id` header** and still requires
non-member channels to be absent — a middleware-only fix cannot pass it.

## What was changed

- `modules/group/db.go` — `QueryActiveMemberGroupNosWithUID`, the caller's full
  active-group set. Predicate mirrors `ExistMemberActive` exactly
  (`is_deleted=0 AND status=Normal`), so a blacklisted or departed member is not
  a member. Selects `group_no` only.
- `modules/group/service.go` — exposed as `IService.ActiveMemberGroupNos`. The
  interface addition is safe: the only stub (`modules/robot/
  stream_card_gate_test.go`) embeds `group.IService` rather than implementing
  it.
- `modules/message/db_reminders.go` — `channelLevelVisibility` builds the
  channel-level predicate; `sync` takes the member set and applies it. The four
  near-duplicate `Where` branches collapsed into one composed predicate.
- `modules/message/api_reminders.go` — resolves the member set before the query.
  A membership lookup failure aborts the request; it must not degrade to an
  empty set (legitimate users silently lose red dots) nor skip the filter (the
  boundary opens on a DB blip).

## Decisions worth keeping

### Bind parameters, not a JOIN to `group_member`

The obvious implementation is `EXISTS (SELECT 1 FROM group_member …)`. It was
rejected on a schema hazard:

`reminders` was force-converted to `utf8mb4_general_ci` by
`20260711000001_reminders_channel_mention_index.sql`. `group_member` declares no
`CHARSET`/`COLLATE` at all (`20191106000002_group_legacy01.sql:33`) and inherits
the *database* default at creation time, and it is not in the
`20260512000001_base_oss_compat_repair.sql` normalisation set. On a deployment
whose database default is MySQL 8's `utf8mb4_0900_ai_ci`, the two tables differ
and a column-to-column comparison raises Error 1267 — in the reminder-sync hot
path. Pinning `COLLATE` to dodge it is the trap that same migration documents:
it destroys index ordering and degrades to a full scan.

Comparing a column against **bound literals** has no such problem — literals
coerce to the column's collation. `TestRemindersSync_SurvivesGroupMemberCollationMismatch`
builds `group_member` as `utf8mb4_0900_ai_ci` against a `general_ci` `reminders`
and pins this; reverting to a column-to-column join turns it red.

Note the local test database is created `utf8mb4_general_ci`, so simply running
the suite could never have surfaced this — the collations match *because the
setup chose them*. The hazard had to be reasoned from the migration history and
then reproduced deliberately.

### Only channel types 2 and 5 — a knowingly-retained residual

`getReminders` copies `ChannelType` straight off the message
(`api_reminders.go:247`) and `hasMention` (`:427`) does not look at channel type
at all, so all six `common.ChannelType` values can structurally produce a
channel-level reminder. But octo-server has no general membership table
(conversations live in WuKongIM; `conversation_extra` is metadata, not
authority). Membership is resolvable only for Group and CommunityTopic.

Requiring `group_member` for Person / CustomerService / Community / Info would
silently drop their legitimate reminders — a functional regression, not a
security gain. Those four are left unfiltered, with maintainer sign-off, and
`TestChannelLevelReminderChannelTypes` pins the difference between "types that
emit channel-level reminders" and "types the predicate covers". Narrowing the
gate, widening the emitter, or adding a channel type all turn it red, forcing a
human to re-decide rather than letting the boundary drift.

### The client's `channel_ids` narrows, never widens

The list is caller-controlled, so it can only ever intersect the authorized set.
The empty list now means "no channel filter requested", not "no filter at all" —
that conflation was the bug. The web client builds it from its conversation
list (`octo-web/packages/dmworkdatasource/src/im-callbacks/reminders.ts:15-28`);
`[]` is a hand-made shape.

### Filtering stays in SQL

Same reason YUJ-1377 gives for the publisher exclusion: post-filtering in Go
leaves the version/limit cursor parked on hidden rows, and the client re-requests
the same version forever.

## Verification

- `go test ./modules/message/` — **pass** (fresh database), including 9 new tests.
- `go test ./modules/group/` — 7 failures, **all pre-existing**: the failing test
  names are byte-identical to a clean `origin/main` (2d8c875) worktree run
  against a fresh database.
- `go build ./...`, `go vet` — pass. `gofmt -l` count unchanged at the repo's
  pre-existing 18 (only the two files authored here were formatted; untouched
  files were left alone).

### EXPLAIN, 50 000 rows

| | type | key | rows | Extra |
|---|---|---|---|---|
| before | ALL | NULL | 50037 | Using where; Using filesort |
| after | ALL | NULL | 50037 | Using where; Using filesort |

Plan-neutral. The brief's original acceptance said "the plan must not be a full
scan"; that was rewritten after measuring, because **it was already a full scan
on `main`** — the top-level `OR reminders.uid` plus `ORDER BY version` against a
table with no `version` index. Holding this change to a bar its baseline never
met would only have forced an out-of-scope index migration.

Two follow-ups fall out of that measurement, neither actioned here:

1. `reminders` has no index supporting `version > ? ORDER BY version`. Every
   reminder sync full-scans. Pre-existing and unrelated to this fix.
2. Because the membership predicate rejects rows, a caller in few groups now
   scans further to fill `LIMIT`. Inherent to any correct fix, but it interacts
   with (1): the cheapest real mitigation is the missing index, not a weaker
   predicate.

## Not fixed here

- `SpaceMiddleware`'s opt-in fail-open. Documented old-client compatibility
  across conversation / message sync / reactions / search / pinned; needs
  per-route evaluation. This endpoint no longer depends on it.
- The same fail-open shape in DM Space filtering
  (`modules/message/space_filter.go:593` and `:482`, `api.go:1973` / `:2165`,
  `api_message_get.go:262`). Real, but a different blast radius — a caller only
  ever reaches DMs they are already a party to.
- Retest §4.10 `real_name` disclosure: YUJ-413 product decision
  (`modules/user/api.go:3893`), not an oversight. Needs a product call.
