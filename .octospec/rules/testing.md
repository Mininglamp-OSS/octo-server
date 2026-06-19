---
type: Rule
title: Testing conventions
description: Test setup must bound the DB connection pool and clean up; coverage expectations.
tags: ["test", "testing"]
timestamp: 2026-06-19T00:00:00Z
# --- octospec extension fields (OKF-permitted; consumers must preserve) ---
id: testing
tier: repo
priority: 70
load_bearing: false
inject_when:
  paths: ["modules/**/*_test.go", "**/*_test.go"]
  touches: ["test", "testing"]
source: self
supersedes: []
---

# Testing conventions

## Setup

```go
_, ctx := testutil.NewTestServer()
defer testutil.CleanAllTables(ctx)
```

## Rules

- Tests require MySQL + Redis + WuKongIM running (see CI or `make env-test`).
- A change carrying risk must carry tests proportional to that risk.
- Run a focused test with `go test ./modules/<name>/ -run TestName`; full suite
  with `go test ./...`.
- `CleanAllTables` does NOT clear Redis rate-limit buckets — reset them
  explicitly in setup when a test hits a rate-limited route.

## Database

- ORM: `gocraft/dbr` v2.
- Migration files: `modules/<name>/sql/<yyyyMMdd>-<seq>_<name>.sql`, embedded via
  `//go:embed sql`.
- Field naming: underscore (`util.AttrToUnderscore()`).
