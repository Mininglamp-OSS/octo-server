---
type: Task
title: "Task: conversation-manual-unread"
description: Add Group and CommunityTopic manual-unread state, dedicated set and clear APIs, conversation-extra synchronization, and clearUnread integration.
tags: ["message", "conversation", "manual-unread", "sidebar", "sync", "testing"]
timestamp: 2026-08-27T00:00:00+08:00
slug: clear-manual-unread
upstream: none
source: self
---

# Task: conversation-manual-unread

## Goal

Deliver the complete application-level manual-unread flow for Group and
CommunityTopic conversations:

- `PUT /v1/conversation/setManualUnread` stores the authenticated user's
  manual-unread marker;
- `PUT /v1/conversation/clearManualUnread` clears that marker as a dedicated
  fallback in the frontend's conversation-open flow;
- the existing `PUT /v1/conversation/clearUnread` clears the marker whenever it
  successfully updates a Group or CommunityTopic unread state;
- conversation synchronization and Sidebar responses expose the marker to
  clients;
- every persisted marker change advances the conversation-extra version and
  notifies the user's online clients with `CMDSyncConversationExtra`.

The marker is stored in `conversation_extra.manual_unread` under the current
user's exact `(uid, channel_id, channel_type)` row. It is a display state that
can coexist with the conversation's real unread count and leaves the current
message read position intact.

The shipped scope is Group and CommunityTopic. Person conversations remain on
their existing real-unread behavior because one Person channel is shared
across Spaces while the current `conversation_extra` key has no Space
dimension.

## Background

Manual unread represents a user's request to keep a conversation visually
marked as unread. It is stored separately from the real unread count so the
client can render both states with OR semantics without inventing an unread
message or moving the message read position.

The frontend may enter a conversation whose initial message sync returns no
messages and consequently may not use its existing `clearUnread` path. In that
case, `clearManualUnread` completes the conversation-open flow by clearing the
display marker while preserving the real unread state. When the existing
`clearUnread` path is used, that endpoint performs both updates and the
frontend avoids a second clear request.

Conversation-extra versioning is the multi-device synchronization source of
truth. The CMD is a transient change notification; clients fetch the updated
extension rows and refresh Sidebar after receiving it.

Recent-window behavior is deliberately rollout-safe across Web, Android, and
iOS. Web may learn to render `manual_unread` before the Android and iOS clients
are upgraded. If the server used a manual marker to exempt an old conversation
from the Recent activity window during that mixed-version period, an older
mobile client would receive and display the out-of-window conversation but
would not render its manual-unread dot. The resulting "old conversation shown
without a red dot" experience is avoided by keeping the existing Recent
window unchanged. A manually unread conversation outside that window therefore
remains hidden in the current implementation; this is an intentional
multi-client compatibility decision, not a bug.

## Load-bearing list

- `setManualUnread` accepts Group and CommunityTopic targets, writes
  `manual_unread=true`, and returns `changed=true` with the new extension
  version. An already-set marker returns the idempotent
  `changed=false, reason=already_manual_unread` result.
- `clearManualUnread` accepts the same channel types, writes
  `manual_unread=false`, and returns the new extension version. A missing or
  already-clear marker returns `changed=false` as an idempotent success.
- Person and unknown channel types return the localized request-invalid
  envelope before any manual-state write or synchronization CMD.
- The manual state is keyed by the authenticated user's exact
  `(uid, channel_id, channel_type)` row. Parent groups and community topics
  maintain independent markers without parent aggregation, and the same row
  remains available for stale-state cleanup after the user leaves a group or
  topic.
- Every successful Group or CommunityTopic `clearUnread` request clears an
  existing manual marker for both `unread=0` and `unread>0`. Person
  `clearUnread` retains its existing real-unread behavior.
- A marker write and its new conversation-extra version are committed before
  `CMDSyncConversationExtra` is sent to the user's Person channel with
  `NoPersist=true`.
- Ordinary conversation-extra updates atomically preserve
  `manual_unread`; the dedicated endpoints and the Group/CommunityTopic
  `clearUnread` integration own marker state transitions.
- `/v1/conversation/sync` exposes the marker in `conversation.extra`,
  `/v1/conversation/extra/sync` exposes it in each top-level extension row,
  and `/v1/sidebar/sync` exposes it as an item-level `manual_unread` field.
- Sidebar first builds entries from its existing IM, follow, and thread data
  sources—including the existing DB-only CommunityTopic merge—and then
  overlays manual-unread state on the resulting items.
- Recent visibility continues to use the configured activity window and its
  existing real-unread, pinned, and system-bot exemptions. A surviving item
  carries its manual marker, while the marker itself does not extend that
  window. This preserves consistent rendering while Web, Android, and iOS may
  run different manual-unread feature versions.
- Legacy Person markers are projected as `manual_unread=false` by conversation
  and Sidebar synchronization.
- The `conversation_extra.manual_unread` column is installed through the
  message module's embedded SQL migration mechanism.

## Out of scope

- Frontend changes to decide when `clearManualUnread` is called.
- Parent-group aggregation for community-topic manual-unread state.
- Space-scoped Person manual-unread state.
- Manual-unread-driven creation of new Sidebar entries or exemption from the
  Recent activity window.
- A coordinated future rollout that makes manual unread a Recent-window
  exemption after every supported client can render the marker.
- Changes to the existing real-unread cursor semantics.

## Acceptance

- A first Group or CommunityTopic `setManualUnread` call persists `true`,
  returns `changed=true` and a version, and emits
  `CMDSyncConversationExtra`.
- Repeating `setManualUnread` returns `changed=false` with
  `reason=already_manual_unread` and leaves the stored version unchanged.
- A first `clearManualUnread` call against a set marker persists `false`,
  returns `changed=true` and a version, and emits
  `CMDSyncConversationExtra`.
- Clearing an absent or already-clear marker returns `changed=false` without
  creating a new version.
- Group and CommunityTopic requests can set and clear independent markers.
- Person requests to `setManualUnread` and `clearManualUnread` return the
  localized request-invalid response before a write or synchronization CMD.
- Conversation, conversation-extra, and Sidebar synchronization expose Group
  and CommunityTopic markers with the JSON field name `manual_unread` and
  project legacy Person markers as false.
- `PUT /v1/conversation/clearManualUnread` is authenticated and space-scoped.
- `setManualUnread` and `clearManualUnread` update only the authenticated
  user's exact row, including when the caller is no longer a member of the
  requested group or topic.
- Group and CommunityTopic `clearUnread` clears manual-unread for both
  `unread=0` and `unread>0`; Person `clearUnread` leaves legacy manual rows
  untouched.
- Ordinary draft and read-position extension updates preserve a concurrently
  stored manual marker.
- Manual markers annotate eligible Sidebar entries but do not independently
  create Group or CommunityTopic entries and do not retain an item beyond the
  configured Recent window. An out-of-window manually unread conversation
  remaining hidden during a mixed Web/Android/iOS rollout is expected behavior,
  not a defect.
- Every successful marker change is visible through extension-version sync and
  sends `CMDSyncConversationExtra` to the user's online clients.
- Formatting, package compilation, focused regression tests, and
  `git diff --check` pass.
