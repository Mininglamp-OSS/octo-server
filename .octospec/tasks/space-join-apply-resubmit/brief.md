---
type: Task
title: "Task: space-join-apply-resubmit"
description: Let a user re-submit a Space join application with a fresh invite code so a pending record bound to a spent/disabled/expired code can recover without an admin rejecting it first.
tags: ["space", "isolation", "acl", "error-response", "i18n", "wire-contract", "rate-limit", "testing", "commit"]
timestamp: 2026-07-30T11:25:47+00:00
# --- octospec extension fields ---
slug: space-join-apply-resubmit
upstream: Mininglamp-OSS/octo-server#683
source: user
---

# Task: space-join-apply-resubmit

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

A pending Space join application must never become a record that can neither be
approved nor repaired by the applicant. Concretely:

1. **R1** — When a user re-submits `POST /v1/space/join` with a *different*
   valid invite code while their application is still pending, the existing row
   adopts that code and its application time is refreshed.
2. **R2** — Such a re-submission re-notifies the Space admins, so the newest
   application is visible to whoever approves it.
3. **R4** — Both application lists sort and display by the latest application
   time (decision A: refresh `created_at`; `applied_at` is deliberately not
   introduced, so octo-admin needs no change).
4. **R5** — When approval fails because the bound invite code is no longer
   consumable, the response distinguishes *use cap reached* from *disabled or
   expired*, and the precise reason is logged.
5. **R6** — Re-submission and approval running concurrently must not corrupt
   state: no application already approved/rejected gets silently reset to
   pending, no invite slot is double-consumed.
6. **R7** — When approval fails on invite-code consumption, the applicant is
   told to re-submit with a valid code. Today they are told nothing, so the
   self-repair path R1 opens would never be used.

## Background

Reported in #683 and reproduced against `main` @ `a6e186f`. Four defects
compound into a stuck state:

- **Re-submission short-circuit** — `joinSpace` returns a bare `PENDING`
  response as soon as it finds a `status=0` row (`modules/space/api.go:1006-1018`),
  *before* `upsertJoinApply` (`:1021`) and `notifyAdminsNewJoinApply` (`:1032`).
  The new invite code is never written back, the time is never refreshed, and
  admins are never re-notified.
- **Slot consumed at approval, not at submission** — submission only does a
  read-only check (`api.go:989-992`); the slot is consumed at approval via
  `incrementInviteUsedCountAtomic` (`api.go:1563-1577`, `api_manager.go:1179-1194`).
  So N pending applications can hang off a code with one slot left, and all but
  one necessarily fail at approval time.
- **Rollback plus the unique key = deadlock** — a failed consumption rolls the
  row back to pending (`api.go:1565-1576`), and `UNIQUE KEY uk_space_uid
  (space_id, uid)` (`modules/space/sql/20260410000002_space_legacy02.sql:13`)
  means the applicant has exactly one row. Approval keeps failing, re-submission
  is short-circuited, and only an admin rejection breaks the cycle.
- **`created_at` never refreshed** — `upsertJoinApply`'s `ON DUPLICATE KEY
  UPDATE` touches `status/invite_code/reviewer_uid/updated_at` but not
  `created_at` (`modules/space/db.go:586-597`), while both list paths order by
  and return `created_at` as the application time (`db.go:619-628` + `api.go:1494`;
  `db_manager.go:590-604` + `api_manager.go:1116`).

Decision A was chosen for R4 because both APIs already treat `created_at` as
"application time" on the wire; refreshing it aligns the implementation with the
contract that already exists rather than changing that contract. octo-admin
binds `created_at` (`src/api/space.ts:52`, `src/api/space-user.ts:52`,
`src/pages/Spaces/SpaceJoinAppliesPanel.tsx:193`) and needs no change.

R5 needs no new error code: `ErrSpaceInviteCodeExhausted` covers the use cap and
`ErrSpaceInviteCodeInvalid` already reads "invalid or has expired". Both are
registered, both have zh-CN translations, and octo-admin renders
`error.message` straight from the envelope (`src/api/index.ts:42`).

## Load-bearing list

- **`space` / `isolation` / `acl` — Space join admission path.** `joinSpace`
  (`modules/space/api.go:955`) is the only writer of `space_join_apply`.
  Changing its pending branch changes who can enter a Space and on whose
  authority. Membership must still require an admin approval; a re-submission
  must never itself grant entry.
- **`wire-contract` — join/apply response shapes.** `POST /v1/space/join`
  returns `{status: PENDING | NEED_APPROVAL, space_id, msg}`; the two list
  endpoints return `created_at` as the application time. Clients (mobile, web,
  octo-admin) read these. `created_at` keeps its wire position and type; only
  its value refreshes.
- **`error-response` / `i18n` — approval failure codes.** `approveJoinApply`
  currently collapses exhausted/disabled/expired into
  `ErrSpaceInviteCodeExhausted`. Splitting it changes the code an admin client
  sees. Both approval paths (`api.go:1505`, `api_manager.go:1126`) must stay
  consistent — they are two entry points to the same decision.
- **Invite-slot accounting.** `incrementInviteUsedCountAtomic` /
  `decrementInviteUsedCountAtomic` (`db.go:299-335`) and the rollback helpers
  (`rollbackApplyAndInvite`, `refundInvite`) pair consumption with refunds. A
  re-submission must not touch `used_count` at all — submission has never
  consumed a slot and must not start.
- **`uk_space_uid` single-row invariant.** One applicant, one Space, one row.
  Every write must remain compatible with that; state transitions must be
  guarded so a concurrent approval is not clobbered.
- **`rate-limit` — admin notification amplification.** R2 makes a user-triggered
  request fan out one DM per Space admin. `POST /v1/space/join` sits on the plain
  auth group (`api.go:77-81`) with no `SharedUIDRateLimiter`, so this needs a
  containment story (see Open question).
- **`test`** — `modules/space/api_test.go` already covers approval-time invite
  failures (`:1716` exhausted, `:1761` disabled); those assertions must keep
  passing except where this brief deliberately changes the returned code.

## Out of scope

- **Moving invite-slot consumption to submission time (root cause ②).** Approval
  can still fail when the bound code was spent between submission and approval;
  this change makes that state *recoverable* (R1 + R7), not impossible. Tracked
  as a separate follow-up issue — the trade-off (pending applications holding
  slots, refunds on rejection) deserves its own review.
- **Per-application history / audit trail.** `uk_space_uid` keeps one row per
  applicant; earlier submissions are still overwritten. A history table is a
  separate change.
- **octo-admin.** No frontend change follows from decision A.
- **The email-invite join path** (`api_email_invite*.go`) — it does not write
  `space_join_apply`.
- **`updated_at` semantics** and any schema migration — `created_at` already
  exists; no DDL.

## Acceptance

Regression tests mirroring #683 Steps to Reproduce 6-9, in
`modules/space/api_test.go`:

- **A1 (R1/R2/R4)** — pending application bound to code A; user submits code B;
  assert the row's `invite_code` becomes B, `created_at` is strictly greater
  than before, and the response is not a bare short-circuit.
- **A2 (R1)** — after A1, admin approval succeeds and consumes B's slot, not A's.
- **A3 (R4)** — reject-then-re-apply refreshes `created_at`; the row sorts ahead
  of an older pending application in `queryPendingAppliesBySpace` and
  `queryJoinAppliesAdmin`.
- **A4 (R6)** — a re-submission against a row that is no longer `status=0` must
  not reset it to pending; assert an already-approved application stays
  approved and the applicant is reported as a member.
- **A5 (R5)** — approval against a disabled or expired code returns
  `err.server.space.invite_code_invalid`; against a use-capped code returns
  `err.server.space.invite_code_exhausted`. Updates the existing `:1761`
  disabled-code assertion.
- **A6 (R7)** — approval failing on invite consumption sends the applicant a
  notification; the application stays pending.
- **A7 (R2 containment)** — re-submitting the *same* invite code does not
  re-notify admins and does not bump `created_at` (nothing changed).

Gates: `go test ./modules/space/...`, `golangci-lint run ./...`,
`make i18n-extract-check`, `make i18n-lint`. No new error code is expected, so
`active.zh-CN.toml` should need no edit — if the implementation proves otherwise,
that is a signal to re-check R5's design before adding one.

## Open question (needs a decision before Implement)

**R2 amplification containment.** Each qualifying re-submission DMs every Space
admin. Two layers are available:

1. **Change-detection (in scope, no decision needed).** Only update and notify
   when the submitted code actually differs from the stored one. Re-submitting
   the same code stays a no-op `PENDING` — this is A7. It makes spamming require
   a supply of *distinct* valid codes, which only admins can mint.
2. **Mounting `SharedUIDRateLimiter` on `POST /v1/space/join` (needs your
   call).** It is the repo default for authenticated endpoints and this route
   lacks it, so this arguably fixes a pre-existing gap. But it adds 429s to a
   route that has never returned them, and existing tests hitting `/join`
   repeatedly would need `ratelimit:uid:*` reset in setup. I lean toward
   including it; say the word if you would rather keep this change surgical and
   file the limiter separately.
