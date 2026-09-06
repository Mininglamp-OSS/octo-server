---
type: Task
title: "Task: project-p0-foundation"
description: P0 of the Project collaboration layer between Space and Group — a new modules/project with octo_project / octo_project_member / octo_project_invitation, CRUD + membership under invariant I1 (project members are a subset of active Space members), member_epoch bumped in the same transaction as every membership/role write, Space-removal cascade via the existing reverse-registered cleanup step, an I1 reconcile job, and one fail-closed create flag. Deliberately touches no group code and produces no project-owned groups, so it is independently verifiable and revertible.
tags: ["space", "isolation", "acl", "error-response", "i18n", "rate-limit", "wire-contract", "testing", "commit"]
timestamp: 2026-09-04T00:00:00Z
# --- octospec extension fields ---
slug: project-p0-foundation
upstream: self
source: self
---

# Task: project-p0-foundation

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Land the storage and membership foundation of the **Project** layer: a
Space-internal, flat, overlappable collaboration unit that owns a member roster
and (from P1 on) a set of groups.

P0 ships exactly the slice that can be verified on its own and reverted by
dropping three tables:

1. `modules/project` — a new module registered the standard way (`1module.go` +
   `register.AddModule()`).
2. Three new tables: `octo_project`, `octo_project_member`,
   `octo_project_invitation`.
3. Project CRUD, membership add/remove/leave/role, invite-code join.
4. **Invariant I1**: an active `octo_project_member.uid` is an active
   `space_member.uid` of the same Space. **Enforced synchronously on every
   Project-side write; restored asynchronously on the Space-removal path** — the
   two halves have different guarantees and the difference is load-bearing, see
   "I1 is not synchronous in both directions" below.
5. **`member_epoch`** on `octo_project`, incremented in the *same transaction* as
   every membership/role write.
6. **Space-removal cascade**: when a user leaves or is removed from a Space,
   their Project memberships are deactivated, via the existing reverse-registered
   cleanup step (`modules/space/member_removal.go:74`).
7. An I1 reconcile job and a fail-closed `project_create_enabled` flag.

### I1 is not synchronous in both directions

Project-side writes (add, invite-accept, join, role change) check Space
membership **inside the request transaction** with
`pkg/space.CheckMembership` — `space_member.status = 1` **and** `space.status = 1`
(`pkg/space/membership.go:9-24`). So a non-member can never be admitted.

The reverse direction is **eventually consistent, and that is a property of the
existing machinery, not a choice**: cleanup work is enqueued in the Space-removal
transaction but executed by a poller — `ctx.Schedule(10*time.Second,
processMemberRemovalCleanups)`, a 10-minute lease, exponential backoff, and a
terminal `abandoned` state after the attempt budget is exhausted
(`modules/space/member_removal.go:274-285`, `db_member_removal.go:121,201,290`).
Therefore, between the Space removal committing and the step running, rows exist
with `octo_project_member.status = 1` and no active Space seat.

Two consequences the implementation must handle, or P0 ships an alert generator:

- **The reconcile job must exempt rows with an outstanding cleanup job** for the
  same `(space_id, uid)`, exactly as the design brief exempts in-flight
  `removing = 1` rows in P1. Without the exemption every kick produces an I1
  "violation" for at least one poll interval, and the alert becomes noise before
  the feature has a single user. Cleanup jobs stuck in `abandoned` are a
  *separate* alert with a different meaning — that one is a real leak.
- **The cascade step must use cleanup semantics, not membership semantics.**
  `CheckMembershipForCleanup` (`pkg/space/membership.go:26-51`) differs from
  `CheckMembership` on exactly one axis: in a **banned** Space (`space.status = 2`)
  the member still holds their seat, so cleanup must **skip**. A step written
  against `CheckMembership` would deactivate every Project membership in a Space
  the moment it is banned, and un-banning would not restore them. For the same
  reason the reconcile scan must not treat members of a banned Space as I1
  violations.

P0 tolerates the window because a Project membership grants nothing yet — no
group, no channel, no message. **That tolerance expires in P1**, where the same
row gates group admission; the design brief's `removing` intermediate state and
IM-pending table exist for that reason.

**P0 changes no file under `modules/group/`** and no group route. No group can
belong to a Project yet (`group.project_id` is P1), so invariants I2 and I3 have
nothing to violate and are out of scope here.

At the end of P0 the product can already do "pick people out of the Space
directory and form a team". It cannot yet create a project-owned group.

## Background

Design brief: `.context/[octo-server]brief-project.html` v8 (local, git-ignored;
not required to review this task). The parts P0 depends on:

- **Chosen architecture (option A of four)**: Project is a *member pool plus a
  group-ownership tag*, **not** a new tenant or read boundary. Space remains the
  only security boundary. Group visibility in octo-server is already
  membership-based, so "only project members can see a project group" reduces to
  "only project members can be admitted into a project group" — a write-side
  constraint. That is why P0 adds no middleware to any read path.
- **Why not a sub-Space**: `space_id` is baked into channel IDs
  (`pkg/space/channel.go`), `category` is keyed `(uid, space_id)`, users have a
  default Space, `dm_space_presence` is indexed by `space_id`, and
  `space_member_removal_cleanup` is a security cascade. Another Space-equivalent
  layer underneath means rewriting all of it.
- **Three invariants** (I2/I3 land in P1): **I1** project members ⊆ active Space
  members. **I2** a project group's active members ⊆ that project's active
  members (system bots exempt). **I3** a group belongs to at most one Project,
  immutably in v1.
- **Reliability posture for future subsystem consumers** (fleet / drive /
  matter): correctness may only depend on **idempotent, replayable pulls**;
  pushes are a latency optimisation and never a correctness dependency, because
  octo-server has no reliable outbound push. `member_epoch` is the single
  invalidation primitive that whole design rests on — which is why P0 lands it
  even though P0 has no consumer (see Decisions, D2).

### Existing machinery this reuses rather than reinvents

| Need | Reuse | Where |
|---|---|---|
| Cascade from Space member removal | `RegisterMemberRemovalCleanupStep` reverse registry (downstream module registers in `init`; space imports nobody) | `modules/space/member_removal.go:74-95`, contract at `:56-64` |
| Transactional outbox with lease, backoff, budget exhaustion, purge | `space` member-removal cleanup job | `modules/space/db_member_removal.go:49,121,201,268,290,319` |
| At-most-once delivery ledger + reconciler + worker (needed in P1, not P0) | `notify` welcome ledger: `UNIQUE` key for at-most-once, `status`/`attempts`/`next_retry_at`, `claim_owner`/`claim_expire_at` CAS lease, `error_class` taxonomy, sweep index, app-side UTC (no `NOW()`), cursor rotation under a shared budget, enqueue never blocks the write | `modules/notify/group_welcome.go:127-140,188-205,277-346`; DDL `modules/notify/sql/20260723000001_add_octo_group_welcome_delivery.sql` |
| Anti-enumeration 404 shape | `channel` merges not-found and forbidden | `modules/channel/api.go:179-194` |
| Membership cache invalidation discipline | space invalidates `space:member:{spaceID}:{uid}` (TTL 60s) **synchronously in-request**, explicitly *not* in the background, because it is an isolation boundary | `modules/space/member_removal.go:105+` |
| Request-bound short-lived credential (only if a gateway is ever added, P3) | HMAC-SHA256 assertion over (version, issuer, subject, space, method, requestURI, body SHA-256, iat, exp=60s, nonce), secret ≥ 32 bytes | `modules/agentmailgateway/assertion.go:45-79` |
| Outbound callback delivery, **if one is ever justified** (P3 at the earliest) | `internal/cardactiondispatch` — the repo's only real outbound channel: Redis durable queue with lease tokens and reclaim, exponential backoff, `MaxAttempts` → DLQ with retention and a non-destructive replay path, per-route URL/timeout/backoff config, HMAC-SHA256 signature (`X-Octo-Signature` / `X-Octo-Timestamp` / `X-Octo-Event-ID`, canonical string `v1\nMETHOD\npath\ntimestamp\neventID\nsha256(body)`, with an exported `Verify` for the receiver), retryable classification (transport, 408, 429, 5xx), Prometheus metrics | `internal/cardactiondispatch/{queue,dispatcher,http,signature,config}.go` |
| **Not usable** for cross-system notification | `modules/base/event` — a listener error only sets the row to `Fail` and the scan picks up `Wait` only, so **`Fail` rows are never retried**; multiple listeners share one row's status and `version_lock` does not advance. Fine as an in-process trigger, never as a delivery channel | `modules/base/event/api.go:58-70,130-143,147` |

## Load-bearing list

- **`space_member` removal path and its cleanup-step contract.** Steps must be
  idempotent, must decide "nothing to do" themselves and return `nil`, must not
  assume any other step succeeded, and a returned error reruns the *whole* job
  including already-successful steps (`modules/space/member_removal.go:56-64`).
  The Project step is written to that contract.
- **Space membership cache invalidation is synchronous and in-request.** The
  Project membership cache must follow the same rule; deferring it to a worker
  leaves a removed member authorised for up to a full TTL.
- **`SpaceMiddleware` does not read path parameters.** It reads only the
  `space_id` query parameter and the `X-Space-ID` header, and when neither is
  present it calls `c.Next()` — i.e. it *passes* (`pkg/space/middleware.go:158-165`).
  `/v1/space/:space_id/projects` therefore cannot rely on it; every Project route
  declares its own auth chain and is tested for 401/403 with no header and no
  query. The context key is written and read by the exported
  `spacepkg.SetSpaceID(c, spaceID)` / `GetSpaceID(c)`
  (`pkg/space/middleware.go:215,220`), so a path-param variant can populate the
  same key. Cache TTLs to mirror are `cacheTTL = 60s` / `negativeCacheTTL = 30s`
  (`:18-19`).
- **`SpaceMiddleware` is itself not i18n-compliant, and must not be copied
  verbatim.** Its three failure exits use raw
  `c.AbortWithStatusJSON(status, gin.H{"msg": "…"})` with hardcoded Chinese
  (`pkg/space/middleware.go`, the 401 / 403 / 500 branches). The new
  `SpaceIDParamMiddleware` and `ProjectMiddleware` must return the localized
  envelope via `httperr` instead, and the `TestProjectNoLegacyResponseError`
  guard must cover **middleware files as well as handler files** — a guard scoped
  to `api*.go` would miss exactly the code most likely to be copy-pasted from a
  non-compliant precedent.
- **i18n error envelope.** All user-facing errors go through
  `httperr.ResponseErrorL` with codes registered in a new
  `pkg/errcode/project.go`. No `c.ResponseError` / `c.ResponseErrorf` /
  `c.AbortWithStatusJSON` / non-OK `c.JSON`. 5xx ⟺ `Internal=true`; 4xx never
  `Internal`. Auth and invite-code failures collapse to one generic code each
  (anti-enumeration); the specific reason goes to logs only.
- **Rate limiting.** `SharedUIDRateLimiter` mounted *after* `AuthMiddleware`
  (before it, the limiter cannot read the uid and silently fails open). The two
  public invite endpoints get `StrictIPRateLimitMiddleware("project_invite", …)`,
  matching `space_invite`. No hand-rolled Redis counters.
- **`pkg/authtree` tenant census.** Every new route is registered with its
  per-request-input tenant scope. The census standard is per *input*, not per
  route: PR #713 found four cross-tenant reads precisely by walking from a
  guarded input to an unguarded sibling (`pkg/authtree/authtree.go:34-48`). The
  decision "`uk_*` User API Keys stay Space-scoped, deliberately not
  Project-scoped" is registered explicitly as intentional rather than left
  unlisted.
- **`app_config` version short-circuit.** A batch of policy fields
  (`LocalLoginOff`, `DisableUserCreateSpace`, `SearchEnabled`, `DocsOn`,
  `DriveOn`, `DmloopOn`) are deliberately decoupled from `app_config.version` and
  emitted on both branches so an old client cannot cache stale policy
  (`modules/common/api.go:389`, `:815-822`, `:829-830`, `:904-908`). Anything
  Project-related that a client uses to decide what it may do must follow that
  precedent, not sit behind the short-circuit.
- **Migration and schema conventions.** New tables and indexes carry the `octo_`
  prefix (never `dm_`); existing unprefixed tables are not renamed. Files live in
  `modules/project/sql/` embedded via `//go:embed sql`, named
  `<yyyyMMddNNNNNN>_<name>.sql` with `-- +migrate Up` / `-- +migrate Down`,
  inline index definitions, and the `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COLLATE=utf8mb4_general_ci COMMENT='…'` suffix used by every existing script.
  Target is MySQL 8.0 only; no `ALGORITHM=INPLACE,LOCK=NONE` pinning.
- **Column type and collation alignment.** `space_id`/`uid`/`project_id` widths,
  charset and `COLLATE` must match `space.space_id` / `user.uid` /
  `space_member.uid`. Mismatched collations across a `JOIN` produce error 1267 on
  MySQL 8.0 — already hit once in this repo.
- **dbr table-name quoting.** `Update` / `InsertInto` / `DeleteFrom` take the
  bare name (dbr quotes it); `From` / `Select` need manual backticks. Getting it
  backwards yields error 1064 on reserved words.

## Out of scope

- **Anything under `modules/group/`.** `group.project_id`, the
  `(space_id, project_id)` index, `AdmitOrRestoreMemberTx`, the single removal
  entry, the 11-path convergence, the source guards forbidding direct
  `group_member` DML, the `botfather` bare `UPDATE`, `IService.AddMember`,
  `CheckForbiddenLoop`, and the `joinPresetGroups` hardening are **all P1**.
- **`octo_project_member.removing` and `octo_project_member_removal_im_pending`.**
  Both exist to bound cascade-to-groups work, and P0 has no project group, so
  landing them now would be untestable scaffolding. P1.
- **Every form of subsystem integration.** No `projects[]` in
  `POST /v1/auth/verify*?include=context`, no epoch polling endpoint, no removal
  log table, no replica report, no callback/webhook registry, no fleet or drive
  work. P0 writes `member_epoch` and exposes it only to its own clients (see D3).
- **Read-path hardening** (`/v1/sidebar/sync` member fallback, deprecated
  `/v1/coversations`, `/v1/groups/:group_no/avatar` enumeration oracle,
  `querySavedGroups` after leaving) — P2, tracked separately.
- Pinning, the `is_official` management endpoint, the nested project → group →
  thread tree, Project-level auto-join, and the member-picker data source — P2.
- Email invitation, cross-Space Projects, nested Projects, Project as a read
  boundary (option B), external members.

## Decisions

Recorded because each one deviates from, or resolves an ambiguity in, the design
brief.

**D1 — Only one flag in P0: `project_create_enabled`.** The brief specifies two
(`project_create_enabled` plus a non-disableable `project_enforce_membership`).
The second one gates I2, which does not exist until P1. A flag whose enforcement
point does not exist yet reads to operators and reviewers as protection that is
in place; it lands in P1 together with the gate it controls. `project_create_enabled`
is fail-closed: off ⇒ create/update/disband/member-write/invite endpoints return
403 while reads keep working so existing data stays observable.

**D2 — `member_epoch` lands in P0, not P1.** The brief assigns it to P1
"alongside the unified write entry". That is necessary but late: P0 is where
Project membership writes are *born* (add, remove, leave, role change, Space
cascade, disband). Adding the column later means retrofitting increments into
write paths that already exist — the exact failure the brief argues against when
it notes that eleven group write paths prove retrofits leak. The column is one
`BIGINT`; P1 extends the same rule to the cascade paths it adds.

**D3 — `member_epoch` is emitted to first-party clients but to no subsystem.**
It ships in the Project list/detail responses next to `my_role` and the
capability bits, so clients never derive permissions from a role number. It is
*not* exposed through any new machine-to-machine endpoint in P0: this repo has
already built "an endpoint and waited for a consumer" twice (two dead
`spaceMembers` code paths), and the P3 design deliberately delivers epoch through
the existing `verify?include=context` channel instead of a new one.

**D4 — Name uniqueness uses a generated column so disbanding frees the name.**
A plain `UNIQUE (space_id, name)` keeps a disbanded project's name reserved
forever: create "Q3 delivery" → disband → cannot recreate. That is the same shape
as the `joinPresetGroups` unique-index defect where a member who left could never
be re-added (`modules/space/api.go:1366`). MySQL 8.0 has no partial index, so:

```sql
`name`        VARCHAR(64) NOT NULL DEFAULT '',
`active_name` VARCHAR(64) GENERATED ALWAYS AS (IF(`status` = 1, `name`, NULL)) STORED,
UNIQUE KEY `uk_space_active_name` (`space_id`, `active_name`),
```

Repeated `NULL`s do not collide in a unique index, so disbanded rows drop out
while active names stay unique per Space. **Implementation note:** `INSERT` and
`UPDATE` statements must not mention `active_name` — struct-driven column lists
(`util.AttrToUnderscore()`-style) will try to write it and MySQL rejects it with
error 3105. Cover that with a test that inserts through the real DAO.

**D5 — `ResponseErrorL` (pinned 400), not `ResponseErrorLWithStatus`.** The
module is brand new and has no clients depending on a fixed 400, so `WithStatus`
would be defensible, but it needs maintainer sign-off and currently has exactly
one user (`modules/oidc` bind). P0 uses the default facade so nothing about the
error surface blocks the merge; switching later is one call site per handler and
the envelope body is identical either way.

**D6 — `is_official` column ships with no writer.** The badge is a P2 endpoint
restricted to Space admins, but the column lands now (default 0) rather than in a
second migration against a table that by then has production traffic. Official
projects are forced to `discoverability = space_listed` when the P2 endpoint
arrives; P0 only guarantees the column exists and is never written.

**D7 — The reconcile job is read-only and may run on every pod.** Duplicate
detection only duplicates alerts. Any *mutating* reconcile action added later
must take a DB CAS claim first, in the shape of the welcome ledger's
`claim_owner`/`claim_expire_at` lease. The interval is jittered so pods do not
synchronise.

**D8 — `discoverability` is named for what it is.** `space_listed` / `unlisted`
filter the Space project list and directory search; they are not a security
boundary. Space admins can still enumerate project metadata from the admin
surface. The field is deliberately not called `visibility` or `secret`, both of
which would invite readers to treat it as isolation.

## Deferred, but constrained: how membership changes will reach other systems

P0 implements none of this. It is recorded here because P0 must not foreclose it,
and because the shape is already decided.

**No subsystem is notified. Each one pulls.** A consumer polls a cheap
counters-only epoch view (or reads `member_epoch` from
`verify?include=context`, which is the existing channel and is safe because
verify only ever answers about the token holder), drops its cache when the epoch
moved, and re-asks. Convergence is bounded by the poll interval, ~10s. Nothing in
that path needs a delivery guarantee, which is the point: octo-server has no
reliable outbound push, so any design that depends on one is broken before it
ships.

**If a callback is ever added, it is a hint and carries no facts.** The payload
would be `(space_id, project_id, member_epoch)` and nothing else — no member
list, no "user X was removed". A lost, duplicated, delayed or reordered hint then
costs latency only; it cannot produce a wrong authorization decision, and the
consumer's pull path stays mandatory. A callback that shipped membership *content*
would make delivery load-bearing and reintroduce exactly the failure mode being
avoided.

**And it would extend `internal/cardactiondispatch`, not add a queue.** That
package already has durable queueing with lease reclaim, bounded retries, a DLQ
with retention and replay, per-route timeouts, and request-signing with an
exported verifier. Writing a second delivery mechanism next to it would be the
third half-built outbound path in this repo.

**A member replica in a subsystem stays forbidden.** fleet and drive each
already copy octo-server membership and neither ever deletes a copied row —
drive's own middleware says as much ("the enforced boundary is entry, not exit").
A subsystem may only hold a replica after it demonstrably consumes a removal
cursor (including the resync signal), runs its own full reconcile with an alert,
reports its cursor and replica count so "not applying" becomes an alertable fact,
member-checks *every* read path, and states a measurable staleness SLO. Those are
entry conditions, not follow-ups.

## Implementation slices

Two PRs, each independently revertible.

**PR-1 — module, schema, CRUD, membership, cascade, flag, reconcile**

- `modules/project/{1module.go, api.go, api_i18n.go, service.go, db.go, model.go, sql/}`;
  blank import added to `internal/modules.go`.
- Migration creating `octo_project` (incl. `member_epoch`, `is_official`,
  `discoverability`, `join_mode`, `max_members`, `status`, `active_name`) and
  `octo_project_member` (`(project_id, uid)` PK, redundant `space_id`, `role`,
  `status`, `invite_uid`).
- Routes: `POST|GET /v1/space/:space_id/projects`,
  `GET|PUT|DELETE /v1/projects/:project_id`,
  `GET /v1/projects/:project_id/members`,
  `POST /v1/projects/:project_id/members/add`,
  `POST /v1/projects/:project_id/members/remove`,
  `POST /v1/projects/:project_id/leave`,
  `PUT /v1/projects/:project_id/members/:uid/role`.
- `SpaceIDParamMiddleware` (reads `c.Param("space_id")`, writes the same context
  key `SpaceMiddleware` uses, then checks Space membership) and
  `ProjectMiddleware` (resolves `octo_project.space_id`, checks Space
  membership, then Project visibility/membership; 60s positive / 30s negative
  Redis cache, invalidated synchronously on every membership write).
- Permission matrix with transitive protection: admins cannot remove or demote
  admins/owners; the last owner must transfer ownership before leaving or being
  demoted, atomically in one transaction.
- Quotas: 100 projects per creator per Space, 1000 per Space, 500 members per
  project, plus a per-day creation cap mirroring the group module's
  `querySameDayCreateCountWitUID` + `SameDayCreateMaxCount` pattern
  (`modules/group/db.go:694`, `modules/group/api.go:1033-1039`) — the pattern is
  copied, the group package is not imported. All configurable.
- `RegisterMemberRemovalCleanupStep("project_member", …)`: deactivate every
  active `octo_project_member` row for `(space_id, uid)` and bump each affected
  project's `member_epoch` **only when rows were actually affected** — the step is
  re-run on every job retry, so an unconditional bump would inflate the epoch on
  no-op reruns and break the "no-op does not change epoch" rule. It must use
  `CheckMembershipForCleanup` semantics (skip when the seat still exists, e.g. a
  banned Space), return `nil` when there is nothing to do, and be safe to run
  twice. Its failure must be self-limiting: it shares the job with the existing
  group/conversation cleanup steps, and a step that keeps returning errors keeps
  the whole job being re-claimed, which burns lease cycles and crowds healthy
  jobs out of each batch (`modules/space/member_removal.go:371-381` documents that
  failure shape for panics).
- Reconcile job: I1 violations **excluding** `(space_id, uid)` pairs with an
  outstanding cleanup job and excluding members of banned Spaces,
  `member_epoch` monotonicity, projects whose `space_id` no longer exists,
  orphaned invitations. Every scan is bounded (`LIMIT` + cursor) and runs on a
  sparse tick: the space module deliberately put its full-table aggregate on a
  slower schedule than its worker for this exact reason
  (`modules/space/member_removal.go:281-284`). Metrics: project and member count
  distribution, admission-rejection counts *broken down by entry point* (that
  breakdown is what exposes a missed write path), reconcile violation count.
- Audit entries for create, disband, member add/remove, role change.

**PR-2 — invite codes and self-service join**

The endpoint shapes mirror `modules/space`'s invite/join surface rather than
inventing a parallel one (`modules/space/api.go:157-171`):

- `octo_project_invitation` (`invite_code` unique, `project_id`, `creator`,
  `max_uses`, `used_count`, `expires_at`, `status`), 7-day default expiry.
- Anonymous preview, mounted on an auth-less group with the strict IP limiter
  applied **per route** (that is how space does it, not group-wide):
  `GET /v1/projects/invite/:invite_code` — and `/preview` if the client needs the
  richer payload, matching `open.GET("/invite/:invite_code"[/preview])`. Limiter:
  `StrictIPRateLimitMiddleware(ctx, rlRedis, "project_invite", 10.0/60, 5)`,
  i.e. the same 10-per-minute / burst-5 budget as `space_invite`.
- **Redemption is `POST /v1/projects/join`, not `/invite/:code/accept`.** Space
  redeems a code through `joinLimited.POST("/join", s.joinSpace)` under
  `AuthMiddleware` + `SharedUIDRateLimiter`, with the code in the body; one
  endpoint covers both open join and code join. Reusing that shape keeps
  `join_mode = 0` (open) and `join_mode = 1` (code) on one code path, and avoids a
  second anonymous-looking URL that in fact requires a login. The body carries
  either `project_id` (open join) or `invite_code`.
- Management, under the project auth chain:
  `POST /v1/projects/:project_id/invite` (create),
  `GET /v1/projects/:project_id/invites` (list — space has it, and without it an
  admin cannot see or revoke what they issued),
  `DELETE /v1/projects/:project_id/invite/:code` (revoke → `status` invalid).
- Concurrent consumption relies on the unique constraint plus an atomic
  `UPDATE … SET used_count = used_count + 1 WHERE invite_code = ? AND status = 'valid'
  AND expires_at > ? AND used_count < max_uses` (zero affected rows ⇒ reject);
  revocation flips `status` and clears the cache immediately.

Routing note: `/v1/projects/invite/:invite_code` and `/v1/projects/:project_id`
coexist without conflict — verified against this repo's gin (v1.9.1), which
accepts a static segment and a `:param` as siblings and prefers the static one.
`project_id` is 32 hex characters, so no real project can be shadowed by the
literal `invite`.

`join_mode` defaults to 1 (invite only), which is why the invitation table and
these endpoints belong in P0 rather than later: without them the default
configuration has no join path, and the public preview endpoint carries the
brute-force requirement that is cheaper to land with the module than to retrofit.

## Non-regression: Space, group, thread, DM and IM transport

**Stated commitment: P0 changes no existing messaging behaviour.** No existing
table gains a column, no existing row is rewritten, no existing route's request or
response shape changes, no WuKongIM call is added or altered, and no read path
gains a filter. Specifically untouched: Space membership and its middleware,
group creation and group membership, thread membership, DM (including
`dm_space_presence` and the symmetric fake channel IDs), `category`, the sidebar
and conversation sync endpoints, and every channel-ID derivation in
`pkg/space/channel.go`.

That claim is only worth writing down next to the places where it could fail.
P0 has exactly three contact points with running code, and each one gets a
verification, because "we added a new module" is not by itself a guarantee:

**C1 — the module is registered into the running server.** A blank import in
`internal/modules.go` plus `register.AddModule()` means any `init()` or `Route()`
failure takes down the whole process, not just Project endpoints. This is not
hypothetical: the i18n helper `mustLookupSharedCode` is *designed* to panic at
init when a code is unregistered. A single unregistered shared code therefore
turns into "no IM service at all".

**C2 — the Project step joins the Space member-removal cleanup job.** That job is
an existing security cascade shared with the group and conversation cleanup steps.
A Project step that returns errors indefinitely keeps its job being re-claimed,
consuming lease cycles and batch slots that unrelated removals need — degrading a
path that today removes people from groups. A step that panics is recovered one
level up, but still burns a full lease cycle per attempt
(`modules/space/member_removal.go:371-381`).

**C3 — new middleware, new reconcile job, new cache keys share Redis and MySQL
with everything else.** Unbounded reconcile scans and a chatty membership cache
compete with message paths for the same connections.

### Non-regression acceptance

- [ ] The server boots with the module registered and all routes mounted — an
      end-to-end smoke test, not a unit test, because the failure mode is a panic
      during init/Route (C1).
- [ ] The full existing test suites for `modules/space`, `modules/group`,
      `modules/thread` and `modules/message` pass unchanged, with no test file
      edited to accommodate this change. If any existing test needs editing, that
      is the signal that behaviour changed and it must be explained in the PR,
      not accommodated.
- [ ] A Space member removal still removes the user from their groups with the
      Project step registered and **deliberately failing** — proving the new step
      cannot starve the existing ones (C2). Asserted on the existing steps'
      outcome, not on log output.
- [ ] `git diff --stat` for the PR touches no file under `modules/group/`,
      `modules/thread/`, `modules/message/`, `pkg/space/channel.go`, or any
      existing `sql/` migration. New files plus `internal/modules.go` plus
      `pkg/errcode/project.go` plus the i18n locale file only.
      **Amended (round 2):** plus `modules/space/member_removal_registry.go`, a new
      read-only accessor (`MemberRemovalCleanupStepNames`) added so a downstream test
      can assert the cleanup step is actually registered — a round-1 finding was that
      nothing verified it. New file, read-only, no behaviour change in that module.
- [ ] Every reconcile query is bounded by `LIMIT` and cursor; none is an
      unbounded full-table `JOIN` on `space_member` or `user` (C3).
- [ ] Project cache keys are namespaced `project:*` and collide with no existing
      key prefix — checked against the `space:member:*` and `ratelimit:*`
      namespaces in particular.


## Acceptance

Machine-checkable unless noted. Integration tests use
`testutil.NewTestServer()` + `testutil.CleanAllTables(ctx)`; the test database is
created with an explicit `COLLATE utf8mb4_general_ci` and each test package gets
a fresh one; setup resets `ratelimit:uid:*` because the shared UID bucket lives
in Redis and `CleanAllTables` does not clear it; setup installs an error renderer
via `SetErrorRenderer` or assertions on `error.code` will not see a code.

**Schema**

- [ ] Up migration applies to a fresh database; down migration drops all three
      tables. Migrating up → down → up leaves no residue.
- [ ] `space_id` / `uid` column width, charset and `COLLATE` match
      `space.space_id` / `user.uid`; a test joins `octo_project_member` to
      `space_member` and to `user` without error 1267.
- [ ] Insert and update through the real DAO succeed with the generated
      `active_name` column present (guards against error 3105).
- [ ] Create → disband → create the same name in the same Space succeeds;
      creating a duplicate *active* name fails with the registered code.

**Invariant I1**

- [ ] Adding a uid that is not an active `space_member` of the project's Space is
      rejected, on both the direct-add path and the invite-accept path.
- [ ] A uid active in Space A cannot be added to a project in Space B.
- [ ] Removing a user from a Space deactivates their `octo_project_member` rows
      across every project in that Space once the cleanup job runs, and the
      cleanup step is idempotent (running the job twice produces the same final
      state, no error, and **no second `member_epoch` bump**).
- [ ] Banning a Space (`space.status = 2`) does **not** deactivate any
      `octo_project_member` row, and un-banning needs no repair — the step
      follows `CheckMembershipForCleanup`, not `CheckMembership`.
- [ ] The reconcile job flags an I1 violation injected directly by SQL, and does
      **not** flag a `(space_id, uid)` pair whose cleanup job is still
      pending/running, nor members of a banned Space.
- [ ] A cleanup job that reaches `abandoned` with the Project step unfinished
      raises its own distinct alert — it is a real leak, not an in-flight window.

**member_epoch**

- [ ] `member_epoch` strictly increases across each of: add, remove, leave, role
      change, Space cascade, disband.
- [ ] It does *not* change on a no-op write (re-adding an existing active member,
      setting the role a member already has).
- [ ] It is incremented in the same transaction as the membership write: a forced
      rollback leaves both the membership row and the epoch unchanged.
- [ ] It appears in the Project list and detail responses alongside `my_role` and
      the capability bits.

**Authorization and enumeration**

- [ ] Every Project route returns 401/403 when called with no `X-Space-ID` and no
      `space_id` query parameter — i.e. the routes do not inherit
      `SpaceMiddleware`'s pass-through.
- [ ] `GET /v1/projects/:project_id` on an `unlisted` project returns the same
      shape to a non-member as a nonexistent project (no existence oracle);
      members and Space admins get the real payload.
- [ ] An admin cannot remove or demote another admin or the owner; the last owner
      cannot leave or be demoted without a transfer; transfer is atomic.
- [ ] A member removed from a project is denied on the very next request, not
      after the cache TTL (proves synchronous invalidation).
      **Scoped (round 2):** this holds for the non-racing interleaving, which is what
      synchronous invalidation buys. It does NOT hold against a cache-aside race — a
      request that missed and read an active role can reinstate a positive entry for a
      full TTL after a concurrent removal DELs the key. Impact is read-only: every
      write path re-reads the role in-transaction under the project lock, so the stale
      positive can gate a read but cannot authorize a write. Closing it properly means
      versioning the key on `member_epoch`, which is P1 work.
- [ ] Invite-code brute force is throttled by `StrictIPRateLimitMiddleware`; an
      invalid, expired, revoked and exhausted code all return the *same* generic
      code.
- [ ] Concurrent consumption of a `max_uses = 1` invite admits exactly one user.
- [ ] An invite past `expires_at` is rejected; the default expiry is 7 days.
- [ ] `POST /v1/projects/:project_id/join` succeeds only when `join_mode = 0`, and
      still enforces I1 and the member quota.

**Quotas, audit, observability**

- [ ] Each quota rejects at its boundary with its own registered code: projects
      per creator per Space (100), projects per Space (1000), projects created
      per day per user. Each limit is read from config, not a literal. The
      members-per-project cap (500) is the one exception, and it is deliberate:
      it rejects inside a batch endpoint whose contract is per-target outcomes, so
      it surfaces as a per-uid reason in a 200 response (other targets in the batch
      may have succeeded) rather than as a batch-level error code; the metric carries
      the same reason string.
- [ ] Create, disband, member add, member remove and role change each write an
      audit entry carrying the actor, the target and the reason.
- [ ] The admission-rejection metric is broken down by entry point — that
      breakdown is the signal that exposes a missed write path in P1, so a
      single undifferentiated counter does not satisfy this.
- [ ] The reconcile job also flags: an `octo_project` row whose `space_id` no
      longer exists, and an invitation whose project is disbanded.
- [ ] `is_official` is never written by any P0 code path (D6). A source-level
      check or a test asserting it stays 0 through the full CRUD surface.

**Conventions**

- [ ] `TestProjectNoLegacyResponseError` source guard covers every handler **and
      middleware** file in the module — the `SpaceMiddleware` it is modelled on
      uses raw `c.AbortWithStatusJSON(gin.H{"msg": …})`, so a guard scoped to
      `api*.go` would miss the most likely place for that pattern to be copied.
- [ ] `make i18n-extract`, `make i18n-extract-check`, `make i18n-lint` all pass;
      every new code has a zh-CN entry in `pkg/i18n/locales/active.zh-CN.toml`;
      no new entry in `tools/lint-direct-error-response/baseline.txt`.
- [ ] `SharedUIDRateLimiter` is mounted after `AuthMiddleware` on every
      authenticated group (a test asserting 429 under load would silently pass
      even when mounted wrongly, so this is asserted on the route chain).
- [ ] Each new route has a `pkg/authtree` census entry, including the explicit
      "`uk_*` stays Space-scoped, intentionally unconstrained" record.
- [ ] `go vet ./...` and `golangci-lint run ./modules/project/... ./pkg/errcode/...`
      are clean. `go build` alone is not sufficient — it does not compile
      `_test.go` files.
- [ ] With `project_create_enabled` off: create/update/disband/member-write/invite
      return 403; list and detail still succeed.
- [ ] Cross-module check: no existing module's integration test asserts on
      responses this change alters (P0 adds routes rather than changing any, so
      this is expected to be vacuous — but it is checked, because a previous
      migration was caught by a reviewer exactly here).

**Not machine-checkable, required before merge**

- [ ] `modules/project` does not import `modules/group` (enforced by the fact
      that `modules/group` will import `modules/project` in P1; a cycle would not
      compile then). P0 additionally has no reason to reference group at all.
- [ ] Lock order documented and followed: space → project → group →
      group_member → octo_project_member.

## Rollback

Turning `project_create_enabled` off stops new Projects and freezes all
membership writes while leaving existing rows readable. Full revert is dropping
the three `octo_project*` tables and removing the blank import from
`internal/modules.go`: nothing else in the codebase references them, no existing
table gains a column, and no existing row is modified — which is the whole reason
P0 is scoped this way.

## Open questions (product, not blocking the schema)

- **Who may create a Project?** Implemented as "any Space member, subject to
  quota", with the flag available to tighten it. If the answer is admin-only, it
  is a permission check at one call site.
- **A Project whose only owner is removed from the Space.** The cascade closes their seat, so
  the project is left active with no owner — and in P0 role change and disband are owner-only
  while a Space admin has read access only, so nobody can rename, disband or re-own it. Both
  candidate resolutions are product calls, which is why P0 implements neither: auto-promoting
  the senior remaining member changes who controls a project without anyone asking, and
  auto-disbanding destroys data. P0 logs it at Warn and leaves the row intact. Note this is the
  same end state the repo already accepts one layer down — `handOverGroupCreator` leaves an
  ownerless group when nobody can inherit — so it is not a new class of state. Likely landing
  place is the P2 admin surface (the same endpoint that owns `is_official`), which could adopt
  or disband such a project explicitly.
- **Disbanding a Project with groups in it** — groups revert to Space-direct
  (`project_id = ''`) versus being disbanded with it. Only bites in P1; P0
  disband touches no group.
- The prototype's "customer members under independent permissions" narrative
  collides with two v1 constraints (no external members; Project is not a read
  boundary). Needs a product decision before that copy ships, so users do not
  form the expectation that external parties can be admitted today.
