---
type: Learning
title: "CleanAllTables clears tables, not in-process caches — and the leak only shows under -shuffle"
description: Process-wide singletons holding an in-memory snapshot (SystemSettings) survive CleanAllTables exactly like the Redis rate-limit buckets do, so a case that writes a deployment default leaks it into every later case in the binary. Order-dependent, so the default sequential run hides it. Setup must reset every layer the case writes to, not just the DB.
tags: ["testing", "config", "system-setting", "flaky"]
timestamp: 2026-08-06T16:20:00+08:00
# --- octospec extension fields ---
source: self
origin_task: bot-setting-store
status: pending
candidate_rule: testing
---

# CleanAllTables clears tables, not in-process caches

## Context

The `testing` rule already carries one instance of this:

> `CleanAllTables` does NOT clear Redis rate-limit buckets — reset them
> explicitly in setup when a test hits a rate-limited route.

`bot-setting-store` hit the same shape in a second place, which suggests the
rule's phrasing is one example short of the general statement.

## What happened

The per-bot config resolver reads `modules/common`'s `SystemSettings` as its
middle tier. That type is a **process-wide singleton** holding an immutable
snapshot swapped by `Load`/`Reload`, refreshed on a 60s timer — by design, so
readers never take a lock.

One integration case wrote a `system_setting` row (a deployment default) and
called `Reload()` to observe it. The next case called `CleanAllTables`, which
truncated the row — but nothing reloaded the snapshot, so the singleton kept
serving the deleted value. The later case saw `source="global"` where it
expected `source="default"`.

Under the default sequential order the writer happened to run last and
everything passed. `go test -shuffle=on` (which CI runs) failed on 2 of 4 seeds.

## The general statement

**Test setup must reset every layer the case under test can write to, not just
the database.** In this repo that is at least three layers:

| Layer | Cleared by `CleanAllTables`? |
|---|---|
| MySQL tables | yes |
| Redis (rate-limit buckets, locks, queues) | **no** |
| In-process singletons holding a snapshot (`SystemSettings`) | **no** |

The tell for the third layer: the value is served by something constructed via
an `EnsureXxx` / `sync.Once` accessor. Anything reachable that way outlives
`CleanAllTables` and outlives the individual test.

## What to do

In setup, after `CleanAllTables`, explicitly reset each non-DB layer the case
touches:

```go
assert.NoError(t, testutil.CleanAllTables(ctx))
resetUIDRateLimit(t, ctx)                                  // Redis
assert.NoError(t, common.EnsureSystemSettings(ctx).Reload()) // in-process snapshot
```

And run new integration tests with `-shuffle=on` at least once before opening
the PR: a leak of this kind is invisible in the authored order and only appears
when some other case runs first.

## Why it is worth a rule line

The failure does not look like a test bug. It surfaces as an assertion on
business semantics (`source` reporting the wrong layer), so the natural first
reading is "the resolver has a priority bug" — and the resolver is fine. Naming
the three layers turns a confusing debugging session into a checklist.
