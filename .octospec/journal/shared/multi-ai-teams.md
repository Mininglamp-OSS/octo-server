---
type: Journal
title: "Journal: multi-ai-teams"
description: Adds multiple user-created AI-only teams while preserving the automatic all-agent group and private AI session containers.
tags: ["ai-team", "group", "space", "acl", "bot", "thread", "testing"]
timestamp: 2026-09-09T16:30:00+08:00
# --- octospec extension fields ---
task: multi-ai-teams
upstream: user-request
source: user
---

# Journal: multi-ai-teams

## Result

Added custom AI teams by reusing `group` as the team authority and
`group_member` as the only roster authority, distinguished by
`purpose=ai_custom_team_group`. A user can create multiple teams in one Space,
reuse an eligible owned Agent across teams, change each team's profile and
selected Agents independently, and disband a team without changing Agent
activation, the automatic all-agent group, or private AI sessions.

The team list now returns the automatic all-agent group and custom teams through
one paginated contract with explicit type and editability flags. The Agent list
remains the authoritative picker directory and no longer embeds the automatic
team.

Custom teams reuse ordinary conversation, contact, profile, personal setting,
message, and subarea behavior. Server-side admission and mutation guards keep
the creator as the only human member and reject invite, owner-transfer,
manager, Bot-admin, unauthorized Bot, cross-Space, leave, and recovery paths
that could violate that invariant. Roster projection synchronizes both parent
and existing subarea subscribers.

## Structural learnings / gotchas

- The automatic all-agent group remains a projection of every active Agent,
  while a custom team's selected roster lives directly in `group_member`; a
  second team or team-member table only duplicates ordinary group state.
- Treating every AI-purpose group as fully hidden is too coarse. Custom teams
  must remain visible on ordinary conversation surfaces while only their roster
  and single-human invariant stay protected.
- UI capability filtering remains necessary even with server-side rejection.
  A custom team keeps ordinary profile and personal settings, but its group
  management panel must expose only disbanding rather than controls that can
  never succeed.
- Subscriber projection must update existing subareas in both directions;
  updating only the parent group leaves newly added Agents absent and removed
  Agents subscribed.
- Local DB-backed Go packages share one fixed test database. Recreate it per
  package instead of treating a parallel `go test ./...` failure as a product
  regression.

## Verification

See `.octospec/tasks/multi-ai-teams/verification.md` for the focused Go and Web
gates and the complete local CUA/OpenClaw flow.
