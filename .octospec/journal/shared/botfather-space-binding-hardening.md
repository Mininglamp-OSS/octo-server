---
type: Journal
title: "Journal: botfather-space-binding-hardening"
description: Record of making BotFather conversational Bot creation validate and commit Space binding from server-authoritative membership state.
tags: ["botfather", "space", "isolation", "auth", "data-integrity", "testing"]
timestamp: 2026-08-11T16:56:17Z
# --- octospec extension fields ---
task: botfather-space-binding-hardening
upstream: none
source: user
---

# Journal: botfather-space-binding-hardening

## What was done

- Treat the WuKongIM payload or legacy channel Space ID only as a selector.
  Before creating durable Bot artifacts, require an active creator, active
  Space, and active creator membership from the database.
- When no selector is present, derive a target only when exactly one active
  creator Space exists. Zero or multiple candidates fail closed.
- Recheck the creator, Space, and membership under row locks and insert the Bot
  membership in the same transaction. The insert must affect exactly one row;
  no best-effort `INSERT IGNORE` remains in this path.
- On core-creation or binding failure, compensate newly created App, robot,
  user, friendship, and membership artifacts. If hard deletion cannot complete,
  disable the Bot and blank credential-bearing fields as a fail-closed fallback.
- Keep the existing generic localized creation-failure reply. New security logs
  contain only bounded reason values and no identifiers, payloads, or tokens.

## Review hardening

- Scoped friendship compensation to the two creator/Bot directions instead of
  scanning every row that mentions the newly-created Bot. The predicate uses
  the existing paired friendship index; a local `EXPLAIN` confirmed indexed
  range access rather than a full-table scan.
- Made the final binding transaction use the normal deferred rollback guard,
  documented its `user -> space -> space_member` lock order, and keep
  membership timestamps database-authoritative with `NOW()`.
- Added focused SQL-mock coverage for a zero-row membership insert and for a
  failed transaction begin. Both outcomes return failure and do not reach a
  success commit.

## Verification

- `go test ./modules/botfather/... -count=1 -coverprofile=...`: PASS with a
  synthetic test-only master key and branch-isolated MySQL/Redis. The BotFather
  package reports 40.4% statement
  coverage (legacy package-wide baseline); the changed authorization/binding
  functions report 88.6% (`createBot`), 100.0%
  (`resolveAuthorizedCreationSpace`), 84.8% (`bindCreatedBotToSpace`), and 96.2%
  (`deleteCreatedBotArtifacts`).
- The regression suite includes real MySQL persistence tests for forged,
  missing, inactive, removed, ambiguous, authorized, concurrent-change, query
  failure, insert failure, compensation, and core-creation failure paths.
- The end-to-end test drives the actual BotFather message state machine for a
  rejected forged selector and an authorized creation, then verifies `/mybots`
  Space isolation from persisted rows.
- Focused real-message-flow E2E re-ran in the branch-isolated MySQL/Redis and
  WuKongIM environment: a forged selector was rejected; an authorized
  selector created exactly one persisted membership and remained isolated in
  `/mybots`. The relevant `go test -race` target, `go build`, `go vet
  ./modules/botfather/...`, `golangci-lint run ./modules/botfather/...`,
  `make i18n-extract-check`, and `make i18n-lint`: PASS.
- Local MySQL, Redis, and WuKongIM checks passed. The branch-isolated schema
  had no residual synthetic rows or triggers after validation; that schema and
  its dedicated Redis container were then removed without touching shared test
  infrastructure.
- Diff scan found no incident identifiers, production domains, message bodies,
  or credentials. Standalone `octospec-lint` is not installed; the task YAML
  parses and the brief/context/diff were checked manually against injected
  rules.

## Structural learning and rollout

- A tenant selector is not authorization evidence. The decisive check must be
  repeated at the write linearization point, not only before a multi-step
  provisioning flow.
- A success reply is not persistence evidence; tests assert the committed
  membership row and its isolation from another Space.
- Any mixed-version deployment remains vulnerable while an old replica can
  process `/newbot`. Drain old replicas or temporarily disable conversational
  creation during rollout. Rollback must likewise block this flow until a
  corrected build is active.
