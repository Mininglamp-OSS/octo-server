---
type: Journal
title: "Digital avatar Bot kind (Route B)"
description: "Implemented the server-owned robot.kind=avatar capability boundary and its AI Team-only collaboration flow."
tags: ["avatar", "bot-api", "ai-team", "space-isolation", "lifecycle"]
timestamp: 2026-09-09T21:46:00+08:00
---

# Digital avatar Bot kind (Route B)

Implemented organisation-managed Digital Avatars as `robot.kind=avatar`.
The server, rather than user BotFather or runtime self-reporting, owns type,
scope, publication and credential lifecycle.

Key outcome: an Avatar receives a real Space seat and can be independently
added to a user's AI Team, but it cannot enter or recover access to ordinary or
project groups. The AI Team service is the only path allowed to project it into
its managed container, automatic and custom AI groups and session threads.
Project membership remains an administrator-controlled collaboration seat and
never grants project-group access.

The Bot API is explicit-allowlist based for Avatars. Group management, thread
management, search, voice, OBO, user-key and Space-principal routes fail closed;
an unrecognised mounted route is denied by default. Lifecycle revocation first
closes local authority and relationships, then retries external cleanup without
restoring permissions.

Rebased onto `feat/multi-ai-teams` at `0bc8edac` (source PR #875). Removed the
last two retired `ai_team_group` table queries from Bot authorization and group
listing, sharing one live owner/Agent predicate over `group` and `ai_team_agent`.
Custom teams now use the same Avatar eligibility as private/automatic teams.
Ordinary-group rejection, removal revocation and independent per-user
membership remain enforced. The directory retains hosted-only personal bots
and separately lists organization-managed digital employees.

Final verification: 85 real-HTTP checks passed with no retired table present;
build, full vet, i18n and three affected regression-package compilations passed.
User requested API-only verification and authorized commit, push and PR.

Noteworthy gotcha: shared package tests hardcode `test` and delete its rows
before migration validation. An earlier invocation removed temporary fixtures;
the final verification instead used an explicitly isolated database and compiled
DB-backed tests without executing their harness. Preserve backups and never
assume an environment DSN overrides that harness.
