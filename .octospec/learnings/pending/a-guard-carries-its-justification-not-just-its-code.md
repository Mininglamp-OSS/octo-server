---
type: Learning
title: "Learning: moving a guard to a shared path moves its code, not its justification — split it by what each half is a property of"
description: Consolidating a subject-validation guard into one funnel broke an unrelated credential path, because half the guard was a heuristic about upstream-asserted values and did not hold for values we derive ourselves.
tags: ["architecture", "validation", "refactoring", "testing", "identity"]
timestamp: 2026-09-02T12:30:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# A guard carries its justification, not just its code

## What happened

A validation guard on an identity `subject` lived in one protocol adapter. A
review found it should apply everywhere: the same value reaches the same
`VARCHAR(255)` column no matter which protocol produced it, and a signature
proves provenance, not representability.

So the guard was moved to the one funnel every identity path passes through
before any side effect. Correct instinct — and it immediately broke a third,
unrelated credential path. Three tests went red.

The guard had two halves that had never been distinguished:

| half | what it is a property of | scope |
|---|---|---|
| non-empty, `<= column width` | the **column** | every path |
| "short all-digit value looks like an employee number" | the **producer** | upstream-asserted values only |

The second half's argument is that a personnel system reuses an employee number
between a leaver and a joiner, so an immutable `(issuer, subject)` key built on
one eventually points a new hire at a former employee's account. That argument
requires a personnel system. The third path's subject is our *own* business
database primary key — never reused — so a small value like `42` is entirely
normal. Applying the heuristic there locked every deployment with short user ids
out of that path.

The failure was loud only because the path already had tests. Had it not, the
consolidation would have shipped as a hard functional break with a generic 401.

## Why this is the same mistake as the one being fixed

The finding under repair was "a guard applied to one of two consumers". The fix
attempt was "a guard applied to three paths whose justification covers two".
Both are the same error: **reasoning about where a check is called instead of
what makes it true.** Unification and omission are symmetric failures.

## Rule of thumb

- Before hoisting a guard, write one sentence per condition: *this is true
  because of X*. If X is the storage schema, it is provenance-neutral — hoist it.
  If X is an assumption about who produced the value, it belongs where
  provenance is known, and hoisting it is a widening, not a de-duplication.
- Split the guard along that line and name the halves after what they are
  properties of (`checkSubjectStorable` vs `checkUpstreamSubjectShape`), so the
  next reader does not have to reconstruct the distinction.
- For the provenance half, avoid re-deriving provenance at the shared entry
  point (e.g. sniffing an issuer suffix). Keep it at the two or three places that
  *are* the source of that fact, and pin the set with a contract test over all
  implementations — that is what stops it from silently surviving in only one.
- Treat "consolidating a guard broke a passing test" as evidence about the
  guard's scope, not as a test to relax. The red test is the justification
  boundary telling you where it is.
