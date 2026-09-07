---
type: Journal
title: "Journal: space-cloud-agents-by-owner"
description: Adds a Space-scoped directory endpoint that returns active human members with each owner's visible cloud-agent summaries, supports literal human/Bot-name keyword filtering, and preserves real per-owner counts while bounding detail rows.
tags: ["space", "isolation", "directory", "bot", "wire-contract", "rate-limit", "testing"]
timestamp: 2026-09-06T00:00:00+08:00
# --- octospec extension fields ---
task: space-cloud-agents-by-owner
upstream: "通讯录改版原型（口头需求，无 issue）"
source: user
---

# Journal: space-cloud-agents-by-owner

## What was done

Added `GET /v1/space/directory?space_id=<id>[&only_with_agents=true][&keyword=<keyword>]` for the
contacts directory. The route is ordered as `AuthMiddleware` →
`SharedUIDRateLimiter` → `SpaceMiddleware`; the handler requires the query
selector, then queries through the middleware-published verified Space ID.

The response contains every eligible active human in the Space, including
people with zero agents. Each agent list contains only active user bots that
are active members of the same Space and whose self-reported hosting is not
`self_hosted`. The endpoint deliberately exposes hosting only as a presentation
filter: it is never an authorization signal.

- The owner query applies the active-human predicates and the shared
  `SystemBotList()` exclusion, and reuses `MemberDetailModel.DisplayName()` for
  `user.name` → verified real name → stable-placeholder behavior.
- The agent query uses MySQL 8 `COUNT(*) OVER` plus `ROW_NUMBER() OVER` so
  `agent_count` remains exact while at most 50 stable (`robot_id ASC`) details
  per owner cross the database/application boundary. A `LEFT JOIN friend`
  provides per-viewer `is_friend` without N+1 reads.
- Both reads use one three-second request context. Either failed or canceled
  query returns `ErrSpaceQueryFailed`; no partial success envelope is emitted.
- `keyword` is a trimmed literal contains match over the owner display name or
  visible Bot name. The owner query retains either type of match; the agent
  candidate query applies the Bot-name match before window counting, so the
  returned details, `agent_count`, and `agents_truncated` describe the same
  filtered Bot set. `%`, `_`, and backslash are escaped with the module's
  explicit LIKE escape clause.

No migration, cache, pagination, frontend change, hosting authority column, or
new error code was introduced.

## Review-driven guards

- The package's manually maintained `robot` test fixture now reconstructs its
  schema rather than relying on `CREATE TABLE IF NOT EXISTS`; an old shared
  test table would otherwise silently lack the new `description` and hosting
  columns.
- The HTTP integration suite exercises the response contract for zero, exactly
  50, and 51 eligible agents. The 50-row assertion was mutation-checked: a
  temporary `agent_count >= 50` truncation condition made the exact-cap test
  fail, and the restored `agent_count > 50` implementation passed.

## Verification

- `go build ./...`
- `go test ./modules/space/... -count=1` against the local MySQL/Redis/WuKongIM
  test environment after recreating only the `test` database and flushing its
  Redis instance.
- `golangci-lint run ./modules/space/...`, `go vet ./modules/space/...`,
  `make i18n-extract-check`, `make i18n-lint`, and
  `go test ./modules/space -run TestSpaceNoLegacyResponseError -count=1`.
- `EXPLAIN` for both production query shapes: the owner query used
  `spacemember_spaceid_status`, user and verification UID joins were `eq_ref`,
  and the keyword path first materialized matching Bot owners from the same
  Space/status index before joining humans. The agent candidate CTE used the
  Space/member and UID indexes. The window calculation and final stable order
  used temporary/filesort as expected; the friend lookup used its `(uid, to_uid)`
  unique key.

## Structural learnings / gotchas

`SpaceMiddleware` intentionally passes through a request with no selector.
For a query-scoped endpoint, the handler must both require that public query
contract and consume `spacepkg.GetSpaceID()` after middleware verification;
using only the former turns a middleware convention into an unguarded data
selector.

The shared-test-schema issue is promoted as a pending testing-rule candidate:
manually maintained dependency fixtures must not use table-existence checks as
schema-version checks.
