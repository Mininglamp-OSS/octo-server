---
type: Journal
title: "space-welcome-per-space-admin-crud: per-Space welcome message with admin CRUD"
description: Make the onboarding welcome message per-Space and self-service — space admins (Role>=1) CRUD one config per Space via /v1/space/:space_id/welcome; the platform-global config stays a superadmin fallback. Delivery driver goes single-Space to all-enabled-Spaces.
tags: [notify, space, onboarding, isolation, auth, acl, i18n, error-response, system-setting, migration, rate-limit, idempotency, testing]
timestamp: 2026-07-21T00:52:00Z
---

# space-welcome-per-space-admin-crud

Branch `claude/space-welcome-per-space-admin-crud`. Product follow-up to
#604/#606 (which shipped a single **platform-designated** welcome Space,
superadmin-only). This lifts the origin brief's out-of-scope item
("Multi-Space configuration with per-Space copy; per-Space admin self-service
API") into scope: **one welcome config per Space, managed by that Space's own
admins**, with the global config kept as a superadmin-managed fallback.

Ships per-Space `enabled=false` by default; with no per-Space row written,
behavior is identical to today.

## What was done

1. **Config storage (`modules/common`)** — new `octo_space_welcome_config`
   table (one row per Space; migration under `common/sql/`) + stateless
   `SpaceWelcomeConfigStore` (Get / Upsert / Delete / Exists / ListEnabled).
   `ResolveEffectiveSpaceWelcome` encodes precedence: a **present per-Space row
   wins outright — even when disabled** (lets an admin opt their Space out of a
   global campaign); otherwise the global config applies iff enabled and it
   names the Space; otherwise off. The resolver reuses the existing
   `SpaceWelcomeConfig` shape, so `ValidateSpaceWelcomeCombination` /
   `ParsedActiveFrom` apply unchanged. `active_from` is stored as the RFC3339
   string (not DATETIME) precisely so those validators are reused verbatim.
2. **CRUD API (`modules/space/api_welcome.go`)** — `GET/PUT/DELETE
   /v1/space/:space_id/welcome`. Authz reuses the module's existing
   `checkSpaceActive` + `requireSpaceAdmin` (Role>=1); a request can only ever
   affect the path `:space_id`. `PUT` is a prospective merge (patch onto the
   existing row, validate the composite). `DELETE` hard-deletes the row (reverts
   to the global fallback or off). Mounted on the auth + `SharedUIDRateLimiter`
   group; all errors via `httperr` with **existing** error codes (no new codes).
3. **Delivery driver (`modules/notify`)** — went single-Space →
   all-enabled-Spaces. The event path, reconciler and worker resolve the
   per-Space **effective** config each cycle instead of a single global equality
   filter. Reconciler and worker each rotate a per-replica cursor over the
   enabled-Space set (fairness). Cross-space sweep (`sweepClaimedAll` /
   `sweepDispatchingAll`, backed by a new `idx_sweep (status, claim_expire_at)`)
   reclaims stale in-flight rows across Spaces — including a Space disabled while
   rows were in flight. The ledger state machine, at-most-once semantics,
   `FOR UPDATE SKIP LOCKED` claim, CAS-on-`claim_owner`, and the fixed
   `notification` sender identity are **unchanged**.

## Structural learnings / gotchas

- **Reuse the effective-config shape, not a new type.** By having the per-Space
  resolver return the same `common.SpaceWelcomeConfig` the single-Space code
  already consumed, the entire notify delivery path kept using
  `ValidateSpaceWelcomeCombination` / `ParsedActiveFrom` verbatim — the change
  was "swap the config *source*", not "rewrite the path". Storing `active_from`
  as an RFC3339 string (not DATETIME) was the enabling decision.
- **`modules/common` is the DI hub.** Both `space` (write) and `notify` (read)
  import `common`; `common` imports neither. Putting the shared store there
  avoided a `space`↔`notify` import cycle.
- **Multi-Space fairness needs a rotating cursor, not greedy drain.** First cut
  drained enabled Spaces in `space_id` order; a Space that keeps saturating the
  per-wake cap (20) starves later Spaces forever. The reconciler had a rotation
  cursor; the worker did not. Fixed by mirroring the cursor into the worker
  (commit 741641f) + a regression test that keeps the first Space over-cap and
  asserts the second is still served. See learnings/pending.
- **Cross-space sweep must be owner-agnostic but lease-gated.** `sweepAll` drops
  the `space_id` filter (so a now-disabled Space's stale rows aren't stranded)
  and, like the original, guards only on `claim_expire_at<=now` — it never
  steals a healthy peer's live claim. Needed a `(status, claim_expire_at)` index
  or it degrades to a full scan on the monotonically-growing ledger.
- **Per-Space config is read-through (no snapshot).** Unlike the global
  `SystemSettings` 60s snapshot, an admin PUT/DELETE is visible on the next read
  across replicas immediately — strictly better convergence for the per-Space
  path.
- **WuKongIM verification gotchas (e2e).** `/message/send` returns a `message_id`
  only on accept+persist, but persistence is **async** — an immediate
  `/channel/messagesync` read races the write (poll it). A personal DM is stored
  under `channel_id=<recipient>`, read via `messagesync{login_uid: notification,
  channel_id: <recipient>}` — querying the peer's uid returns empty. The IM data
  dir persists across test runs (only MySQL is reset), so real-wire e2e must use
  run-unique uids.

## Verification

- Gates: `go build ./...`, `go vet`, `golangci-lint` (0 issues), `gofmt`,
  `make i18n-extract-check` + `make i18n-lint`, `git diff --check` — all clean.
- `go test -race` green for `modules/common`, `modules/notify`, `modules/space`
  (each on a fresh DB — cross-package migration sets differ, so the shared
  `test` DB is dropped+recreated between packages).
- New tests: config-store CRUD; effective-config precedence (both directions);
  multi-Space independent delivery + **no cross-Space body/space_id mixing**;
  worker fair-rotation (starves without the fix); cross-space sweep; per-Space
  event enqueue scoping; CRUD authz matrix (admin/member/non-member/inactive) +
  prospective validation + partial-merge + delete.
- **Real-wire e2e** (ad-hoc, not committed) against live MySQL/Redis/WuKongIM:
  single-Space delivery (ledger SENT + real IM message_id) and two-Space
  concurrent delivery, reading each recipient's channel back from WuKongIM to
  confirm actual receipt and that each got ONLY their own Space's body +
  authoritative `payload.space_id`.

## Open items

- DELETE is a hard-delete → revert to global fallback (confirmed default). If a
  Space wants "off" while the global names it, PUT `enabled=false` (a present
  disabled row overrides the global).
- Per-replica `sweepAll` runs every wake (2 indexed UPDATEs / 5s / replica);
  bounded write amplification at high replica counts — revisit only if it shows
  up in DB load.
- The per-Space enable gate inherits #604's production open questions
  (at-most-once acceptability, `skipped` terminal, throughput vs peak join
  rate), now applied per Space.
