---
type: Learning
title: "A per-cycle worker over N resources needs a rotating start cursor, not greedy in-order drain"
description: When one background worker drains work from N keyed resources under a per-cycle cap, iterating them in a fixed order lets a perpetually-busy first resource starve the rest; rotate the start index each cycle.
tags: ["notify", "worker", "fairness", "starvation", "review", "concurrency"]
timestamp: 2026-07-21T00:52:00Z
# --- octospec extension fields ---
source: self
origin_task: space-welcome-per-space-admin-crud
origin_pr: Mininglamp-OSS/octo-server#TBD
status: pending
candidate_rule: null
---

# A per-cycle worker over N resources needs a rotating start cursor

## Context
The Space-welcome send worker was generalized from one Space to "all enabled
Spaces". Each wake it drained up to a global cap (`swWorkerWakeCap = 20`) claimed
rows, iterating the enabled Spaces **in `space_id` order** and fully draining
each before the next. Correct for a fixed backlog, but under **sustained** inflow
a Space whose `space_id` sorts first and keeps producing >20 pending rows/wake
consumes the entire cap every wake — later Spaces are **never** reached and their
members never get welcomed. The sibling reconciler already rotated a cursor; the
worker was written without one, silently contradicting the "round-robin / no
starvation" contract in its own doc-comment and the task brief.

## Rule of thumb
When a single background worker services N keyed resources under a per-cycle
budget:
1. **Rotate the starting resource each cycle** (`start = cursor % n; cursor++`),
   in-memory and per-replica — it's a fairness hint, not shared state, so no
   lock and no correctness dependency (the DB claim already serializes work).
2. **A per-cycle cap alone does not give fairness** — it bounds one cycle's work
   but, without rotation, re-serves the same head resource every cycle.
3. **Test starvation with sustained load, not a fixed backlog.** A fixed backlog
   drains in-order eventually; the bug only appears when the head resource is
   *topped back up over the cap before every cycle*. The regression test must
   re-seed the head resource each wake and assert a tail resource still makes
   progress (it fails without the cursor, passes with it).

## Why worth a rule
Fairness bugs in fan-out workers are invisible in unit tests with small fixed
inputs and in single-resource legacy behavior; they only bite in production
under load, as a specific tenant/resource going silently unserved. Cheap to
prevent (one cursor), easy to miss (the cap *looks* like it bounds fairness).
