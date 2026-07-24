---
type: Journal
title: "Journal: bot-card-template-consumption (roadmap E1b)"
description: Add an explicit Bot template catalog and Registry-backed template_ref send/edit modes while preserving the raw-card protocol.
tags: ["cardtmpl", "bot-api", "template-ref", "wire-contract", "trust-boundary", "space-isolation", "testing", "e1b"]
timestamp: 2026-07-24T15:10:16+08:00
# --- octospec extension fields ---
task: bot-card-template-consumption
upstream: "PR #657; roadmap E1b"
pr: 659
source: self
---

# Journal: bot-card-template-consumption (roadmap E1b)

## What was done

- Extended the additive-only `GET /v1/bot/card/profile` response with a
  `templating` subtree (`supported`, `wire=template-ref/v1`, explicit templates,
  views, states, wire profiles, and Submit action IDs). The list is a
  code-reviewed Bot allowlist, not `Registry.List()` and not an implicit grant
  to internal docs/summary templates.
- Added Registry mode to `POST /v1/bot/sendMessage`: callers provide an explicit
  `template_ref`, outer `state`, and schema data; the server owns state-to-view
  selection, profiles, rendered card metadata, authoritative Space, and plain
  text. Existing mention/reply behavior survives the compilation boundary.
- Added the symmetric Registry mode to `POST /v1/bot/message/edit`. It renders a
  complete replacement frame and reuses `CardMutator` for the second ownership
  and lifecycle lookup, positive `card_seq` CAS, idempotent replay, revision
  policy, and CMD synchronization. Transient frames update/sync without entering
  revision history.
- Kept raw Model B behavior. Raw and Registry modes are total XORs; unlisted,
  partial, cross-template/version, stale, forged, or malformed requests fail
  closed with zero dispatch/mutation side effects.
- Added server-authored top-level `template_ref` provenance. Raw send/edit cannot
  forge it, and Registry edit requires both provenance and
  `metadata.octo.template` to match the requested immutable id/version.
- Moved JSON-template interaction-report consistency to registration: each v2
  sample's rendered Submit/input surface must match its report before the
  Registry can freeze and before the catalog can advertise it.

## Load-bearing decisions

- Capability discovery and usable send/edit wires land atomically; the server
  never advertises an unusable template wire.
- Version is mandatory on the wire. Registry defaults remain internal deployment
  choices and cannot silently retarget an Agent request.
- The outer `state` is authoritative for view selection. When `data.state`
  exists it must exactly mirror the outer value, preventing two state authorities.
- Registry edit may change state/view but cannot migrate template id/version.
  Any future migration requires a separate explicit protocol.
- Bot action clicks keep the existing `/v1/bot/events` `card_action` pull path;
  this task does not add a callback RouteSpec or stop/retry orchestration.

## Verification

- Green focused race suites cover the E1b Bot API handlers/catalog,
  `pkg/cardtmpl/...`, and `CardMutator`.
- Green shared checks: `go test ./pkg/cardmsg/... ./internal/carddispatch/...`,
  `go build ./...`, `go vet ./pkg/cardtmpl/... ./modules/bot_api/...`,
  `make i18n-extract-check`, `make i18n-lint`, and `git diff --check`.
- The local full-package Bot API race attempt is not green because the shared
  local test database contains an unknown legacy migration
  (`20191106000001_event_legacy01.sql`). The focused E1b race suite is green;
  fresh-service CI evidence is required before merge.

## Structural learnings / gotchas

- A dual-mode JSON request must define mode by **key presence**, not by decoded
  non-empty value. `content_edit:""` and `template_ref:null` still mean both
  fields were supplied. Go zero values collapse those states unless custom
  unmarshalling records raw field presence; the first implementation reached
  the Registry snapshot path for a both-present request before the regression
  test caught it.
- The published `ai.reasoning-process@0.1.0` remains frozen and has no field-level
  caps for its free-form strings or nested arrays. Platform body/node/final-size
  budgets still fail closed, but a bounded successor and catalog cutover are a
  production-enablement prerequisite, not an in-place edit to `0.1.0`.

## Out of scope / follow-up

- OpenClaw consumption, reasoning session registration, stop/retry semantics,
  cross-repository E2E, and production gray rollout remain downstream.
- Publish a bounded `ai.reasoning-process` successor and switch the explicit Bot
  catalog before enabling Model A in production, unless the deployment keeps
  `OCTO_BOT_CARD_ENABLED=false` during the intermediate server rollout.
