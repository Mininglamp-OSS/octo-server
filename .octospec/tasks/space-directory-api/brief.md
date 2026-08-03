---
type: Task
title: "Task: space-directory-api"
description: Add a single OpenAPI-specified space directory endpoint that serves the contacts All/AI/Human tabs from one bot-visibility contract, replacing the client-side merge of space members and space bots.
tags: ["space", "isolation", "auth", "acl", "wire-contract", "error-response", "i18n", "rate-limit", "testing", "commit"]
timestamp: 2026-08-03T00:00:00Z
# --- octospec extension fields ---
slug: space-directory-api
upstream: none (diagnosed from an octo-web contacts API usage audit; see Background)
source: user
---

# Task: space-directory-api

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add **one** new authenticated, Space-isolated, paginated endpoint that answers the
contacts directory question directly:

```
GET /v1/space/{space_id}/directory?type=all|human|bot&page=&limit=
```

so that each contacts tab (全部 / AI / 人类) is one server call with a server-computed
`total`, instead of the client fetching two endpoints with two different bot-visibility
rules and merging/deduping/counting them locally.

The endpoint establishes **one** bot-visibility contract for the directory:

- A directory row is a `space_member` row with `status=1`, keyed uniquely by `uid`.
- `kind=bot` iff the uid has a `robot` row with `status=1`; everything else is
  `kind=human`. There is no third state and no row where the two sources disagree.
- Bot rows are **not** filtered by `robot.creator_uid`. Visibility is
  Space-membership-scoped only — identical to what `GET /v1/robot/space_bots`
  already returns today, but reached through a handler that actually verifies the
  caller belongs to the Space.
- `botfather` is excluded, matching `spaceBots`.

Existing `GET /v1/space/:space_id/members` and `GET /v1/robot/space_bots` keep their
current behavior — mobile app and the current web build depend on both. This task is
purely additive.

The endpoint is specified in **OpenAPI 3.0.3**, in a new
`modules/space/swagger/directory.yaml`. It is not appended to
`modules/space/swagger/api.yaml`, which is Swagger 2.0; mixing versions in one
document is invalid. OpenAPI 3.x already has precedent in this repo
(`modules/integration/swagger/api.yaml`, `modules/botfather/swagger/api.yaml`).

### Proposed wire contract

Request:

| param | in | type | default | notes |
|---|---|---|---|---|
| `space_id` | path | string | — | required |
| `type` | query | enum `all`\|`human`\|`bot` | `all` | unknown value → 400 business error, not a silent fallback |
| `page` | query | integer | 1 | `<=0` → 1 |
| `limit` | query | integer | 50 | `<=0` → 50; capped at 500 |

Response `200`:

```jsonc
{
  "total": 137,          // total matching rows for this `type`, before paging
  "page": 1,
  "limit": 50,
  "items": [
    {
      "uid": "u_1",
      "name": "Alice",                 // reuses the #344 DisplayName fallback chain
      "kind": "human",                 // "human" | "bot"
      "role": 3,                       // space_member.role — present for bots too
      "created_at": "2026-01-02T03:04:05Z",
      "bot": {                         // present iff kind == "bot", omitted otherwise
        "description": "",
        "creator_uid": "u_9",
        "creator_name": "Bob",
        "bot_commands": "",
        "auto_approve": 0,
        "relation": "not_added"        // "added" | "pending" | "not_added"
      }
    }
  ]
}
```

`relation` is the viewer-scoped friend state, carried over verbatim from `spaceBots`
(`friend` → `added`, else `friend_apply` row → `pending`, else `not_added`). It is
the one field in the payload that depends on who is asking.

Ordering is `sm.role DESC, sm.created_at ASC, sm.uid ASC` for every `type`. The
`sm.uid` tiebreaker is required: without a unique final sort key, `LIMIT/OFFSET`
paging can drop or repeat rows when many members share a `(role, created_at)`.
`queryMembers` currently lacks this tiebreaker; `searchMembers` already has it.

The caller's own uid is **included** in the result (it is a directory, and
`listMembers` includes it today). Client-side self-filtering stays a client concern.

## Background

An audit of octo-web's usage of the two existing endpoints produced the following
grounded findings.

**`GET /v1/space/:space_id/members`** — `modules/space/api.go:617`,
`modules/space/db.go:158`

- Authorization: `s.db.queryMember(spaceId, loginUID)`; non-members get
  `ErrSpaceNotMember`. Correctly isolated.
- Bot rows are **viewer-filtered**: `WHERE ... AND (r.robot_id IS NULL OR
  r.creator_uid = ?)`. A bot created by someone else is invisible in the member
  list entirely.
- `page`/`limit`, default 20, capped 10000. No `total` in the response.
- `memberResp` (`modules/space/model.go:178`) = `uid`,`name`,`role`,`robot`,
  `created_at`. No avatar (octo-web's `SpaceMember.avatar` in
  `packages/dmworkbase/src/Service/SpaceService.tsx:244` has never been populated
  by this endpoint).

**`GET /v1/robot/space_bots`** — `modules/robot/api.go:1479`, route at
`modules/robot/api.go:332`

- Authorization: `AuthMiddleware` **only**. No Space membership check, no
  `SpaceMiddleware`. Any authenticated user can pass an arbitrary `space_id`.
- No `creator_uid` filter — returns every `robot.status=1` bot with an active
  `space_member` row, excluding `botfather`.
- No pagination; full list plus three follow-up batch queries (`friend`,
  `friend_apply`, creator names).
- Returns `status` (added/pending/not_added), `description`, `creator_uid`,
  `creator_name`, `bot_commands`, `auto_approve`.

**Consequence.** The bot sets are nested, not equal:
`{bot in members} = {bot | creator_uid = me}` ⊊ `{bot in space_bots}`. octo-web
therefore cannot serve its three tabs from one call:

- `packages/dmworkcontacts/src/Contacts/index.tsx:362` `loadAllData` fans out to
  both endpoints (plus `my_bots`, `group/my`); tab switching does no I/O and
  re-slices the two cached arrays in `buildIndex` (`index.tsx:454`).
- Counts (`getFilterCounts`, `index.tsx:707`) and search
  (`bridge/contactsSearch/searchContacts.ts:32`) re-derive the same union client-side.
- `packages/docs/` hit the identical split from the other direction: docs must issue
  a separate `space_bots` call purely to recover display names that `members` drops
  (`packages/docs/src/members/memberNames.ts:9`, `octoweb/index.ts:218`).

**Defects confirmed while reading the handlers.** Two are fixed *by construction*
in the new endpoint; the third is explicitly deferred:

1. *(fixed here, by construction)* `queryMembers` joins `LEFT JOIN robot r ON
   r.robot_id=sm.uid` without `r.status=1`, while the projected `robot` column is
   `CASE WHEN r.robot_id IS NOT NULL AND r.status=1`. A **disabled** bot the caller
   created therefore passes the `creator_uid` WHERE branch and is returned with
   `robot=0` — the client renders it as a human. `spaceBots` INNER JOINs
   `r.status=1`, so the same account is simply absent there. The new endpoint's
   single `kind` derivation cannot express this disagreement.
2. *(fixed here, by construction)* Unstable paging: `queryMembers` orders by
   `sm.role DESC, sm.created_at ASC` with no unique tiebreaker.
3. *(deferred, NOT in this task)* `/v1/robot/space_bots` has no Space membership
   check. This is a live cross-Space exposure on an **existing** endpoint and must
   be fixed on its own, on its own timeline — see Out of scope.

Uniqueness was verified rather than assumed: `space_member` has
`UNIQUE (space_id, uid)` (`modules/space/sql/20260307000002_space_legacy01.sql:32`),
`robot.robot_id` is UNIQUE, and `user_verification` is `PRIMARY KEY (user_id)`. No
join in the current member query can fan out, so the duplicate-uid dedup workaround
in `packages/dmworkbase/src/Components/ForwardModal/hooks/useForwardCandidates.ts:262`
has no reproducible backend source today. The new endpoint must nonetheless
**guarantee** one row per uid, and a test must assert it.

## Load-bearing list

- **space**, **isolation**, **auth** — the endpoint exposes the full Space roster
  including every bot in the Space. It MUST mount `spacepkg.SpaceMiddleware`
  (`pkg/space/middleware.go:99`), which reads `space_id` from query/`X-Space-ID`
  and verifies membership against a Redis-cached `CheckMembership`. Because
  `space_id` here is a **path** param, the middleware's query/header lookup does not
  fire on its own — the handler must either receive the id in a form the middleware
  reads, or perform the same `queryMember` check `listMembers` does. Whichever is
  chosen, a non-member MUST NOT get rows. (rules: space-isolation)
- **acl** — bot rows drop the `creator_uid` restriction relative to `listMembers`.
  This is **not** a net widening: the same set is already returned by
  `/v1/robot/space_bots` to any authenticated caller. Net effect is a narrowing,
  since the new path requires Space membership. This reasoning is load-bearing and
  must be restated in the PR's COMPREHENSION answers. (rules: space-isolation)
- **wire-contract** — new endpoint, no existing consumer, so the shape is free;
  but `name` MUST reuse the `MemberDetailModel.DisplayName()` fallback chain from
  #344 (`user.name` → `user_verification.real_name` → stable placeholder) and MUST
  NOT expose `short_no` / `username`, which are privacy-gated. (rules: error-handling)
- **error-response**, **i18n** — all failures go through
  `httperr.ResponseErrorL` + a registered `pkg/errcode` code. Reuse existing space
  codes (`ErrSpaceQueryFailed`, `ErrSpaceNotMember`, `respondSpaceRequestInvalid`)
  where they fit; register new ones only if none does. `ResponseErrorL` (pinned 400),
  not `ResponseErrorLWithStatus`. (rules: error-handling)
- **rate-limit** — this is an enumerable list endpoint on an authenticated route.
  Mount `appwkhttp.SharedUIDRateLimiter(r, s.ctx)` **after** `AuthMiddleware`, as the
  existing `search` / `joinLimited` groups in `modules/space/api.go:114` do. Tests
  hitting the route must reset `ratelimit:uid:*` in setup. (rules: rate-limit)
- **testing** — `TestSpaceNoLegacyResponseError` (or the space module's equivalent
  source guard) must list any new handler file. (rules: testing)
- **commit** — English Conventional Commits. (rules: commit-style)

## Out of scope

- **Changing `GET /v1/space/:space_id/members` or `GET /v1/robot/space_bots` in any
  way.** Both stay byte-compatible; the mobile app and shipped web builds consume
  them. In particular the `r.status=1` join defect and the missing `sm.uid` sort
  tiebreaker are *not* repaired in the old query here — the new endpoint simply does
  not reproduce them.
- **The `/v1/robot/space_bots` cross-Space exposure.** It is a security fix to an
  existing endpoint with its own blast radius (four octo-web call sites plus the
  mobile app) and its own urgency; it must not ride along with an additive feature
  and must not wait on this task. Track as a separate brief
  (`robot-space-bots-space-isolation`).
- **Any octo-web change.** Migrating the contacts tabs, `getFilterCounts`, the local
  search index, and the docs name-backfill onto the new endpoint is a follow-up in
  the octo-web repo. This task ships the server contract only.
- **`keyword` / server-side search.** `GET /v1/space/:space_id/members/search`
  already covers admin-scoped search; contacts search is currently local and stays
  local. Adding a filter to two overlapping search surfaces at once is a separate
  decision.
- **`avatar`.** Not returned by either endpoint today; clients derive it from uid.
  Adding it is a distinct contract change.
- **`my_bots` / `group/my`.** The contacts page's 「已添加AI」and 「我的群聊」
  accordions are friend-dimension and group-dimension respectively, not directory
  rows. Unchanged.
- **Admin surfaces** (`/v1/manager/spaces/:space_id/members`, `queryMembersAdmin`).
- **Any octo-lib change.**

## Acceptance

- `go test ./modules/space/...` passes; `golangci-lint run ./...` clean.
- `make i18n-extract-check` and `make i18n-lint` pass; no raw
  `c.ResponseError` / `c.JSON(non-200)` / `AbortWithStatusJSON` introduced, and the
  space module's legacy-response guard test covers the new handler file.
- `modules/space/swagger/directory.yaml` exists, declares `openapi: 3.0.3`, parses
  as valid OpenAPI 3, and documents the path, all four parameters with their
  defaults and the `limit` cap, the `200` schema (including `bot` present iff
  `kind == "bot"`), and the 400/403 error envelope.
- New tests in `modules/space/` cover:
  1. **Non-member is refused.** An authenticated user with no `space_member` row in
     the target Space receives an error and **zero** rows — asserted on the body,
     not only the status code.
  2. **Bot visibility is creator-independent.** A bot created by user B, member of
     Space S, appears with `kind="bot"` for viewer A (also a member of S). The same
     bot is absent from `GET /v1/space/S/members` for A — both assertions in one
     test, pinning the intended divergence.
  3. **Disabled bot never renders as human.** A bot created by viewer A with
     `robot.status != 1` is absent from `type=bot` **and** absent from
     `type=human` — i.e. it does not reappear as `kind="human"` (the defect
     described in Background 1).
  4. **`botfather` is excluded** from every `type`.
  5. **`type` partitions exactly:** `total(all) == total(human) + total(bot)` on a
     fixture holding both, and the union of the paged `all` uids equals the union
     of paged `human` and `bot` uids.
  6. **One row per uid** across a full pagination sweep — no duplicates.
  7. **Stable paging:** with ≥3 members sharing an identical `(role, created_at)`,
     walking `limit=1` pages to `total` yields every uid exactly once.
  8. **`relation` is viewer-scoped:** the same bot reads `added` for a viewer with a
     `friend` row, `pending` for a viewer with only a `friend_apply` row, and
     `not_added` for a third viewer.
  9. **Param handling:** `page<=0` → 1, `limit<=0` → 50, `limit>500` → 500,
     unknown `type` → business error via `httperr.ResponseErrorL`.
  10. **Name fallback is inherited:** a member with empty `user.name` but a
      `user_verification.real_name` returns `real_name`; both empty returns the
      stable placeholder, never `""`, never `short_no`/`username`.
- Rate limiting: the route sits behind `AuthMiddleware` + `SharedUIDRateLimiter`;
  tests reset `ratelimit:uid:*` in setup per the repo testing rule.
- No diff to `modules/space/api.go:617` `listMembers`, `modules/space/db.go:158`
  `queryMembers`, or `modules/robot/api.go:1479` `spaceBots` beyond additive
  helpers they do not call.
