---
type: Journal
title: "Journal: token-lifecycle-hardening PR 2"
description: Record of the guarded v3 session rollout, durable revocation, and controlled legacy-token migration implementation.
tags: ["auth", "security", "redis", "session", "migration", "observability"]
timestamp: 2026-08-10T01:17:49+08:00
# --- octospec extension fields ---
task: token-lifecycle-hardening-pr2
source: self
---

# Journal: token-lifecycle-hardening PR 2

## What was done

- Added a startup-fixed, default-`expand` rollout mode and an operator-only,
  monotonic Redis floor. No deployment automatically enables v3, migration
  apply, or legacy enforcement.
- Added v3 payload absolute expiry, random session generations, issuance
  fences, deadline-preserving reuse/snapshot updates, and a bounded session
  index. Authentication reads the Token key and generation sequentially through
  the existing shared Session Store pool and performs no steady-state write.
- Scoped indexes by UID and generation. Revocation Lua records the generation
  replaced by an event, so first application and exact replay clean only that
  old index; a post-event login cannot be removed from the cap by an old event.
- Added transactional MySQL revocation intents for active password changes and
  resets, account disable/final destruction, and administrator deletion. API
  paths apply synchronously; a leased, multi-replica worker retries pending
  intents idempotently without logging credentials or raw UIDs.
- Added a default-dry-run admin tool for monotonic floor changes and fixed-cutoff
  legacy migration. Apply requires an explicit campaign, cutoff, batch size,
  QPS, lease, and persisted revoke floor; it uses a separate two-connection
  pool, owner lease, aggregate checkpoint, and single-key shorten-only Lua.
- Revalidated passwords and authoritative account state after the issuance
  fence across local, manager, external, OAuth, scan, and device-verification
  paths. A review-found gap in manager status handling was fixed so disabled or
  destroyed administrators cannot immediately obtain a new v3 bearer.

## Security and rollout boundary

The code remains inert in `expand`. Production closure still requires Redis
topology/Lua/SCAN/lease and non-eviction checks, connection and two-read auth
capacity budgets, an approved session cap and migration cutoff, old-replica
retirement at every phase, apply plus two complete observations, enforce
canarying, and security retest.

`/v1/user/pc/quit`, device deletion, and OIDC sync `invalid_grant` still need a
signed device-scope policy. The implementation deliberately does not claim that
matrix is complete or widen those operations without a stable device identity.

## Verification

- `go test ./pkg/auth -count=1` and `go test -race ./pkg/auth/... -count=1` — pass.
- `GIN_MODE=release go test ./modules/user -count=1` — pass against local
  MySQL, Redis, and WuKongIM (31.781s final run).
- `GIN_MODE=release go test ./modules/oidc -count=1` — pass against an isolated
  temporary `test` schema; the original local schema was backed up and restored
  automatically (13.038s final run).
- `go test ./tools/token-session-admin ./pkg/metrics -count=1` — pass.
- `go build ./...`, `go vet ./...`, `golangci-lint run ./...`,
  `make i18n-extract-check`, and `make i18n-lint` — pass.
- `go test ./... -count=1` — not green. Unchanged `internal/msgextraseq` held
  the shared `test` database until the default 10-minute timeout; concurrent
  integration packages then failed with MySQL 1205 lock waits. Relevant auth
  packages passed in that run, and focused integration was rerun separately.

## Learnings and follow-ups

- A monotonic generation protects bearer validity but does not protect a shared
  mutable index from stale cleanup. Cleanup ownership must carry the revoked
  generation so retry and post-event issuance commute safely.
- A credential fence must precede the authoritative credential/account read;
  merely rereading a password while omitting status fields leaves a disabled
  management account able to log in again.
- The rollout floor prevents phase rollback but cannot prove Kubernetes replica
  retirement or production capacity. Those remain explicit go/no-go checks.

