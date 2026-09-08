---
type: Task
title: "Task: eva-assistant-eager-ai-group"
description: Automatically register newly created EVA assistant Bots as AI Team agents and eagerly provision their private two-member group.
tags: [space, isolation, auth, acl, thread, wire-contract, error-response, i18n, test]
timestamp: 2026-09-08T00:00:00+08:00
# --- octospec extension fields ---
slug: eva-assistant-eager-ai-group
upstream: chat-request
source: self
---

# Task: eva-assistant-eager-ai-group

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Make “create assistant” converge to the complete Octo state expected by EVA:
after EVA creates the assistant's User Bot, EVA registers that Bot through
`POST /v1/ai-team/agents/{bot_id}`, and octo-server returns only after the
protected owner+Bot AI group has been created or reconciled. This removes the
current mismatch where an Octo Bot exists but its private AI group is deferred
until the first session is created.

## Background

- EVA currently selects an Octo Space, exchanges its XRUI token for a `uk_`
  User API Key, and calls `POST /v1/user/bots`.
- EVA already maintains a separate Octo login Session Token through
  `OctoAuthManager`; AI Team endpoints use that token plus `X-Space-ID`.
- octo-server's `AddAgent` currently creates the `ai_team_agent` row but creates
  the `ai_session_container` group lazily in `CreateSession`.
- A User Bot is user-owned, but AI Team registration and its group are scoped by
  `(space_id, user_uid, bot_id)`. Generic User Bot creation must remain usable
  without automatically registering every Bot as an AI Team agent.

## Load-bearing list

- `space`, `isolation`, `auth`, `acl`: the request must use the selected Bot
  Space, require the authenticated owner, and admit only that owner and Bot.
- `thread`: eager Agent registration creates the parent AI group only; it must
  not create an `ai_team_session`, thread, or topic channel.
- `wire-contract`, `error-response`, `i18n`: preserve the existing
  `POST /v1/ai-team/agents/{bot_id}` request/response contract and localized
  error envelope while surfacing group/IM provisioning failure.
- `test`, `testing`: update lifecycle expectations and add focused EVA request
  coverage for token and Space header propagation.
- Idempotency and recovery: repeated Agent registration must reuse the same
  group, repair owner/Bot DB membership and WuKongIM parent-channel state, and
  preserve the legacy `CreateSession` recovery path for Agent rows with no
  group.
- Cross-repository integration: octo-server and EVA client must agree on Bot ID,
  selected Space ID, and Octo Session Token usage.

## Out of scope

- Do not make `POST /v1/user/bots` implicitly register every User Bot as an AI
  Team agent.
- Do not add `uk_` authentication support to AI Team endpoints or weaken their
  existing authenticated-user and Space middleware.
- Do not expose the private AI group to other Space members or change group
  mutation protections.
- Do not create an initial AI Team session/thread during assistant creation.
- Do not change Space selection policy, Bot deletion semantics, or unrelated
  EVA/OpenClaw flows.

## Acceptance

- On a valid first `POST /v1/ai-team/agents/{bot_id}`, the response contains a
  non-empty stable `group_no`; exactly one active protected
  `ai_session_container` group exists with exactly the owner and Bot as active
  members; its WuKongIM parent channel is provisioned; and no AI Team session or
  thread is created.
- Repeating the Agent request returns the same `group_no` without duplicate DB
  rows and repairs missing/deleted owner/Bot membership and parent-channel state.
- An unauthorized owner, inactive/foreign Space membership, or invalid Bot is
  still rejected through the existing localized error contract.
- Creating a session after eager registration reuses the same parent group;
  legacy Agent rows with an empty `group_no` still converge successfully.
- EVA's local assistant auto-connect flow calls the Agent endpoint after Bot
  creation with `token: <Octo Session Token>` and `X-Space-ID: <selected space>`.
  The Bot ID is URL-encoded and no token is logged.
- If EVA cannot obtain Octo Session credentials or Agent registration fails,
  assistant auto-connect reports failure and does not persist/enable the local
  Bot as successfully bound.
- The cloud BotFather-style flow remains unchanged unless it is also part of
  “create assistant”; generic Bot creation alone does not create the AI group.
- Focused Go tests, EVA Vitest coverage, changed-file ESLint, and Electron
  TypeScript compilation pass.
