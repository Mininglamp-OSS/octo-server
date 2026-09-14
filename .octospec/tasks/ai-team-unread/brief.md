---
type: Task
title: "Task: ai-team-unread"
description: Expose AI session unread through the existing conversation sync without changing ordinary chat behavior.
tags: ["wire-contract", "space", "thread", "testing"]
timestamp: 2026-09-14T00:00:00Z
slug: ai-team-unread
upstream: user-request
source: self
---

# Task: ai-team-unread

## Goal

Use `/v1/conversation/sync` as the unread source while preserving ordinary sync
behavior and the existing AI Team metadata API contracts. The frontend owns
per-session unread state. An agent's total is the sum of its sessions' locally synchronized unread in the
current Space, not the shared AI Team group or private parent group unread.

Existing sync filters protected AI parent/session channels out of ordinary
`conversations`, so completely unchanged responses cannot deliver this data.
Add an independent `ai_team_conversations` sideband from the already-fetched
raw IM response, with no server unread storage or mutation hooks.

## Load-bearing list

- `wire-contract`: existing IM call, request parameters, normal conversations,
  filters, cursor calculation and clear-unread logic remain unchanged. Add the
  optional response field after existing processing completes.
- `space`, `auth`, `thread`: resolve only AI session candidates actually returned
  by IM, with indexed association lookup. Enforce owner, Space, ready/non-deleted
  session, protected active parent, active group membership, active identities
  and Space seats. Include archived/muted sessions. Exclude shared-group threads.
- `wire-contract`: `mode=full|delta` follows the effective version/message cursors
  actually sent upstream. Each item carries absolute `unread`, explicit zero,
  bot/session/channel IDs and IM sequence/version/time. A delta sum is not a
  whole-agent total; clients merge their channel state before aggregating.
- `availability`: sideband lookup failure omits only the new field. Existing
  chat sync still succeeds. Feature-off, unscoped and `msg_count=0` requests do
  not add the field. No AI candidates means no extra database query.
- `performance`: no extra IM request, Redis cache, scan of all agent sessions,
  per-session API call or server unread mutation. One indexed metadata lookup
  only for AI session candidates in the current IM response.

## Out of scope

- Changing AI Team metadata API contracts, unread storage, read endpoints,
  ordinary filtering/cursor logic, frontend UI implementation or IM message logic.
- Original worktree edits and deployment.

## Acceptance

- Same sync input with AI sideband enabled/disabled produces identical original
  response fields and IM request parameters. Ordinary group conversation remains
  present; protected AI channels do not leak into ordinary conversations.
- Session unread comes from the single existing IM result. Client aggregation
  isolates agents, owners and Spaces, includes archived/muted ready sessions,
  excludes deleted/non-ready/unregistered/wrong-type/wrong-parent channels.
- Full/delta/empty/error responses have distinct safe semantics. Mapping failure
  preserves normal sync. There is no new Redis state or AI metadata enrichment.
- Real WuKongIM reply, existing clear-unread and session delete are reflected by
  subsequent existing sync requests, with no mutation hooks.
- Relevant AI Team and message tests, build, vet, i18n and formatting pass.
