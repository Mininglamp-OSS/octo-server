---
type: Learning
title: "Learning: when two copies of a rule must agree, delete one; a comment asking for sync is not a mechanism"
description: A hand-maintained mirror of validation logic drifts the moment either side gains a condition; move the rule into a leaf package both sides import and pin their tests to one shared table.
tags: ["architecture", "import-cycle", "configuration", "availability", "testing"]
timestamp: 2026-09-02T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# When two copies of a rule must agree, delete one

## What happened

Two packages needed the same answer to "can this OIDC configuration boot?"

- the module itself, to refuse startup with a specific error;
- a settings helper elsewhere, to decide whether a login method counts as
  configured — which in turn gates whether local password login may be disabled.

The second could not import the first (the dependency runs the other way), so it
had been written as a hand-maintained copy, with a comment on both sides
explaining the invariant and warning what breaks if they diverge.

This change added five new fatal conditions to the first copy and none to the
second. The comment was accurate; it just wasn't a mechanism.

The resulting failure chain is worth spelling out, because each step is
individually reasonable:

```
provider kind has a typo
  -> config load fails, the module registers 404 handlers for every endpoint
  -> but the copy still reports "fully configured"
  -> so "some third-party login exists" stays true
  -> so "local password login is off" is honoured
  -> an SSO-only deployment has no working login path, and no recovery
     short of a redeploy
```

The warning comment had been written by the same hand that then drifted it.
Knowing about a hazard and being protected from it are different states.

## Why "just remember to update both" does not hold

The two copies are read at different times by different people for different
reasons. Whoever adds a provider kind is thinking about the provider, not about
whether password login can be safely disabled — and nothing in their diff points
at the other file. Review does not catch it either: the new condition looks
complete on its own.

The asymmetry is what makes it dangerous. Being stricter in the module than in
the mirror is not a small inconsistency; it is precisely the combination that
removes every login path at once.

## Rule of thumb

- If two places must agree on a rule and cannot import each other, that is a
  signal to **extract the rule downward** into a leaf package both can import —
  not a signal to duplicate it with a comment. A stdlib-only package is cheap
  and inverts nothing.
- Where a full extraction is too invasive, extract the **decision** (a predicate
  returning an error) and leave the value-shaping where it is. The refusal rules
  are the part that must not diverge.
- Export the table of cases from the shared package and have **both** sides'
  tests iterate it. That is what makes the next addition impossible to apply to
  only one side — a new entry immediately fails the side that ignores it.
- Verify the pin is not vacuous: remove the delegation and confirm the other
  side's tests go red. Here, removing it failed all nine scenarios, which is how
  we learned the drift was already live rather than hypothetical.
- Suspect this pattern wherever a comment says "keep in sync with", "mirrors",
  or "duplicated to avoid an import cycle". Those phrases mark a rule that has
  no owner.
