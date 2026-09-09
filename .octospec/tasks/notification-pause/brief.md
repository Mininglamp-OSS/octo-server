---
type: Task
title: "Task: notification-pause"
description: Add account-level notification pause state with cross-device synchronization and offline-push filtering.
tags: ["wire-contract", "reliability", "test"]
timestamp: 2026-08-12T10:15:00Z
slug: notification-pause
upstream: octo-web settings-center T19
source: self
---

# Task: notification-pause

## Goal

Provide an account-level notification pause API and synchronize the authoritative
state to all online devices. A valid pause suppresses message offline Push for
paused recipients while RTC/call Push remains unaffected.

## Reliability and contract

- GET/PUT/DELETE operate only on the authenticated UID and return revisioned state.
- Successful mutations send `user.notification_pause.changed` after the database write.
- Known paused UIDs are excluded from message offline Push; duplicates and order are preserved.
- A pause lookup failure logs an observable error and preserves the original offline-Push batch.
- UID lookups are bounded in batches; `paused_until` is limited to 30 days.
- RTC/call Push behavior remains unchanged.

## Out of scope

- RTC/call Push suppression.
- Independent sync channels, outbox, ACK protocol, or device-level pause state.
- Client-side scheduling UX beyond server-side validation.

## Verification

- Focused Go tests for notification state, webhook filtering, and lookup failure.
- `make i18n-extract-check` and `make i18n-lint`.
