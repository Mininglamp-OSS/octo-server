---
type: Learning
title: "Mutation-check the assertion, not just the guard"
description: Deleting the code under test only proves the test notices its absence. It does not prove the test notices the defect coming back in a different shape. Re-introduce the behaviour the way a future change plausibly would, not the way you just deleted it.
tags: ["testing", "review"]
timestamp: 2026-08-23T11:00:00Z
# --- octospec extension fields ---
source: self
origin_task: space-removal-creator-handover-notice
origin_pr: none
status: pending
candidate_rule: testing
---

# Mutation-check the assertion, not just the guard

## Context

A design decision was that removing an ordinary member from a Space must produce
**no** group-visible message. The guard:

```go
assert.Equal(t, 0, countGroupVisible(stub.sentPayloads(), "受害者小明"),
    "普通成员被移出 Space 不得产生全群可见消息")
```

Mutation check: re-add a broadcast and confirm the test goes red. It did not.

`countGroupVisible` matches a fragment against the payload's `content`. The
mutation re-added the broadcast the way the codebase's other system messages are
written — name in `extra`, `content` holding only `“{0}”…` — so the victim's name
never appeared in `content` and the fragment never matched. The exact defect the
guard existed to prevent walked straight through it.

The fix was to stop matching text and count the property instead:

```go
// group-visible AND carrying any visible content, excluding the type=99 CMD
func countGroupVisibleTips(payloads []map[string]any) int
```

That version goes red for either shape.

## The rule

A mutation that reverts your own diff is the weakest possible one: you wrote the
assertion and the mutation against the same mental model, so of course they
match. It confirms the test is wired up. It does not confirm the test is
*sufficient*.

Add at least one mutation that reintroduces the behaviour **differently** —
plausibly, the way the next person would write it without having read your diff:

- data moved to a different field (string → structured, `content` → `extra`)
- a different but equivalent primitive (hand-rolled payload → library call)
- the same effect on a different code path (a sibling caller, a retry branch)

If the guard only fails for the one spelling you deleted, it is pinned to your
implementation rather than to the invariant.

## Heuristic

An assertion that matches on **text** is pinned to a spelling. An assertion that
matches on **shape** — how many, addressed to whom, carrying what kind of thing —
is pinned to the invariant. Prefer the second whenever the invariant can be
expressed structurally, and when a fragment match is unavoidable, mutate by
re-expressing the data rather than by deleting it.

## Addendum — and the mutation has to be the right one, and faithful

Two further ways this went wrong on the same change, both caught by reviewers
rather than by me.

**Aim the mutation at the decision, not at the feature.** The load-bearing choice
was *where* a message is sent from (inside the transaction-committing helper,
versus from its caller after a step that can fail). The mutation I ran deleted the
message entirely — red, and meaningless: it proves the test notices a missing
message, not that it notices the message moving. Two reviewers ran the placement
mutation and watched five tests stay green. Ask what the code comment warns the
next editor *not* to do, and mutate exactly that.

**A mutation must be faithful to the shape it impersonates.** My first attempt at
the placement mutation passed the successor through a package-level variable,
which survived across calls; the real "announce from the caller" shape uses a
local, reset per call. The stale global made the second run announce, so the test
stayed green — for a reason that had nothing to do with the code under test. A
green mutation result is only evidence if the mutation is a plausible
implementation. Reset state, match scoping, then trust the outcome.

## Addendum — adding a recovery layer can defang a guard you already verified

A later round of the same PR added a bounded deadlock retry around the batch
removal, to close a cross-path AB-BA. An existing guard,
`TestRemoveMembersLockedNoDeadlockOnReversedOverlap`, asserts that two reversed
overlapping batches produce **zero** deadlocks. It called the public function —
which now routed through the retry. The retry swallows exactly the 1213s the
assertion counts.

Measured, with a real regression (per-uid caller-order locking) mutated back in:

| guard calls | deadlocks seen / 80 calls | verdict |
|---|---|---|
| `removeMembersLockedOnce` (unwrapped) | 47, 48, 48 | red every run |
| the retried wrapper, 5 attempts (as shipped) | 0, 1, 0 | **passed 2 of 3 runs** |

The guard had not been deleted, moved, or edited. It still ran, still asserted the
right invariant, and had been mutation-verified when written. A change to code it
called *through* turned a detector into a coin flip. Nobody touching the retry had
any reason to look at a test whose name is about deadlock ordering.

## The rule

When you introduce a layer that **absorbs failures** — a retry, a fallback, a
circuit breaker, a `recover()`, a default-on-error — the failures it absorbs
become invisible to every test above it. So: enumerate the existing tests that
call through the new layer, and for each one ask whether it asserts on something
the layer can now hide. Those tests must bind to the *inner* function, below the
absorbing layer.

Two corollaries worth stating separately:

- **The retry gets its own test, and the invariant keeps its own.** Testing "the
  retry recovers" through the same entry point that tests "no deadlock occurs"
  collapses two independent claims into one, and the recovery masks the invariant.
- **A near-miss is a miss.** After reducing the retry to 3 attempts the wrapper
  version did go red — with 1 deadlock out of 80, against 48 for the unwrapped
  call. It "passed" the check I first ran, and I nearly wrote down that the
  reviewer's finding was unconfirmed. Sensitivity is the measurement, not the
  pass/fail bit: a guard that fires on 1/80 instead of 48/80 is one scheduling
  change away from silence.
