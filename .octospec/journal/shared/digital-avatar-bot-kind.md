---
title: "Digital avatar Bot kind (Route B)"
description: "Implemented the server-owned robot.kind=avatar capability boundary and its AI Team-only collaboration flow."
tags: ["avatar", "bot-api", "ai-team", "space-isolation", "lifecycle"]
timestamp: 2026-09-09T19:25:00+08:00
---

# Digital avatar Bot kind (Route B)

Implemented organisation-managed Digital Avatars as `robot.kind=avatar`.
The server, rather than user BotFather or runtime self-reporting, owns type,
scope, publication and credential lifecycle.

Key outcome: an Avatar receives a real Space seat and can be independently
added to a user's AI Team, but it cannot enter or recover access to ordinary or
project groups. The AI Team service is the only path allowed to project it into
its managed container group and session thread. Project membership remains an
administrator-controlled collaboration seat and never grants project-group
access.

The Bot API is explicit-allowlist based for Avatars. Group management, thread
management, search, voice, OBO, user-key and Space-principal routes fail closed;
an unrecognised mounted route is denied by default. Lifecycle revocation first
closes local authority and relationships, then retries external cleanup without
restoring permissions.

Noteworthy gotcha: shared package tests use a fixed `test` database and cannot
be safely rerun while another worktree has changed its migration history. The
recorded integration runs used an isolated database; later static gates still
passed without mutating the shared test database.
