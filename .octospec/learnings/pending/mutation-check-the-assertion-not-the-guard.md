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
