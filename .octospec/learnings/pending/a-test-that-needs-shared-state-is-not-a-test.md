---
type: Learning
title: "Learning: a test that must mutate process-wide state to run is unreliable in a full-package run, no matter how well it pins its invariant"
description: A test that offsets the DB session timezone (or otherwise reconfigures a shared pool) can prove exactly the right thing in isolation and still make the whole package fail non-deterministically. Prefer a deterministic guard one layer down over a faithful end-to-end test that needs exclusive process state.
tags: ["testing", "flaky", "shared-state", "connection-pool", "mutation-testing"]
timestamp: 2026-09-04T00:00:00+08:00
# --- octospec extension fields ---
task: bot-agent-hosting
status: pending
---
# A test that must mutate process-wide state to run is unreliable, no matter what it pins

## What happened

A column had to be written with SQL `NOW()` rather than Go's `time.Now()`, because
the driver converts Go times through `Config.Loc` (UTC) while the app image pins a
non-UTC `TZ` — so on a non-UTC DB session the value would land hours away from the
sibling timestamp rendered beside it.

The obvious end-to-end test: offset the session timezone, register, compare
against a `NOW()`-written column. Written that way it passed under the mutation
(`NOW()` → `time.Now()`), because the offset was applied to a *dedicated*
connection while the code under test wrote on a different pooled one — both
`TIMESTAMP`, same UTC instant, zero skew either way.

The fix for *that* was to squeeze the pool to a single connection so the offset
and the write share one session. It worked: the mutation now produced a measured
`7h59m59s` skew. It also broke the package. `SetMaxOpenConns(1)` is
**process-wide**: every other test's queries queued behind it, unrelated tests
started failing and timing out, and the failure set changed between runs. An
attempt to clean up with `SetConnMaxLifetime(1ns)` traded one nondeterminism for
another.

The test was deleted, not fixed. The invariant it guarded already had a
deterministic guard one layer down: an assertion that the emitted `UPDATE`
contains a literal `NOW()`. That kills the same mutation, touches no shared state,
and runs in microseconds.

## The rule

**Before writing a test that reconfigures something process-wide — a connection
pool, a global clock, an env var, a package-level singleton — ask whether a
narrower assertion kills the same mutation.**

Ranking, best first:

1. **Assert the artifact.** "Is the SQL statement written with `NOW()`" is a
   string assertion on generated SQL. It is exactly as strong as the behavioural
   claim (SQL `NOW()` ⟹ same clock as any other `NOW()` in that session) and has
   no environmental dependency at all.
2. **Assert the structure.** A source guard ("this function's signature cannot
   receive the row, so it cannot merge from it") is deterministic and survives
   refactors that a behavioural test would silently pass.
3. **Assert the behaviour end to end** — but only if it does not need exclusive
   process state.

A faithful end-to-end test is usually the most convincing, which is why this
tradeoff is easy to get backwards. The deciding question is not "which test is
closer to production" but "can this test coexist with the other tests in its
package".

## Corollaries

- **When you delete a test, leave the reasoning where it was.** Otherwise the next
  person reads "the timezone invariant has no test" and re-adds the same shape. A
  comment block in place of the deleted function, naming the guard that replaced
  it, costs nothing.
- **`SetMaxOpenConns` / `SetConnMaxLifetime` on a shared `*sql.DB` are not test
  fixtures.** They have no scope. Neither does `SET time_zone` on a pooled
  connection — the deferred reset may land on a different connection than the one
  you changed.
- **Non-deterministic failure sets are diagnostic.** If the same run fails
  different tests each time, stop looking for a logic bug. Either something shares
  state, or the environment is degrading — and both are distinguishable from a
  logic bug by running the package serially (`-p 1 -parallel 1`): a genuine
  ordering dependency becomes stable, a capacity problem stays random.
