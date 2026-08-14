---
type: Journal
title: "Journal: notification-pause-manual-mode"
description: Add explicit manual notification pause state and unify REST/CMD responses.
tags: [notification, wire-contract, migration, testing]
timestamp: 2026-08-14T00:00:00+08:00
task: notification-pause-manual-mode
source: self
---

# Journal: notification-pause-manual-mode

## What was done

- Added nullable `mode` persistence with `manual` and `timed` semantics.
- Added server-calculated `duration` requests for `30m` and `1h`, explicit
  `mode: manual`, strict mutually exclusive request validation, and the mode
  migration.
- Unified GET/PUT/DELETE and notification-pause CMD state fields, including
  `mode`, `revision`, and `server_time`; DELETE is idempotent and only emits a
  CMD when it actually clears an active row.
- Preserved legacy rows with a future `paused_until` as timed pauses and made
  manual rows active in webhook recipient filtering.

## Verification and gotchas

- Focused notification, errcode, webhook pure-logic tests, `go vet`, i18n
  extraction check, direct-error lint, and diff checks pass.
- The DB round-trip test skips when local MySQL is unavailable and runs in CI.
- The branch was verified against remote `upstream/main=6059503` before the
  implementation was based on it.
