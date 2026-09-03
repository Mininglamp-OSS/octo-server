---
type: Learning
title: "Learning: when a review says 'this guard only covers one path', check whether the guard's *justification* generalises before the guard does"
description: Making a check protocol-neutral was right for the storage bound and wrong for the heuristic beside it, because only one of the two arguments was about the protocol.
tags: ["security", "generalisation", "review-response", "blast-radius"]
timestamp: 2026-09-02T22:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# Generalising a guard also generalises its justification — check that it survives

## What happened

One function held two checks on an identity `subject`: an upper length bound, and a
refusal of short purely-numeric values. A review pointed out it ran on only one of
two provider implementations. The response was to make it protocol-neutral and apply
it to both.

That was right for the length bound: the column is 255 wide regardless of which
protocol produced the value. It was wrong for the numeric heuristic, whose argument
was *one vendor's documentation says its `sub` may be an employee number, and
employee numbers are reused between a leaver and a joiner*. That is a fact about one
personnel system.

The second implementation was the generic client that existing deployments already
run against whatever IdP their operator chose. A self-hosted IdP that surfaces its
own user-table primary key as `sub` — `1001`, `42` — would have had **every user
refused at the first request after rollout**, with the threshold a `const`, no
operator override, and recovery only by redeploy.

The same codebase already contained the counter-argument, one axis over: the guard
deliberately skipped the path where subjects are our own database keys, "not reused,
`userId=42` is a completely normal value". A self-hosted IdP's primary key is that
situation exactly. The axis chosen — *did we derive this subject, or did an upstream
assert it* — was not the axis the argument needed, which is per-deployment: *does
this IdP's subject come from a reused personnel identifier*.

## Why it slipped

"Apply the guard everywhere" reads as strictly safer, so it does not trigger the
scrutiny that removing a guard would. But widening a refusal is not a safe direction
by default — it converts a possible identity collision into a certain outage, for
deployments that never had the hazard. The blast radius moved from "one new
integration" to "every existing install", and the review round that requested the
generalisation was reasoning about the former.

## Rule of thumb

- When generalising a check, restate its justification and ask which nouns are
  load-bearing. "The column is 255 bytes" survives a change of protocol. "This
  vendor's IdP reuses employee numbers" does not survive a change of vendor.
- Two checks sitting in one function are not one decision. Split them before moving
  them.
- A guard that can refuse **all** users of an existing deployment is not the safe
  default just because it is a refusal. Weigh blast radius, not only direction.
- If a value that motivates a refusal is per-deployment, express it per deployment —
  a declared capability or an operator setting — and default it to off. Approximating
  it by something structural (protocol, kind, module) works only while there is one
  deployment of each, and it stops working silently.
- Grep the codebase for your own prior argument before generalising. Here the exact
  counter-argument was already written down on an adjacent path; the mistake was
  reachable by reading code that already existed.
