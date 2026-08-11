---
type: Learning
title: "Characterize the current behavior before designing the change, not after"
description: For a load-bearing change to a system already in production, a runnable characterization of what the code does today belongs in the Plan phase as an input. Writing the design first and verifying it afterwards means any defect the verification finds arrives as a patch to a design that was made without it — and the verification tends to find one, because reading code establishes what it intends while running it establishes what it does.
tags: ["process", "testing", "review", "octospec"]
timestamp: 2026-08-11T09:10:00+08:00
# --- octospec extension fields ---
source: self
origin_task: token-session-rollout-simplify
origin_pr: Mininglamp-OSS/octo-server#733
status: pending
candidate_rule: testing
---

# Characterize the current behavior before designing the change, not after

## Context

`token-session-rollout-simplify` set out to simplify the #725 session rollout.
The brief was drafted from code reading, and only afterwards was a harness
written to check whether its claims about current behaviour actually held.

All five claims held. But writing the harness surfaced a **sixth** defect
nobody had noticed: a corrupt payload with a finite TTL inside `maxTTL` wedged
the floor permanently. `natural` policy left it alone, `observe` kept reporting
`decode_invalid`, and the evidence validator rejected any observation carrying
one — so the floor could not advance until the token expired on its own, up to
720h, with no tool able to clear it.

That defect changed a design decision: an automated reconciler stuck on a
condition that does not self-heal needs its own alert, and the fix belonged in
the design rather than bolted on. Because the design was already written, it
arrived as a patch.

The same pass also found the repo's `pkg/auth` v1 fixtures were simply wrong —
they used JSON, but the v1 wire format is `uid@name[@role]`. The old migration
Lua had been hiding it for as long as it existed by treating anything without a
v2/v3 prefix as v1. No amount of reading the brief would have surfaced that;
running two implementations against one keyspace did, immediately.

## The general shape

Reading code establishes what it **intends**. Running it establishes what it
**does**. A design built only on the first is a design built on the subset of
reality the previous author managed to express.

This bites hardest where the change is a simplification, because simplifying
means deciding which existing behaviours are load-bearing and which are
ceremony — and that judgement is exactly the one that needs measurements rather
than inference.

## Proposed rule

For a change to behavior that is already deployed — a state machine, a
migration path, an activation gate — produce a runnable characterization of
current behaviour **before** the brief's design section, and cite it from the
brief.

Two files with opposite intent, so a reviewer can tell them apart at a glance:

| file | pins | after the change |
|---|---|---|
| `*_invariants_test.go` | rules that must survive | **still green** |
| `*_legacy_behavior_test.go` | defects being removed | **goes red** |

Concrete constraints learned the hard way:

- The defect tripwires must **compile**, or "going red" becomes a build failure
  that takes the whole package down. Compiled-but-skipped behind an environment
  variable keeps CI green without letting the file rot.
- Their inverted forms are the acceptance suite. Write them so inverting is a
  mechanical edit.
- Prefer end-to-end assertions over pure-function ones. An early version
  "proved" a scope fingerprint could not distinguish two Redis instances by
  comparing a pure function's output twice — nearly tautological. The
  end-to-end version pointed a fully configured store at the wrong instance and
  watched it perform a real destructive apply unchallenged. Same conclusion,
  far stronger evidence, and it is the second version that convinces a reviewer.

## Cost

About twenty minutes of harness for this task. It bought one unknown defect,
one wrong long-standing fixture, and a design decision that would otherwise
have been retrofitted.
