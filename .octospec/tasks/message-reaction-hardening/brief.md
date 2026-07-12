---
type: Task
title: "Task: message-reaction-hardening"
description: Harden message reactions (channel-scoped, visibility-gated, text-only) and upgrade to a multi-reaction model with an atomic upsert; align write/sync/read authorization.
tags: ["message", "reaction", "acl", "wire-contract", "auth", "thread", "i18n", "error-response"]
timestamp: 2026-07-16T00:00:00Z
# --- octospec extension fields ---
slug: message-reaction-hardening
upstream: web reaction handoff
source: self
---

# Task: message-reaction-hardening

## Goal

Make the message reaction API safe to ship and aligned with the web reaction UX:
bind every reaction entry point (write / sync / inline read) to the real message
channel and the caller's current visibility, and upgrade storage from
single-reaction-per-user to a multi-reaction model (Slack/企微-style: each emoji
an independent toggle).

## Background

The server already had `POST /v1/reactions`, `POST /v1/reaction/sync`, and inline
`reactions` on message responses. The original implementation stored a single
`emoji` per user/message and only gated the group write path (`ExistMember`);
sync/read paths and cross-channel scoping were unguarded. The web client (reaction
API spec handoff) implements a multi-reaction UI with optimistic updates, so the
server needed both a security pass and a semantics change.

Scope grew in two rounds, both delivered under this task:
1. Channel-scoping + write-path visibility hardening + text-only restriction.
2. Multi-reaction model + write/sync/read authorization parity (external review F1–F4).

## Load-bearing list

- All reaction reads/writes are scoped by `(channel_id, channel_type, message_id)`;
  the global `message_id`-only queries are removed (no cross-channel row leak).
- **Write** (`POST /v1/reactions`) rejects a reaction unless the target message
  exists in the requested channel and is visible to the caller (revoked / deleted /
  user-deleted / `visibles` / expired / offset-truncated all fail closed), enforces
  active-member (group) and parent-group active-member + non-deleted-thread (topic)
  access, and only allows plain-text messages (`payload.type == 1`).
- **Sync** (`POST /v1/reaction/sync`) uses the same membership posture
  (`ExistMemberActive`, topic parent-group + non-deleted-thread, unknown type
  rejected) and filters out reactions whose target message is no longer visible to
  the caller (shared `messageVisibleToViewer` predicate).
- Multi-reaction: `(uid, message_id, channel, emoji)` is unique; toggling is an
  atomic `INSERT ... ON DUPLICATE KEY UPDATE is_deleted = 1 - is_deleted` upsert.
- Wire compatibility: inline `reactions[]` fields (`seq`/`uid`/`name`/`emoji`/
  `is_deleted`/`created_at`) unchanged; write endpoint additionally returns the
  final `{emoji, seq, is_deleted}`; `syncMessageReaction` CMD param gains
  `message_id/emoji/seq`.
- Error responses stay on the i18n envelope; new `reaction_unsupported_type` code.

## Out of scope

- Sticker reactions (`reaction_type`, sticker metadata / renderable URL) — server
  stores only the `emoji` string; clients fall back `reaction_key = emoji`.
- `reaction_type` / `reaction_key` wire fields (web derives them from `emoji`).
- Inline-read summary/count form for large channels (full detail is inlined).
- A dedicated "reactions by message_id" endpoint (sync-by-channel+seq is reused).
- Server-side feature flag / appconfig field for reaction rollout.
- Frontend reaction menu / chip rendering / CMD client handling.

## Acceptance

- Wrong-channel writes are rejected and leave no `reaction_users` row.
- Revoked / deleted / invisible / non-text messages cannot be reacted to.
- Multi-emoji: different emojis are independent rows; same emoji toggles in place;
  repeated toggles never create duplicate rows (unique index + atomic upsert).
- Sync rejects non-member topic / unknown channel type, and hides reactions whose
  target message is no longer visible (e.g. revoked).
- `go build ./...`, `go vet -tags integration ./modules/message/`, focused unit +
  integration tests, `make i18n-lint` all pass.
