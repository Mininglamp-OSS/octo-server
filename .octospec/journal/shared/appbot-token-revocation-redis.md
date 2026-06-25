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
  (`AppBotAuthCacheTTLSeconds`, category `app_bot`, default 60s, clamped
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

## Review hardening (PR #458)

Four reviewer approvals surfaced fixes folded into the same PR (no behaviour
change to the fix's contract; all reduce blast radius / tighten the staleness
window):

- **Token never stored verbatim in a Redis key.** `appBotAuthKey` now hashes the
  bearer token with SHA-256 (`appbot:auth:{sha256hex(token)}`) so a live
  credential can't leak via `KEYS`/`MONITOR`/RDB dumps. The hash is stable, so
  every replica still derives the same key.
- **Default safety-net TTL lowered 300s → 60s** (both `defaultAppBotAuthCacheTTL`
  in registry_redis.go and `defaultAppBotAuthCacheTTLSeconds` in
  modules/common, kept in sync). Tightens the worst-case window for a failed DEL
  or the re-populate race from 5 min to 1 min; operators can still retune via
  `app_bot.auth_cache_ttl_seconds` (clamped [30, 86400]).
- **The TTL knob is now registered in `systemSettingSchema`** (category `app_bot`,
  `Positive: true` to opt out of the day-window [0,3650] int bound) so the admin
  API accepts writes to it instead of rejecting an unknown key.
- **DB-fallback write-through Add is fire-and-forget** (`go reg.Add(...)`). The DB
  lookup already produced the authoritative answer; the cache warm-up must not
  block the auth response on a Redis SET — critical when Redis is degraded (the
  exact condition that drove the fallback), where an inline SET would add the
  client write-timeout to every request.
- **Global registry slot switched to `atomic.Pointer[regHolder]`** so it accepts a
  nil/clearing Store and differing concrete types — `atomic.Value` type-locked the
  slot to the first implementation stored, which prevented a test from restoring
  the previous (possibly nil) registry. Lock-free reads on the hot path preserved.
- **Test hygiene:** the multi-instance test now `t.Cleanup`-restores the global
  registry (no leak into sibling tests) and bounds its `*config.Context` pool
  (`MySQLMaxOpenConns=2 / MaxIdleConns=1`) so it stops aggravating the parallel-CI
  "Error 1040: Too many connections" flake (#419).

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
