---
type: Task
title: "Task: appbot-token-revocation-redis"
description: Propagate App Bot token revocation across instances via a shared Redis write-through auth cache
tags: [auth, bot-api, space, isolation, multi-instance]
timestamp: 2026-06-25T01:52:00Z
# --- octospec extension fields ---
slug: appbot-token-revocation-redis
upstream: mininglamp-oss/octo-server#309
source: self
---

# Task: appbot-token-revocation-redis

## Goal

App Bot API authentication resolves a presented token to `{UID, Scope, SpaceID}`
through a **per-process in-memory registry** (`bot_api.AppBotRegistryAdapter`).
Token revocation operations (rotate / unpublish / delete) only mutate the
registry of the **instance that handled the admin request**; there is no
cross-instance propagation and no periodic reload, and on the auth path an
in-memory hit short-circuits the authoritative DB check. Under multiple replicas
a revoked App Bot token therefore keeps authenticating on every peer replica
until that replica restarts (a security-relevant staleness — see #309, which has
an end-to-end reproduction).

Replace the per-process in-memory auth registry with a **shared Redis
write-through cache** (Option A, chosen by maintainer): the fast-path lookup and
all mutations go through one shared Redis store, so rotate/unpublish/delete take
effect **instantly on all replicas**. The DB (`app_bot` table via
`queryAppBotByToken`, with the `status=1` published gate) remains the
authoritative fallback, so a Redis outage degrades to a correct-but-slower DB
lookup rather than failing closed or serving stale auth.

## Background

- Issue: mininglamp-oss/octo-server#309 (filed from this multi-instance audit;
  includes an end-to-end reproduction showing a revoked token still authorized on
  a peer instance).
- Defect site: `modules/bot_api/auth.go:authAppBot` (in-memory hit returns before
  the DB fallback at lines ~80-104), `modules/bot_api/registry.go`
  (`AppBotRegistryAdapter` = in-memory `map[string]*AppBotRegistrySpec`, mutated
  locally only), `modules/app_bot/app_bot.go` (`syncAuthRegistry` /
  `removeAuthRegistry` / `updateAuthRegistry` write the local process only;
  `loadRegistryFromDB` runs once at startup).
- DB fallback already exists and is authoritative; the availability direction
  (new/rotated tokens reaching peers) already works via that fallback. This task
  fixes the **revocation direction** (stale tokens lingering on peers).
- Redis client is built the same way `modules/opanalytics/etl_lock.go` does:
  `rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), ...))`
  (`pkg/redis/options.go`).
- Config knob (cache TTL safety-net) goes through `modules/common/system_settings.go`
  (DB-backed, hot-reloaded every ~60s, typed getters), **not** a new env var.

## Load-bearing list

- `auth` — App Bot API authentication decision (token → identity); security
  boundary. Must fail safe (Redis error → authoritative DB fallback, never
  serve-stale-on-error or fail-open on a revoked token).
- `bot-api` — `bot_api.authAppBot` / `AppBotRegistryInterface` /
  `GetAppBotRegistry` / `SetAppBotRegistry` contract; bot ownership/identity.
- `space` / `isolation` — the cached spec carries `Scope` + `SpaceID`, which gate
  space-scoped App Bot send permission downstream; a stale/incorrect spec must
  not leak a bot across spaces. Cache value must stay consistent with the DB row.
- `error-response` — `modules/bot_api/auth.go` is touched; auth failures must keep
  using the existing i18n helpers (`respondBotAPIAuthFailed` /
  `respondBotAPIAuthCheckFailed` / `respondBotAPIBotUnavailable`), no raw
  responses, no new per-reason auth codes (anti-enumeration: one generic failure).
- `wire-contract` — the anti-enumeration / fixed auth-failure response shape on
  the bot API must not change.
- App Bot admin handlers (`rotateToken` / `publishBot` / `unpublishBot` /
  `deleteBot` / `updateBot`) and their existing DB-then-registry ordering and
  rollback semantics (DB stays the source of truth; cache write is best-effort).
- `system_settings` config surface (new typed getter + category `app_bot`).
- The existing reproduction in the audit (to be converted into a committed
  regression test).

## Out of scope

- The app_bot module's **internal** registry `ab.registry` (`byUID`/`byID`) used
  by `user.SetAppBotResolver` for display-name resolution — it has the same
  cross-instance staleness but only affects a cosmetic display name, not auth.
  Left as a noted follow-up.
- The other multi-instance findings from the audit (event-timer duplicate,
  read-count aggregator) — tracked separately; not touched here.
- User Bot (`bf_` token / robot table) auth path — unchanged.
- Redis pub/sub or any new background goroutine — the write-through model needs
  neither.
- Changing the bot API auth-failure wire status / error codes.

## Acceptance

- A revoked App Bot token (after rotate / unpublish / delete on one instance) is
  rejected on a **peer instance** that shares the same Redis — asserted by a
  committed regression test using two registry instances + shared Redis + real
  `authAppBot` (the audit reproduction, inverted to assert the fix).
- A newly rotated/published token authenticates on a peer instance (availability
  direction preserved), via Redis hit or DB fallback.
- Redis outage path: `FindByToken` Redis error → returns nil → existing DB
  fallback authenticates a valid token and rejects a revoked one (fail-safe to
  DB, never serve stale on error). Covered by a test.
- Auth failures still render through the existing i18n helpers; no new error
  codes; `make i18n-lint` / `make i18n-extract-check` still pass (expected: no
  code changes needed, confirm clean).
- Cache TTL safety-net is read from `system_settings` (category `app_bot`), no new
  env var; `messageSaveAcrossDevice`-style getter with a code default.
- `go build ./...`, `go test ./modules/bot_api/... ./modules/app_bot/...`, and
  `golangci-lint run ./modules/bot_api/... ./modules/app_bot/...` pass.
- No behavior change to single-replica deployments (registry semantics identical
  from a single process's view; just backed by Redis instead of a local map).
