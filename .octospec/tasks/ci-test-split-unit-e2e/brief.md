---
type: Task
title: "Task: ci-test-split-unit-e2e"
description: Split CI tests into service-free unit tests and sharded MySQL/Redis/WuKongIM-backed E2E tests
tags: ["ci", "test", "testing"]
timestamp: 2026-08-14T00:00:00Z
slug: ci-test-split-unit-e2e
source: self
---

# Task: ci-test-split-unit-e2e

## Goal

Reduce CI Test wall time and make failures easier to diagnose by separating
service-free unit packages from MySQL/Redis/WuKongIM-backed E2E packages.

## Load-bearing list

- Preserve a stable required check named `Test`; branch protection should not
  need to chase matrix job names.
- Unit packages must not require MySQL, Redis, or WuKongIM. The allowlist is
  conservative; new packages default to E2E.
- E2E packages keep the existing per-package database recreate and Redis flush
  semantics so migration-ledger isolation does not change.
- E2E shards use independent GitHub runners and service containers.
- WuKongIM remains pinned to `wukongim/wukongim:v2.2.4-20260313`.

## Out of scope

- Moving tests between files or introducing build tags.
- Changing `testutil.NewTestServer`, `CleanAllTables`, or octo-lib.
- Enabling the `integration` build tag.
- Tuning race coverage policy.

## Acceptance

- `Unit Test` runs without service containers.
- `E2E Test` runs as a 4-way matrix with MySQL, Redis, and WuKongIM services.
- Aggregated `Test` fails if either split lane fails and succeeds for docs-only
  or draft PR skips.
- `go test ./tools/workflow-guards/` passes.
