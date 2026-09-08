---
type: Journal
title: "Journal: eva-assistant-eager-ai-group"
description: Eagerly provisions each AI Team Agent's private owner-and-Bot group and wires EVA assistant creation to the authenticated Agent endpoint.
tags: [ai-team, space, auth, group, idempotency, eva]
timestamp: 2026-09-08T00:00:00+08:00
# --- octospec extension fields ---
task: eva-assistant-eager-ai-group
upstream: chat-request
source: user
---

# Journal: eva-assistant-eager-ai-group

## Result

`POST /v1/ai-team/agents/{bot_id}` now creates or repairs the protected
`ai_session_container` group before returning. The existing Agent row lock
serializes group allocation, the group admission bridge enforces exactly the
owner and Bot as active members, and the WuKongIM parent channel is provisioned
without creating a thread or `ai_team_session` row. `CreateSession` shares the
same transactional helper, retaining recovery for legacy Agent rows whose
`group_no` is empty.

The companion EVA change calls the Agent endpoint after User Bot creation with
the existing Octo Session Token in `token` and the selected Space in
`X-Space-ID`. It completes registration before persisting/enabling the local
Bot, and retries Agent reconciliation for existing BotStore records that still
have their Bot and Space identifiers. The `uk_` credential remains confined to
User Bot creation.

## Structural learnings and gotchas

- User Bot ownership and AI Team membership are different boundaries: Bot
  creation uses the user API key, while AI Team registration remains an
  authenticated, Space-scoped user action.
- A local `container_state=ready` value cannot prove that the external
  WuKongIM channel still exists. Explicit Agent registration therefore replays
  the idempotent parent/topic upserts even for a ready row.
- IM failure happens after the group transaction commits. The durable Agent and
  group remain retryable, but EVA must not mark the local binding successful
  until the Agent request succeeds.

## Verification

- `go test ./modules/ai_team/... -count=1`
- `npm test -- --run src/main/libs/octoAutoConnect.test.ts`
- Changed-file ESLint with zero warnings.
- `tsc --project electron-tsconfig.json --noEmit`

