---
type: Journal
title: "Journal: appbot-token-revocation-redis (octo-server #309)"
description: Record of replacing the per-process App Bot auth registry with a shared Redis write-through cache so token revocation propagates across replicas.
tags: ["auth", "bot-api", "space", "isolation", "multi-instance", "redis"]
timestamp: 2026-06-25T02:00:00Z
# --- octospec extension fields ---
task: appbot-token-revocation-redis
upstream: mininglamp-oss/octo-server#309
source: self
---
# Journal: appbot-token-revocation-redis (octo-server #309)

## What was done

App Bot API auth resolved a presented token to `{UID, Scope, SpaceID}` through a
**per-process in-memory map** (`bot_api.AppBotRegistryAdapter`). Revocations
(rotate / unpublish / delete) only mutated the registry of the instance that
handled the admin request, and on the auth path an in-memory hit short-circuited
the authoritative DB check — so under multiple replicas a revoked token kept
authenticating on every peer until that peer restarted (#309, reproduced
end-to-end in the multi-instance audit).

Replaced the in-memory registry with a **shared Redis write-through cache**
(`RedisAppBotRegistry`, new `modules/bot_api/registry_redis.go`), chosen as
Option A by the maintainer:

- `AppBotRegistryInterface` was widened from `FindByToken` to also include
  `Add/Remove/Update` so the app_bot admin helpers drive the registry through the
  interface (the in-memory `AppBotRegistryAdapter` still satisfies it and is kept
  for unit tests).
- `FindByToken` does `GET appbot:auth:{token}`; a miss (`redis.Nil`) **or any
  Redis error** returns nil so `authAppBot` falls through to the existing
  authoritative DB lookup — auth fails safe, never serves a stale/revoked spec on
  a degraded backend.
- `Add/Remove/Update` write-through to the shared store, so a revoke (DEL) is
  visible to every replica immediately. The DB write in the admin handler stays
  the source of truth; cache writes are best-effort.
- `authAppBot`'s DB-fallback success path now write-through repopulates the cache
  (it previously didn't), so an active token returns to O(1) after a miss while
  only ever caching a DB-confirmed valid+published spec.
- The safety-net key TTL is read live from **system_settings**
  (`AppBotAuthCacheTTLSeconds`, category `app_bot`, default 300s, clamped
  [30, 86400]) — no new env var; injected into bot_api as a
  `func() time.Duration` so bot_api stays decoupled from modules/common.

## Verification

- New regression test `modules/bot_api/registry_redis_multiinstance_test.go`:
  - `TestAppBotTokenRevocationPropagatesToPeer` — two `RedisAppBotRegistry` over
    one Redis + DB; after a rotate on replica A, the peer replica B **rejects**
    the revoked old token (shared DEL → DB fallback → gone) and **accepts** the
    new token (shared hit). PASS.
  - `TestAppBotAuthFailsSafeWhenRedisDown` — registry pointed at a dead Redis:
    `FindByToken` safely misses, a valid token still auths via DB fallback, an
    unknown token is rejected. PASS.
- Gates: `go build ./...` ok; `golangci-lint` 0 issues; `make i18n-extract-check`
  + `make i18n-lint` clean (no new error codes / raw responses); existing
  bot_api adapter unit tests pass.

## Learnings / decisions

- **Fail-safe direction matters for an auth cache.** A cache backend error must
  degrade to the authoritative DB (return nil → fallback), never fail open. This
  is the load-bearing space-isolation/auth invariant for the whole change.
- **Documented bounded-staleness residual** (mirrors
  `modules/incomingwebhook/cache.go`): an auth miss that read the DB as valid
  immediately before a concurrent revocation can re-populate the just-deleted
  key; it expires within the safety-net TTL. Instant revocation still holds for
  the overwhelming common case via the shared DEL; the TTL also self-heals a
  failed DEL.
- **Out of scope (follow-up):** the app_bot internal `ab.registry`
  (`byUID`/`byID`, used by `user.SetAppBotResolver` for display-name resolution)
  has the same cross-instance staleness but only affects a cosmetic display name,
  not auth — left for a separate change.
