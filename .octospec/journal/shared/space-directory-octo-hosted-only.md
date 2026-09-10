---
type: Journal
title: "Journal: space-directory-octo-hosted-only"
description: Tightens GET /v1/space/directory so only octo_hosted bots owned by an eligible human in the same Space appear as agents.
tags: ["space", "isolation", "directory", "bot", "wire-contract", "testing"]
timestamp: 2026-09-09T21:00:00+08:00
# --- octospec extension fields ---
task: space-directory-octo-hosted-only
upstream: "product follow-up to space-cloud-agents-by-owner"
source: user
---

# Journal: space-directory-octo-hosted-only

## What was done

`GET /v1/space/directory` previously treated every non-`self_hosted` bot as a
visible cloud agent. That included empty hosting (never reported), retracted
hosting, and third-party `<vendor>_hosted` slugs. Product now wants only
platform-hosted clones: `robot.agent_hosting = 'octo_hosted'`.

A visible bot must also belong to an eligible human in the same Space. That
owner join already existed on the agent query; this change copies it onto the
keyword `matched_bot` subquery so a bot owned by another bot cannot surface
anyone by name. The handler still fail-closes agents whose `creator_uid` is
missing from the human owner index.

Project agent eligibility, AI Team grouping, members/space_bots, and leftover
test accounts such as `ProjectApiTest` were left alone.

## Tests

- Directory handler tests now pin unreported / withdrawn / vendor / self_hosted
  / creator-is-bot exclusions, keep `hosting_reported_at: null` for
  `octo_hosted` with no timestamp, and require `only_with_agents=true` to keep
  only humans who own at least one `octo_hosted` bot.
- Keyword tests pin that vendor, empty, self-hosted, and nested-bot names do
  not match.
- `golangci-lint run ./modules/space/...` was clean.
- CI unit lane (`ci/run-unit-tests.sh`) passed. Directory e2e passed once
  against the local MySQL/Redis/WuKongIM stack; a later full-package rerun
  collided with other worktrees dropping the shared `test` database.

## Learning

Two SQL statements that define the same “visible bot” set (keyword owner match
and agent details) must share the hosting predicate **and** the human-owner
join. Matching only `creator_uid <> ''` on the keyword side lets a bot-owned
bot’s name pull a row that the agent query would drop.
