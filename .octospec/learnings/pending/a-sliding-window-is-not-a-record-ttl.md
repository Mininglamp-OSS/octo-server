---
type: Learning
title: "Learning: a sliding window cannot be expressed by the record's TTL"
description: When absence of a record means "never seen", letting the record expire with the window makes expiry readmit exactly what the window was meant to exclude; the record must outlive the window and the window must be computed from a stored timestamp.
tags: ["redis", "ttl", "expiry", "replay", "state-modelling", "auth"]
timestamp: 2026-09-06T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-bearer-jwt-redemption-ledger
status: pending
---

# A sliding window is not the record's TTL

## What happened

A credential could be redeemed repeatedly, and the guard was an **idle window**:
a token unused for longer than `T` is treated as abandoned and refused. The
obvious implementation is to store one record per credential with `TTL = T` and
refresh it on each use — the key's own expiry then *is* the window, and the code
is a `SET` with no arithmetic.

It is exactly backwards. The decision table has four rows, and the two that
matter share a state:

| record | meaning | verdict |
|---|---|---|
| absent | never redeemed | admit (it is a first use) |
| absent | **expired — abandoned** | refuse |

Absence cannot carry both meanings. With `TTL = T`, the record's death does not
refuse the abandoned credential; it *promotes* it back to "never redeemed" and
admits it. The window fires only in the one case where nothing consults it.

The fix inverts what the TTL expresses: the record lives as long as the
credential itself (`TTL = exp - now`), and idleness is computed from a
`last_at` timestamp stored inside it. Expiry now means only "the credential is
dead anyway", which is a state where the answer is the same either way.

## Why it was invisible

The decision table as written down looked complete — four rows, all four
implemented, all four tested. What it did not say was **which state each row
reads**, and two rows read the same one. Writing the absent-record branch out as
code was what surfaced it: the branch had to choose a verdict, and there was no
information available to choose with.

## The rule

Before making expiry carry a decision, ask what an **absent** record means, and
whether it means more than one thing. If it does, expiry is not the mechanism:
keep the record alive across the whole span in which the question can be asked,
and store the value the decision needs.

A corollary for the bound that *does* get expressed as a TTL: any policy value
that must be enforceable within the record's lifetime has to be capped by it. A
window longer than the record cannot fire, and the refusal it should have
produced surfaces under some other branch's name — an operator then tunes the
wrong knob.
