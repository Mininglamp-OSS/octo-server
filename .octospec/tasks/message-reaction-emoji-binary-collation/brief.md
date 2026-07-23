---
type: Task
title: "Task: message-reaction-emoji-binary-collation"
description: Make reaction emoji identity byte-exact in MySQL and return the persisted toggle result.
tags: ["message", "reaction", "mysql", "migration", "wire-contract", "testing"]
timestamp: 2026-07-22T00:00:00Z
slug: message-reaction-emoji-binary-collation
upstream: production reaction collision report
source: self
---

# Task: message-reaction-emoji-binary-collation

## User journey

As a user who already reacted with one emoji, I want a different emoji to create
an independent reaction, so that adding or cancelling it never mutates the first
reaction and every write response matches the durable server state.

## Root cause

`reaction_users.emoji` inherits `utf8mb4_general_ci`. On the deployed MySQL-compatible
database that collation compares `👍` and `🎉` as equal. The five-column unique
index therefore treats the second emoji as a duplicate, and the atomic upsert
toggles the existing `👍` row while the handler echoes the requested `🎉`.

## Load-bearing changes

- Add a new forward migration that changes only `reaction_users.emoji` to
  `utf8mb4_bin`; do not edit an already-applied migration or change the table-wide
  collation.
- Keep `(channel_id, channel_type, message_id, uid, emoji)` as the unique key.
- Make `toggleReaction` return the persisted `emoji`, `seq`, and `is_deleted` from
  its existing post-upsert read.
- Build the sync command and HTTP response from that persisted result. Treat a
  requested/persisted emoji mismatch as a storage invariant failure.

## Out of scope

- Unicode normalization or collapsing visually similar but byte-distinct emoji
  sequences such as `❤` and `❤️`.
- Reconstructing historical reactions that were never persisted.
- Changing reaction authorization, visibility, Space isolation, or rate limits.
- Changing the remaining `reaction_users` column collations.

## Acceptance

- The `emoji` column reports `utf8mb4_bin` after migrations.
- Adding `👍` and then `🎉` creates two active rows and sync returns both.
- Clicking `🎉` again changes only the `🎉` row to deleted; `👍` stays active.
- Write responses and sync commands use the persisted emoji/seq/deleted state.
- Focused integration and DB-helper tests, `go test ./modules/message`, race,
  `go vet`, formatting, and migration checks pass.

## Rollout and rollback

- Check the production table size before rollout because a collation change may
  rebuild the table or hold a metadata lock.
- Application rollback is compatible with the binary collation. The migration
  Down path is intentionally a no-op: reverting to `general_ci` can fail once
  byte-distinct emoji rows coexist under the unique index.
