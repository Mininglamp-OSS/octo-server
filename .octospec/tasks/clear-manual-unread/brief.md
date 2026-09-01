---
type: Task
title: "Task: conversation-manual-unread"
description: Add and maintain the conversation manual-unread state, including set, dedicated clear, WuKongIM compatibility, and clearUnread integration.
tags: ["message", "conversation", "manual-unread", "wukongim", "compatibility", "testing"]
timestamp: 2026-08-27T00:00:00+08:00
slug: clear-manual-unread
upstream: none
source: self
---

# Task: conversation-manual-unread

## Goal

Implement the complete application-level manual-unread flow for conversations:

- `setManualUnread` marks a conversation as manually unread only when WuKongIM
  reports that its normal unread count is zero;
- `clearManualUnread` provides a dedicated endpoint for the frontend's
  conversation-open flow and clears only the application-level marker;
- `clearUnread` continues to update WuKongIM's real unread state and also
  clears the manual marker for every valid explicit unread operation.

The manual marker is stored in `conversation_extra` and is independent of
WuKongIM's real unread cursor or browse position.

## Background

The pinned WuKongIM version removed the legacy recent-conversations endpoint
used by `setManualUnread`. The target conversation must therefore be located through
the supported `IMSyncUserConversation` call using an unfiltered current-state
query (`version=0`, `msgCount=1`, and an empty `lastMsgSeqs` value). This avoids
the client's cursor filtering from hiding the conversation whose unread state
is being checked.

The frontend may enter a conversation whose initial message sync returns no
messages and consequently may not call `clearUnread`. The dedicated
`clearManualUnread` endpoint lets it clear the manual marker after opening the
conversation without changing WuKongIM's real unread count. The endpoint is
idempotent, so the frontend can call it after the initial conversation-open
sync without needing to inspect the message count on the server.

## Load-bearing list

- `PUT /v1/conversation/setManualUnread` must locate the current user's target
  conversation with the supported WuKongIM conversation-sync API and must
  distinguish normal unread from manual unread.
- The `setManualUnread` WuKongIM query uses `IMSyncUserConversation(loginUID, 0, 1,
  "", nil)` or an equivalent target-preserving query. It must not reintroduce
  the removed `IMGetConversations` endpoint.
- The manual state is keyed by the authenticated user's exact
  `(channel_id, channel_type)` pair. Parent groups and community topics keep
  independent manual-unread states; there is no implicit parent aggregation.
- The new endpoint must only clear the authenticated user's exact
  `(channel_id, channel_type)` manual-unread row.
- The new endpoint must update the conversation-extra sync version and notify
  other logged-in clients with `CMDSyncConversationExtra`.
- Existing `clearUnread` calls with any valid `unread` value must clear the
  target conversation's manual-unread marker as well as update WuKongIM.
- Manual state operations are scoped to the authenticated user's exact
  `(uid, channel_id, channel_type)` row. They intentionally do not require
  current group or parent-group membership, so users can clear stale state
  after leaving a group or topic.
- The database write for a manual-state change must happen before the sync
  command is sent, so clients never receive a stale `manual_unread` value.
- Ordinary conversation-extra updates that omit `manual_unread` must preserve
  the existing value for compatibility with older clients; an explicit field
  is required to change it through that update path.
- The `conversation_extra.manual_unread` migration must be registered through
  the existing message-module SQL migration mechanism.

## Out of scope

- Frontend changes to decide when `clearManualUnread` is called.
- Parent-group aggregation for community-topic manual-unread state.
- Changes to WuKongIM or the real unread cursor semantics.
- Changes to octo-lib's removed/legacy conversation endpoint or unrelated
  callers.

## Acceptance

- `PUT /v1/conversation/setManualUnread` uses the supported conversation-sync API
  and no longer calls `IMGetConversations`.
- `setManualUnread` writes `manual_unread=true` only for a target conversation whose
  WuKongIM normal unread count is zero, and not when the marker is already set.
- `PUT /v1/conversation/clearManualUnread` is authenticated and space-scoped.
- The endpoint is idempotent when the row is absent or already clear.
- It does not call `IMClearConversationUnread` or `CMDConversationUnreadClear`.
- `setManualUnread` and `clearManualUnread` never modify another user's row,
  even when the caller is no longer a member of the requested group or topic.
- `clearUnread` clears manual-unread for both `unread=0` and `unread>0`.
- A successful state change updates the conversation-extra version and sends
  `CMDSyncConversationExtra` to the user's logged-in clients.
- Formatting, package compilation, focused regression tests, and
  `git diff --check` pass.
