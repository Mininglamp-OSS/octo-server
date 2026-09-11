---
type: Learning
title: "A fail-open path disguises its own implementation bugs as the feature simply not working"
description: When a feature degrades silently on error, a defect inside that feature's own lookup is indistinguishable from the pre-fix behavior — the fix ships, the tests pass, and production looks exactly as it did before. Stub-based unit tests cannot see it, because the stub replaces the layer that is broken. Fail-open paths need a test against the real dependency and a test of the assembly, not just of each layer.
tags: ["fail-open", "testing", "dbr", "sql", "integration-test"]
timestamp: 2026-09-11T00:00:00Z
# --- octospec extension fields ---
source: self
origin_task: ios-apns-mute-of-app
status: pending
candidate_rule: testing
---

# A fail-open path disguises its own implementation bugs

## What happened

The change makes muted recipients receive a soundless push, gated on a live
desktop session. The gate is deliberately **fail-open**: if the online lookup
errors, send the push *with* sound, because a notification nobody hears is a
lost message.

The batch lookup was written as:

```go
Where("uid in ? and device_flag in ? and `online`=1",
    uids, []uint8{config.PC.Uint8(), config.Web.Uint8()})
```

`[]uint8` **is** `[]byte` in Go. dbr binds it as a blob instead of expanding an
`IN` list, so `?` was never expanded and MySQL returned Error 1064 on every
single call. Fail-open caught the error, logged it, and sent an audible push to
every muted user.

That is byte-for-byte the bug the change set out to fix.

## Why nothing caught it

- **Unit tests were green.** They injected a stub lookup. The stub *is* the layer
  that was broken, so the tests asserted that correct inputs produce correct
  outputs — which was never in question.
- **The payload tests were green.** They call `Silence()` directly; they never
  ask who decided to call it.
- **The compiler was happy.** `[]uint8` is a perfectly valid argument.
- **Code review did not see it.** `[]uint8{...}` for a list of small integers
  reads as natural, even careful.
- **Production would have looked normal.** The user-visible symptom is identical
  to the pre-fix behavior: the setting does nothing. The only signal is an error
  log nobody is watching, on a code path everyone believes is working.

It surfaced only because a reviewer pointed out that the *assembly* — the one
place the feature is switched on for real — had no coverage. The first run of
that test failed immediately.

## The transferable rule

**Fail-open converts your own bugs into the absence of your feature.** With
fail-closed, a broken lookup is loud: pushes stop, alerts fire, someone pages.
With fail-open, a broken lookup is silent and looks exactly like "we never
shipped it". So the safety property that makes fail-open correct for users is
the same property that makes it undetectable for developers.

Two consequences when writing a fail-open path:

1. **Test the real dependency, not a stub of it.** For a DB-backed lookup that
   means a test against a real database. Stubbing the failing layer guarantees
   the test cannot observe the failure. This is where an `IN`-list binding bug,
   a column typo, or a wrong table lives — none of them reachable through a
   stub.
2. **Test the assembly, not only the layers.** Each layer passing says nothing
   about the wiring between them, and the wiring is what runs in production.
   Here the wiring crossed a worker-pool boundary through a
   `map[string]interface{}` with a discarded type assertion — a key typo would
   have failed open just as silently.

## Smell to look for

When a change adds a fail-open branch, ask: *if the thing inside this branch
were completely broken, which test would go red?* If the answer is "none, the
feature would just stop working" — that is the pre-fix behavior, and no test
distinguishes shipped from not-shipped. Write that test before trusting the
change.

Corollary for reviewers: "each layer is unit tested" is not coverage of a
feature. Ask which test fails if the layers are wired together wrongly.
