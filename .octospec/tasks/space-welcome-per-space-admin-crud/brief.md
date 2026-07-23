---
type: Task
title: "Task: space-welcome-per-space-admin-crud"
description: Make the Space new-user welcome message per-Space and self-service — each Space's admins create/update/delete their own welcome config; keep the platform-global config as a superadmin-managed fallback.
tags: ["notify", "onboarding", "space", "isolation", "auth", "acl", "i18n", "error-response", "wire-contract", "system-setting", "migration", "rate-limit", "idempotency", "observability", "testing"]
timestamp: 2026-07-20T23:32:44+08:00
# --- octospec extension fields ---
slug: space-welcome-per-space-admin-crud
upstream: self
source: self
---

# Task: space-welcome-per-space-admin-crud

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Turn the Space new-user welcome message from a **single platform-designated
Space** (configured only by a platform superadmin) into a **per-Space,
self-service** feature: each Space's own admins can create / update / delete
**one** welcome config for **their** Space, and every Space with an enabled
config independently welcomes its first-join human members.

Scope is **one welcome config per Space** (single plain-text body, continuing
the PR #606 model). `PUT` upserts (增/改), `DELETE` removes (删). The existing
delivery ledger, state machine, at-most-once semantics, sender identity, and
human/isolation filters are **reused unchanged**; only the config source and
the drive loop's single-Space assumption change.

The existing platform-global config (`onboarding.space_welcome_*`) is **kept as
a superadmin-managed fallback**: a Space with no per-Space config falls back to
the global config (if it names that Space), so nothing that works today breaks.

Ships with per-Space configs defaulting to `enabled=false` (no behaviour change
on deploy until a Space admin opts in).

## Background

- **Prior art (this feature's origin).**
  - PR #604 `feat(notify): at-most-once Space new-user welcome DM` built the
    delivery machinery (ledger `octo_space_welcome_delivery`, reconciler, send
    worker, notify-local HTTP sender). Brief:
    `.octospec/tasks/space-new-user-welcome-message/brief.md`.
  - PR #606 `refactor(notify): single plain-text Space welcome message`
    collapsed the bilingual split into one `onboarding.space_welcome_message`.
  - The origin brief's **Out of scope** explicitly deferred: "Multi-Space
    configuration with per-Space copy; per-Space admin self-service UI/API."
    **This task lifts exactly that item into scope.**

- **Current config is a global singleton** (`system_setting` KV, category
  `onboarding`), read atomically via `common.SystemSettings.SpaceWelcomeConfig()`
  (`modules/common/system_settings.go:1070`). Four keys, one target Space
  (`space_welcome_space_id`), managed only through the platform manager API
  (`modules/common/api_manager_system_setting.go`). Space admins have no access.
  - Schema: `modules/common/system_setting_schema.go:213-220`.
  - Combination validation: `common.ValidateSpaceWelcomeCombination`
    (`modules/common/system_settings.go:1123`) — space active + `active_from`
    parseable + message trimmed-non-empty ≤ 2000 code points. **Reusable.**

- **The delivery core is already per-Space.** The ledger is keyed
  `UNIQUE (space_id, uid)` with `idx_claim (space_id, status, next_retry_at)`
  (`modules/notify/sql/20260716000001_add_octo_space_welcome_delivery.sql`). The
  state machine, `sweep`, CAS-on-`claim_owner`, at-most-once, the 15s
  notify-local sender, and the fixed `notification` sender identity are all
  per-row and Space-agnostic. **No ledger schema change is required** for the
  single-config-per-Space model.

- **Only two layers assume a single Space and must change:**
  1. **Config source.** `spaceWelcomeService` reads one global
     `SpaceWelcomeConfig()` and hard-filters `spaceID != cfg.SpaceID`
     (`modules/notify/space_welcome.go:151`); the reconciler scans one
     `cfg.SpaceID` (`:233`); the worker claims scoped to one `cfg.SpaceID`
     (`:302`) and sends `cfg.Message` (`:409`).
  2. **Who can write.** No Space-scoped API exists; config is superadmin-only.

- **Space-admin authz pattern already exists** and is the template to copy:
  `searchMembers` does `queryMember(spaceId, loginUID)` then
  `member.Role < spaceRoleAdmin` → `ErrSpacePermissionDenied`
  (`modules/space/api_member_search.go:30-43`). Roles:
  `0=member 1=admin 2=owner` (`modules/space/model.go:38`). Space user routes
  are registered on the `/v1/space` auth group (`modules/space/api.go:70-98`).

- **Confirmed product decisions for this task** (from design review):
  - **One config per Space** (not multiple messages).
  - **Admin scope = Role ≥ 1** (admin + owner), matching `searchMembers`.
  - **Keep the platform-global config as a superadmin fallback** (not a full
    replacement).
  - **CRUD endpoints mount `SharedUIDRateLimiter`** (authenticated default).

## Load-bearing list

- **Space isolation & authorization (tags: space, isolation, auth, acl).**
  - A Space admin may read/write the welcome config for **only** the Space(s)
    where they hold `Role ≥ 1`. Enforcement mirrors `searchMembers`: resolve the
    caller's membership row for the path `:space_id` via the space module's
    `queryMember`, reject `Role < 1` with the generic
    `ErrSpacePermissionDenied` (anti-enumeration: same 403 for non-member and
    under-privileged). The path `:space_id` is the **only** Space a request may
    affect; the Space must be active (`space.status=1`, dissolved/banned → same
    reject path as `searchMembers`'s `checkSpaceActive`).
  - The delivery path's existing isolation invariants are **preserved verbatim**:
    per-`(space_id, uid, status=1)` membership re-check bypassing stale cache;
    recipient-only `SystemBots`/`IsSystemBot` scoping (the `notification` sender
    must never be self-filtered); orphan `space_member` (missing `user` row)
    excluded; fail-closed never delivers cross-Space or to a non-member.
  - Cross-Space worker: a claimed row is dispatched using **that row's**
    `space_id` config and re-checked against **that** Space only; one Space's
    config or backlog can never cause a send into another Space.

- **Config storage migration (tags: system-setting, migration).**
  - New table (single source of truth for per-Space config), one row per Space,
    e.g. `octo_space_welcome_config`:
    `space_id` (UNIQUE, length/charset/**COLLATE aligned to `space.space_id`**),
    `enabled TINYINT`, `active_from DATETIME NULL` (UTC, app-written, **never
    `NOW()`**), `message VARCHAR(2000)`, `updated_by VARCHAR(40)` (audit),
    `created_at`/`updated_at DATETIME NOT NULL` (UTC, app-written). Migration in
    `modules/notify/sql/` (the module already embeds `sql`).
  - **Time discipline** identical to the ledger: all timestamps are
    application-supplied UTC bound params; no naked `NOW()`; `active_from`
    parsed as RFC3339 UTC. COLLATE must match the reconciler's JOIN partners
    (`space`, `space_member`, `user` = `utf8mb4_general_ci`) to avoid MySQL 1267.
  - **Precedence & fallback (contract):** effective config for a Space =
    per-Space row if present, else the platform-global `onboarding.space_welcome_*`
    **iff** its `space_welcome_space_id` names that Space. Exactly one effective
    config per Space; the resolution is deterministic and documented.
  - **Global config atomic read preserved:** `SpaceWelcomeConfig()` stays the
    atomic snapshot reader for the global fallback; per-Space reads use the new
    accessor. No caller reads the four global keys individually.

- **Config validation (tags: error-response, i18n, wire-contract).**
  - Reuse `common.ValidateSpaceWelcomeCombination` semantics: for the CRUD
    write path the target Space is the path `:space_id` (already known
    active via the authz gate), `active_from` must parse RFC3339 UTC, `message`
    trimmed-non-empty ≤ 2000 code points, newlines preserved verbatim, no
    markdown. Enabling with an invalid combination is rejected at the write path.
  - **Runtime re-validation unchanged:** worker/reconciler re-validate the
    effective config each cycle and fail closed (`config_invalid_total`) on a
    bad combination (e.g. a Space later dissolved). Fail-closed applies from the
    next cycle after the config cache refreshes, never mid-cycle.

- **Error responses & i18n (tags: error-response, i18n, wire-contract).**
  - All new CRUD responses go through the `pkg/httperr` i18n envelope with
    registered `pkg/errcode` codes (reuse `ErrSpacePermissionDenied` /
    `ErrSpaceNotMember` / a validation code — reuse
    `err.server.common.space_welcome_config_invalid` or a space-scoped analog).
    No raw `c.ResponseError` / `c.JSON` non-OK / `AbortWithStatusJSON`.
  - `make i18n-extract-check` + `make i18n-lint` pass; new codes get a zh-CN
    block in `pkg/i18n/locales/active.zh-CN.toml`; new handler files added to the
    module's `Test<Module>NoLegacyResponseError` source guard.

- **Rate limiting (tags: rate-limit).**
  - The Space-admin CRUD route group mounts `SharedUIDRateLimiter` **after**
    `AuthMiddleware` (per the repo rate-limit rule); no hand-rolled Redis
    counter. Tests hitting the routes reset `ratelimit:uid:*` in setup.

- **Delivery drive loop: single-Space → all-enabled-Spaces (tags: notify,
  onboarding, idempotency).**
  - Event path `handleMemberJoin(spaceID, uid)` resolves the **effective config
    for that `spaceID`** (not a global equality filter) and enqueues iff enabled
    + eligible. Enqueue failure never blocks/rolls back the completed join.
  - Reconciler iterates **every Space with an enabled effective config**, scans
    each for `status=1 AND created_at >= active_from` members lacking a ledger
    row, under a **global per-cycle cap** (keep total bounded; do not let one
    Space starve others).
  - Worker claims **across Spaces**; on claim it resolves the claimed row's
    Space config for the body + re-validates (fail-closed → skip / pre-IM). The
    cross-Space claim predicate (`status=0 AND next_retry_at<=?`, no `space_id`)
    needs an index that leads with `(status, next_retry_at)` — add it, or claim
    per-Space round-robin — so claiming does not degrade to a full scan.
  - **Enqueue idempotency preserved:** `INSERT ... ON DUPLICATE KEY UPDATE id=id`
    on `(space_id, uid)`; `enqueue_total` counts only real inserts,
    `enqueue_dedup_total` the rest.

- **Config cache & convergence (tags: notify).**
  - The global `SystemSettings` 60s snapshot no longer covers per-Space config.
    Add a per-Space config cache (TTL + write-time invalidation, mirroring
    `modules/notify/space_verify.go`'s `memberCache`) or read-through by PK.
    Admin write refreshes/invalidates on the handling replica; peers converge
    within the chosen TTL (document the window). Reads must be atomic per Space
    (never straddle a refresh and combine fields from two snapshots).

- **Observability (tags: observability).**
  - Existing counters/gauges/`stage`/`error_class` are retained; per-Space
    dimension is `space_id` (already a structured log field). Never log the
    welcome body or raw upstream error strings. Reconciler/worker metrics stay
    aggregate (per-Space label optional, not required for this task).

- **Commit style (tags: commit, git).** Conventional Commits, English.

## Out of scope

- **Multiple welcome messages per Space** — this task is exactly one config per
  Space. No `welcome_id`, no ledger `UNIQUE(space_id,uid,welcome_id)`, no
  per-message audience/selection rules.
- **Removing or reworking the platform-global config** — it stays as a
  superadmin-managed fallback via the existing manager API and `system_setting`
  keys; this task does not delete those keys or that endpoint.
- **Delivery reliability semantics** — the state machine, at-most-once,
  `unknown`/`failed` handling, sweep, backoff `{5s,30s,120s}`, CAS, 15s sender,
  `SELECT ... FOR UPDATE SKIP LOCKED`, no leader election — all unchanged.
- **Sender identity / wire protocol** — fixed `from_uid=notification`,
  `channel_type=PERSON`, `red_dot=1`, `payload.type=Text`, authoritative
  `payload.space_id`. Admins cannot choose/forge the sender or submit arbitrary
  payload. Still plain text — no rich text, cards, attachments, placeholder
  rendering (`{nickname}`), scheduled send, or audience targeting.
- **Ledger schema change / archival / TTL** — ledger keeps
  `UNIQUE(space_id,uid)`, grows monotonically, operator-cleaned via SQL. (Only
  a possible **new claim index** `(status, next_retry_at)` for cross-Space
  claiming is in scope; the columns are unchanged.)
- **Retroactive bulk send** to members who joined before a Space's `active_from`.
- **Client / admin UI** — server-side API + validation only. Frontend rendering,
  conversation-list/red-dot rules, and per-client (Web/iOS/Android) logic are
  out of scope.
- **Per-Space multi-language copy** — the message stays a single language-
  agnostic plain-text body (PR #606 model).
- **BotFather Space welcome and `u_10000` register/login welcome** — untouched;
  may coexist as today.
- **octo-lib changes** — none; the notify-local sender remains self-contained.

## Acceptance

- **Space-admin CRUD authz.** `GET/PUT/DELETE /v1/space/:space_id/welcome`
  succeed for a caller with `Role ≥ 1` in that Space; a member with `Role = 0`,
  a non-member, and a caller against a dissolved/banned Space all get the
  generic `ErrSpacePermissionDenied` / `ErrSpaceNotMember` path (no
  privilege-reason leak). A request can affect only the path `:space_id`.
- **CRUD semantics (single config per Space).** `PUT` on a Space with no config
  inserts one (增); `PUT` on an existing config updates it (改); `DELETE` removes
  it (删) and the Space reverts to the global fallback (or off). `updated_by`
  records the acting admin uid.
- **Write-path validation (prospective).** With `enabled=true`, writes with an
  unparseable `active_from`, empty-after-trim, or > 2000 code point `message` are
  rejected via the i18n envelope. A partial `PUT` is accepted iff the resulting
  effective combination is valid (validate the merged prospective config, not
  the incoming patch alone). The target Space is the path Space (already active).
- **Precedence & fallback.** With a per-Space row present, it wins; with none, a
  Space named by the global `space_welcome_space_id` uses the global config;
  other Spaces have no welcome. A test asserts exactly one effective config per
  Space and the deterministic resolution in each direction.
- **Multi-Space delivery.** Two Spaces each with an enabled config each welcome
  their own first-join human members: exactly one `(space_id, uid)` ledger row
  per Space per member; no cross-Space delivery; disabling Space A does not
  affect Space B. The reconciler catches up all enabled Spaces within one cycle;
  a per-cycle global cap bounds work and no single Space starves the others.
- **Isolation invariants preserved (regression).** Recipient re-check bypasses
  stale cache; `notification` (sender) is never self-filtered; robots / system
  bots (recipient) / orphan rows / pre-`active_from` / non-members receive
  nothing; fail-closed on any anomaly.
- **Cross-Space claim.** The worker claims rows from any enabled Space; the
  claim predicate is index-backed (no full scan) — verified by the presence of a
  `(status, next_retry_at)`-leading index (or documented per-Space round-robin).
  A claimed row is dispatched with its own Space's `message` and re-validated
  against its own Space.
- **Config cache convergence.** An admin write takes effect immediately on the
  handling replica (cache invalidated/refreshed); peers converge within the
  documented TTL. A concurrent cache refresh never yields a half-updated
  per-Space config to a cycle.
- **Rate limiting.** The CRUD group carries `SharedUIDRateLimiter` mounted after
  `AuthMiddleware`; a burst test observes the `uid` scope headers /
  `rate.limited` envelope; test setup resets `ratelimit:uid:*`.
- **i18n / error contract.** `make i18n-extract-check` and `make i18n-lint`
  pass; new codes have zh-CN blocks; new handler files are in the module's
  `Test<Module>NoLegacyResponseError` guard; no raw error responses.
- **Time discipline & schema alignment.** No feature SQL uses naked `NOW()`;
  `octo_space_welcome_config.space_id` matches the length/charset/COLLATE of
  `space.space_id`; a JOIN smoke test confirms no MySQL 1267.
- **Backward compatibility.** With no per-Space rows written, behaviour is
  identical to today (global config still drives its one Space); existing tests
  in `modules/notify`, `modules/common`, `modules/space`, and bot provisioning
  continue to pass; no change to existing wire responses.
- **Command-line verification.**
  - `go test -race ./modules/notify/...`
  - `go test -race ./modules/common/...`
  - `go test -race ./modules/space/...`
  - `make i18n-extract-check && make i18n-lint`
  - `go test -race ./...`
  - `git diff --check`
- **Tests cover at minimum:** CRUD authz matrix (admin/owner/member/non-member/
  inactive Space); PUT insert-then-update, DELETE→fallback; prospective
  validation both directions; precedence per-Space-vs-global resolution;
  multi-Space independent delivery + per-Space disable; cross-Space claim single
  winner (`FOR UPDATE SKIP LOCKED`); recipient re-check + human/system-bot/orphan
  exclusions with `notification` not self-filtered; config cache
  invalidation/convergence; UID rate-limit headers; migration executes on a
  fresh DB.

## Open questions (product / operations sign-off before enabling)

- **DELETE semantics** — hard-delete the row vs. soft (`enabled=0`). Default
  proposed: hard-delete so "no per-Space config" and fallback are one code path;
  audit lives in logs. Confirm no need to retain deleted copy.
- **Fallback visibility** — when a per-Space `DELETE` reverts a Space to the
  global fallback that still names it, that Space keeps receiving the global
  welcome. Confirm this is intended (vs. `DELETE` meaning "off for this Space").
- **Per-Space enable gate** — the origin task's production-enable open questions
  (at-most-once acceptability, `skipped` terminal, throughput vs. peak join
  rate) now apply **per Space**; enabling for a high-traffic Space still needs
  the throughput envelope confirmed. Carried forward, not re-litigated here.
