---
type: Task
title: "Task: bot-task"
description: Add a source-authenticated, idempotent generic Bot Task ingress and bot event contract
tags: [bot-task, bot-api, wire-contract, auth]
timestamp: 2026-09-06T00:00:00Z
slug: bot-task
upstream: Loop comment Bot mentions
source: self
---

# Task: bot-task

## Goal

Add `POST /v1/internal/bot-tasks` so trusted business backends can deliver a
complete prompt to a target User Bot. The server authenticates the configured
source, validates and deduplicates the request, and atomically enqueues one
fixed `bot_task` event for the existing Bot Event poller.

The transport is business-neutral: `source` and `task_type` are opaque
observability fields. The server does not interpret Loop, Doc, profiles,
capabilities, or execution steps.

## Load-bearing list

- Source authentication is fail-closed and uses constant-time token checks.
- Only active robots may receive tasks; per-source Bot allowlists are enforced.
- Idempotency scope is `(source, bot_uid, idempotency_key)` and stores a request fingerprint.
- Claim completion and Bot Event queue insertion are one atomic Redis operation.
- The wire event type is always `bot_task`.
- New error paths use localized error envelopes with real HTTP status codes.
- Prompt and structured JSON input are bounded at the ingress trust boundary.

## Out of scope

- Business-specific prompt generation or task execution.
- `execution`, `profile`, `capabilities`, and `steps` fields.
- ACK result payload changes or migration of `doc_comment_mention`.
- Loop write-operation idempotency.

## Acceptance

- A configured source can create a task and receives HTTP 202.
- Same-key/same-request replay receives HTTP 200 without another event.
- Same-key/different-request receives HTTP 409.
- Invalid sources, disallowed/nonexistent Bots, malformed and oversize payloads are rejected.
- The queued event is `bot_task` and preserves the validated generic payload.
- Existing Bot Event types and the Doc Mention ingress remain unchanged.
