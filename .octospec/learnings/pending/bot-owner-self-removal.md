---
type: Learning
title: A predicate that ignores out-of-scope subjects is safe to grant with and unsafe to revoke with
description: checkBotOwnership returns nil for every non-bot UID, which is correct when gating who may invite a bot and a privilege escalation when reused to gate who may remove one. Ownership predicates are directional; reusing one across a grant/revoke boundary needs the polarity re-derived, not assumed.
tags: ["security", "auth", "acl", "authorization", "review", "predicate-polarity"]
timestamp: 2026-08-23T13:13:43Z
status: pending
source: bot-owner-self-removal
---

# A predicate that ignores out-of-scope subjects is safe to grant with and unsafe to revoke with

## What happened

`checkBotOwnership(session, inviterUID, memberUIDs)` (`modules/group/bot_ownership.go`)
gates who may invite a bot into a group. Its query is:

```sql
SELECT u.uid, IFNULL(r.creator_uid,'')
FROM user u LEFT JOIN robot r ON r.robot_id = u.uid AND r.status = 1
WHERE u.robot = 1 AND u.uid IN ?
```

Human UIDs match no rows, so the verdict loop never rejects them. The doc says so
outright: `user.robot=0 (human) → always OK`. On the invite path that is exactly
right — the function's job is "of the bots you named, are they all yours?", and
non-bots are some other guard's problem.

The task at hand was the mirror-image feature: let a bot's owner **remove** their
own bot from a group. The obvious move — the one the function's name invites — is
to call the same helper on the removal path. That would have shipped a privilege
escalation: an ordinary group member submits a batch of *human* UIDs, every one
of them matches no row, the loop rejects nothing, and the member kicks people out
of a group they have no authority over.

The fix was not to patch `checkBotOwnership` but to use a different shape:
`QueryBotUIDsOwnedByUIDs(groupNo, ownerUIDs)` returns the closed set of bots that
are in this group **and** owned by this caller, and every target must be in that
set. Default-deny instead of default-ignore.

## The generalizable shape

The two functions answer questions that look alike and differ in polarity:

| | question | out-of-scope subject |
|---|---|---|
| grant path | "is anything here **not** yours?" | ignore it — someone else's guard owns it |
| revoke path | "is everything here yours?" | **reject** — it is not on your whitelist |

An "ignore what I don't recognize" predicate is safe wherever failing to object
means some other check still runs. It is unsafe wherever the predicate is the
last thing between the caller and the write, because there "no objection" reads
as "authorized".

This is not specific to bots. The same trap exists for any helper written as
`for each recognized subject: if not mine → deny` — scope filters, tenant
filters, resource-type filters. The name tells you the domain; it does not tell
you which way it fails on the subjects it skips.

## Proposed rule

Before reusing an authorization helper on a code path it was not written for,
derive its behaviour on the inputs it does **not** recognize, and state which of
the two shapes it is:

- **default-ignore** (silent pass for unrecognized subjects) — only valid where a
  further check is guaranteed to run.
- **default-deny** (whitelist; anything not explicitly permitted is rejected) —
  required wherever the predicate is the final gate before a mutation.

A single test pinning the skipped class is what keeps this from regressing: here,
"an ordinary member cannot remove a human member via the self-service path". That
test fails loudly the moment someone swaps the whitelist back for the
name-alike helper.

## Two smaller candidates from the same task

Both are narrower and may belong as amendments to existing rules rather than as
rules of their own:

1. **A new authorization predicate must use the active-member form**
   (`ExistMemberActive`, `is_deleted=0 AND status=Normal`), not the
   `is_deleted`-only form. `db.go`'s `QueryActiveMemberGroupNosWithUID` already
   documents this, but phrased as guidance for *replacing existing call sites*;
   this task showed it applies just as much to predicates being introduced. The
   miss let a blacklisted member reach a group-mutating write. Candidate
   amendment to `rules/space-isolation.md`.

2. **In this repo's `modules/group` tests, assert system messages at the service
   layer, never through the HTTP route.** `register.GetModules` (octo-lib
   `pkg/register`) builds module instances under a process-wide `sync.Once`, so
   every handler in a test binary keeps the ctx of the *first* `NewTestServer`.
   An IM stub installed on any later test's ctx is silently bypassed — the
   symptom is a test that passes alone and fails in company. Candidate amendment
   to `rules/testing.md`.
