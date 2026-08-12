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

> [!IMPORTANT]
> **Historical #725 record; rollout control was superseded by PR #733.** Redis floor, startup-fixed
> MODE, REQUIRED_FLOOR and the old standalone tools below are no longer the active design. The
> current implementation uses one MySQL `floor/max_per_uid/version/paused` authority; see
> [`token-session-rollout-simplify`](../../tasks/token-session-rollout-simplify/brief.md) and the
> [current runbook](../../../docs/token-session-rollout-runbook.md).

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
- Closed the review fault paths without adding replica-local state: v3 issue
  compensation removes only its owned generation-scoped index member; disabled
  accounts cannot redeem an outstanding scan code; synchronous revocation uses
  the same owner-lease invariant as workers; committed account destruction and
  ordinary logout still attempt IM/device cleanup when Redis is degraded.
- Namespaced migration state by campaign, made rate knobs resumable, required an
  explicit `natural` or `cap` finite-token policy, and added persistent aggregate
  evidence gates before bounded/enforce floor transitions. The repository writer
  guard now covers all security-key families, and a command-count test pins v3
  steady-state validation to two Redis reads and zero writes.
- Hardened the local re-review findings without replica-local coordination. An
  elapsed campaign cutoff now classifies dry-run `would_delete` separately from
  apply `deleted` while preserving the approved absolute deadline; new
  revocation intents reserve a five-second MySQL visibility window for the
  synchronous by-ID claimant before workers take over; rollout control persists
  an explicit minimum observation gap that later floor transitions must satisfy.
- Made those rollout gates forward compatible without Redis surgery. Observation
  gaps have a hard one-hour floor, may only increase between phases, and validate
  evidence against the newly approved value. A legacy control record without the
  gap remains readable and the next monotonic floor CAS backfills it. Applying an
  elapsed migration cutoff now requires explicit confirmation in both the CLI
  parser and Session Store API before any remaining records can be deleted.
- Closed the remaining mid-run cutoff window. The single-key migration Lua now
  refuses its first immediate deletion with a distinct action when an
  unconfirmed apply crosses the immutable cutoff during a throttled scan. The
  store returns a resumable error without completion evidence; an explicitly
  confirmed rerun can then delete the remaining records under the same campaign.
- Added `docs/token-session-rollout-runbook.md` with exact environment pairing,
  Redis/non-eviction and connection preflight, phased commands, old-replica
  checks, stop conditions, and the irreversible rollback boundary.

## Security and rollout boundary

The code remains inert in `expand`. Production closure still requires Redis
topology/Lua/SCAN/lease and non-eviction checks, connection and two-read auth
capacity budgets, an approved session cap and migration cutoff, old-replica
retirement at every phase, apply plus two complete observations, enforce
canarying, and security retest.

Finite legacy handling is now an explicit campaign decision: `natural` preserves
already-bounded finite deadlines while still bounding persistent/over-max keys;
`cap` also compresses finite deadlines to the cutoff and can force earlier
re-login. The code supports both but does not approve either for production.

`/v1/user/pc/quit`, device deletion, and OIDC sync `invalid_grant` still need a
signed device-scope policy. The implementation deliberately does not claim that
matrix is complete or widen those operations without a stable device identity.

## Verification

- `go test -race ./pkg/auth/... -count=1` — pass (4.330s); the additional
  finite-policy boundary test also passed after adding the over-max case. After
  the local re-review hardening, the same command passed again (4.049s),
  including elapsed-cutoff deletion and observation-gap regressions. After the
  forward-compatibility fixes, it passed again (4.768s), including legacy
  control backfill, monotonic gap increases, the one-hour floor, and elapsed
  cutoff confirmation. After closing the mid-run cutoff window, it passed again
  (4.533s); the new Redis regression also passed ten consecutive focused runs.
- `GIN_MODE=release go test ./modules/user -count=1` — pass against local
  MySQL, Redis, and WuKongIM (57.932s final re-review run), including the new
  synchronous-claim priority-window integration test.
- `GIN_MODE=release go test ./modules/oidc -count=1` — the shared `test` schema
  first failed before assertions because of an unrelated stale migration ID;
  the package passed against a fresh temporary `test` schema again after the
  re-review changes (18.153s). The original schema was restored from a
  checksum-verified dump with the same 118 tables and 190 migration records;
  before/after dump SHA256 matched, then the temporary database and dumps were
  removed.
- `go test ./tools/token-session-admin ./tools/token-session-observe ./pkg/metrics
  -count=1` — pass after the re-review changes (`token-session-observe` has no
  test files; its shared store behavior is covered in `pkg/auth`). It passed
  again after the forward-compatibility fixes (`token-session-admin` 0.418s,
  `pkg/metrics` 0.699s), and after the mid-run cutoff fix
  (`token-session-admin` 0.704s, `pkg/metrics` 0.447s).
- `go build ./...`, `go vet ./...`, `golangci-lint run ./...`,
  `make i18n-extract-check`, and `make i18n-lint` — pass again at the
  mid-run cutoff head; golangci-lint reported zero issues.
- `go test -cover ./pkg/auth -count=1` — pass with 74.8% package statement
  coverage. The repository has no 80% coverage gate; this journal does not
  claim one, while the new destructive branch is directly exercised by the
  RED-to-GREEN Redis regression above.
- `go test ./... -count=1` was not rerun at the final documentation/policy head.
  The earlier PR-head attempt was not green: unchanged `internal/msgextraseq`
  held the shared `test` database until the default 10-minute timeout, then
  concurrent packages failed with MySQL 1205 lock waits. Relevant packages were
  rerun serially above; this journal does not claim an all-package pass.

## Learnings and follow-ups

- A monotonic generation protects bearer validity but does not protect a shared
  mutable index from stale cleanup. Cleanup ownership must carry the revoked
  generation so retry and post-event issuance commute safely.
- A credential fence must precede the authoritative credential/account read;
  merely rereading a password while omitting status fields leaves a disabled
  management account able to log in again.
- The rollout floor prevents phase rollback but cannot prove Kubernetes replica
  retirement or production capacity. Those remain explicit go/no-go checks.
