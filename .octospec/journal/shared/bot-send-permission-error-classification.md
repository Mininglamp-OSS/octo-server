---
type: Journal
title: "Journal: bot-send-permission-error-classification"
description: Missing Bot send targets are classified separately from query failures, with bounded and identifier-free telemetry.
tags: ["bot-api", "error-response", "observability", "privacy", "testing"]
timestamp: 2026-07-24T09:13:44+08:00
# --- octospec extension fields ---
task: bot-send-permission-error-classification
upstream: none
source: self
---

# Journal: bot-send-permission-error-classification

## What was done

- Preserved `dbr.ErrNotFound` from the group-status lookup and mapped a missing
  target group or missing thread parent group to the existing
  `err.server.bot_api.group_not_found` envelope. D14 transport compatibility is
  unchanged: wire HTTP 400 with semantic `error.http_status=404`.
- Kept real DB/query failures on the internal `query_failed` path and retained
  the existing DM business denials: User Bot `not_friend`, App Bot
  `conversation_not_started`, and Space/member denials are unchanged.
- Added `dmwork_bot_send_permission_failure_total{stage,reason}` with closed
  label enums and one structured terminal log carrying only `trace_id`, stage,
  reason, bounded Bot kind, and channel type. Raw error text is excluded at the
  observer API boundary so future driver or wrapper changes cannot leak request
  identifiers or SQL arguments.
- Removed raw Bot/channel/grantor identifiers from OBO friend-gate failure logs.
  OBO lookup and grantor-access failures now reach the same sanitized observer
  while preserving the historical fail-closed `not_friend` response.

## Load-bearing decisions

- `checkSendPermission` is shared by `sendMessage`, `typing`, and
  `readReceipt`; classification remains centralized in
  `respondSendPermissionError` so all three callers keep one wire contract.
- `isGroupDisbanded` now preserves error identity, but its direct callers
  outside the send-permission path keep their existing fail-closed response
  behavior. Only the shared permission classifier interprets
  `dbr.ErrNotFound` as `group_not_found`.
- Authorization and diagnostics are separate concerns on the OBO fallback:
  query failure still denies access as `not_friend`, while a bounded internal
  sentinel ensures the failure is observable without logging identifiers or
  exposing infrastructure details to the client.

## Testing and verification

- Handler-level tests cover missing group, missing thread parent, real
  group-status query failure, D14 wire/semantic statuses, request-ID/log
  correlation, one metric increment, and zero IM dispatch.
- Focused tests cover User/App Bot DM relationship outcomes, OBO grant lookup
  and grantor-access failures, Space context/member failures, disbanded and
  non-member groups, unknown Bot kind, and metric label bounding.
- Green: `go test ./modules/bot_api/... -count=1`, `go build ./...`,
  `go vet ./...`, `golangci-lint run ./...`, `make i18n-extract-check`, and
  `make i18n-lint`.
- Critical-function coverage: `hasOBOAccessToChannel` 95.2%,
  `isFriendOrOBOBypass` 100%, `checkSendPermission` 88.0%, and
  `observeSendPermissionFailure` 86.7%. The historical Bot API package total
  remains 45.1%.

## Gotchas

- `SELECT ... LoadOne` uses `dbr.ErrNotFound` for absence; collapsing that
  error too early makes a normal missing target look like an infrastructure
  outage.
- A fail-closed helper may intentionally keep a business denial on the wire,
  but it must not discard the diagnostic signal needed for request-correlated
  logs and persistent metrics.
