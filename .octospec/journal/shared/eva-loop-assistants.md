---
type: Journal
title: "Journal: eva-loop-assistants"
description: Credential-bound Bot owner context and owner-facing hosting metadata, with fail-closed account and Space checks.
tags: ["auth", "bot-api", "space", "project", "trust-boundary", "testing"]
timestamp: 2026-09-08T14:10:08+08:00
# --- octospec extension fields ---
task: eva-loop-assistants
upstream: "Mininglamp-OSS/octo-server#857"
source: user
---

# Journal: eva-loop-assistants

## Result

- `owned_bots` now returns hosting together with its report timestamp and keeps
  owner/Space/account filtering in the SQL predicate.
- `verify-bot?include=owner_context` derives the owner from the validated Bot
  credential, returns Bot and owner facts separately, and answers only the
  requested Project IDs.
- Disabled, cooling-off, terminally destroyed, removed-from-Space, and
  disbanded-Space cases all deny Project facts. Context lookup errors and a row
  disappearing between credential verification and context lookup fail closed.
- The default five-field `verify-bot` response remains unchanged.

## Structural learnings

- `user.status=1` is not sufficient liveness for a human account. Final account
  deletion sets `is_destroy=2` without clearing `status`, and Space/Project
  cascade cleanup is intentionally deferred. Authorization-shaped reads must
  test both fields.
- Account activity and Space membership are independent facts. Keep them
  separate on the wire, then require their conjunction before disclosing roles.
- A module test binary does not automatically register migrations owned by a
  sibling module. When production SQL gains such a dependency, import the
  migration owner from an external test package to avoid an import cycle.
- `agent_hosting` is caller-reported telemetry. Any response that carries it
  must also carry `agent_reported_hosting_at`, and neither field may become an
  authorization or quota signal.

## Gotchas

- A SQL mock that injects already-computed booleans does not prove the SQL that
  computes them. The query expectation pins every account, Space, and
  membership predicate, and an isolated MySQL matrix executes those predicates
  for disabled/destroyed principals, revoked memberships, and inactive scopes.
- The CI package runner recreates the shared `test` database before every
  package because migration ledgers differ by linked module set. Local
  verification used fresh isolated service containers for the same reason.
