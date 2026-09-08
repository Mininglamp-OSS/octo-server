---
type: Learning
title: "A green test can encode the premise instead of testing it"
description: A fixture that sets up the world the way the assumption describes will pass whether or not the assumption holds in production. The test is not vacuous and not wrong — it is simply blind to the case it was written to rule out. Check what the fixture assumes, and check who calls the primitive you cite as a guarantee.
tags: ["testing", "review", "design"]
timestamp: 2026-08-24T03:00:00Z
# --- octospec extension fields ---
source: self
origin_task: space-removal-creator-handover-notice
origin_pr: Mininglamp-OSS/octo-server#804
status: pending
candidate_rule: testing
---

# A green test can encode the premise instead of testing it

## Context

A batch member removal could announce a chain of already-obsolete group-owner
handovers. The fix suppressed a link when its elected successor was itself queued
for removal. The justification, written into both the code comment and the brief:

> a batch is enqueued atomically in one transaction
> (`enqueueMemberRemovalCleanupBatchTx`), so every sibling job's pending row exists
> before any worker starts

`TestGroupCascadeBatchHandoverAnnouncesOnce` asserted exactly one notice for a
3-uid batch, and passed.

It passed because its fixture called `seedPendingRemovals(batch)` **before** the
loop — inserting every sibling row up front. That is precisely the state the
premise describes. The test could not fail for the case it existed to rule out,
because the fixture *was* the assumption.

In production, the cited function had two callers, both on the disband path — which
suppresses the announcement anyway, so the primitive named in the justification
never protected an announcing path at all. The busiest path that *does* announce
enqueues one transaction per uid. A worker tick landing mid-loop sees the not-yet-
enqueued successor as "not queued" and announces the obsolete link. Measured: 1
notice with rows seeded up front, 2 with a single tick after the first commit, 3
fully interleaved.

Three reviewers found it independently; two of them had already approved that same
head, and one said explicitly that trusting the green test is what caused the miss.

## The rule

When a test's fixture establishes the same condition the code's justification
assumes, it verifies the implementation *given* the premise. It says nothing about
whether the premise holds. Ask of every fixture: **is this setup a fact about
production, or is it the assumption I am trying to validate?**

Two cheap checks that would each have caught this:

- **Grep the callers of any primitive you cite as a guarantee.** The claim named a
  function by name. `grep` on it would have shown two callers, both on a path where
  the guarantee is irrelevant. Naming a function is not the same as checking it is
  reached.
- **Enumerate the entrypoints, not the happy path.** The invariant held for two of
  three ways in; the third was the default user-facing one. A table of
  entrypoint → transaction granularity → reason → does-the-premise-hold makes the
  gap immediate, and is now in the brief.

## Relation to the other learnings from this task

`mutation-check-the-assertion-not-the-guard.md` says to aim the mutation at the
decision. This is the layer above: a mutation of the *code* cannot reach a false
premise in the *fixture*. To test a premise you have to vary the fixture — here,
enqueue-as-you-go instead of seed-up-front — which is what the reviewers did.
