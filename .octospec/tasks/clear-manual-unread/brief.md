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
  manual-unread marker without querying WuKongIM conversation existence or
  real unread state;
- `PUT /v1/conversation/clearManualUnread` is the frontend's explicit marker
  clear whenever it re-enters a conversation that was manually unread before
  navigation;
- the existing `PUT /v1/conversation/clearUnread` preserves its real-unread
  clear and broadcast contract, and only `unread=0` best-effort clears the
  marker for Group and CommunityTopic conversations;
- conversation synchronization and Sidebar responses expose the marker to
  clients;
- persisted marker changes advance the conversation-extra version and attempt
  to notify the user's online clients with `CMDSyncConversationExtra`, whose
  payload directly identifies the channel, final marker state, and version.
- every production path that writes `conversation_extra` participates in one
  UID-scoped Redis lease so the user's shared extension cursor advances in
  commit order.

The marker is stored in `conversation_extra.manual_unread` under the current
user's exact `(uid, channel_id, channel_type)` row. It is a display state that
leaves the current message read position intact and is independent of
WuKongIM conversation existence and real unread state. If real messages and a
marker coexist, the frontend renders the real count without adding the marker
as an extra message.

The shipped scope is Group and CommunityTopic. Person conversations remain on
their existing real-unread behavior because one Person channel is shared
across Spaces while the current `conversation_extra` key has no Space
dimension.

## Background

Manual unread represents a user's request to keep a conversation visually
marked as unread. It is stored separately from the real unread count so the
client can render both states with OR semantics without inventing an unread
message or moving the message read position.

When the frontend re-enters a conversation that was manually unread before the
navigation, it calls `clearManualUnread` to clear the display marker regardless
of whether the message pull is empty. The existing `clearUnread` path remains
compatible only for a full real-unread clear: after its core clear and legacy
broadcast, `unread=0` best-effort clears the marker. A positive `unread` update
must leave the marker unchanged.

Conversation-extra versioning is the multi-device synchronization source of
truth. The CMD is a transient change notification whose `param` directly
carries `channel_id`, `channel_type`, `manual_unread`, and `version`; clients
may update that channel immediately and still use extension sync for recovery.

Manual unread is an explicit user request to keep a conversation visible as
unread. Group and CommunityTopic markers therefore exempt matching entries
from the Recent activity window in both `/v1/sidebar/sync` and the opt-in
`/v1/conversation/sync` Recent projection.

## Load-bearing list

- `setManualUnread` accepts Group and CommunityTopic targets, writes
  `manual_unread=true`, and returns `changed=true` with the new extension
  version without calling `IMGetConversations` or otherwise querying WuKongIM
  conversation existence or real unread state. An already-set marker returns
  the idempotent
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
- After WuKongIM accepts a `clearUnread` request, the handler sends the legacy
  `CMDConversationUnreadClear` broadcast before accessing conversation-extra.
  Group and CommunityTopic marker cleanup is attempted only for `unread=0`
  and is best-effort after that broadcast; `unread>0` leaves the marker
  unchanged. Once the core clear and broadcast complete, the supplementary
  cleanup runs on a request-value-preserving context that ignores client
  disconnect cancellation and has its own five-second timeout. Query, version,
  store, and extension-CMD failures are logged and cannot change the completed
  core response. Person `clearUnread` retains its existing real-unread
  behavior.
- A marker write and its new conversation-extra version are committed before
  the service attempts `CMDSyncConversationExtra` on the user's Person channel
  with `NoPersist=true`. Its `param` carries the exact `channel_id`,
  `channel_type`, final `manual_unread`, and `version`. Notification delivery
  is best-effort: a failure is logged and does not replace the committed
  success result with an HTTP error.
- The ordinary extension update, dedicated set/clear endpoints, and
  `clearUnread` marker cleanup all acquire the same UID-scoped Redis lock. Each
  lease has a request-unique UUID owner and a 10-second TTL; ownership is
  compare-renewed immediately before the database write and compare-deleted on
  release. CMD delivery occurs only after release.
- Version allocation under the lease compares the existing per-user database
  high-water mark with `GenSeq`, and every database update accepts only a
  strictly newer version. Conditional UPSERT/UPDATE results determine whether
  a state transition actually occurred, so an expired stale owner cannot move
  an existing row's version backwards.
- Ordinary conversation-extra updates atomically preserve
  `manual_unread`; the dedicated endpoints and the Group/CommunityTopic
  `clearUnread` integration own marker state transitions.
- `/v1/conversation/sync` exposes the marker in `conversation.extra`,
  `/v1/conversation/extra/sync` exposes it in each top-level extension row,
  and `/v1/sidebar/sync` exposes it as an item-level `manual_unread` field.
- Sidebar first builds entries from its existing IM, follow, and thread data
  sources—including the existing DB-only CommunityTopic merge—and then
  overlays manual-unread state on the resulting items.
- The Sidebar Follow lookup runs after its final item set is known. The Recent
  lookup runs before the time window against only the bounded, visible IM
  candidate keys so manual unread can act as an exemption. Both queries select
  only `channel_id` / `channel_type`; neither scans the user's full extension
  row set. A Recent marker-query failure skips the time window for that poll so
  incomplete auxiliary state cannot hide a manually unread conversation.
- Recent visibility uses the configured activity window with real-unread,
  manual-unread, pinned, and system-bot exemptions. Group and CommunityTopic
  markers retain matching conversations beyond the configured window in both
  Recent response paths.
- Legacy Person markers are projected as `manual_unread=false` by conversation
  and Sidebar synchronization.
- The `conversation_extra.manual_unread` column is installed through the
  message module's embedded SQL migration mechanism.

## Out of scope

- Frontend changes to decide when `clearManualUnread` is called.
- Parent-group aggregation for community-topic manual-unread state.
- Space-scoped Person manual-unread state.
- Manual-unread-driven creation of new Sidebar entries.
- Changes to the existing real-unread cursor semantics.

## Acceptance

- A first Group or CommunityTopic `setManualUnread` call persists `true`,
  returns `changed=true` and a version, and emits
  `CMDSyncConversationExtra`.
- A Group or CommunityTopic can be marked manually unread whether its
  WuKongIM conversation is missing, has `unread=0`, or has real `unread>0`;
  `setManualUnread` performs no WuKongIM conversation lookup.
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
- Group and CommunityTopic `clearUnread(unread=0)` attempts to clear
  manual-unread after the real-unread clear and legacy CMD; `unread>0` leaves
  manual-unread unchanged. Any supplementary cleanup failure is logged without
  suppressing the legacy broadcast or changing the core success response.
  Client disconnect after the legacy broadcast does not cancel that cleanup;
  its independent context remains bounded to five seconds. Person
  `clearUnread` leaves legacy manual rows untouched.
- Ordinary draft and read-position extension updates preserve a concurrently
  stored manual marker.
- Manual markers annotate eligible Sidebar entries but do not independently
  create Group or CommunityTopic entries. Existing manually unread entries are
  retained beyond the configured Recent window in both response paths.
- Dedicated marker changes remain visible through extension-version sync and
  attempt `CMDSyncConversationExtra` as a best-effort notification. A CMD
  failure after commit is logged while the endpoint still returns the changed
  state and version. The `clearUnread` integration applies the same policy to
  its supplementary marker notification after the legacy broadcast.
- Two concurrent first-time `setManualUnread` requests are serialized: exactly
  one returns `changed=true`; the other observes the committed marker and
  returns the idempotent `changed=false` result.
- Redis lock tests prove the owner is a UUID, the lease has a positive TTL,
  compare-renew rejects a replaced owner, and an expired owner cannot delete a
  successor's lock. Database tests prove stale set, clear, and ordinary-extra
  writes cannot replace a newer stored version.
- The manual-unread database, concurrent-transition, version-high-water,
  notification-failure, and `clearUnread` ordering regressions run in the
  default `modules/message` test lane. They do not depend on the optional
  `integration` build tag and remain executable under CI `-race -shuffle`.
- Formatting, package compilation, focused regression tests, and
  `git diff --check` pass.
