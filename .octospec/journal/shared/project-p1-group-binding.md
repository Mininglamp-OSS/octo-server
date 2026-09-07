---
type: Journal
title: "Learning: binding groups to projects, and what a write-path convergence actually costs"
description: "Converging eleven group-admission paths onto one entry to make invariant I2 real; the migration-placement rule that took two wrong answers to find; three places where the brief was wrong about shipped code; and the review round that showed a two-phase state machine re-opens every status check in the module."
tags: ["octospec-learning", "space", "isolation", "acl", "migration", "testing", "wire-contract", "mysql", "collation"]
timestamp: 2026-09-06T18:00:00Z
source: self
---

# Learning: project-p1-group-binding

## What changed

A group can now belong to a project (`group.project_id`), and invariant I2 — a
project group's active members are a subset of that project's active members —
is enforced by one admission entry that all eleven previous admission paths were
converged onto. Four source guards keep it that way, a per-entry-point metric
shows a path that stopped enforcing, and three reconcile scans report violations
that got in anyway.

Around it: a two-phase project-seat close with its own outbox and worker, a
reverse-registered group detach (including the group's normal owner handover
when the departing member owns it), the create parameter gated behind the
feature switch, that switch shipped to clients through appconfig, and the
point-query read contract on `/v1/auth/verify`.

## What we learned

### A migration's home is a dependency question, not an ownership question

`group.project_id` was placed in `modules/project/sql` on the brief's reasoning
that "the module that owns the concept owns the column", citing `group.space_id`
having been added from `modules/space`'s own directory. That took two wrong
answers to correct, and each was found the same way — an entire test package
failing with `Error 1054: Unknown column`, never by reasoning about it:

* in `modules/project/sql`: `modules/group`'s test binary has no project
  migrations, so 82 group and channel tests failed at once;
* in `modules/group/sql`: `modules/space`'s binary has no group migrations, so
  the Space settings endpoint failed the moment it read the column.

The rule is: **a column ships from a migration directory present in every binary
that reads it.** Three modules read this one, and `go list -deps` says
`modules/space/sql` is the only directory in all three. That is also the real
reason `group.space_id` lives there — the cited precedent was right, its
explanation was not.

### A guard is worth more than the invariant it guards

Four of the defects in this change were caught by guards, not by tests of the
feature:

* P0's `TestNoWriteAuthorisingAggregateIsANonLockingRead` caught a
  transaction-scoped non-locking read in the new job claim;
* P0's migration test caught an apostrophe in a SQL comment that broke its naive
  statement splitter;
* P0's cursor-coverage guard caught two new reconcile cursors missing from the
  test reset;
* the new I2 test caught `addOneMemberOnce` short-circuiting on `status == 1`
  and therefore never cancelling an in-flight cascade — the silent one, since
  the API returned OK, no metric moved, and the reconcile scan exempts exactly
  that state.

The last one is the pattern worth keeping: a two-phase state machine makes every
existing `status == active` check a question that has to be re-answered, and the
ones that answer wrongly fail silently in the safe-looking direction.

### The brief was wrong about shipped code in three places, and right to be doubted

Written against P0's code rather than the design document, the brief still
carried three claims that measurement contradicted:

1. **"P0's Space cascade disbands ownerless projects."** It does not. No such
   metric exists, `disbandProject` has one caller, and P0's own comment says
   making it visible was the whole treatment. There is no cascade branch to
   reach.
2. **"Three DAO methods are dead."** Two are. `DeleteMember` has a test caller.
3. **"`context_included` staying true on a failed lookup is a live defect."** It
   is not a defect, and fixing it as written would have opened a trust hole: the
   flag means "this server speaks the v2 contract", and a consumer reading false
   falls back to trusting the client-supplied `X-Space-Id`. The real gap — a
   consumer cannot tell a failure from an honest empty answer — is filled by a
   separate additive field.

A spec written against code still ages, and the parts that age worst are the
ones describing what another change *will have* shipped.

### An acceptance criterion can contradict a design decision

D3 said the admission primitives should become unexported. The same brief's
non-regression acceptance said no existing test file may be edited. Forty-one
test files in the protected packages build fixtures through those primitives, so
both cannot hold literally. A source guard delivers the intent — callable only
from the funnel — without touching the fixtures, and "callable only from" turns
out to be a property a guard asserts better than the compiler's export rules do.

### Exemptions have to be exempt from the whole gate

System bots were exempted from the project half of the composite gate and left
subject to the Space half. Platform bots have no `space_member` row at all, so
that "exemption" refused them from every project group — the exact opposite of
its purpose. An exemption that applies to one half of a conjunction is not an
exemption.

## What the first review round changed

Six high-priority findings, and **four were one shape**: a two-phase state
machine turns every existing `status == active` check into a question that has to
be re-answered, and the ones that answer wrongly fail silently in the
safe-looking direction. The journal above already recorded that pattern from one
instance; the review found it was systemic.

* `status = 0 AND removing = 1` — the combination the schema documents as MUST
  NOT EXIST — was reachable from two writers, and unrecoverable once reached:
  `finishMemberRemovalTx` is guarded on `status = 1 AND removing = 1`, so nothing
  could ever match the row again, while the stall scan has no status filter and
  alerted on it every tick with no remedy.
* A closing seat kept `actorRoleTx`'s answer, so a departing owner held disband
  for the whole cascade window — on the in-transaction re-read whose only job is
  to catch a role the middleware cache got wrong.
* `promoteSuccessorTx` accepted a closing successor while `countActiveOwnersTx`
  already excluded one, so the last-owner guard could be satisfied by an owner
  who was disappearing: the outcome the guard exists to prevent, reached through
  the guard itself.

### "Self-correcting" is a claim that needs proving

The I2 cursor advanced to the page's last GROUP id, so members past a page
boundary were skipped, and a comment asserted the next rotation would catch them.
It would not: the pages fall on the same boundary every time. A composite
`(group_id, uid)` cursor was always available — P0's I1 scan already used that
shape over `(project_id, uid)` — and the single-column version had also needed a
second, differently-filtered query, which double-counted.

### The COLLATE rule is about which SIDE a column is on

Two mistakes in one file, in opposite directions, both found by a test rather
than by reasoning: a legacy ⟷ legacy join carried a COLLATE it did not need (and
lost its index for it), and `space_member_removal_cleanup` was treated as legacy
when it is migration-created and pinned. A comparison in the SELECT list raises
1267 exactly as one in an ON clause does.

The second half only surfaced because the drift test ran the REAL query methods
against a deliberately drifted database. The general form is in
`learnings/pending/project-p1-group-binding.md`, including the measurement that
settles when a COLLATE can replace a compatibility flag and when it cannot: it
depends on which table DRIVES the join, not on whether the error went away.

### Apostrophes pair up

The migration test splits statements naively and treats a quote as a string
delimiter, so a file with an EVEN number of apostrophes can pass by luck. It
failed here because an unrelated edit forty lines away removed one and flipped
the parity — and then pointed at an entirely innocent statement. The fix is not
to count them; it is to use none.

## What we did not deliver

The stale-subscriber reconcile scan. There is no subscriber-listing call in
octo-lib's shared client, and the pinned broker only exposes one on its
management plane (`/cluster/channels/{id}/{type}/subscribers`), which needs
manager credentials and moves with the admin UI. Delivering it would mean either
extending octo-lib or coupling a reconcile job to the broker's management API.
The consequence is stated rather than glossed: the IM-unsubscribe leak P1
deliberately inherits stays unmeasured at project granularity. It should land
with whichever of #797 / `im-pending-outbox` adds the capability.
