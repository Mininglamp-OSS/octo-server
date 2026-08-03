---
type: Task
title: "Task: robot-space-bots-space-isolation"
description: Require Space membership on GET /v1/robot/space_bots, which currently lets any authenticated user enumerate every bot in an arbitrary Space.
tags: ["space", "isolation", "auth", "acl", "bot-api", "rate-limit", "error-response", "i18n", "testing", "commit"]
timestamp: 2026-08-03T00:00:00Z
# --- octospec extension fields ---
slug: robot-space-bots-space-isolation
upstream: none (found during an octo-web contacts API usage audit; see space-directory-api brief)
source: user
---

# Task: robot-space-bots-space-isolation

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

`GET /v1/robot/space_bots?space_id=` must reject callers who are not members of the
requested Space, and must return **zero** rows to them.

Today the route is mounted with `AuthMiddleware` only
(`modules/robot/api.go:332`). The handler (`modules/robot/api.go:1479`) reads
`space_id` straight from the query string and runs its SQL with no membership check
anywhere in the path. Any authenticated user of the platform can pass an arbitrary
`space_id` and receive that Space's complete bot roster: `uid`, `name`,
`description`, `creator_uid`, `creator_name`, `bot_commands`, `auto_approve`.

This is a cross-tenant read, i.e. the P0 class named in the repo's
`space-isolation` rule ("a missing or fail-open check is a cross-tenant data leak").

The fix is small and mechanical: mount `spacepkg.SpaceMiddleware(ctx)` on the route.
The middleware already resolves `space_id` from the query string
(`pkg/space/middleware.go:108`), which is exactly how this endpoint receives it, so
no handler signature or response shape changes.

## Background

- Route: `modules/robot/api.go:332`, inside
  `auth := r.Group("/v1", rb.ctx.AuthMiddleware(r))`. No `SpaceMiddleware`, no
  `SharedUIDRateLimiter`.
- Handler: `modules/robot/api.go:1479` `spaceBots`. Validates only that `space_id`
  is non-empty (`respondRobotRequestInvalid`), then queries
  `space_member sm INNER JOIN user u (u.robot=1) INNER JOIN robot r (r.status=1)
  WHERE sm.space_id=? AND sm.status=1 AND sm.uid != 'botfather'`.
- `modules/robot/space_inject.go:100` already records that legacy `/v1/robot/...`
  routes have no `SpaceMiddleware` and no Space context — this task closes that gap
  for the one route among them that takes an explicit `space_id`.
- Contrast: the sibling roster endpoint `GET /v1/space/:space_id/members`
  (`modules/space/api.go:617`) *does* check, via `s.db.queryMember(spaceId,
  loginUID)`, returning `ErrSpaceNotMember`. Two endpoints reading the same
  `space_member` table currently enforce different isolation strength.
- Existing coverage is effectively nil: the only test naming this endpoint,
  `TestSpaceBots_ExcludesDeletedSpaceMembers` (`modules/robot/api_test.go:199`), is
  `t.Skip`ped pending octo-server#17.
- Known consumers, all of which already pass the caller's *current* Space and are
  therefore unaffected by the fix:
  - `packages/dmworkcontacts/src/Contacts/index.tsx:367` and `:969` (contacts AI tab)
  - `packages/dmworkbase/src/Pages/BotStore/index.tsx:62` (bot store)
  - `packages/dmworkbase/src/Components/PersonaSettings/vm.tsx:198` (persona picker)
  - `packages/docs/src/octoweb/index.ts:218` and
    `packages/docs/src/pages/DocsHome.tsx:1385` (docs bot-name backfill / badging)
  - the mobile app (same endpoint, per the contacts API review)

## Load-bearing list

- **space**, **isolation**, **auth**, **acl** — this IS the isolation boundary being
  repaired. After the change, Space membership must be a precondition for every row
  returned; there must be no fail-open branch on cache miss or Redis error.
  (rules: space-isolation)
- **bot-api** — the payload carries bot ownership metadata (`creator_uid`,
  `creator_name`) and operational metadata (`bot_commands`, `auto_approve`).
  Ownership validation semantics inside the Space are unchanged; only the Space
  gate is added. (rules: space-isolation)
- **wire-contract** — `spaceBots` is consumed by five octo-web call sites and the
  mobile app. For a legitimate caller (a member of the Space they are asking about)
  the response must be **byte-identical** to today: same fields, same ordering
  (`u.created_at DESC`), same `status` derivation, same `botfather` exclusion. The
  only behavioral delta is that non-members now get an error instead of data.
- **error-response**, **i18n** — the rejection travels through the middleware, which
  currently answers with `c.AbortWithStatusJSON(http.StatusForbidden, ...)`
  (`pkg/space/middleware.go:126,148`) — a raw response that predates the i18n
  envelope and is shared by every route already mounting the middleware. Changing it
  is **out of scope** here (see below), but the new test must assert on the resulting
  wire shape so a later i18n migration has a pinned baseline. (rules: error-handling)
- **rate-limit** — the route currently has no per-UID limiter. It is an enumerable
  list endpoint; mounting `appwkhttp.SharedUIDRateLimiter(r, rb.ctx)` after
  `AuthMiddleware` is in scope. Tests hitting the route must reset
  `ratelimit:uid:*` in setup. (rules: rate-limit)
- **commit** — English Conventional Commits; `fix(robot):`. (rules: commit-style)

## Out of scope

- **Any change to the response payload, SQL, ordering, or `status` derivation.** This
  task adds a gate; it does not touch what a permitted caller sees.
- **Migrating `spaceMiddleware`'s 401/403 responses to `httperr.ResponseErrorL`.**
  Those are shared by every route mounting the middleware, so the migration is its
  own task with its own blast radius. Pin the current shape in a test here.
- **The `c.Param("space_id")` lookup source** being added to `spaceMiddleware` by the
  `space-directory-api` task. This endpoint takes `space_id` from the **query**
  string, which the middleware already reads, so the two tasks do not depend on each
  other and may land in either order.
- **`GET /v1/robot/my_bots`.** It is friend-dimension: rows are scoped by
  `f.uid = loginUID`, so a caller can only ever see their own relationships. Its
  optional `space_id` narrows that set further and cannot widen it.
- **Other legacy `/v1/robot/...` routes** flagged in `space_inject.go:100`. They do
  not accept an explicit `space_id` and need separate analysis.
- **The 60-second revocation window.** `SpaceMiddleware` caches a positive
  membership verdict in Redis for 60s (`pkg/space/middleware.go:17`), and
  `InvalidateMembershipCache` has **no production caller anywhere in the repo** —
  `event.SpaceMemberCacheInvalidator`, which member removal does fire, targets
  the unrelated in-process cache in `modules/notify`. So a user removed from a
  Space keeps access to this endpoint for up to 60s if they touched any
  SpaceMiddleware-mounted route just before removal. That is a pre-existing gap in
  the shared middleware, not something this task introduces, and wiring the
  invalidation into `removeMembers` / `leaveSpace` / disband touches every
  consumer of that middleware. Recorded here so it is not mistaken for covered:
  `TestSpaceBots_RemovedMemberLosesAccess` calls `InvalidateMembershipCache`
  by hand, so it verifies that the verdict flips once the cache is cleared, NOT
  that production clears it.
- **Un-skipping `TestSpaceBots_ExcludesDeletedSpaceMembers`** (blocked on
  octo-server#17). New tests here must stand on their own rather than depend on
  that fixture being revived.
- **Any octo-web or mobile change.** No client change is required.

## Acceptance

- `go test ./modules/robot/...` and `go test ./pkg/space/...` pass;
  `golangci-lint run ./...` clean; `make i18n-extract-check` and `make i18n-lint`
  pass.
- New tests in `modules/robot/` cover:
  1. **Non-member is refused.** Viewer A, authenticated, with no `space_member` row
     in Space S, requests `?space_id=S` and receives an error **and zero rows** —
     asserted on the response body, not only the status code.
  1b. **Disabled Space now refuses members too.** `CheckMembership` additionally
     requires `space.status = 1`, which the handler never checked, so a member of
     a disbanded Space goes from 200 + bot list to 403. This is a correct
     tightening but it IS a behaviour change for a legitimate caller, so it is
     recorded here rather than left to surface in production.
  2. **Member is unaffected.** Viewer B, a member of S, receives exactly the rows
     they receive today: same field set, same `u.created_at DESC` order, `botfather`
     absent, and `status` still viewer-scoped (`added` / `pending` / `not_added`).
  3. **Removed member loses access.** A viewer whose `space_member.status` is set to
     0 for S is refused on the next call — i.e. the membership cache TTL does not
     leave an indefinite window. (Assert against the cache's documented TTL
     behaviour; do not assert instant revocation if the cache does not provide it.)
  4. **Empty `space_id` still yields the existing request-invalid business error**,
     not a middleware 403, so the current client-facing shape for that case is
     unchanged.
- The route declaration at `modules/robot/api.go:332` mounts, in order,
  `AuthMiddleware` → `SharedUIDRateLimiter` → `SpaceMiddleware`.
- ~~`modules/robot/swagger/api.yaml` documents the `403` response for
  `space_bots`.~~ **Not done.** That file has no `/robot/space_bots` entry at all
  today, so satisfying this would mean authoring the endpoint's full spec — new
  documentation for a deprecated endpoint, which cuts against freezing it. Left
  undone deliberately; the deprecation notice on the route points readers at
  `GET /v1/space/{space_id}/directory`, which IS specified (OpenAPI 3.0.3).
- Diff to `modules/robot/api.go` `spaceBots` body (lines 1479+) is limited to
  nothing, or to comments; the SQL and response construction are untouched.
