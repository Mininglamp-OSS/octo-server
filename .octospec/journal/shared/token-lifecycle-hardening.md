---
type: Journal
title: "Journal: token-lifecycle-hardening (PR 1)"
description: Record of bounding new and touched user HTTP-token lifetimes while preserving mixed-version rollout safety.
tags: ["auth", "security", "redis", "session", "migration", "observability"]
timestamp: 2026-08-09T18:42:28+08:00
# --- octospec extension fields ---
task: token-lifecycle-hardening
source: self
---
# Journal: token-lifecycle-hardening (PR 1)

## What was done

This first delivery stops the server from creating or accidentally restoring an
unbounded user HTTP bearer while retaining the existing opaque-token client
contract:

- Kept the existing `cache.tokenExpire` / `TS_CACHE_TOKENEXPIRE` entry point,
  made explicit values fail loudly unless they are valid positive Go durations,
  and capped them at 720h. Empty-value detection is scoped to the one env key and
  does not mutate shared Viper semantics.
- Added one process-wide Session Store and bounded Redis pool for authentication.
  Writers issue finite credentials, preserve the existing deadline on reuse or
  profile updates, compensate failed new issuance, and never recreate a bearer
  deleted by a concurrent replica.
- Added a per-issue ownership marker inside the versioned Token payload. Reuse
  atomically removes that marker on the existing single Token key, while failed
  issuance compensation compare-deletes only the still-owned payload. A stalled
  replica therefore cannot revoke the same Token after another replica has
  successfully adopted it; no process lock, extra Redis key, or auth-hot-path
  write was introduced.
- Replaced split `GET`/TTL assumptions with one single-key Lua read. API replicas
  execute the real read script as a read-only startup probe and refuse to serve
  if the configured Redis path cannot execute it.
- Routed explicit token readers through the canonical validator. Release A keeps
  legacy v1/v2 compatibility, while any v3 value is immediately subject to a
  finite Redis TTL, absolute expiry, and session-generation validation.
- Added low-cardinality session metrics and a separate, rate-limited, read-only
  observation tool. It reports aggregate migration facts without emitting
  bearer values, Redis keys, or UIDs.
- Added repository-wide AST guards so production token reads/writes cannot
  bypass `pkg/auth`; direct `Set`, `SetAndExpire`, `SetNX`, `Persist`, `Expire`,
  and `ExpireAt` calls using token prefixes are rejected outside the store.

## Security and rollout boundary

PR 1 is a stop-the-bleeding release, not final vulnerability closure. Historical
persistent v1/v2 sessions remain readable until an approved migration handles
them, and v2 has no payload-level absolute deadline. PR 2 must add guarded v3
issuance, generation/session indexes, the high-risk-event revocation matrix, and
migration apply/enforce. The finding can close only after production observation,
controlled migration, old-replica removal, and security retest.

The change does not alter the token header, login response shape, or database
schema and does not run a bulk expiry during deployment. Intentional operational
changes are fail-loud configuration validation, fail-closed Lua startup probing,
non-sliding Web/PC token reuse, APP bearer replacement, and an additional bounded
Redis pool whose capacity must be budgeted across replicas.

## Verification

- `go build ./...` — pass.
- `go vet ./...` — pass.
- `golangci-lint run ./...` — 0 issues.
- `go test . ./pkg/auth ./pkg/metrics -count=1` — pass.
- Focused `modules/user` writer, login, Redis, and HTTP TTL tests — pass.
- `make i18n-extract-check` and `make i18n-lint` — pass.
- Two independent Redis clients/Session Stores reproduce the issue-adopt-fail
  ordering and prove compensation preserves the adopted Token and device index.
- `go test -race -count=1 ./pkg/auth/...` — pass; auth coverage 87.2%.

A broad user-package regex initially selected an unrelated scan-login test and
hit the shared MySQL migration drift (`group` table already present without the
matching migration state). Re-running the exact token-lifecycle targets passed.
The shared database was not cleaned or modified to hide that environment issue.

## Learnings and follow-ups

- Detecting an explicitly empty env override must not change global configuration
  semantics. Read the one security-sensitive env key directly and test unrelated
  empty and non-empty overrides before and after validation.
- Authentication Lua remains one Redis round-trip in steady state and does not
  write on every request. Before rollout, verify `EVALSHA`/`EVAL` through the
  production endpoint and budget Session Store `PoolSize x replica count`
  against Redis `maxclients`.
- A Token string or payload equality is not sufficient issue-attempt ownership:
  Web/PC login intentionally reuses the same Token. Ownership must be represented
  in shared Redis state and removed atomically when another request adopts it;
  compensation then uses exact compare-delete rather than unconditional `DEL`.
- Web/PC explicit login currently reuses the bearer without extending its
  deadline. Product approval is required because a near-expiry bearer can still
  expire shortly after a successful login.
- PR 2 must preserve v3 security claims when updating display snapshots and must
  expose incomplete migration scans explicitly rather than silently treating
  Redis read failures as complete data.
