---
type: Task
title: "Task: incomingwebhook-webhooks-alias"
description: Add /v1/webhooks/{id}/{token} push route alias for /v1/incoming-webhooks/... with matching token scrubbing.
tags: ["webhook", "trust-boundary", "wire-contract"]
timestamp: 2026-06-24T00:00:00Z
# --- octospec extension fields ---
slug: incomingwebhook-webhooks-alias
upstream: Mininglamp-OSS/octo-server#455
source: self
---

# Task: incomingwebhook-webhooks-alias

## Goal
Make the incoming-webhook **push** endpoints reachable at a shorter alias path in
addition to the existing canonical path:

- `/v1/incoming-webhooks/{webhook_id}/{token}[/...]` — canonical, unchanged.
- `/v1/webhooks/{webhook_id}/{token}[/...]` — new alias (native + 5 adapters).

Same handlers, same middleware chain, same behavior — purely an additional
accepted path. Only the push ingress is aliased; management endpoints unchanged.

## Background
Follow-up to #454. `/v1/webhooks` (plural) is currently unused; only singular
`/v1/webhook*` exists in `modules/webhook/api.go`. Conflict analysis (issue #455 +
my diagnosis comment) confirms no gin route-registration conflict: `/v1/` has only
static children, so a new static `webhooks` segment cannot collide with a sibling
wildcard, and `:webhook_id` appears only after `webhooks/`.

## Load-bearing list
- **webhook / trust-boundary**: the push ingress is the unauthenticated,
  token-in-URL attack surface. The new alias must inherit the **identical**
  middleware chain (`requirePushEnabled → localFloorMiddleware → ipLimit →
  ipFailureGateMiddleware`) — no auth/rate-limit divergence between the two paths.
- **trust-boundary (token-in-path scrubbing, #246)**: `pkg/accesslog` masks tokens
  ONLY for `/v1/incoming-webhooks/`. The alias would otherwise leak plaintext
  tokens into access logs + `gin.Recovery` panic dumps. Both `ScrubPath`
  (`incomingWebhookPrefix`) and the `tokenInText` regex MUST cover both prefixes.
  This is the adapter-parity invariant: a defense for one path must hold for its
  sibling.
- **wire-contract**: push request/response shape and status codes must be
  byte-identical across both prefixes (additive, backward-compatible).

## Out of scope
- Changing the canonical push URL or `publicURL`/`publicURLs` (still advertise
  `/v1/incoming-webhooks/...`).
- Any management endpoint (`/v1/groups/.../incoming-webhooks`).
- The separate `modules/webhook` (`/v1/webhook*`) module.

## Acceptance
- `POST /v1/webhooks/{id}/{token}` and `.../{adapter}` (github/wecom/multica/
  gitlab/feishu) behave identically to their `/v1/incoming-webhooks/...`
  equivalents (same auth / rate-limit / delivery).
- `pkg/accesslog`: `ScrubPath`, the panic-dump `scrubbingErrorWriter`, and
  `Formatter` mask the token for BOTH prefixes; management/singular/unrelated
  paths still pass through unchanged. New table cases prove it.
- No gin route-registration panic (covered by `NewTestServer` route build).
- `go test ./modules/incomingwebhook/... ./modules/webhook/... ./pkg/accesslog/...`
  passes; `golangci-lint run` clean on touched files.

## Implementation notes
- `modules/incomingwebhook/api.go` `Route`: factor the 6 push registrations into a
  small helper `mountPush(prefix)` (or a loop over the adapter-suffix list) and
  call it for both `/incoming-webhooks` and `/webhooks` on the same `push` group
  with the same `chain(...)`.
- `pkg/accesslog/accesslog.go`: generalize prefix matching to
  `/v1/(incoming-)?webhooks/`. Verified no false positives for
  `/v1/groups/.../incoming-webhooks`, `/v1/webhook`, `/v1/webhook/github`.
