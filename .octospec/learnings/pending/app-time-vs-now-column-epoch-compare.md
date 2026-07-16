---
type: Learning
title: "Compare app time against NOW()-written DATETIME columns via UNIX_TIMESTAMP, not a UTC time.Time param"
description: The DB session runs a deployment-local zone and the DSN sets no driver loc, so a Go time.Time param and a NOW()-written column are off by the session offset unless compared as epochs.
tags: [time, timezone, mysql, dbr, correctness]
timestamp: 2026-07-16T00:00:00Z
status: pending
---

# Compare app time vs NOW()-written DATETIME columns as epochs

## Context

octo-server's MySQL runs a deployment-local session timezone (this deployment
is `+08:00`; overseas mounts a `TZ` env var to run UTC). Legacy columns such as
`space_member.created_at` are written by `NOW()` in that local wall clock. The
MySQL DSN sets `charset=utf8mb4&parseTime=true` with **no `loc`**, so
go-sql-driver serializes a Go `time.Time` bound parameter as its **UTC** wall
clock.

## The trap

`WHERE created_at >= ?` with a `time.Time` (e.g. a UTC `active_from`) compares a
UTC-serialized string against a local-wall-clock column → off by the session
offset (8h here). This is silent: it "works" only when the DB session happens to
be UTC.

## The rule

When comparing an application-supplied absolute instant against a DATETIME
column that was written by `NOW()`/`CURRENT_TIMESTAMP` (deployment-local),
compare epoch-to-epoch:

```sql
WHERE UNIX_TIMESTAMP(col) >= ?     -- bind param = t.Unix()
```

`UNIX_TIMESTAMP(col)` interprets the column in the session zone and yields a UTC
epoch; `t.Unix()` is an unambiguous UTC epoch. This is TZ-agnostic and needs no
`CONVERT_TZ` (no tz tables) and no DSN `loc`. Valid because writer and reader
connections share one global session zone.

Precedent already in the repo: `modules/opanalytics/etl_db.go`
(`UNIX_TIMESTAMP(created_at)`, `SELECT UNIX_TIMESTAMP()` for "now").

A table you fully own can instead store all timestamps as app-supplied values in
one canonical zone and only ever compare them against each other (self-consistent
regardless of DB zone) — but the moment you compare against a `NOW()`-written
column you do not own, use the epoch form.

## Promotion note

Candidate for a `time`/`persistence` rule. Not auto-promoted — review separately.
