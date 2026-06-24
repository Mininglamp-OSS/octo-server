---
type: Journal
title: "Journal: incomingwebhook-webhooks-alias (octo-server #455)"
description: Record of the /v1/webhooks push route alias + token-scrub generalization and the rules honored.
tags: ["webhook", "trust-boundary", "routing", "accesslog"]
timestamp: 2026-06-24T13:00:00Z
# --- octospec extension fields ---
task: incomingwebhook-webhooks-alias
upstream: Mininglamp-OSS/octo-server#455
source: self
---
# Journal: incomingwebhook-webhooks-alias (octo-server #455)

## What was done
Added a shorter push-route alias so the incoming-webhook push endpoints are
reachable at both `/v1/incoming-webhooks/{id}/{token}[/adapter]` (canonical,
unchanged) and `/v1/webhooks/{id}/{token}[/adapter]` (new alias). Purely
additive — same handlers, same middleware chain, same behavior.

1. **Routes** — `modules/incomingwebhook/api.go` `Route`: factored the 6 push
   registrations (native + github/wecom/multica/gitlab/feishu) into a
   `mountPush(prefix)` closure and called it for both `/incoming-webhooks` and
   `/webhooks` on the same `push` group via the identical `chain(...)`
   (`requirePushEnabled → localFloorMiddleware → ipLimit →
   ipFailureGateMiddleware`). No auth/rate-limit/quota divergence between paths.

2. **Token-in-path scrubbing (security, #246)** — `pkg/accesslog/accesslog.go`:
   replaced the single `incomingWebhookPrefix` const with a
   `webhookPushPrefixes` slice (`/v1/incoming-webhooks/`, `/v1/webhooks/`);
   `ScrubPath` now loops over both. Generalized the panic-dump `tokenInText`
   regex to `(/v1/(?:incoming-)?webhooks/[^/\s?"']+/)[^\s?"']+`. Both stay
   case-insensitive. Without this the alias would leak plaintext tokens into
   access logs and the `gin.Recovery` panic dump.

3. **Canonical URL unchanged** — `publicURL`/`publicURLs` still advertise
   `/v1/incoming-webhooks/...` (alias is backward-compatible only).

4. **Tests** — `modules/incomingwebhook/alias_push_test.go` (alias native push
   passes auth+validation; wrong token → 401 proving the same chain; github ping
   → 200 skipped like canonical). `pkg/accesslog/accesslog_test.go` added alias
   table cases + panic-dump scrub + two false-positive guards (`/v1/webhook` and
   `/v1/groups/.../incoming-webhooks` must NOT be scrubbed).

## Rules honored
- **trust-boundary** (load-bearing): adapter/path parity — the token-scrub
  defense added for the canonical path now holds for the sibling alias; the alias
  reuses the exact same middleware chain rather than a divergent one.
- **testing**: e2e via `testutil.NewTestServer`; accesslog table cases for both
  prefixes incl. negative guards.
- **commit-style**: Conventional Commits, English.

## Verification
`go test ./modules/incomingwebhook/... ./modules/webhook/... ./pkg/accesslog/...`
all green (full integration stack: MySQL 8.0 + Redis + WuKongIM
v2.2.4-20260313). `go vet`, `golangci-lint`, `make i18n-extract-check`, and
`make i18n-lint` all clean. No gin route-registration panic (the
`NewTestServer` route build exercises it).

## Learning
The `/v1/` segment has only static children, so a new static `webhooks` segment
cannot collide with a sibling wildcard in gin's httprouter radix tree — the
divergence from the existing singular `/v1/webhook` is just the char `s` (a node
can be both a handler and have children). This made the alias safe to add with no
route restructuring. Worth remembering when adding sibling prefixes: the risk is
wildcard-vs-static at the same tree level, which does not exist here.
