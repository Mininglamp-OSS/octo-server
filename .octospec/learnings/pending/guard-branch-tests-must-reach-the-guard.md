---
type: Learning
title: "A test for a defensive branch must be shown to reach it — an earlier short-circuit makes it pass for the wrong reason"
description: An acceptance test written for a concurrency guard was satisfied by an unrelated earlier check in the same handler, so it would have stayed green if the guard were deleted.
tags: [testing, concurrency, guard, false-green, space]
timestamp: 2026-07-30T11:25:47+00:00
status: pending
---

# Guard-branch tests must be shown to reach the guard

## Context

`space-join-apply-resubmit` (#683) added a `WHERE id=? AND status=0` guard to
`refreshPendingApplyInvite`, so an applicant re-submitting an invite code cannot
knock a concurrently-approved application back to pending.

The acceptance test drove it over HTTP: approve the application, then have the
applicant POST `/v1/space/join` again, then assert the row is still `status=1`.
It passed.

It would also have passed with the guard deleted. `joinSpace` checks Space
membership *before* it looks at the pending application, and approval has by
then made the applicant a member — so the request returned `already_member` and
never reached the update at all. The test asserted a true fact about the system
via a code path that had nothing to do with the change under test.

## Learning

A test named after a defensive branch is only evidence if the branch is on the
executed path. Handlers accumulate earlier validation, and the check that
short-circuits is usually older and less interesting than the one being added.

Two cheap ways to tell them apart:

- **Delete the guard and re-run.** If the test still passes, it is testing
  something else. (Cheapest and most conclusive.)
- **Test the guard where nothing precedes it.** For a DB-level invariant, call
  the DB method directly against each state it is supposed to reject. Timing
  windows that are hard to construct over HTTP are trivial to construct one
  layer down.

The fix here kept the HTTP test — the user-visible outcome is worth locking —
and added a direct test of `refreshPendingApplyInvite` against approved and
rejected rows, asserting 0 rows affected and an unchanged `invite_code`.

## Applies to

Any conditional UPDATE guard (`WHERE ... AND status=?`), optimistic-concurrency
check, or rollback path whose test drives it through a handler that has its own
earlier validation.
