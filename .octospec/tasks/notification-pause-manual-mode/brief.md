---
type: Task
title: "Task: notification-pause-manual-mode"
description: "Add explicit manual notification pause mode and unify pause API state responses."
tags: ["notification", "wire-contract", "error-response", "test"]
timestamp: 2026-08-14T00:00:00Z
slug: notification-pause-manual-mode
upstream: self
source: self
---

# Task: notification-pause-manual-mode

## Goal

Extend the account-level notification pause service with an explicit `manual`
mode that remains active until DELETE, while preserving timed and custom pause
behavior. Make GET/PUT/DELETE and the notification-pause CMD expose the same
authoritative state shape, including `mode`, `revision`, and `server_time`.

## Background

The current service stores only `paused_until`, so it cannot represent a pause
that has no automatic expiry and its request/response contract only supports a
custom absolute timestamp. The client must use server-generated time and state
without putting device-local `scope` into the server contract.

## Load-bearing list

- `wire-contract`: REST and CMD response fields must remain aligned.
- `error-response`: invalid combinations and timestamps use the registered
  localized error envelope.
- `auth`: notification pause remains account-scoped behind the existing auth
  middleware.
- `webhook`: manual pauses must filter offline notifications until DELETE.
- `test`: cover request validation, state transitions, time and revision rules.

## Out of scope

- Frontend implementation, timer scheduling, server-time offset calculation,
  and local device `scope` storage.
- Voice-input settings UI.
- A global DSN/timezone refactor outside notification-pause persistence.
- New event fan-out beyond the existing notification-pause CMD.

## Acceptance

- `duration` accepts only `30m` and `1h`, calculated from server time.
- `mode: "manual"` persists and returns `paused=true`, `mode="manual"`, and
  `paused_until=null` until DELETE.
- Timed responses return `mode="timed"`; inactive responses return
  `mode=null` and `paused_until=null`.
- `duration`, `mode`, and `paused_until` are mutually exclusive; invalid,
  timezone-less, past, and over-limit absolute timestamps return the existing
  localized 400 error envelope.
- GET/PUT/DELETE and CMD responses contain `paused`, `mode`, `paused_until`,
  `revision`, and `server_time`; revisions increase for every state-changing
  request and repeated DELETE remains idempotent.
- Manual and timed states are both covered by service/DB/webhook tests, and
  focused Go tests pass.
