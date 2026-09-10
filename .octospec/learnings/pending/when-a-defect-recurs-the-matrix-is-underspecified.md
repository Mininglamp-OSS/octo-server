---
type: Learning
title: "Learning: when the same defect keeps reappearing through new doors, the under-specified thing is the matrix, not the line"
description: Three rounds of review each found the same class of defect via a route the previous fix had not enumerated; patching per-finding cannot converge, and dismissing a request to write the matrix down as ceremony was wrong.
tags: ["process", "code-review", "security", "architecture"]
timestamp: 2026-09-02T13:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# When a defect recurs through new doors, the matrix is under-specified

## What happened

One security-sensitive change went through six review rounds. Across the last
three, the findings were not a converging tail — they were the *same class*,
reached differently each time:

- a guard applied to one of two consumers of a query;
- an initialisation deleted along with the unrelated neighbour it sat beside;
- one freshness policy applied to two purposes with opposite requirements;
- a credential classification that was right for one half of a sentinel's range;
- a component whose **construction failure** silently disabled the guard built on it.

Each was fixed properly, with a test, structurally where possible. And each fix
was scoped to the door that had been found.

A reviewer asked for one consolidated pass writing the matrix down explicitly:
for each identity path × each credential type — which guards apply, at what
stage, with what lifetime, in which namespace, and **what happens when the
component implementing the guard failed to construct**. I initially agreed, then
retracted it as ceremony, on the reasoning that the document "produces the same
fix list without changing a line of code".

That reasoning was wrong, and specifically wrong in a way worth remembering: I
evaluated the matrix against the findings I *already had*. Its value is in the
cells nobody has looked at. The next round's two blocking findings both sat in
columns the reviewer had named — "at which stage" and "what if the guard failed
to construct".

## Why per-finding patching cannot converge here

A review finding names a location. A fix that takes the location as its scope
inherits the reviewer's enumeration of routes, which is exactly the thing that
was incomplete — otherwise the defect would not have recurred. So each round
closes one door and leaves the count of unexamined doors unknown, including to
the person doing the fixing, who now feels *more* confident because the last
three fixes were real.

Structural fixes help but do not substitute. Unexporting an unsafe query, or
making one funnel the only reachable path, closes a class **as far as the
identified axis goes**. It says nothing about a second axis nobody has listed.

## Rule of thumb

- Two independent findings of the same class through different routes is the
  signal. Stop fixing findings and enumerate the space once: every path × every
  input type, one row per cell, including the degenerate cells (what if this
  component is nil / failed to build / is misconfigured?).
- "This document produces no code change" is not an argument against it when the
  recurring failure is *omission*. Ask instead: would a filled-in cell have made
  the last finding obvious before a reviewer found it?
- The construction-failure column is worth naming separately. Guards are usually
  written assuming they exist; a guard that failed to build tends to fail open,
  and the surrounding `if guard != nil` makes the fall-through invisible.
- For a large security-sensitive change, prefer landing it as a sequence where
  each piece can be reviewed to convergence. An 81-file diff makes every review
  round a sampling exercise, and sampling is what lets a class survive.
