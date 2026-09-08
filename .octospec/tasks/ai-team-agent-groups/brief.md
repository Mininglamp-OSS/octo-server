---
type: Task
title: "Task: ai-team-agent-groups"
description: Group the AI Team agent list into cloud clones, personal assistants, and a reserved digital-employee App Bot group.
tags: ["space", "isolation", "auth", "bot-api", "wire-contract", "trust-boundary", "test"]
timestamp: 2026-09-08T12:00:00+08:00
slug: ai-team-agent-groups
upstream: Mininglamp-OSS/octo-server#849 follow-up
source: user
---

# Task: ai-team-agent-groups

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Change `GET /v1/ai-team/agents` from a flat `items` page to a fixed, UI-ready
group list matching the AI Team navigation: `cloud_clone`,
`personal_assistant`, and `digital_employee`. Digital employees are backed by
App Bots in the future; App Bot AI sessions are not implemented yet, so the
wire group is defined now and remains empty. Publish a standalone Swagger 2.0
document for all current `/v1/ai-team` routes so frontend consumers can inspect
the complete contract without registering the document in server code.

## Background

The merged AI Team endpoint returns only owner-scoped User Bots and has no
business grouping. `robot.agent_hosting` is an open, runtime-self-reported slug:
`self_hosted` is the project convention for a user-operated runtime,
`octo_hosted` is the project convention for platform hosting, and third parties
may report other non-empty slugs. Grouping by this field is presentation only;
it must never grant access or relax the existing owner/User-Bot checks.

Proposed response:

```json
{
  "groups": [
    {"type": "cloud_clone", "count": 3, "items": []},
    {"type": "personal_assistant", "count": 2, "items": []},
    {"type": "digital_employee", "count": 0, "items": []}
  ],
  "next_cursor": "123"
}
```

`items` contains the current cursor page; `count` is the total eligible,
currently-added Agent count for that group before pagination. The three groups
are always present in the order above and empty `items` encode as `[]`.

User Bot presentation mapping:

- exact `octo_hosted` -> `cloud_clone`;
- exact `self_hosted` -> `personal_assistant`;
- empty/unreported or any other open hosting slug -> `personal_assistant`
  fallback, so no existing User Bot disappears;
- App Bot -> `digital_employee` (reserved empty in this task and never inferred
  from `agent_hosting`).

The fallback is deliberately conservative: an unknown self-report must not
label a Bot as an Octo-hosted cloud clone. Existing Bots remain visible; only
their presentation bucket changes.

## Load-bearing list

- **wire-contract** — replace the unmerged flat `items` shape with fixed groups;
  keep the existing opaque `next_cursor` semantics across the combined User-Bot
  result order. Group `count` is total, not current-page length.
- **space / isolation / auth / bot-api** — retain the exact current eligibility:
  same Space, active human and Bot seats, active owner-created User Bot, and
  `is_added=1`. Classification must happen after those predicates and must not
  change authorization.
- **trust-boundary** — `agent_hosting` is caller self-reported and may choose any
  valid slug. It only selects a display bucket; no route, capability, or session
  access may depend on it.
- **pagination** — page selection remains SQL-bounded and stable by descending
  `ai_team_agent.id`. Totals and page rows may come from separate reads and do
  not promise a cross-query snapshot.
- **testing** — cover all hosting mappings, fixed group order, total counts,
  empty arrays, cursor continuation, and the permanently empty App Bot group.

## Out of scope

- App Bot admission, App Bot sessions, App Bot group/thread support, or querying
  App Bot records into `digital_employee`.
- A new authoritative provisioning/business-category column.
- Changing `agent_hosting` validation, reporting, freshness, or authorization
  semantics.
- Front-end implementation or changing session-list APIs.
- Registering or serving the Swagger document from octo-server itself.

## Acceptance

- `GET /v1/ai-team/agents` always returns exactly the three documented groups in
  fixed order, and never serializes a group's `items` as `null`.
- Exact `octo_hosted` appears under `cloud_clone`; `self_hosted`,
  empty/unreported, and arbitrary third-party values appear under
  `personal_assistant`.
- `digital_employee` is present with `count: 0` and `items: []`; no App Bot is
  admitted, queried, or granted AI Team capability.
- Group counts reflect all eligible `is_added=1` agents before pagination;
  current-page items are partitioned without duplication or omission.
- Existing cursor bounds/order, Space isolation, Bot ownership checks, and
  add/remove/session behavior remain unchanged.
- `docs/ai-team-agents-swagger.yaml` documents all 11 current AI Team operations
  and validates as Swagger 2.0.
- Focused AI Team unit/API tests, `go test ./modules/ai_team/...`, `go build
  ./...`, `go vet ./...`, and `git diff --check` pass.
