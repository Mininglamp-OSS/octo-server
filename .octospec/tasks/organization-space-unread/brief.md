---
type: Task
title: "Task: organization-space-unread"
description: "Expose an optional per-Space unread snapshot for the Web organization switcher."
tags: ["message", "space", "isolation", "wire-contract", "testing"]
timestamp: 2026-09-22T00:00:00Z
slug: organization-space-unread
source: self
upstream: "https://github.com/Mininglamp-OSS/octo-server/issues/903"
---

# Task: organization-space-unread

## Goal

Extend the existing conversation sync response with an opt-in, authoritative per-Space unread snapshot for Web display, reusing existing unread values and DM message traversal.

## Background

Conversation sync already returns conversation unread counts and computes current-Space DM unread counts. The Web organization switcher needs the same unread state grouped by effective Space without a new endpoint, persistence layer, poller, or extra per-Space query.

## Load-bearing list

- `space/isolation`: aggregation must not change conversation visibility or leak an invalid group membership.
- `wire-contract`: old clients receive no new sideband or organization-aggregation work unless they opt in; the separately documented DM peer-ID normalization and current-Space unread-window correction also apply to existing clients.
- `message-unread`: muted DMs and groups are excluded; thread unread follows the parent-group mute state; existing read/unread semantics remain authoritative.
- `resource-cost`: aggregation stays linear and must not add a third DM message query.

## Out of scope

- Desktop notifications, sounds, and notification click behavior.
- Mobile behavior or cross-device synchronization of the Web-only new-unread marker.
- New database tables, Redis state, polling, or a separate unread endpoint.
- Changes to current-Space conversation filtering and read protocols.
- Normalizing legacy Space-prefixed Person IDs in message-history visibility, system-Bot placeholder, or sidebar filtering paths; this task only normalizes conversation metadata and unread accounting.
- Reassigning historical unread after a default Space becomes inactive or inaccessible; those buckets are outside the active-Space snapshot domain.
- Changes to the existing current-Space `space_last_message` preview fallback.
- New cross-Space Bot-membership queries; any regular-Bot unread makes the optional authoritative snapshot unavailable because a message's send-time Space tag cannot prove current Bot membership.
- Independent per-thread mute lookup (`thread_setting.mute`); organization aggregation intentionally follows the parent group's mute state.

## Acceptance

- `include_space_unreads` is optional and omitted responses remain backward compatible.
- A complete opted-in sync returns `space_unreads`, including `{}` when there are no eligible unread messages.
- Snapshot keys are restricted to active Spaces where the caller remains an active member; unauthorized or inactive Space keys are not published.
- Dropping an out-of-domain bucket does not make the active-Space snapshot incomplete; otherwise inaccessible historical unread could suppress updates for every active Space indefinitely.
- Authorization uses one batched active-Space membership query for an eligible opted-in snapshot, with no per-Space query fan-out.
- Group, external group, thread, and DM unread counts use effective Space ownership; muted DMs and groups are excluded, and threads inherit their parent group's mute state.
- Any regular-Bot DM unread omits the authoritative snapshot; tagged messages are also excluded because the send-time Space tag cannot prove current Bot membership.
- An unread DM with an invalid payload, or an untagged unread DM without a default Space, omits the authoritative snapshot instead of silently under-counting it.
- When DM Recents do not cover all unread messages, the organization-unread window follows pinned WuKongIM tag `v2.2.4-20260313` (revision `94b06a4694fa`): `PullModeDown=0`, `end <= start`, returning `(end, start]`. The separate message-preview fallback remains unchanged.
- An incomplete baseline omits `space_unreads` so Web can retain its previous value.
- A baseline that reaches the pinned WuKongIM `conversation.userMaxCount` is treated as potentially truncated and omits `space_unreads`.
- Once the snapshot is known to be incomplete, filtered-out DMs do not trigger organization-only unread-window pulls that would be discarded.
- Focused tests cover aggregation, serialization, mute behavior, and failure/omission behavior.
