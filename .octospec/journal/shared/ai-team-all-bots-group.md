---
type: Journal
title: "Journal: ai-team-all-bots-group"
description: Projects each user's active AI Team Agent roster into protected two-member containers and one visible owner-plus-all-agents group, including automatic Bot creation hooks and reconciliation.
tags: ["ai-team", "group", "space", "isolation", "acl", "bot", "testing"]
timestamp: 2026-09-08T19:20:00+08:00
# --- octospec extension fields ---
task: ai-team-all-bots-group
upstream: "User request; design informed by server PR #855"
source: user
---

# Journal: ai-team-all-bots-group

## Result

Added one durable `ai_team_group` registration per `(space_id, user_uid)` and a
visible `purpose=ai_team_group` group named “我的AI团队”. Its exact live roster
is the owner plus every eligible `ai_team_agent.is_added=1` Bot whose private
two-member container is ready. Removing an Agent keeps its private container and
session history but removes it from the team group; reactivation reuses both
group identities.

AI Team listing now acts as the legacy-data convergence point: it creates Agent
rows only for owned Bots that never had one, repairs all active pair containers,
and then projects the complete team group. BotFather conversation creation,
`POST /v1/user/bots`, and `MintBotOBO` call the same idempotent provisioner after
the Bot's Space membership is durable. Temporary MySQL 1213/1205 conflicts retry
the whole idempotent add/remove/list reconciliation up to three times.

Ordinary web, manager, service, Bot API, scan/invite, org-directory, ownership,
and disband paths cannot mutate either managed group. The visible team group is
included in normal human and Bot group lists and allows personal settings, while
the existing pair container and its threads remain hidden. Lifecycle cleanup can
still remove deleted or departed identities from both managed group types.

## Structural learnings / gotchas

- The Agent table must remain the only roster authority. Accepting a Bot directly
  into the team group would create a second source of truth and make repair
  direction ambiguous.
- The safe convergence order is pair containers first, then one exact team-roster
  projection. Projecting only the newly added Bot misses inactive, invalid, or
  stale rows and cannot repair old data.
- Unique keys prevent duplicate durable identities but do not prevent InnoDB
  from choosing a transaction as a deadlock victim. The entire orchestration is
  idempotent, so bounded whole-operation retries are a safe backstop.
- The repository's test packages share a hard-coded MySQL database. Running
  `go test ./...` in package-parallel mode lets schema-owning fixture tests race
  migrations and table cleanup; target integration tests must be rerun serially
  after rebuilding that disposable database.

## Verification

See `.octospec/tasks/ai-team-all-bots-group/verification.md` for passing focused,
race, vet, i18n, and whitespace checks plus the documented shared-database limits
on full-suite execution.
