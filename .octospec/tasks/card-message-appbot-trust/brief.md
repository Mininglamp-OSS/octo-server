---
type: Task
title: "Task: card-message-appbot-trust"
description: Make published App Bots first-class trusted card senders across display masking and the card-action event loop.
tags: ["message", "card", "bot-api", "app-bot", "wire-contract", "trust-boundary", "auth", "space", "isolation", "acl", "error-response", "i18n", "rate-limit", "testing"]
timestamp: 2026-07-13T00:00:00+08:00
# --- octospec extension fields ---
slug: card-message-appbot-trust
upstream: self
source: self
---

# Task: card-message-appbot-trust

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Close the existing App Bot card-sender trust gap without changing the card send
pipeline, target authorization, Space enrichment, or wire contract.

A published App Bot can already authenticate to `/v1/bot/*`, send type-17 card
payloads, poll `/v1/bot/events`, ACK events, and edit its own cards. App Bot
creation persists an `app_bot` row and a `user.robot=1` presentation row, but
does not create a `robot` table row. Two server-side card trust gates currently
call `robot.ExistRobot` and therefore reject/mask App Bot cards after send:

- `modules/cardtrust.Resolver`, used by offline push and message-search card
  projections;
- `POST /v1/message/card/action`, before enqueueing `card_action` to the sender's
  bot event queue.

Introduce one authoritative bot-identity resolver that recognizes both active
User Bots and published App Bots, then use it at those two trust gates. A valid
App Bot card must retain trusted `plain` on server-authored display surfaces and
deliver a valid Submit action through the existing App Bot event poll/ACK loop.

User journey:

- As a published App Bot owner, I can send an interactive DM card; the recipient
  sees the trusted card fallback text, taps a valid action, and my App Bot polls
  exactly one `card_action` event and ACKs it through existing Bot API routes.

## Background

### Existing identity models

- User Bot authority: `robot.robot_id` with `robot.status=1`.
- App Bot authority: `app_bot.uid` with `app_bot.status=1` (`StatusPublished`).
- App Bot presentation: `user.uid=app_bot.uid`, `user.robot=1`.
- Incoming webhook identity: synthetic `iwh_` prefix, intentionally trusted for
  display but intentionally rejected by `card/action` because it has no bot event
  consumer.

Bot API authentication already distinguishes User Bot and App Bot tokens. The
App Bot event routes are UID-keyed Redis operations; typed events do not require
a `robot` DB row. The missing component is a shared authoritative predicate for
“is this UID currently an active bot identity that may own a card action?”.

### Decisions locked by this task

1. **Complete App Bot card support.** Do not disable App Bot cards or hide the
   card profile. Active User Bots and published App Bots are trusted bot card
   senders.
2. **One resolver, no HTTP-module dependency.** Add a small library package
   (proposed name `modules/botidentity`) that reads the existing `robot` and
   `app_bot` tables without importing `modules/robot`, `modules/app_bot`, or
   `modules/bot_api`. This avoids import cycles and keeps lifecycle authority in
   the existing tables.
3. **Fail closed.** Empty, unknown, disabled/unpublished, deleted, ambiguous, or
   lookup-error identities are not trusted. A UID simultaneously active in both
   bot tables is an invariant violation and returns an explicit internal error;
   no precedence rule silently chooses one kind.
4. **Live action authorization.** `cardAction` resolves sender identity on every
   first action attempt. It does not use the presentation cache, preserving
   immediate App Bot unpublish/revocation for side effects.
5. **Presentation cache stays bounded.** `modules/cardtrust` retains its current
   LRU capacity, 60-second TTL, webhook-prefix shortcut, and “do not cache lookup
   errors” behavior. The cached value now comes from the unified resolver.
6. **Event transport is unchanged.** A valid App Bot action is enqueued by the
   existing `robotService.EnqueueBotTypedEvent(msgM.FromUID, ...)`, then polled
   and ACKed through existing `/v1/bot/events` routes. No new queue or event
   schema is introduced.
7. **No behavior change for other senders.** Active User Bot and `iwh_` display
   trust remains unchanged; webhook actions, human-forged cards, and inactive
   bot actions remain rejected.

## Load-bearing list

<!-- touches: trust-boundary, wire-contract, auth, bot-api, space, isolation,
     acl, error-response, i18n, rate-limit, testing -->

- **Bot identity trust (`trust-boundary`, `auth`)** — `robot.status=1` and
  `app_bot.status=1` are the only bot-table authority states. `user.robot=1`
  alone is presentation metadata and must not authorize an action.
- **Display masking (`trust-boundary`, `wire-contract`)** — type-17 stored
  `plain` may surface only for an active User Bot, published App Bot, or existing
  `iwh_` sender. Human/unknown/inactive senders and lookup errors remain masked
  to `[卡片]`.
- **Card action side effects (`acl`, `space`, `isolation`)** — only the sender
  identity predicate changes. Existing anti-IDOR channel binding, operator
  membership, group/thread status, canonical visibility, revoke/delete/expiry,
  action-id, input-shape, rate-limit, and idempotency gates must remain byte-for-
  byte behaviorally unchanged.
- **Event ownership (`bot-api`, `wire-contract`)** — a successful action is
  enqueued to the stored message `from_uid`; it must be retrievable only by that
  authenticated App Bot and retain the existing additive `card_action`
  `event_data` shape.
- **Revocation semantics** — App Bot unpublish/delete must block new action side
  effects immediately through the live resolver. Presentation masking may lag
  by the existing cache TTL, matching current User Bot cache semantics.
- **Failure handling (`error-response`, `i18n`)** — resolver/storage failures in
  `cardAction` use the existing localized query-failed envelope; an inactive or
  untrusted sender uses the existing collapsed card-action-invalid response.
  No new raw HTTP error path or new client-visible identity enumeration is added.
- **Rate limiting (`rate-limit`)** — no route or middleware changes. The existing
  authenticated `card/action` shared UID limiter and Bot API/global limits stay
  in force; no new Redis request-frequency counter is introduced.
- **Testing (`testing`)** — resolver behavior, cardtrust cache semantics, live
  action authorization, App Bot event poll/ACK, and unchanged negative sender
  cases require focused tests. DB/Redis-backed tests clean state and reset any
  shared rate-limit buckets they touch.

## Out of scope

- `internal/carddispatch`, internal producer registry/configuration, direct-send
  source guards, or onboarding an internal business producer. These become a
  separate task when the first concrete internal caller and target capability
  are known.
- Refactoring `/v1/bot/sendMessage`, its validation order, target authorization,
  Space resolution, mention handling, metrics, or IM dispatch.
- Changes to non-card sends, OBO, fan-out, typing, read receipts, message sync,
  bot card edit/CAS, card revision history, or card-profile response fields.
- Incoming webhook profiles/actions/callbacks or changes to its display trust.
- Legacy robot API changes.
- New card elements, actions, profiles, limits, or client-renderer behavior.
- New database migrations or copying App Bot rows into the `robot` table.
- Changing the existing 60-second presentation-cache TTL or adding distributed
  invalidation for it.
- Send idempotency, `client_msg_no`, outbox, retries, or exactly-once semantics.
- New metrics. Existing structured error logs remain the observability surface
  for identity lookup failures in this narrowly scoped fix.

## Acceptance

### Unified identity resolver

- The resolver returns a typed result (`user_bot` or `app_bot`) and the minimum
  authoritative metadata required by consumers; a boolean convenience method
  may wrap it but cannot swallow lookup/invariant errors.
- Table-driven unit tests and a DB-backed integration test prove:
  - `robot.status=1` → trusted User Bot;
  - disabled/deleted/missing `robot` → not trusted;
  - `app_bot.status=1` → trusted App Bot;
  - draft/unpublished/deleted/missing `app_bot` → not trusted;
  - a `user.robot=1` row without an active bot-table row → not trusted;
  - UID active in both tables → explicit ambiguous-identity error;
  - DB failure → error and no trusted result.
- Resolver queries use the current DB state. No package-level mutable identity
  cache is added.

### Display trust

- `modules/cardtrust.New` uses the unified resolver instead of
  `robot.NewService`/`robot.ExistRobot`.
- Tests prove active User Bot, published App Bot, and `iwh_` sender are trusted;
  inactive App Bot, presentation-only `user.robot=1`, human, empty UID, nil
  resolver, and lookup error are untrusted.
- Existing cache behavior remains covered: successful positive/negative verdicts
  cache for the configured TTL; lookup errors are not cached and are retried.
- Offline-push and message-search card projection tests include a published App
  Bot case and expose authoritative stored `plain` rather than `[卡片]`.

### Card action and App Bot event lifecycle

- `cardAction` uses the live unified resolver for the stored sender UID and keeps
  `robotService` only for event enqueue.
- Existing User Bot happy path and webhook/human/inactive-bot negative tests stay
  green.
- A DB/Redis-backed App Bot flow proves:
  1. a published App Bot sender plus an otherwise-valid card action returns
     `{accepted:true,replay:false}`;
  2. exactly one typed `card_action` event is written for the App Bot UID;
  3. the same App Bot token can poll `/v1/bot/events` and receives that event
     with the existing `event_type`/`event_data` contract;
  4. `/v1/bot/events/:event_id/ack` removes it;
  5. a later poll does not return the ACKed event.
- Unpublishing the App Bot before a first action attempt yields the existing
  localized invalid response and enqueues no event. Re-publishing permits a new
  valid attempt; action idempotency behavior remains unchanged.
- Existing action visibility tests (cross-channel IDOR, membership,
  group/thread status, expires, visibles, revoke/delete, inputs) remain green,
  demonstrating that only sender identity changed.

### Verification gates

- TDD RED is captured before production changes and the same focused targets
  pass GREEN afterward; Conventional Commit checkpoints remain reachable from
  this branch.
- `gofmt` and `git diff --check` pass.
- New resolver package coverage is at least 80%:
  - `go test -cover ./modules/botidentity/...`
- Focused suites pass:
  - `go test ./modules/cardtrust/...`
  - published-App-Bot push/search projection tests;
  - relevant `modules/message` card-action integration tests;
  - relevant `modules/bot_api` event poll/ACK tests.
- Broader gates pass where local infrastructure permits:
  - `go test ./pkg/cardmsg/... ./modules/cardtrust/... ./modules/bot_api/...`
  - `go test ./...`
  - `make i18n-extract-check` and `make i18n-lint`
  - `golangci-lint run ./...`
- octospec Implement records injected rules in `context.yaml`; Check confirms
  every load-bearing rule against the actual diff and verifies no out-of-scope
  send/Space/permission behavior changed.
- Finish writes the shared journal/log entry and opens a PR against `main` with
  Linked Spec and substantive COMPREHENSION answers.
