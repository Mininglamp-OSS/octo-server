---
type: Task
title: "Task: octo-266-cli-authorization"
description: Implement the independent CLI v2 authorization integration.
tags: [auth, space, bot-api, thread, wire-contract]
timestamp: 2026-11-11T00:00:00Z
slug: octo-266-cli-authorization
upstream: OCTO-266
source: self
---

# Task: octo-266-cli-authorization

## Goal
Implement the approved CLI v2 authorization design in `docs/specs/2026-11-10-octo-server-authorization-integration-design.md`.

## Load-bearing list
- Independent v2 router and SDK authentication, action authorization and trusted identity.
- Token-bound Space, group/thread/DM authorization, resource ownership and search visibility.
- Existing v1 credentials, router, business behavior, rate limits and localized errors.
- v2 OpenAPI wire contracts and safe dependency-failure behavior.
- Existing file access boundary and storage behavior.

## Out of scope
- App Bot, v2 OBO, commands, events and heartbeat.
- Changes to legacy file business logic and external service implementation.
- Production deployment or enabling access before real dependencies are verified.

## Acceptance
- All v2 routes and per-route actions in the spec work for user and bound User Bot as specified.
- Authentication, authorization, actor and owner transitions, bound Space, error envelope and old v1 behavior are tested.
- OpenAPI documentation and targeted integration tests are run with real available dependencies; unavailable external prerequisites are reported.

## File download boundary
- The owner approved the existing object-key download behavior for `/v2/cli/files/_download` as a scoped exception: object keys have no Space ownership source, and the route does not establish object-level Space isolation.
