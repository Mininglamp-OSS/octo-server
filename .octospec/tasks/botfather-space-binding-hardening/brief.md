---
type: Task
title: "Task: botfather-space-binding-hardening"
description: Make the server authoritative for BotFather Bot-to-Space binding and reject untrusted cross-Space selectors before Bot creation.
tags: [space, isolation, auth, acl, bot-api, trust-boundary, wire-contract, i18n, testing, commit, security, data-integrity]
timestamp: 2026-08-12T00:23:48+08:00
# --- octospec extension fields ---
slug: botfather-space-binding-hardening
upstream: none (P0 production incident hardening; incident identifiers are intentionally omitted)
source: user
---

# Task: botfather-space-binding-hardening

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. Production identifiers, credentials, domains, and message bodies are
> deliberately excluded; tests and examples must use synthetic values only.

## Goal

Make octo-server the sole policy authority for the Space assigned to a Bot
created through the conversational BotFather `/newbot` flow.

The `space_id` received from a WuKongIM message payload, or derived from a
legacy channel prefix, is an untrusted selector. Before creating any durable
Bot artifact, the server must prove that:

1. the creator account is active;
2. the selected Space is active; and
3. the creator is an active member of that Space.

An explicit selector that fails any check, or a database error while checking,
must fail closed. A missing selector may use a server-derived fallback only
when the creator belongs to exactly one active Space; zero or multiple active
Spaces are ambiguous and must fail closed. The implementation must not choose
an arbitrary "first Space".

The authorization decision and Bot membership write must remain valid under a
concurrent membership or Space-status change. A rejected or failed binding must
not return a success message, expose whether a foreign Space exists, or leave a
usable active Bot without an active Space binding.

## Background

- `modules/botfather/api.go::messagesListen` reads `space_id` directly from the
  inbound IM payload and installs it as message-processing context.
- `modules/botfather/command.go::createBot` currently creates the App, `robot`,
  and `user` first, then uses `resolveSpaceID(fromUID)` directly in an
  `INSERT IGNORE INTO space_member` statement.
- The command path does not verify creator membership or Space/account status.
  A client-controlled selector can therefore create a cross-Space membership
  row. The insertion is also best-effort: an empty selector or insert failure
  can still produce an active orphan Bot and a success reply.
- `/mybots` and Space Bot listings correctly filter by `space_member`; after a
  wrong binding, the Bot disappears from the creator's intended Space and is
  visible to members of the incorrectly selected Space.
- Other provisioning code already demonstrates stronger server-side
  membership checks, but this task must not copy their best-effort partial
  creation behavior. The conversational path must fail closed before reporting
  success.
- The repo `space-isolation` rule classifies a missing or fail-open cross-Space
  boundary as P0.

## Security and consistency contract

- Payload `space_id` and channel-prefix Space IDs are never authorization
  evidence; they only select the Space against which the server checks current
  database state.
- The canonical authorization query includes active `user`, active `space`,
  and active `space_member` rows.
- Authorization query errors are denials, not legacy fallbacks.
- The final authorization check and `space_member` write must be atomic or use
  equivalent locking/conditional-write semantics so a stale preflight check
  cannot authorize the commit.
- `INSERT IGNORE` must not hide a zero-row or conflicting membership outcome.
  Success requires a verified active Bot membership in the selected Space.
- Any post-validation persistence failure must return an error and compensate
  newly created App/robot/user artifacts sufficiently that no active usable Bot
  or exposed Bot token remains.
- User-visible rejection continues through the existing localized generic
  BotFather creation-failure message. It must not echo a submitted Space ID or
  distinguish nonexistent, inactive, and unauthorized foreign Spaces.
- New security logs use low-cardinality reason values and must not contain Bot
  tokens, raw message payloads, production UIDs, production Space IDs, or
  production domains.

## Load-bearing list

- **space**, **isolation**, **auth**, **acl**, **bot-api** — this change repairs
  a cross-tenant write boundary. Membership and parent Space/account status are
  required at the moment the Bot binding is committed. (rule:
  `space-isolation`)
- **trust-boundary**, **wire-contract** — WuKongIM message payload fields are
  attacker-controlled input. Existing clients may keep sending `space_id`, but
  it becomes a selector rather than policy authority. No payload shape changes.
  (rule: `trust-boundary`)
- **i18n**, **error-response** — the IM command flow keeps the existing
  localized generic failure response; no raw database or authorization error is
  sent to the user. (rule: `error-handling`)
- **data-integrity** — App, `robot`, `user`, and `space_member` persistence spans
  more than one existing service operation. Failure and compensation semantics
  must prevent a new active orphan or cross-Space Bot.
- **multi-instance**, **concurrency** — authorization correctness must not
  depend on process-local state or a race-prone check-then-write window.
- **testing** — the P0 boundary requires behavior-level negative tests for
  forged, missing, inactive, removed, ambiguous, and query-failure cases, plus
  the authorized happy path. Test fixtures are synthetic and isolated. (rule:
  `testing`)
- **secret-handling**, **redaction** — Bot tokens and incident identifiers must
  not enter tests, fixtures, logs, the brief, journal, commits, or PR text.
- **commit** — implementation follows English Conventional Commits and records
  RED/GREEN evidence without sensitive data. (rule: `commit-style`)

## Out of scope

- Fixing octo-web `currentSpaceId` caching, account namespacing, `SpaceGate`, or
  other client-side Space selection behavior.
- Repairing existing production Bot/Space rows, bulk classifying orphan or
  multi-Space Bots, migrating Bot ownership, or changing WuKongIM whitelists.
- Changing `POST /v1/user/bots`, OBO minting, App Bot provisioning, Bot deletion,
  or other Bot creation/removal paths except where a narrowly shared helper is
  required for this command-path fix.
- Fixing duplicate welcome-message delivery.
- Replacing the existing per-UID `sync.Map` message context. Same-UID concurrent
  context isolation is a separate correctness task; this task's server-side
  membership authorization must remain safe even if the selector is wrong.
- Adding database migrations, feature flags, new HTTP endpoints, or a Bot
  migration API.
- Returning detailed Space authorization failures to clients.

## Acceptance

- An explicit synthetic Space selector succeeds only when the synthetic creator
  account, Space, and membership are all active.
- A selector for a Space where the creator has no active membership is rejected
  before any App, robot, user, friend, or Bot `space_member` artifact is created.
- Nonexistent Space, inactive Space, inactive creator, removed membership, and
  membership-query failure all fail closed with the same external behavior.
- A missing selector succeeds only for exactly one active creator Space. It is
  rejected for zero active Spaces and for two or more active Spaces; no query
  ordering is used to guess intent.
- A concurrent creator-membership removal or Space disable cannot produce a
  successful Bot binding based solely on a stale preflight result.
- A Bot membership insert/commit failure does not produce the creation-success
  reply and leaves no newly created active usable Bot or exposed Bot token.
- The authorized happy path creates exactly one active Bot membership in the
  authorized Space and preserves the existing BotFather success-message shape.
- `/mybots` continues to return the new Bot in its authorized Space and not in a
  different synthetic Space.
- Tests exercise the actual `createBot` persistence boundary, not only a copied
  predicate. Critical authorization-decision branches reach 100% coverage; new
  code meets at least 80% line and branch coverage.
- Focused tests pass, including the new regression target and existing
  BotFather tests. Run the relevant race target where the environment permits.
- `gofmt`, `go vet` for the touched package, `make i18n-extract-check`,
  `make i18n-lint`, and octospec lint/check pass, or an infrastructure-only
  blocker is recorded with its exact command and sanitized error.
- A repository scan of the new diff finds no production UID, Space ID, Bot ID,
  Bot token, domain, or unredacted message body from the incident evidence.

## Rollout and rollback

- Ship the server fix before relying on any client correction. Client fixes are
  defense in depth and cannot authorize a weaker server path.
- A rolling deployment remains vulnerable while any old replica can process
  BotFather messages. Prefer blue/green or drain all old replicas before
  declaring the P0 closed; otherwise temporarily disable conversational Bot
  creation during the mixed-version window.
- Observe only low-cardinality accepted/rejected/failure outcomes. Do not add
  UID, Space ID, Bot ID, payload, or token labels.
- Rolling back to the current behavior reopens the cross-Space write. Prefer a
  forward fix; if emergency rollback is unavoidable, disable or block the
  conversational `/newbot` processing path until a corrected build is active.
- No schema rollback is required.
