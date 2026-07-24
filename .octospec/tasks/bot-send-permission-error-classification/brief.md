---
type: Task
title: "Task: bot-send-permission-error-classification"
description: Classify missing Bot API send targets separately from infrastructure query failures and persist enough low-cardinality telemetry to diagnose permission-check failures after pod logs rotate.
tags: ["bot-api", "error-response", "i18n", "wire-contract", "space", "isolation", "acl", "observability", "metrics", "testing", "commit"]
timestamp: 2026-07-24T08:46:00+08:00
# --- octospec extension fields ---
slug: bot-send-permission-error-classification
upstream: none (diagnosed from a production /v1/bot/sendMessage incident)
source: user
---

# Task: bot-send-permission-error-classification

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Make Bot API send-permission failures actionable without widening access or
changing the legacy transport contract:

1. A missing target group (or missing parent group for a thread) is a not-found
   business result, not `err.server.bot_api.query_failed`.
2. A DM with no established Bot relationship keeps its existing safe business
   denial (`not_friend` for User Bot, `conversation_not_started` for App Bot),
   while a real friend-query failure remains distinguishable in server-side
   telemetry.
3. A real DB/query failure remains the internal, retryable
   `err.server.bot_api.query_failed` result.
4. Every unexpected permission-check failure carries the existing request trace
   ID in structured logs and increments a bounded-label Prometheus counter, so
   the failing stage remains identifiable after container logs rotate.

## Background

During a production display-card incident, a call to
`POST /v1/bot/sendMessage` returned wire HTTP 400 with
`error.code=err.server.bot_api.query_failed` and `error.http_status=500`.
Production Prometheus retained the matching HTTP 400 while the MySQL
driver-level query-error counter did not increase. The container logs had
already rotated and no retained centralized log covered the workload, so the
original `zap.Error` could not be recovered. Environment names, workload names,
request time, caller identity, Bot identity, and target identifiers are
intentionally omitted from this committed brief.

Source tracing pins the response to `sendMessage -> checkSendPermission ->
respondSendPermissionError`; card validation returns `card_invalid` and
WuKongIM dispatch returns `send_failed`, so neither can produce this code.

The strongest zero-driver-error branch is `isGroupDisbanded`: it performs
`SELECT status FROM group WHERE group_no=?` with `LoadOne`. A missing row returns
`dbr.ErrNotFound`, but the helper currently collapses every error to
`errBotSendPermCheckFailed`; the responder then exposes `query_failed`. The repo
already has `ErrBotAPIGroupNotFound` and a zh-CN translation, so no new public
error code is required.

## Load-bearing list

- **Shared Bot send-permission classifier** (`modules/bot_api/send.go`,
  `modules/bot_api/api_i18n.go`): `checkSendPermission` is shared by
  `sendMessage`, `typing`, and `readReceipt`. Missing-group classification must
  be consistent for all callers that use `respondSendPermissionError`.
- **Fail-closed group/thread authorization** (`touches: bot-api, space,
  isolation, acl`): group and parent-group lookup errors must never allow a send.
  A missing row is denied as not found; a real DB error is denied as internal.
- **Existing business denials remain distinct**: not-friend,
  conversation-not-started, not-group-member, group-disbanded,
  not-space-member, App-Bot-DM-only, and malformed-thread responses must not
  regress into `query_failed` or `group_not_found`.
- **DM anti-enumeration boundary**: a missing/deleted friend relationship does
  not reveal whether the target user exists. User Bot keeps `not_friend`; App
  Bot keeps `conversation_not_started`. Do not introduce `user_not_found` on
  the send-permission path. Only an actual friend-query error maps to internal
  `query_failed` and `stage=friend,reason=query_error` telemetry.
- **Localized error wire contract** (`touches: error-response, i18n,
  wire-contract`): reuse `ErrBotAPIGroupNotFound` through
  `httperr.ResponseErrorL`. Preserve D14 transport compatibility: outer HTTP 400,
  body `error.http_status=404`. True internal failures remain outer 400 with
  semantic 500. No raw Gin error response.
- **Shared `isGroupDisbanded` callers**: preserve the underlying error identity
  (including `dbr.ErrNotFound`) without changing direct callers in group update,
  member mutation, thread metadata, message edit, or OBO fan-out. Those paths
  remain fail-closed; only the shared send-permission responder gains the new
  not-found mapping in this task.
- **Request correlation**: retain the existing sanitized `X-Request-ID`
  response header. Permission-failure logs use the same `trace_id`. Logs must
  not contain raw or reversible user/Bot/channel/group/thread/Space identifiers,
  token, payload, card body, SQL argument, or other credential.
- **Persistent observability** (`touches: observability, metrics`): add
  `dmwork_bot_send_permission_failure_total{stage,reason}`. Both labels are
  closed enums; IDs, UIDs, channel IDs, group IDs, error strings, and trace IDs
  are forbidden labels. Required values:
  - `stage`: `friend`, `space_context`, `space_member`, `group_status`,
    `group_member`, `bot_kind`;
  - `reason`: `query_error`, `not_found`, `missing_context`, `unknown`.
- **Testing** (`touches: testing`): error identity, HTTP envelope semantics,
  fail-closed behavior, trace correlation, and metric cardinality require
  focused tests.
- **Commit style** (`touches: commit`): English Conventional Commit.

## Out of scope

- Incoming-webhook statistics SQL, display-card schema/validation, card profile,
  card dispatch, and WuKongIM send behavior.
- Splitting every internal permission failure into a new client-visible error
  code. Stage/reason differentiation is server-side telemetry; only the existing
  `group_not_found` code is reused.
- Switching `/v1/bot/sendMessage` from D14 wire HTTP 400 to real HTTP 4xx/5xx.
- Changing Bot authentication, friend semantics, Space membership, group
  membership, OBO authorization, rate limits, or route middleware.
- Reclassifying direct `isGroupDisbanded` callers outside the shared
  `checkSendPermission` path; their existing fail-closed response behavior is
  preserved.
- Database migrations, Redis state, new configuration flags, deployment
  manifests, or CLS/central-log provisioning.
- Returning raw internal causes to clients or adding high-cardinality metric
  labels.
- Logging raw identifiers or introducing identifier hashing/pseudonymization.
  Request correlation uses the existing opaque `trace_id`; this task does not
  create a second identifier system.

## Acceptance

- For a User Bot targeting `channel_type=2`, an absent `group` row returns:
  wire HTTP 400, `error.code=err.server.bot_api.group_not_found`, and
  `error.http_status=404`; it does not return `query_failed` and no message is
  dispatched.
- For `channel_type=5`, an absent parent group (the part before `____`) has the
  same `group_not_found` result and remains fail-closed.
- `isGroupDisbanded` preserves `dbr.ErrNotFound` through wrapping so
  `errors.Is` classification works. Any non-not-found DB error still returns
  `err.server.bot_api.query_failed` with semantic 500 and no dispatch.
- Existing negative results are unchanged: an existing group with no active Bot
  membership returns `not_group_member`; a disbanded group returns
  `group_disbanded`; DM/App-Bot friend and Space denials retain their current
  codes.
- User Bot DM with no active friend relationship returns `not_friend`; App Bot
  DM with no started relationship returns `conversation_not_started`. Neither
  path emits `user_not_found` or an internal-failure metric.
- A DM friend-query error remains `err.server.bot_api.query_failed`, emits
  exactly one `stage=friend,reason=query_error` metric sample, and logs only the
  approved desensitized fields.
- Card gates remain orthogonal: invalid display cards return `card_invalid`, and
  WuKongIM dispatch failures return `send_failed`.
- Every unexpected permission-check failure emits one structured error log with
  `trace_id`, `permission_stage`, `failure_reason`, `bot_kind`, and
  `channel_type`; include `zap.Error` only when an underlying error exists and
  its message contains no request identifiers or SQL arguments. Logs never
  include Authorization, Bot token, user/Bot UID, channel/group/thread/Space ID,
  request payload, card content, SQL text, or SQL arguments.
- The response retains `X-Request-ID`, and the value equals the structured log's
  `trace_id` for the same request.
- `dmwork_bot_send_permission_failure_total{stage,reason}` increments exactly
  once for each terminal permission-check failure covered by the enum. Tests
  prove the allowed label sets and reject accidental ID/error-string labels.
- No new `pkg/errcode` registration or locale block is added; reuse
  `ErrBotAPIGroupNotFound` and the existing zh-CN translation.
- Focused tests cover group missing, parent group missing, real DB failure,
  disbanded group, non-member, User/App Bot DM no-relationship behavior, DM
  friend-query failure, App Bot Space-context failure, unknown bot kind, trace
  correlation, and metric increments using an isolated Prometheus registry.
- Green: `go test ./modules/bot_api/...`; clean: `go build ./...`, `go vet ./...`,
  `golangci-lint run ./...`, `make i18n-extract-check`, and `make i18n-lint`.
