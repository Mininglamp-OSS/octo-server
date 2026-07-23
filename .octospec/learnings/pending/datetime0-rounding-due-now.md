---
type: Learning
title: "MySQL DATETIME(0) rounds fractional seconds — a zero-delay 'due now' row can be ~0.5s in the future"
description: Writing next_retry_at = now into a DATETIME(0) column rounds up to the next whole second, so an immediate WHERE next_retry_at <= now claim can return nothing. Bites zero-delay tests; harmless in prod behind a real delay + poll cadence. Drive an explicit clock.
tags: [time, mysql, datetime, scheduling, testing, correctness]
timestamp: 2026-07-23T14:40:00Z
status: pending
---

# DATETIME(0) rounds fractional seconds → a "due now" row can be in the future

## Context

The group/space welcome delivery ledgers gate claimability on a `next_retry_at
DATETIME` column (no fractional precision): `WHERE status=pending AND
(next_retry_at IS NULL OR next_retry_at <= ?)`. A coalesce/enqueue path writes
`next_retry_at = now + window` (window can be 0). Both the write value and the
claim's `?` are app-supplied Go `time.Time`s.

## The trap

MySQL **rounds** (not truncates) a fractional-second value inserted into a
`DATETIME(0)` column to the nearest whole second. So `now = 13:24:20.6` stored
as `next_retry_at` becomes `13:24:21`. A claim issued microseconds later with
`? = 13:24:20.7` then evaluates `13:24:21 <= 13:24:20.7` → **false**, and the
row that should be "due now" is skipped until the next whole second.

Symptom seen: with `coalesceWindow = 0`, `enqueue` then an immediate
`claimBatch` in the same instant delivered **nothing** (0 rows), even though the
row was pending and "due". No error — just an empty claim.

## Why it's harmless in production

Real deployments use a positive delay (e.g. a 3s coalesce window) and a worker
that wakes on a poll cadence (e.g. every 5s), so a ±0.5s rounding wobble on the
due time is invisible — the row is always claimed on the next wake.

## The rule

- Do **not** assert delivery in a test that enqueues (`next_retry_at = now`) and
  claims in the **same sub-second instant** — the rounding boundary makes it
  flaky/empty. Drive an **explicit injected clock** and advance it between
  enqueue and claim (e.g. `clock = clock.Add(time.Minute)`), so the claim's
  `now` is unambiguously past any rounded due time.
- If sub-second scheduling precision ever matters for real, give the column
  fractional precision (`DATETIME(3)`) — but for human-facing delivery (welcome
  posts, retries) whole-second precision + a positive delay is the simpler,
  correct choice.
- Keep in mind rounding is **up-or-down to nearest**, so a stored due time can be
  either slightly before or slightly after the intended instant; never treat a
  freshly-written `= now` due time as immediately claimable.

## Promotion note

Candidate for a `persistence`/`testing` rule (pairs with the existing
`app-time-vs-now-column-epoch-compare` learning). Not auto-promoted — review
separately.
