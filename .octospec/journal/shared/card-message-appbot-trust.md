---
type: Journal
title: "Journal: card-message-appbot-trust"
description: Close the App Bot card trust gap with one authoritative identity resolver and a verified action event lifecycle.
tags: ["message", "card", "app-bot", "bot-api", "trust-boundary", "auth", "testing"]
timestamp: 2026-07-13T16:01:27+08:00
# --- octospec extension fields ---
task: card-message-appbot-trust
source: self
---

# Journal: card-message-appbot-trust

## What was done

- Added `modules/botidentity`, a live, cache-free resolver over the two lifecycle
  authorities: `robot.robot_id/status=1` and `app_bot.uid/status=1`. One SQL
  statement reads both predicates from the same snapshot. Missing/inactive rows
  resolve to no identity; simultaneous active rows return a typed invariant error.
  `user.robot=1` is never consulted as authorization.
- Rewired `modules/cardtrust` to the unified resolver while preserving its
  bounded LRU, 60-second TTL, `iwh_` display shortcut, positive/negative caching,
  and no-cache-on-error fail-closed behavior.
- Rewired `POST /v1/message/card/action` to use the live resolver for the stored
  sender UID. Existing `robotService` remains the typed-event enqueue path, so
  the queue and `card_action` wire shape are unchanged.
- Added regression coverage for User Bot/App Bot/inactive/presentation-only/
  ambiguous/error identities, push and search display projection, and a complete
  App Bot `action -> poll -> ACK -> absent-on-repoll` lifecycle. Unpublish blocks
  a first action immediately; republish permits it without changing idempotency.

## Structural learnings

- Bot presentation and bot authority are different layers. `user.robot=1` is a
  UI hint; lifecycle authorization must resolve the owning authoritative table.
  When two independent tables can claim one UID, a precedence rule hides data
  corruption. A single-statement resolver can detect the invariant and fail closed.
- Cache placement belongs to the consumer. Display masking tolerates the existing
  short TTL, while side-effect authorization must read live state. A package-level
  identity cache would have silently weakened App Bot unpublish semantics.

## octospec rules injected

- **space-isolation**: only the stored sender predicate changed; channel binding,
  membership, Space/group/thread status, visibility, revoke/delete, and
  idempotency gates are unchanged and the full card-action matrix stays green.
- **trust-boundary**: only active User Bots, published App Bots, and existing
  `iwh_` display identities expose stored `plain`; errors and ambiguous identities
  fail closed. Webhook actions remain rejected.
- **error-handling**: resolver errors use the existing localized query-failed
  envelope; inactive/unknown identities use the existing collapsed invalid action.
- **rate-limit**: no route, middleware, or Redis frequency-counter change.

## Verification

- TDD checkpoints: `3599b793` (RED reproducers), `2b5f9519` (GREEN fix).
- `go test -race ./modules/botidentity/... ./modules/cardtrust/...` — PASS.
- `go test -cover ./modules/botidentity/...` — PASS, 96.2% statements.
- `go test -tags integration ./modules/botidentity -run '^TestResolverAgainstAuthoritativeBotTables$' -count=1` — PASS.
- `go test -tags integration ./modules/message -run '^TestCardAction' -count=1` — PASS after CI-style MySQL/Redis reset.
- Published App Bot push/search projection tests — PASS after per-package reset.
- `go test ./modules/bot_api/...` and `go test ./pkg/cardmsg/... ./modules/cardtrust/...` — PASS.
- `go vet`, `make i18n-extract-check`, `make i18n-lint`, and `git diff --check` — PASS.

## Gotchas worth remembering

- The repo's shared local `test` schema must be dropped/recreated between module
  packages, matching `.github/workflows/ci.yml`; otherwise one package's partial
  migration set causes another package to fail with `unknown migration in
  database`. Redis must also be flushed for rate-limit/action/event isolation.
- `golangci-lint` was not installed in this workspace. The focused vet, repo i18n
  linters, race tests, integration suites, and diff check ran successfully; CI
  remains the authoritative full golangci gate.

## Deliberately deferred

`internal/carddispatch`, internal producers, incoming-webhook action/callback
support, and bot-send pipeline changes remain out of scope until a concrete
internal caller is specified.
