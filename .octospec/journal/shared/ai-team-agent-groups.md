---
type: Journal
title: "Journal: ai-team-agent-groups"
description: Group the AI Team Agent list into cloud clones, personal assistants, and a reserved empty digital-employee group while retaining the existing authority and cursor contract.
tags: ["ai-team", "bot", "wire-contract", "space", "trust-boundary"]
timestamp: 2026-09-08T15:16:29+08:00
# --- octospec extension fields ---
task: ai-team-agent-groups
upstream: Mininglamp-OSS/octo-server#849
source: user
---

# Journal: ai-team-agent-groups

## Result

- `GET /v1/ai-team/agents` now returns three fixed groups in UI order:
  `cloud_clone`, `personal_assistant`, and `digital_employee`.
- Exact `octo_hosted` User Bots are cloud clones. `self_hosted`, missing, and
  unknown hosting slugs fall back to personal assistants so no existing User
  Bot disappears. Digital employees are reserved for future App Bot support and
  always return `count: 0, items: []` in this change.
- Group counts describe the full eligible set before pagination; page rows keep
  the existing descending `ai_team_agent.id` cursor and are partitioned without
  changing its bounds.

## Load-bearing decisions

- The page and aggregate count queries share one eligibility builder containing
  the existing Space, active-seat, active-Bot, owner-created User Bot, and
  `is_added=1` predicates. Classification is applied only after that boundary.
- `agent_hosting` remains self-reported presentation data. It is not returned on
  the Agent wire model and does not affect authorization, routing, or capability.
- App Bots are not queried or admitted. A future digital-employee implementation
  needs its own App Bot authority and session design rather than interpreting a
  User Bot hosting slug.

## Verification

- CI unit lane: 52 packages under `-race -shuffle=on` passed.
- CI E2E shard 4/4: 16 packages, including AI Team API tests and live WuKongIM,
  passed under `-race -shuffle=on`.
- `go test -race -shuffle=on -count=1 -timeout 12m ./modules/ai_team` passed.
- `go build ./...`, `go vet ./...`, and `git diff --check` passed.
