---
type: Task
title: "Task: ci-test-lane-runtime"
description: Stop the CI Test lane's runtime growth by removing per-call migration replays, and fix the ordering flake it hid
tags: ["test", "testing", "ci"]
timestamp: 2026-08-13T00:00:00Z
# --- octospec extension fields ---
slug: ci-test-lane-runtime
upstream: n/a (no issue filed; raised from CI run 31566358696 / 31591952470)
source: self
---

# Task: ci-test-lane-runtime

## Goal

Stop the CI `Test` job's runtime growth and remove its two known failure modes.

Measured medians of the job across three months: 6.9 min (2026-05) → 9.7 (06) →
13.6 (07) → 17.5 (08), i.e. ~+3.5 min/month, with a 26.4 min outlier. Two
distinct failures rode along with it:

1. `modules/user` exhausted the 5m per-package deadline (run 31591952470,
   `FAIL 327.129s`, `panic: test timed out after 5m0s` with the then-current
   test only 20s in — the package ran out of budget, nothing hung).
2. `TestManagerSystemSetting_OrderingRejectsFromSidebarSide` failed
   intermittently under `-shuffle=on` (runs 31566358696, 31571974333).

## Background

Instrumented in a CI-equivalent local environment (same image tags, same env,
shared netns so `127.0.0.1:3306` resolves like it does on a runner).
`NewTestServer` decomposes as: `module.Setup` 93.6%, `CleanAllTables` 6.1%,
everything else ~0.

Inside `module.Setup`, `executeSQL` is bimodal: p50 132ms (migrations already
applied) but a handful of calls at 13–25s, and those few accounted for 94% of
the total. They are the calls that genuinely replay all 194 migrations, caused by
`newTokenHTTPTestServer` doing `CREATE DATABASE` per invocation across its 14
call sites.

Note for whoever picks this up next: the intuitive target — `CleanAllTables`
issuing one `DELETE` per table, ~150k statements per CI run, correlating with job
runtime at r=0.988 — is a **red herring**. That correlation is spurious (every
cost grows with calls × scale), empty-table `DELETE` is cheap, and an
implementation that batched it made the full suite 5% *slower*.

The ordering flake is unrelated to runtime: `EnsureSystemSettings` memoises a
package-level singleton, and `CleanAllTables` only truncates rows, so a value
written through the admin handler by an earlier test survives into the next one's
"current snapshot".

## Load-bearing list

- `modules/user` test isolation: 14 call sites previously each got a pristine
  database. They now share one schema, so cross-test row leakage inside that file
  is newly possible; rows are wiped per call to compensate. Redis stays isolated
  per call via the existing per-call cache prefixes.
- `testutil.CleanAllTables` hardcodes `table_schema = 'test'`, so it cannot be
  used to reset any other schema (it lists `test`'s table names and deletes from
  the connected database, erroring 1146). The new reset resolves the schema with
  `DATABASE()` instead.
- The CI `Test` job's per-package deadline and the job-level timeout.
- setup-go's cache key for the `Test` job (previously shared with seven other
  jobs, so the fastest job's ~10 MB stub won the key permanently).
- `modules/common` admin system-setting write path reads the process-wide
  `SystemSettings` snapshot; tests must reset it, not just the table.

## Out of scope

- `octo-lib` changes. A migration-parse cache in `module.FindMigrations`
  (measured −8 to −11s on `user`/`group`/`bot_api`, 0 on small module sets) is
  prepared separately; it requires an upstream PR plus a `go.mod` bump.
- Package-level parallelism. It is the only order-of-magnitude lever left, but it
  needs `NewTestServer` to accept a DSN (it hardcodes `127.0.0.1:3306/test`).
- The other 11 build-your-own-database helpers (`modules/message` ×4,
  `card_template_catalog` ×2, `category` ×2, `pkg/auth` ×2, `user` ×1). Same
  pattern, but their packages total only 3.5–7.2s, so the cost is negligible.
- `CleanAllTables` itself, in either the performance or the `DATABASE()` sense —
  both belong upstream and the latter changes behaviour for integration-tagged
  tests this lane does not run.

## Acceptance

- `go test -race -shuffle=on -count=1 ./modules/user/` passes and the package's
  runtime drops substantially. Local CI-equivalent: 326.0s → 96.2s.
- Full suite stays green: 112 packages, 0 failures.
- Ordering flake is fixed and the fix is demonstrated, not asserted: with the
  fix reverted, `-run
  'TestManagerSystemSetting_UpdateAcceptsInRangeIntBoundaries|TestManagerSystemSetting_OrderingRejectsFromSidebarSide'`
  reproduces the exact CI failure (`expected: 200 / actual: 400`,
  `details.recent_days = 3650`); with the fix it passes.
- `go test ./tools/workflow-guards/` still passes (it parses `ci.yml`).
- `go vet ./...` clean.
