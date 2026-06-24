---
type: Task
title: "Task: incoming-webhook-thread"
description: Let an incoming webhook be bound to a thread (子区) so pushes deliver into the thread channel instead of only the parent group; all auth/membership/quota/cascade gates stay anchored on the parent group_no.
tags: ["incomingwebhook", "thread", "webhook", "wire-contract", "trust-boundary", "space", "isolation"]
timestamp: 2026-06-24T09:08:13Z
# --- octospec extension fields ---
slug: incoming-webhook-thread
upstream: self
source: self
---

# Task: incoming-webhook-thread

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Let an incoming webhook be **bound to a thread (子区)** so that its pushes deliver
into the thread channel `(channelID = group_no____short_id, channelType =
ChannelTypeCommunityTopic = 5)` instead of only the parent group
`(group_no, ChannelTypeGroup = 2)`.

Today the delivery target is hardcoded to the parent group in **three** places
(`modules/incomingwebhook/api.go`):
`handlePush` (`SendMessageWithResult`, ~`:1347-1348`), `testPush` (~`:1062-1063`),
and the mention/@AI expansion (~`:1338-1339`). The `incoming_webhook` row stores
only `group_no` + `space_id`, with no channel-type/thread field.

This task adds a per-webhook **delivery target** (group vs. thread) persisted on
the row, a **thread-scoped management mount**
(`/v1/groups/:group_no/threads/:short_id/incoming-webhooks` + the bot-token
variant), and switches the three delivery sites to the row's target. **The only
thing that changes is the SendMessage channel**: every authorization, membership,
quota, and lifecycle gate stays **anchored on the parent `group_no`**, because a
thread's membership and permissions are fully derived from its parent group.

**Push-caller zero-adaptation invariant.** The thread target is encoded ONLY on
the persisted row and bound at create time; it is NOT in the push URL, query, or
body. A thread webhook is pushed with the **exact same** URL shape
(`/v1/incoming-webhooks/:webhook_id/:token[/<adapter>]`) and the **exact same**
body as a group webhook — `handlePush` resolves `(channelID, channelType)` from
the row keyed by `webhook_id`. External callers (CI/platform/scripts) migrate by
swapping one `webhook_id`/`token`; nothing else. The IM client also needs no
adaptation: the message is a normal Text message delivered into the thread
channel (channelType=5), rendered by the existing thread message path, with the
`iwh_` sender identity resolved by the existing `ChannelGet` display datasource.
The ONLY new URL is the **management create** endpoint (nested route), used once
by the admin UI to provision the webhook — never by the push caller or renderer.

## Background

- **Why group-only today.** The `incoming_webhook` table
  (`sql/20260514000003_incomingwebhook_init.sql`) has only `group_no`/`space_id`;
  management routes mount at `/v1/groups/:group_no/incoming-webhooks`
  (`api.go:205`) and `/v1/bot/groups/:group_no/...`
  (`bot_api/incoming_webhook.go:23`); the three delivery sites above pin
  `ChannelID: m.GroupNo, ChannelType: common.ChannelTypeGroup.Uint8()`.
- **Threads are parent-group-derived channels.** A thread's channel is
  `group_no____short_id` with `ChannelTypeCommunityTopic` (`thread/const.go`,
  `thread/service.go:236-243`); its subscribers are **all parent-group members**
  (`thread/service.go:225-243`). Helpers already exist: `thread.BuildChannelID`,
  `thread.ExistThread`, `thread.IsValidShortID`, `thread.IService` (no import
  cycle: `thread` does not import `incomingwebhook`).
- **Established precedent.** `bot_api/obo_db.go:661-663` already models a
  CommunityTopic channel as `(membershipGroup = parent group, channel =
  parent____short)` — the same split this task uses.
- **@AI stays parent-group-scoped.** `pkg/mentionrewrite.ExpandAisToBotUIDs` is
  **GROUP-only** (`expand_ais.go:116` returns the payload unchanged for
  non-group channel types). A thread's bots ARE the parent group's bot members,
  so @所有 AI expansion must keep passing the **parent group identity**
  (`m.GroupNo` + `ChannelTypeGroup`); only the final delivery channel differs.
- **Decisions locked with maintainer:**
  - **Quota** — thread webhooks share the parent group's `max_per_group`
    (`insertWithQuota` already locks the parent `group` row and counts by
    `group_no`); no per-thread quota and no new system_setting.
  - **Lifecycle** — thread delete/archive does **not** actively cascade-disable
    its webhooks in v1; a banned/deleted thread channel makes the downstream
    `SendMessage` fail → push returns 502 (`push_delivery_failed`). Add a `TODO`
    noting a future thread-delete event listener. (Group **disband** already
    cascades via `disableByGroupNo`, which covers thread webhooks since they key
    on the parent `group_no`.)
  - **Push hot path** — no extra per-push thread-liveness query in v1; rely on
    the 502 fallback above.

## Load-bearing list

- **space / isolation / thread** — a thread's membership and permissions derive
  ENTIRELY from its parent group. All push-path gates MUST remain keyed on the
  parent `group_no`: group-Normal check (`cachedRequireActiveGroup`),
  creator-still-internal-member check (`cachedCreatorMembership` /
  `queryMemberRole`), and the mention member filter (`filterGroupMembers`). The
  persisted `space_id` stays derived from the parent group. A webhook must only
  ever target a thread that lives **under its own `group_no`** (cross-group /
  cross-Space delivery is forbidden) — enforced at create time, not from request
  input.
- **wire-contract** — the delivered `(ChannelID, ChannelType)` is the WuKongIM
  channel contract; the thread channelID format `group_no____short_id` +
  `ChannelTypeCommunityTopic` must come from the single source
  `thread.BuildChannelID`, never hand-concatenated. New columns
  (`channel_type` defaulting to the group type, `thread_short_id` defaulting to
  `''`) MUST preserve existing rows' group behavior byte-for-byte (backward
  compatibility for all current group webhooks).
- **webhook / trust-boundary** — the push endpoint is an unauthenticated,
  token-in-URL ingress. The delivery target MUST be derived **solely from the
  persisted webhook row**, never from the request body (consistent with
  `space_id`/`extra` already being discarded in `buildPayload`). Binding to a
  thread is validated **once, at create time** (thread exists, is under this
  `group_no`, is active), so the push hot path trusts the row.
- **error-response / i18n** — new management-side validation (thread not found /
  not under this group / deleted-or-archived) goes through
  `httperr.ResponseErrorL` + a registered `pkg/errcode` code (evaluate reusing
  `mgmt_group_not_found` / `mgmt_not_found` vs. a new `mgmt_thread_not_found`).
  Any new push-side reason stays uniform with the anti-enumeration 401 contract.
- **bot-api** — the bot-token management face
  (`bot_api/incoming_webhook.go`, `incomingwebhook.MountManagementRoutes`) must
  also gain the thread-scoped mount, preserving the existing permission matrix
  (bot must be a parent-group member; admin bot ⇒ admin rights; member bot ⇒
  manages only its own).
- **existing pipeline must be UNAFFECTED for group webhooks and reused as-is for
  thread webhooks** — auth → 4-layer rate-limit → group-active → creator-
  membership → audit; `insertWithQuota` (parent-group row lock + `max_per_group`);
  `handleGroupDisband` → `disableByGroupNo`; the `ChannelGet` display datasource
  (`iwh_` identity keyed by `webhook_id`, channel-agnostic). None of these change
  behavior; they simply continue to key on the parent `group_no`.
- **guard test** — `TestIncomingWebhookNoLegacyResponseError` must list any new
  handler files; new handlers must not use legacy/raw error responses.

## Out of scope

- **Per-thread quota** or a separate system_setting — thread webhooks share the
  parent group's `max_per_group` (locked decision).
- **Active cascade-disable on thread delete/archive** — v1 relies on the 502
  delivery-failure fallback; only a `TODO` + recovery note is added (locked
  decision). A thread-delete event listener is a later phase.
- **Per-push thread-liveness query** on the hot path (locked decision).
- **Platform adapters** (github/wecom/gitlab/feishu/multica) gain no
  thread-specific logic — they deliver to whatever channel the webhook is bound
  to via the shared `handlePush`; their body parsing is unchanged.
- **Existing group-webhook contract / push URLs** — push URL stays
  `/v1/incoming-webhooks/:webhook_id/:token[/<adapter>]` (target is resolved from
  the row, not the URL); `publicURLs` is unchanged.
- **Mention semantics** — mention targets stay filtered to parent-group members
  (`filterGroupMembers`) and @AI stays parent-group-scoped; no thread-member-only
  mention semantics.
- **Data backfill / migrating existing webhooks** — existing rows remain group
  webhooks via column defaults; no backfill.
- **Client rendering** of thread-webhook messages beyond what the existing
  thread message path already does.

## Acceptance

- Migration adds `channel_type` (default = group type, i.e. 2) and
  `thread_short_id` (default `''`); a group webhook created before or after the
  migration still delivers to `(group_no, ChannelTypeGroup)` unchanged.
- `POST /v1/groups/:group_no/threads/:short_id/incoming-webhooks` persists a row
  with `channel_type = CommunityTopic` and `thread_short_id = :short_id`; create
  is rejected with an i18n error when the thread does not exist, is not under the
  path `group_no`, or is deleted/archived.
- A thread-bound webhook push delivers to channel
  `(group_no____short_id, ChannelTypeCommunityTopic)`; a group-bound webhook push
  still delivers to `(group_no, ChannelTypeGroup)` (asserted on the captured
  `MsgSendReq`).
- **Push caller requires zero changes**: a thread webhook is pushed with the same
  URL (`/v1/incoming-webhooks/:webhook_id/:token`, incl. adapter suffixes) and the
  same body as a group webhook; the target is resolved server-side from the row.
  No thread identifier appears in the push URL, query, or body, and supplying one
  in the body is ignored (anti-forgery, same as `space_id`/`extra`).
- `testPush` for a thread webhook delivers into the thread channel and records an
  `adapter=test` audit row.
- Creator-left lazy-disable, group-disband cascade, and group-not-Normal gates
  still 401 / disable a **thread** webhook (all keyed on the parent `group_no`).
- `mention.bots`/@AI on a thread webhook calls `ExpandAisToBotUIDs` with the
  **parent group** identity (group `group_no` + `ChannelTypeGroup`); `mention.uids`
  is still filtered to current parent-group members.
- The bot face `/v1/bot/groups/:group_no/threads/:short_id/incoming-webhooks`
  creates/manages thread webhooks under the same permission matrix as the group
  face.
- Quota: group webhooks + thread webhooks under the same `group_no` count against
  one shared `max_per_group`; exceeding it returns 409.
- `go test ./modules/incomingwebhook/...` passes, including new thread-delivery
  tests and an updated `TestIncomingWebhookNoLegacyResponseError`; `go test
  ./modules/thread/...` unaffected; `make i18n-extract-check` + `make i18n-lint`
  pass; `golangci-lint run ./modules/incomingwebhook/...` is clean.
