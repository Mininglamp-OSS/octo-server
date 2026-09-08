---
type: Learning
title: "Learning: centralising a scattered check must preserve which side yields, not just that the check happens"
description: Replacing N hand-rolled pairwise guards with one symmetric rule looks like strictly more coverage, but the scattered originals also encoded WHICH side loses. Symmetry silently converts a contained misconfiguration into an outage on the next deploy.
tags: ["refactor", "auth", "config", "trust-boundary", "availability", "review"]
timestamp: 2026-09-07T18:00:00+08:00
# --- octospec extension fields ---
task: internal-token-registry
status: pending
---
# Centralising a scattered check must preserve which side yields

## What happened

Four capabilities each owned a fixed internal-token env, and each hand-rolled a
"must differ from sibling X" guard against whatever siblings existed when it was
written — 0, 1, 2 and 3 siblings respectively. The stated goal of the refactor
was to make coverage complete by construction: one registry, one resolve
function, compare against **every other** registered env.

That symmetric rule is strictly more coverage than the ladder it replaced. It was
also, operationally, strictly worse.

The scattered guards had a second property nobody had written down: they were
directional. `OCTO_DOCS_NOTIFY_TOKEN` yielded to `NOTIFY_INTERNAL_TOKEN`, never
the reverse. So a deployment that had mistakenly set both to the same value ran
with docs disabled and the **legacy ingress still serving** — and had been doing
so, quietly, for as long as the mistake existed.

Under the symmetric rule both sides resolve to `""`. The auth middleware's
"nothing configured" branch then rejects *every* request to the whole
`/v1/internal` notify group. Same misconfiguration, same invariant satisfied,
but a working production ingress goes dark on the next rolling deploy — with no
boot failure and no readiness signal, only two log lines.

## Why it was easy to miss

The symmetric version is easier to justify in the abstract. "When one secret
opens two doors, neither door should stay open" is a clean sentence, and it is
even true as a security argument: the invariant ("one credential grants exactly
one capability") is satisfied either way.

What the sentence hides is that the invariant was **already satisfied** by the
asymmetric rule. Disabling one side is sufficient. The second disable buys no
additional containment — it only spends availability.

Every test passed in both designs, because no test encoded "and the other one
keeps working". The scattered originals expressed it only as the *direction* of
a comparison, which is exactly the kind of detail that evaporates when four
call sites are folded into one.

## The rule

**Before folding N scattered guards into one, recover what the scattered
versions decided in addition to what they checked.** Ask specifically:

1. For each existing guard, *which* side does it disable, and would the unified
   rule disable a different or larger set?
2. What does a currently-misconfigured-but-serving deployment do on the next
   deploy under the new rule? Not "is the invariant held" — it was held before —
   but "what stops working that was working".
3. Is the extra strictness buying containment, or only cost?

For this case the answer was a **precedence order**: registration position
decides who yields, and Resolve compares against every env registered *before*
it. That recovers both properties at once —

- every unordered pair is still compared exactly once, by whichever side was
  registered later, so appending an entry needs no edit anywhere else; and
- the incumbent keeps serving, the newcomer that duplicated an existing secret
  is the one disabled — precisely what the four ladders already did.

The ladder built by accident was the right semantics. What was wrong was that it
lived in four places and depended on each author's memory. **The fix was to make
it a property of the data, not to replace it with something tidier.**

## Generalisation

- **"Complete by construction" is a claim about coverage, not about behaviour.**
  A rewrite can be more complete and less correct. Coverage and blast radius are
  independent axes; a centralisation PR is exactly where they get conflated.
- **Order-dependence in scattered code is often load-bearing, not accidental.**
  When each site was written against "everything that existed then", the
  resulting order *is* a precedence relation. Preserve it explicitly and pin it
  with a test, or you will re-derive it after an incident.
- **The same applies to uniform limits.** The same refactor was asked to apply
  one 32-byte length floor everywhere; three of four envs shipped without it, and
  live configs use shorter values. Applying it uniformly would have disabled
  running ingresses for the same "it's more correct now" reason. Explicit,
  test-pinned waivers keep the gap visible and make lifting it a deliberate act.
- **Ask the availability question out loud in review.** "What does a broken-but-
  working deployment do on the next deploy?" catches this class; "does the
  invariant hold?" does not.

## Postscript: the abandoned design leaves fingerprints

Switching from the symmetric rule to precedence updated the implementation and
two of the three module tests. The third kept asserting the symmetric shape —
"refuse against every OTHER registered env" — and **passed**, because its subject
happened to be registered last, which made every sibling a senior. Five prose
comments kept describing the symmetric guard too, in security-sensitive files.

Two reviewers found it independently; the test suite and the linter could not,
because a positional accident is invisible to both.

So the rule has a second half: **when you abandon a design mid-change, grep for
the sentences and the assertions that described it.** An executable assertion
that outlives the design it was written for is worse than a stale comment — the
next person to exercise the new design meets a red test whose comment says the
code guarantees something it does not, and the cheapest way out of a red
assertion is to weaken it. That is the same convention-decay the centralisation
was meant to end, relocated into the test file.

The check that catches it is cheap and specific: **perform the extension the new
design advertises.** Here that was "append one registry entry and re-run" — it
produced the failure in seconds, and after the fix it produced a passing
`outranks_*` subtest instead, which is the positive proof that the claim
"appending needs no edit anywhere else" is now true.
