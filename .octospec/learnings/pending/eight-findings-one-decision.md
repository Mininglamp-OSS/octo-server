---
type: Learning
title: "When review findings cluster, look for the parent decision before fixing them one by one"
description: A single design choice can spawn a dozen findings that each look independent and each have a plausible local fix. Fixing them individually grows the diff and keeps the cause. Check whether the findings share a parent, and price reversing the parent against fixing the children.
tags: ["review", "design"]
timestamp: 2026-08-23T11:00:00Z
# --- octospec extension fields ---
source: self
origin_task: space-removal-creator-handover-notice
origin_pr: none
status: pending
candidate_rule: none
---

# When review findings cluster, look for the parent decision before fixing them one by one

## Context

A change added a group-visible system message for **every** member removed from a
Space. Adversarial review returned fourteen findings. Each was real, each was
verified against source, and each had an obvious local fix:

| finding | the local fix it invited |
|---|---|
| 200-uid batch × M groups = 10k permanent messages | add a batch threshold, or coalesce per group |
| sanitizer missed U+2028/U+2029/U+202E | widen the character class |
| justification comment factually wrong | rewrite the comment |
| `RedDot: 0` left the message with no UI signal | flip to `RedDot: 1` |
| name resolved differently from the adjacent tip | align the two resolvers |
| "当前空间" wrong for external members | reword |
| false claim left in history after a rejoin | add a retraction path |
| content type diverged from the manual transfer path | switch types |

Eight separate fixes, each defensible, each adding code. The parent was one
sentence: *every removed member gets a group broadcast*.

Reversing it — broadcast only when the removal changed the group's owner, which
is the one case not already visible in the roster — deleted all eight at once,
along with four functions (a sanitizer, a length cap, a tip sender, a name
resolver) and roughly 350 lines. What remained after the reversal was one genuine
defect that had nothing to do with the parent, plus test gaps.

## The rule

Before working a review list item by item, sort it by *cause* rather than by
severity, and ask of each cluster: **is there one decision upstream of all of
these?**

Signals that there is:

- Several findings are downstream of the same code path or the same new function.
- The local fixes pull in opposite directions (here: "suppress it, it is too
  loud" and "it is too quiet to notice" were both filed against the same
  message — a strong hint the message itself was miscast).
- Fixing one finding creates the conditions for another (widen the sanitizer →
  now truncation splits grapheme clusters → now the ellipsis needs its own rule).
- The diff grows with each fix while the described behaviour does not improve.

Then price the two options honestly. Reversing a parent decision is usually the
larger edit *today* and the much smaller one over the life of the code, because
the findings it dissolves never come back.

## Caveat

This is not "when review is hard, delete the feature". The reversal has to leave
the original requirement satisfied. Here the reported symptom was "the group is
not told anything", and the narrowed design still tells the group the one thing
it cannot otherwise infer — an ownership change. What it stops telling them is
something the member list already shows.

If the parent cannot be reversed without abandoning the requirement, the findings
are genuinely independent and do deserve individual fixes.
