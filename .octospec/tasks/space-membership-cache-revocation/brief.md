---
type: Task
title: "Task: space-membership-cache-revocation"
description: Invalidate the Redis membership cache when a user leaves or is removed from a Space, so revocation stops being bounded only by a 60s TTL that was never chosen as a security SLA.
tags: ["space", "isolation", "auth", "acl", "testing", "commit"]
timestamp: 2026-08-03T00:00:00Z
# --- octospec extension fields ---
slug: space-membership-cache-revocation
upstream: none (found by code review of space-directory-api / robot-space-bots-space-isolation)
source: user
---

# Task: space-membership-cache-revocation

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Removing a user from a Space must revoke their access to every
`SpaceMiddleware`-gated endpoint **on the next request**, not up to 60 seconds
later.

Today `SpaceMiddleware` caches a positive membership verdict in Redis under
`space:member:{spaceID}:{uid}` for 60s (`pkg/space/middleware.go` `cacheTTL`), and
`InvalidateMembershipCache` — which exists, right there in the same file — has **no
production caller anywhere in the repo**. `grep -rn InvalidateMembershipCache
--include=*.go` returns the definition and test files only.

So the fix is to call it: on member removal, on self-leave, and on Space disband.

## Background

**Where the 60 seconds came from.** Traced to PR #789
(`feat(space): add Space isolation middleware with membership cache`, commit
`043d2812`). The cache exists for throughput, not for security: the middleware is
mounted on hot paths (`/v1/conversation/sync`, `/v1/message/*`, `/v1/search`,
`/v1/sidebar/*`), and without it every one of those requests pays a
`space_member ⋈ space` query. The number moved twice during that PR's review:

1. first cut — 5 minute in-process cache;
2. review round 1 — cleanup goroutine added, **negative** TTL cut to 30s, with the
   stated reason "so new members aren't blocked";
3. review round 2 — `sync.Map` replaced with Redis, positive 60s / negative 30s.

Every step reasoned about how fast a **new** member gets in. Nobody asked how long a
**removed** member stays in, and the invalidation helper was written but never wired.
The TTL became a de-facto revocation SLA by accident.

**Why it surfaced now.** `GET /v1/space/{space_id}/directory` (task
`space-directory-api`) is the first user-facing roster endpoint to sit behind this
middleware. It replaces `GET /v1/space/:space_id/members`, which does an uncached
per-request `queryMember` and therefore revokes **immediately**. So for that
endpoint the migration is a net loosening, and the exposure is wider: `listMembers`
hides bots the caller did not create, while the directory returns every bot in the
Space. `GET /v1/robot/space_bots` (task `robot-space-bots-space-isolation`) inherits
the same window.

**What is NOT the mechanism.** Member removal already fires
`event.SpaceMemberCacheInvalidator` (`modules/space/api.go`), which
`modules/notify/api.go` binds to `n.memberCache.invalidate(spaceID)`. That is an
unrelated in-process uid-set cache inside the notify module; it does not touch the
Redis key this middleware reads. A reader can easily mistake the existing call for
coverage — it is not.

**Exploit shape.** User U is a member of Space S. U touches any
SpaceMiddleware-mounted route with S in scope (conversation sync alone is enough,
and the octo-web client sends `X-Space-Id` on every request), which writes
`space:member:S:U = 1` with a 60s TTL. An admin removes U. For up to 60 seconds U
can still call `GET /v1/space/S/directory?limit=10000` and receive the full roster
including every bot.

## Load-bearing list

- **space**, **isolation**, **auth**, **acl** — this is the revocation path of the
  multi-tenant boundary. The fix must be fail-safe in the direction that matters: a
  failed invalidation should not silently leave the stale grant in place, so log at
  WARN rather than swallowing the error. (rules: space-isolation)
- **`pkg/space/middleware.go` semantics** — `cacheTTL` stays 60s; this task adds
  explicit invalidation, it does not shorten the TTL. Shortening it would tax every
  hot path for a case that explicit invalidation handles precisely. Say so, because
  "just lower the TTL" is the obvious wrong fix.
- **every SpaceMiddleware consumer** — channel, user pinned/space, search, message,
  sidebar, reactions, conversation, follow, messages_search, plus the two new
  endpoints. The change only makes revocation faster, so it is safe for all of them,
  but the blast radius should be stated in the PR.
- **testing** — the current coverage actively misleads. In
  `modules/robot/api_space_bots_isolation_test.go`,
  `TestSpaceBots_RemovedMemberLosesAccess` calls `InvalidateMembershipCache` **by
  hand** before re-checking, so it proves the verdict flips once the cache is
  cleared, not that production clears it. Once this task lands, that hand call must
  be deleted so the test exercises the real path. (rules: testing)

## Out of scope

- **Changing `cacheTTL` or `negativeCacheTTL`.** See above.
- **Migrating the notify module's `memberCache`** or reconciling the two caches into
  one. They answer different questions.
- **Role changes.** Demoting an admin to member does not change membership, so the
  cached boolean stays correct. Only the endpoints' own role checks matter there.
- **Space ban (`space.status = 2`) and disband via any path that does not go through
  the handlers listed in Acceptance.** `CheckMembership` already joins
  `space.status = 1`, so a disbanded Space fails the DB check — but only after the
  cached positive expires. Disband is in scope; a full audit of every status
  transition is not.
- **Bot removal from a Space.** Bots do not call these endpoints.

## Acceptance

- `spacepkg.InvalidateMembershipCache` is called for every removed uid in:
  `removeMembers` (user-facing), `leaveSpace`, the manager-side force-remove
  (`modules/space/api_manager.go`), and `disbandSpace` (all members).
  A source guard or test enumerates these call sites so a future removal path cannot
  quietly skip it.
- Invalidation failures are logged at WARN with `space_id` and `uid`, never
  swallowed. A stale grant that nobody can see is the failure mode this task exists
  to remove.
- New tests in `modules/space/`:
  1. member calls a SpaceMiddleware-gated endpoint (warming the cache) → is removed →
     **next** call is refused, with **no** manual cache clearing anywhere in the test;
  2. same for self-leave;
  3. same for disband, asserting every member loses access, not just the actor;
  4. a member who was never removed still hits the cache — i.e. the fix did not
     degrade into "invalidate everything on every write". Assert the checker is not
     re-invoked, in the style of `TestSpaceMiddleware_CacheHit`.
- `TestSpaceBots_RemovedMemberLosesAccess` drops its manual
  `InvalidateMembershipCache` call and still passes.
- The `space-directory-api` and `robot-space-bots-space-isolation` briefs have their
  "60-second window" out-of-scope entries updated to point here as resolved.
- `go test ./modules/space/... ./modules/robot/... ./pkg/space/...` passes;
  `golangci-lint run ./...` clean; `make i18n-extract-check` and `make i18n-lint` pass.
