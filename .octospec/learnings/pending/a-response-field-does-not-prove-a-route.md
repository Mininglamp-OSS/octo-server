---
type: Learning
title: "Learning: a missing response field does not prove the endpoint exists"
description: Planning a change against a handler's response struct proves only that the struct lacks the field; the route may never be mounted. Grep for the constructor's caller before scoping work onto an endpoint.
tags: ["planning", "dead-code", "routing", "scoping", "verification"]
timestamp: 2026-09-03T00:00:00+08:00
# --- octospec extension fields ---
task: bot-agent-hosting
status: pending
---
# A missing response field does not prove the endpoint exists

## What happened

A task brief scoped two read faces for a new column. The ops-facing half was
justified like this, and the justification checked out:

> `GET /v1/manager/robots{,/:robot_id}` currently returns **no** agent
> information at all — `modules/robot/api_manager.go:445-465`'s two response
> structs have no `agent_*` fields. The place that most needs it for triage is
> empty.

Every clause is true. The structs exist, they lack the fields, and ops really
would want them. The implementation added the fields, wired the assignments, and
wrote a test. The test said:

```
404 page not found
```

`modules/robot/api_manager.go` defines `NewManager(ctx)` and a `Route()` that
mounts `/v1/manager/robots`, `/v1/manager/robots/:robot_id`,
`/v1/manager/robot/menus` and more. **Nothing calls `NewManager`** — one grep
over the repo returns nothing, and `modules/robot/1module.go` registers only
`New(ctx)`. The entire `Manager` is dead code; none of those endpoints exist in
production. The whole change was reverted (`git checkout modules/robot/`).

## Why the research passed

Because the evidence answered a *different* question than the one that mattered.
Reading a handler and its response struct answers "does this code produce the
field" — it cannot answer "is this code reachable". In a repo whose modules
self-register via `init()` + a registry (`register.AddModule`), reachability is
not visible from the handler file at all: it lives in whatever the module's
`SetupAPI` actually constructs. A handler that is never mounted looks exactly
like a handler that is mounted, from inside its own file.

## The rule

**Before scoping work onto an endpoint, prove the route is reachable.** Cheapest
sufficient checks, in order:

```bash
grep -rn "pkg.NewManager\|NewXxx" --include="*.go" .   # constructor has a caller?
grep -rn "SetupAPI" modules/<mod>/1module.go           # what does the module register?
```

For the endpoint itself, a request against the test server is definitive — a
`404` from a route you believe exists is the fastest possible signal.

## Two things this generalises to

**Adding fields to dead code is worse than doing nothing.** It costs the same
review effort, no caller will ever see it, and it leaves the next person
convinced the capability exists. If dead code is found mid-task, reverting and
recording *why* is the deliverable; "it compiles and the tests pass" is not a
reason to keep it.

**Write down what the check did NOT cover.** The brief's evidence was accurate
and still insufficient, which is the hard case — there was nothing to catch by
re-reading it more carefully. The fix is to name the question the evidence
answers ("the struct lacks the field") separately from the question the scope
depends on ("the endpoint is served"), so the gap between them is visible while
still planning.
