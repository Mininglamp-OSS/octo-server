---
type: Task
title: "Task: bot-space-principal-lookup"
description: Add an exact-Space, Bot-authenticated principal eligibility lookup for Docs.
tags: [bot-api, space, auth, docs, principal]
timestamp: 2026-08-27T23:08:00+08:00
# --- octospec extension fields ---
slug: bot-space-principal-lookup
upstream: "approved Docs integration"
source: user
---

# Task: bot-space-principal-lookup

## Goal

Add `GET /v1/bot/space/principals/:uid?space_id=...` so a Docs integration can resolve one principal only when the authenticated Bot and target are eligible in that exact active Space. Return only the stable principal UID and classification.

## Load-bearing list

- `/v1/bot` authentication and context identity (`robot_id`, `bot_kind`).
- Exact-Space isolation: active Space, calling Bot authority for that Space, and active target `space_member` are mandatory.
- User Bot eligibility: active, unambiguous `robot` identity whose active creator is an active member of the same Space.
- App Bots are outside this endpoint's contract: reject an App Bot caller from
  the authenticated `bot_kind` context before any principal DB lookup, and
  reject an App Bot target through the same non-enumerating not-found response.
- Human eligibility: active, non-terminally-destroyed user identity.
- Ambiguous/conflicting Bot identities and database errors fail closed.
- All absent/inactive/ineligible targets share one localized not-found response; infrastructure errors use an internal localized response.
- Response contract is minimal: `uid` and `principal_type` (`human` or `user_bot`).

## Out of scope

- Principal search, prefix/fuzzy lookup, batches, profile/PII fields, or membership mutation.
- Changes to Bot token issuance/authentication, Space membership, robot/app-bot schemas, or Docs persistence.
- Platform- or Space-scoped App Bot compatibility, as either caller or target.
- Granting a caller access based on request-provided identity or a non-exact/default Space.
- Commit, push, PR creation, or deployment.

## Acceptance

- The endpoint is mounted only under the existing authenticated `/v1/bot` group and rejects missing `uid` or `space_id` through the localized invalid-request envelope.
- Human target succeeds only with an active target membership in the exact active Space and returns exactly `uid` plus `principal_type=human`.
- An App Bot caller is rejected before the database lookup. An App Bot target
  returns the same not-found envelope as every absent/inactive/ineligible target.
- Eligible active User Bot targets are classified as `user_bot`; conflicting identities fail closed.
- Target absent, caller outside the Space, disabled Space/member/Bot, and User Bot creator absent/inactive/outside the Space all return the same not-found envelope without revealing which predicate failed.
- Database errors return the localized internal query-failed envelope and never a raw error.
- Focused route tests cover App Bot caller rejection before lookup, validation,
  human success, App Bot target rejection, target absence, caller/target outside
  Space, disabled caller/target/Space/member/Bot, missing Bot creator, and DB failure.
- Database tests pin the exact-Space eligibility query and classification rules.
- Edited Go files pass gofmt; focused tests, `go test`/build compilation, and `git diff --check` are run where infrastructure permits, with exact blockers reported.
