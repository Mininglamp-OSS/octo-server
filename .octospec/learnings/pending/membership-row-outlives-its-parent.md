---
type: Learning
title: "A membership row outlives its parent — join the parent's status, don't trust the association table alone"
description: Relationship/authorization checks that query only an association table (group_member, space_member, …) fail-open when the parent is soft-disabled and rows are cleaned up asynchronously. Join the parent and require its active status; never let an async cleanup be an authorization precondition.
tags: [auth, acl, sql, soft-delete, async-cleanup, fail-closed, isolation, security]
timestamp: 2026-08-09T00:00:00Z
status: pending
---

# A membership row outlives its parent

## Context

`channelGet` graded a caller's access to a peer's profile partly on "do they
share a group". The first cut, `ExistCommonGroup`, self-joined `group_member`
on `is_deleted=0` and nothing else.

But `disband` (group dissolution) only flips `group.status = 2` in its main
transaction; member cleanup is an **async event**. So two users who share only a
*dissolved* group still have live `group_member` rows (`is_deleted=0`) during
the cleanup window — or forever, if the async job fails. The check returned
"common group = true" and handed over full profile detail.

The same shape had already bitten `reminder-sync`: a channel-level reminder
query over an association table with no parent-membership predicate leaked
cross-tenant rows.

## The rule

When an authorization or relationship decision reads an **association table**
(`group_member`, `space_member`, subscription, share, …), the parent entity's
lifecycle state is part of the predicate. Join the parent and require its active
status:

```sql
INNER JOIN `group` g ON g.group_no = m.group_no
WHERE ... AND g.status <> <Dissolved>
```

**Exclude only the state your rationale covers.** The first cut of this used a
`status = Normal` whitelist, which also excluded an admin-**disabled** group — a
distinct, reversible state that every other liveness check in that module treats
as live. A whitelist silently widens the exclusion every time someone adds a new
status value; a blacklist on the state you actually reasoned about does not.
Match the module's existing convention rather than inventing a stricter one in
one query.

Corollary: **an async cleanup is never an authorization precondition.** If
"the rows will be gone soon" is what keeps a check correct, the check is
fail-open for the whole window (and permanently if the job dies). Encode the
invariant in the query, not in a worker's eventual success.

Ask, for every association-table check: *what happens to this row when the
parent is soft-deleted / disabled / dissolved?* If the answer is "a background
job removes it later", your `WHERE` clause is missing a parent-status predicate.

## Applies to

Any read/authz keyed on membership or linkage where the parent can be
soft-disabled: group/space membership, channel subscriptions, resource shares,
bot/app bindings, invitation validity. Prefer an existing "active" helper over
the bare `is_deleted=0` variant when the check is load-bearing for access — but
check what that helper's status predicate actually excludes before reusing it.

Reference: `modules/group/db.go` `ExistCommonGroup`;
sibling case `modules/message/db_reminders.go` (reminder-sync-membership-scope).
