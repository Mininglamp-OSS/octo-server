---
type: Task
title: "Task: unified-obo"
description: Add owner-only Bot OBO resolution with generic ALL Scope and SDK contract
tags: [obo, bot-api, auth, space, wire-contract]
timestamp: 2026-09-21T00:00:00Z
slug: unified-obo
source: self
---

# Task: unified-obo

## Goal

Implement the approved `obo-auth-sdk-architecture.html` design for octo-server
and octo-auth: a two-mode Bot identity resolver, owner-only OBO delegation,
generic ALL Scope management, and read-only Bot Project access.

## Load-bearing list

- Both modes require an explicit nonempty target Space and active Bot membership.
- OBO Subject is derived only from the current active User Bot owner; callers cannot choose it.
- Grant, binding, owner, and Bot membership checks are fail-closed and consistent.
- Resolve uses the existing Bot token. The server checks registered Actions, but
  does not authenticate which downstream service submitted one; SDK consumers
  must derive Action from a controlled route.
- An OBO failure never falls back to As Bot, and business permissions remain with each module.
- Existing Channel OBO behavior and v1 verify endpoints remain unchanged.
- The approved document reuses `obo_grants`; shared state and single-active-persona
  effects must be regression-tested and surfaced during rollout.

## Out of scope

- Changing Fleet, Loop, or CLI code.
- Migrating legacy Channel OBO checks into the new resolver.
- Fine-grained Scopes, OBO decision caches, or dynamic Action policy management.

## Acceptance

- Owner management can atomically configure a Grant and ALL binding; historical
  Grants have no implicit ALL binding.
- New service API resolves AS_BOT and OBO with a uniform principal/error contract.
- Project Bot reads apply existing Human Project permissions after OBO succeeds.
- Go and TypeScript SDKs expose matching resolver behavior without breaking v1.
- Focused tests cover invalid Bot tokens, Space mismatch, owner-record inconsistency,
  Grant/Scope revocation, unregistered Actions, and legacy regression.
