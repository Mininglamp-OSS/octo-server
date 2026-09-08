---
type: Learning
title: "When a lock order forces work past the commit, the transaction stops being your serializer"
description: "A declared lock order can require a step to run after the transaction that authorized it commits. Every 'the row lock protects this' assumption is void on the far side of that commit and must be replaced explicitly — usually by a CAS lease — and the ordering itself needs an assertion, not a comment."
tags: ["concurrency", "transactions", "lock-order", "mysql", "testing", "hooks"]
timestamp: 2026-09-08T02:30:00Z
source: self
status: pending
---

# When a lock order forces work past the commit, the transaction stops being your serializer

## The rule

Modules that call each other through hooks often have a **declared lock order**:
a fixed sequence in which tables may be locked, so two paths cannot form a cycle
and deadlock. When a hook touches tables that come *later* in that order than the
tables its caller has locked, the hook cannot run inside the caller's transaction
— it would take the later locks under the earlier one and invert the order for
every concurrent writer of those tables.

So the hook runs **after commit**. And then:

1. **The row lock that authorized the work is gone.** Anything you were relying on
   it to serialize — "only one of these can be in flight per row" — is no longer
   serialized by anything. Two concurrent callers can now both reach the hook for
   the same row.
2. **Replace it explicitly.** A CAS on a column (`UPDATE … SET lease = ? WHERE
   lease IS NULL OR lease < NOW()`) is the usual answer: a claim that exactly one
   caller wins, with an expiry so a crashed claimant does not park the row forever.
3. **Every predicate that decides "is this already done" must agree with the
   claim's predicate.** If the lookup asks one question and the CAS asks another,
   the two disagree the first time a third party changes the row — and the failure
   is silent, because a lost CAS is not an error.
4. **Assert the ordering; do not comment it.** The comment is the thing that stays
   true after the code stops being.

## Why (the evidence)

In `Mininglamp-OSS/octo-server`, the project module declares
`space_member → space → project → group → group_member → octo_project_member`.
Provisioning a project's group touches `group` and `group_member`, so the hook
had to run after the project transaction committed. Three consequences, all real:

* **The lease was not optional.** Two concurrent `members/add` calls on a project
  with no group both reach the rebuild, and nothing else serializes them. Without
  the CAS lease they build two groups; the project can point at one.
* **Two predicates disagreed and the rebuild silently stopped happening.** A
  review round tightened the *lookup* ("does this project have a usable group?")
  to require the group still be attached to this project, but left the *claim*
  keyed on the pointer being empty — and a separate cascade releases a group
  without clearing the pointer. Lookup said "no group", claim said "already has
  one", the CAS failed on every attempt with no error and no metric, and the
  reconcile scan reported a project that nothing could repair. The shape is
  exactly the bug the round was fixing.
* **The ordering assertion found a gap the comment had not.** The spec asked for a
  case pinning that the hook runs only after commit; nothing checked it. The fix
  is cheap and general: **from inside the hook, read the row on a pooled
  connection.** A different connection cannot see an uncommitted row, so
  visibility *is* the proof. Registered as a cleanup on the test double, every
  case that installs the double carries the check.

## How to apply it

- When a hook or registered step runs across a module boundary, write down which
  tables it touches and where they sit in the declared order **before** deciding
  where to call it from.
- If it must run after commit, say so at the registration point and name the two
  things that follow: the caller's lock no longer serializes it, and its failure
  must not fail the caller (it already committed).
- Give the claim an expiry and release it on failure, so a failed attempt does not
  block the next write path for the whole lease window.
- Write the ordering assertion as a read on a pooled connection from inside the
  hook, and attach it to the shared test double rather than to one dedicated case
  — then every case that uses the double pays for the check.
- Mutate the assertion once to watch it fail. An ordering guard that cannot fail
  is a comment with a `func Test` in front of it.
