---
type: Learning
title: "A cleanup predicate must not be written in the tense of the state it cleans up after"
description: Deferred cleanup runs after the state change that triggered it, so scoping it with an is-currently predicate (status=1, is_active) can return the empty set and make the cleanup a silent no-op. The failure is invisible - the job completes successfully having done nothing. Scope deferred work with was-ever predicates, and test the case where the trigger has already cascaded further.
tags: ["space", "isolation", "data-integrity", "concurrency", "review"]
timestamp: 2026-08-21T05:30:00Z
# --- octospec extension fields ---
source: self
origin_task: space-member-removal-cleanup
origin_pr: none
status: pending
candidate_rule: space-isolation
---

# A cleanup predicate must not be written in the tense of the state it cleans up after

## Context

Space member removal enqueues a durable job that later cuts the removed member
out of the Space's groups and DMs. The job scoped its DM peers with a helper
whose SQL read:

```sql
SELECT sm.uid FROM space_member sm
INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1
WHERE sm.space_id = ? AND sm.status = 1 AND sm.uid IN ?
```

That predicate is the natural way to ask "who are this Space's members", and it
is correct for every *request-time* caller. It is wrong for the cleanup, because
the cleanup by construction runs **after** the membership row has been zeroed:

- **Disband** zeroes `space.status` as well, so the `INNER JOIN ... s.status = 1`
  alone empties the result.
- **Batch removal** zeroes the peer's row too, so each member's job filters the
  other one out and the DM between them is never cut.

In both cases the job ran, found nothing to do, and was marked done. Nothing
errored, nothing logged, and the isolation the feature exists to enforce simply
did not happen. It took an adversarial review round to surface it, and the tests
that existed all passed because each of them removed exactly one member from a
Space that stayed active.

## The shape of the mistake

Deferred work is triggered *by* a state transition and therefore always observes
the **post**-transition world. Writing its scope with a present-tense predicate
("is an active member", "is enabled", "is not deleted") silently asks about a
state the trigger already destroyed — and worse, it fails *open* in the
direction that looks like success.

The same trap widens as the trigger cascades: a predicate that survives a single
removal ("the peer is still active") breaks the moment two removals are batched
or the parent object is torn down in the same transaction.

## Guidance

- Scope deferred cleanup with **was-ever** predicates — the existence of the
  association, not its current status. Name them so the tense is explicit
  (`MembersEverInSpace` vs `ActiveMemberSet`) and document why the status
  predicate is deliberately absent, or the next reader will "fix" it back.
- Keep the **authorization decision** separate from the **scoping query**. Scope
  answers "is this in range"; a per-item re-evaluation answers "should this
  actually be cut". Mixing them means loosening the scope loosens the policy.
- Any request-time helper reused by deferred work needs a second look: the two
  callers are asking different questions in different tenses. If both are needed,
  ship both with names that cannot be confused.
- Test the **cascaded** cases explicitly, not just the single-item happy path:
  the parent already torn down, and two related items removed in one batch. Those
  are the two that return the empty set.

## Detection

A cleanup job that completes successfully while doing nothing leaves no trace.
Worth asserting positively in tests ("N whitelist removals were issued"), not
just negatively ("no error"). A silent-success path deserves the same scrutiny as
an error path.

---

**Where the example lives now.** The DM half of that work was split out of PR #795
into `space-member-dm-isolation`; this learning is kept here because the rule is
about cleanup-scope tense, not about DMs, and the group cascade in the shipped half
follows the same rule.
