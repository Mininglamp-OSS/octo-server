---
type: Learning
title: Stateful tests must own and release their state
description: Database tests must provision an isolated schema, and tests that deliberately exhaust shared Redis state must clean it on exit as well as entry.
tags: ["testing", "mysql", "redis", "isolation", "rate-limit"]
timestamp: 2026-09-09T20:17:10+08:00
status: pending
source: sidebar-project-sections
---

# Stateful tests must own and release their state

## What happened

A database test passed only when another package had already migrated the shared
`test` schema. It failed under the CI-shaped per-package reset with a missing
table. Separately, a rate-limit test correctly cleared its UID bucket on entry,
then intentionally exhausted it and left later shuffled tests receiving 429.

## Rule candidate

- A database-backed test must create the schema it needs in a package-owned
  database, or invoke the package's supported migration harness. Never depend on
  another package having run first.
- A test that deliberately consumes or mutates shared state must restore it with
  `t.Cleanup`, even when every test also tries to clean on entry. Entry cleanup
  protects the current test; exit cleanup protects arbitrary shuffled successors.
- Local multi-workspace runs must not share a destructively reset database. Use
  a dedicated service/schema or serialize through the CI runner's ownership
  boundary before interpreting migration errors as product defects.

## Why it matters

Order-dependent tests can pass targeted runs and fail the package or CI shard,
while concurrent schema resets produce misleading migration and missing-column
errors across unrelated modules. Explicit ownership makes failures attributable.
