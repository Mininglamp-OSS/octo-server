---
type: Learning
title: "A fixture presence check is not a fixture schema check"
description: When a package manually provisions a dependency table in a shared test database, CREATE TABLE IF NOT EXISTS cannot guarantee columns added by the current test contract; reconstruct the dedicated fixture or run its owner migration instead.
tags: ["testing", "database", "fixture", "schema", "review"]
timestamp: 2026-09-06T00:00:00+08:00
# --- octospec extension fields ---
source: self
origin_task: space-cloud-agents-by-owner
status: pending
candidate_rule: testing
---

# A fixture presence check is not a fixture schema check

## Context

`modules/space` owns integration tests that manually provision dependency
tables before its own migrations run. The directory feature added reads of
`robot.description`, `robot.agent_hosting`, and
`robot.agent_reported_hosting_at`. Its original fixture used `CREATE TABLE IF
NOT EXISTS robot`, which succeeds unchanged when a developer or another test
run has left behind an older robot schema. The first insert or query then fails
with `Unknown column`, far from the setup mistake.

## Rule of thumb

`CREATE TABLE IF NOT EXISTS` proves only that a table exists; it proves nothing
about the columns, indexes, defaults, or collation that the current test needs.
When a test package must manually provide a dependency fixture, choose one of:

1. Load the migration-owning module and let its migration create the schema.
2. In that package's dedicated test database setup, explicitly recreate the
   fixture table with the exact current schema.

Do not treat a bare existence fallback as schema migration. A destructive
recreate is acceptable only for the explicitly scoped local test database and
must not be used against a shared operational database. If the table belongs to
an imported migration-owning module, prefer its migrations over a competing
hand-written fixture so migration bookkeeping remains correct.

## Why worth a rule

This failure is silent until an unrelated feature starts reading a new column,
and it recurs whenever a manually maintained fixture evolves. The setup code
looks successful while invalidating every downstream test result, so the
testing rule should make schema freshness explicit rather than relying on
developer-local database history.
