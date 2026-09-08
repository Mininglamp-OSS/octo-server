---
type: Task
title: "Task: project-p2-product-surfaces"
description: The four product surfaces P1 and P2 both deferred to a sibling brief — the project-scoped group list a client needs to render the 群聊 tab, `project_id` passthrough on the group reads that already exist, `join_mode = 0` self-join, `is_official` management, and per-user project pinning. P1 and P2 built a Project that owns groups and enforces I2 on the write side; nothing on the read side lets a client discover that a group belongs to a project at all.
tags: ["space", "isolation", "acl", "wire-contract", "error-response", "i18n", "rate-limit", "testing", "commit", "migration"]
timestamp: 2026-09-08T00:00:00Z
# --- octospec extension fields ---
slug: project-p2-product-surfaces
upstream: self
source: self
---

# Task: project-p2-product-surfaces

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> **Status: DRAFT.** Q1 (`is_official` ownership) is open and gates PR-4 only.
> Named by `.octospec/tasks/project-p2-subsystem-integration/brief.md:1023-1024`,
> which is where these four items were parked.

## Goal

Make a Project's groups **readable**.

P1 (#846) gave a group an owner — `group.project_id`, one admission entry, and
invariant I2 behind it. P2 PR-5 (#850) provisioned the subsystem containers. Neither
shipped a way to ask *which groups belong to a project*, and neither made an existing
group read admit which project it belongs to. Measured at HEAD:

- `modules/project/api.go:175-201` registers nine routes. All nine are the project row
  and its member roster. None returns a group.
- `GET /v1/group/my` (`modules/group/api.go:967`) branches on `c.Query("space_id")` only.
  Repo-wide there is **zero** occurrence of a handler reading a `project_id` query
  parameter.
- `GroupResp` (`modules/group/service.go:905-951`) carries `space_id` and **not**
  `project_id`. `Model.ProjectID` (`modules/group/db.go:858`) exists and is never
  serialized.
- The query the endpoint needs already exists and is unexported:
  `queryProjectGroupNosWithActiveMember` (`modules/group/db.go:1316`), whose only caller
  is the member-removal cascade worker (`modules/group/project_cascade.go:101`).

So a client cannot list a project's group chats, and cannot even tell that a group it
*can* already see belongs to a project. The 群聊 tab of the 2026-09-07 prototype is not
expressible against HEAD. That is the gap this task closes.

Four surfaces, in the order they unblock things:

1. **`GET /v1/projects/:project_id/groups`** — the caller's groups within one project (PR-1).
2. **`project_id` passthrough** on the group reads that already exist (PR-2).
3. **`join_mode = 0` self-join** — a column P0 shipped with no writer and no reader (PR-3).
4. **`is_official` management** and **per-user project pinning** (PR-4, PR-5).

PR-1 and PR-2 are independently shippable and are the whole of the unblock. PR-3 to PR-5
carry their own product questions and must not hold PR-1 up.

## Background

### What P0/P1/P2 already decided, that this brief must not re-open

- **Space is the only security boundary.** `pkg/authtree/authtree.go:101-113` records it
  in as many words: 「Project is explicitly not a read boundary — Space remains the only
  security boundary」. Every route here is gated on the caller's own Space membership
  first; the Project dimension is a **filter**, never the gate. A group list that reads
  as "Project is now a read boundary" is the wrong shape even if it returns the right rows.
- **Anti-enumeration is uniform.** `projectMiddleware` (`modules/project/middleware.go:258-340`)
  folds three refusals into one indistinguishable response: the project does not exist; it
  is in a Space the caller has no seat in; it is unlisted and the caller is neither a member
  nor a Space admin. Any new project-scoped route inherits that, unchanged.
- **I2 is a ceiling, not a floor.** A project group's active members are a *subset* of the
  project's active members. Project membership does **not** imply membership of any
  particular group in it. D1 below is the direct consequence.
- **I3 makes attribution immutable.** `group.project_id` is written exactly twice in the
  tree — the create path and the disband detach (`modules/group/db.go:1444-1450`), pinned by
  a source guard (`TestNoProjectIDRewritesOutsideTheDetachStep`, `modules/group/admission_guard_test.go:134`). Nothing here writes it.
- **Three P0 columns still have no writer**: `join_mode`, `is_official`, and
  `discoverability`'s unlisted value (`modules/project/sql/20260904000001_project_core.sql:86-89`).
  Their own comments assign them here. P2 explicitly claimed none of them
  (`project-p2-subsystem-integration/brief.md:70-74`).

### Where these four items were parked

- P0 and P1, identically: 「**`GET /v1/projects/:project_id/groups`**, the nested
  Project → Group → Thread response, `project_id` passthrough in sidebar / group detail /
  group list, project pinning, the `is_official` management endpoint, Project-level
  auto-join, and the member-picker data source — all **P2**」
  (`project-p1-group-binding/brief.md:279-282`; `project-p0-foundation/brief.md:215`).
- P2: 「**The product surfaces**: `join_mode = 0` self-join, `is_official` management,
  per-user pinning, `GET /v1/projects/:project_id/groups`. Own brief.」
  (`project-p2-subsystem-integration/brief.md:218-219`, repeated at `:1023-1024`).

This is that brief.

## Load-bearing list

- **`space` / `isolation` / `acl` — the Space gate and the anti-enumeration contract.**
  Every route added here mounts `AuthMiddleware` → `SharedUIDRateLimiter` → `projectMiddleware`,
  in that order (`modules/project/api.go:185-189`), and `projectMiddleware` is what writes the
  verified Space via `spacepkg.SetSpaceID` — *after* the Space membership check, never before
  (`middleware.go:315-319`). A new handler that reads `GetSpaceID` without that ordering gets a
  Space the caller has no seat in.
- **`wire-contract` — `GroupResp` gains a field (PR-2).** `modules/group/service.go:905-951` is
  consumed by `GET /v1/group/my`, group detail, and the sidebar. An added field is additive,
  but it is a wire contract with existing clients and both mappers must populate it
  (`from` at `:953`, `fromModel` at `:1000`) or the field silently reads `""` on one path and
  correctly on the other — worse than absent.
- **`error-response` / `i18n` — the envelope.** `httperr.ResponseErrorL` + a registered
  `pkg/errcode` code only. The per-module helpers already exist
  (`modules/project/api_i18n.go`: `respondProjectNotFound`, `respondQueryFailed`,
  `respondForbidden`, `respondParamInvalid`). `TestProjectNoLegacyResponseError`
  (`api_i18n_test.go:96`) globs the module dynamically via `moduleSourceFiles`, so a new
  project file is covered automatically. **`TestGroupNoLegacyResponseError`
  (`modules/group/api_i18n_test.go:30`) does NOT — it scans a fixed file list**, so any new
  file under `modules/group` must be added to it by hand.
- **`rate-limit` — `SharedUIDRateLimiter`, mounted after `AuthMiddleware`.** Already on both
  project route groups. Tests that hit these routes must reset `ratelimit:uid:*` in setup;
  `CleanAllTables` does not clear it.
- **The `group_space_project` index.** `(space_id, project_id)` on `group`
  (`modules/space/sql/20260906000001_group_project_binding.sql:78`) is the index PR-1's query
  must actually use. It is also the first index ever to serve `group.space_id`, so it already
  changed the plan for `queryGroupsWithMemberUIDAndSpaceID` and `modules/bot_api/groups.go:51`.
  A new query that filters `project_id` without `space_id` cannot use it at all — the leading
  column is `space_id`, which is why `queryProjectGroupNos` takes both
  (`modules/group/db.go:1276-1285`).
- **Collation.** `group` / `group_member` are legacy (server default; production measured at
  `utf8mb4_0900_ai_ci`), `octo_project*` is pinned `utf8mb4_general_ci`. Any join that crosses
  the two needs an explicit `COLLATE utf8mb4_general_ci` on the legacy side or it is MySQL
  error 1267 in production while passing in CI — the exact failure
  `reconcile_p1.go:206-218` documents and `reconcile_p1_collation_test.go` pins. A join *within*
  the legacy side (`group` ⋈ `group_member`) must **not** be coerced: it would cost the index.
- **The `group` stub schema.** `reconcile_p1.go:191-198` records that binaries whose migration
  set excludes `modules/group` see a **stub** `group` table with a different shape (its `status`
  is nullable). A query from `modules/project` may only rely on columns present in both. Which
  columns those are must be established before the response field set is frozen (see Q2).
- **Import direction.** `modules/group` imports `modules/project`
  (`modules/group/project_cascade.go:8`). The reverse must never happen —
  `modules/project/cascade_registry.go:10-15` states it and the cascade registry exists to keep
  it true. `modules/project` reading the `group` **table** directly is already established
  practice (`reconcile_p1.go:200,311`) and is not the same thing as importing the package.
- **Disbanded groups.** `group.status = 2` rows keep their `group_member` rows on purpose —
  disband flips status only, and there is no endpoint that cleans them up
  (`modules/group/db.go:1287-1305`). A membership-driven list that omits the status filter
  returns dead groups.
- **`member_epoch` monotonicity (PR-3).** Every member/role write bumps it by exactly 1 in the
  same transaction, pinned by `TestMemberEpochOnlyEverIncrements` (`api_i18n_test.go:119`). A
  self-join is a member write and inherits that.
- **I1 (PR-3).** An active `octo_project_member` uid must hold an active `space_member` seat in
  the same Space. A self-join must re-check it inside the admission transaction, not from the
  cached middleware verdict.
- **`migration` (PR-5).** Pinning needs a new table. Placement rule, from
  `modules/space/sql/20260906000001_group_project_binding.sql:5-42`: a column ships from a
  migration directory present in every binary that reads it. Only `modules/project` reads a
  project-user setting, so `modules/project/sql/` is correct — the same reasoning that keeps
  `octo_project_member.removing` there. Time columns are application-written UTC with no
  `DEFAULT CURRENT_TIMESTAMP` and no `ON UPDATE`.
- **`testing` / `commit`** — repo conventions; Conventional Commits, English.

## Decisions

**D1 — `GET /v1/projects/:project_id/groups` returns the caller's OWN groups in that
project, not every group in it.**

I2 is a ceiling, not a floor: being in the project does not put you in its groups. Returning
every group would hand a project member the names of groups they hold no seat in, and there
is nothing they could do with that — there is no self-join path into a group, and the
all-hands group (P2 PR-0) is unshipped. A name is the most sensitive field a group has at
list granularity (「关键供应商来料异常」 tells you the incident exists), so the default is
membership-scoped.

The predicate already exists, disbanded-group filter included, as
`queryProjectGroupNosWithActiveMember` (`modules/group/db.go:1316-1329`). PR-1 reimplements
that shape in `modules/project` (D2) rather than exporting it across the forbidden import
direction.

Reversible on purpose: an admin-visible `?scope=all` is additive, and can be argued on its own
merits when a product surface needs it. Shipping the wider set first cannot be taken back.

**D2 — The handler lives in `modules/project` and queries `group` directly. No new registry.**

`modules/project` must never import `modules/group` (`cascade_registry.go:10-15`), and the
reverse import already exists, so the package dependency is settled. But reading the **table**
is not importing the package, and `modules/project` already does exactly that in production
code: the I2/I3 reconcile scans join `group` ⋈ `group_member` ⋈ `octo_project_member`
(`reconcile_p1.go:200,311`), and the module's own tests insert into `group`
(`reconcile_i2_test.go:36`). So the read view needs no cascade-registry-style indirection, and
a synchronous request path is the worst place for one anyway: an unregistered provider would
answer an empty list, which is indistinguishable from "this project has no groups".

The cost, recorded because it is real: `modules/project` cannot reuse `GroupResp`. That is
what D3 turns into a feature rather than a workaround.

**D3 — A narrow, purpose-built response — not a second `GroupResp`.**

`GroupResp` is 40-odd fields of per-user group state (`service.go:905-951`). Cloning it here
would create a second wire contract for the same state, and the two would drift the first time
one changes. This endpoint answers one question — *which groups in this project am I in* — so
it returns the identity fields the tree renders and nothing else, and the client fetches full
state through the routes it already calls (`GET /v1/groups/:group_no`, `GET /v1/group/my`).

Proposed field set, to confirm against the prototype and against Q2: `group_no`, `name`,
`is_named`, `avatar_text`, `avatar_color`, `is_upload_avatar`, `member_count`. Anything beyond
that needs an argument, and every field must be checked against the stub schema before it is
frozen.

**D4 — Threads are NOT nested in the response.**

P1's out-of-scope entry calls it 「the nested Project → Group → Thread response」, and the
nesting belongs to the client. `GET /v1/groups/:group_no/threads` already exists
(`modules/thread/api.go:185`) with its own pagination and its own access gate
(`modules/thread/api.go:349,429,519,643,941` gate on `ExistMemberActive` against the parent
group). A nested response would be `O(groups × threads)` with two pagination axes that cannot
both be expressed in one cursor, and it would fork thread access control into a second place.

**D5 — No unread counts, no badge state.**

The prototype's red badges come from the conversation layer (`sidebar/sync`). Serving them
here would make a second source of truth for a number that changes on every message, read
through a route with a 60-second membership cache. Out.

**D6 — PR-2 adds `project_id` to `GroupResp`, and that is a disclosure argument, not a
formality.**

Who learns what: only an existing member of the group learns which project it belongs to. By
I2 they are already an active member of that project, so the field tells them nothing their
own project roster does not. It does **not** go on any unauthenticated or pre-admission
surface — specifically not the public invite preview
(`GET /v1/group/invite/detail`, `modules/group/api.go:191`), where a `project_id` would leak
project existence to an unauthenticated caller holding only an invite code.

Both mappers must populate it (`from` at `service.go:953`, `fromModel` at `:1000`). One
populated and one not is the failure mode this decision exists to prevent.

**D7 — `join_mode = 0` self-join (PR-3) needs a writer before it needs an endpoint.**

`join_mode` today has no writer, no reader, and no wire field — `CreateReq` / `UpdateReq` /
`Resp` (`modules/project/model.go:141-153,193-211`) do not mention it. So PR-3 is three things,
and shipping only the third is the hazard P0's comment warns about: 「自助加入是 P2，本列在 P0
无消费方」.

1. `join_mode` becomes settable on create/update (owner-only, same capability as
   `discoverability`) and readable on `Resp`.
2. `POST /v1/projects/:project_id/join`, admitted only when `join_mode = 0` **and**
   `discoverability = space_listed` — an unlisted project is invisible to a non-member by
   `projectMiddleware`'s own rules, so a join route that answered it would be a new
   enumeration oracle on a surface that has none.
3. The admission goes through the existing entry, `admitMemberTx` (`modules/project/db.go:595`),
   inside a transaction that re-checks I1, the per-project member cap, and bumps `member_epoch`
   by exactly 1. Not a second write path.

No retro-consent problem exists: every row on disk is `join_mode = 1` (the DDL default) and
nothing has ever written the column, so enabling the semantics cannot silently open an existing
project. State that in the PR body — it is the question a reviewer will ask.

The self-join route also needs `StrictIPRateLimitMiddleware`-grade thinking: it is authenticated,
so `SharedUIDRateLimiter` is the right layer, but it is a *membership write* driven by the caller
alone, which is a new shape for this module. Quota interaction with
`ErrProjectQuotaPerSpace` / `ErrProjectQuotaDailyCreate` must be spelled out in the PR.

**D8 — Per-user project pinning (PR-5) is a new table, not a column.**

`octo_project_user_setting (project_id, uid, pinned, pinned_at, created_at, updated_at)`,
`UNIQUE (project_id, uid)`, `octo_` prefix, `utf8mb4_general_ci`, application-written UTC times
with no MySQL-side defaults. It lands in `modules/project/sql/` because only `modules/project`
reads it. It changes the ordering of `GET /v1/space/:space_id/projects`
(`listVisibleInSpace`), which is a wire-visible behaviour change to an endpoint that already
ships — pinned first, then existing order, and the existing order must not otherwise move.

A column on `octo_project_member` was considered and rejected: pinning is not a membership fact
(a Space admin can see a `space_listed` project they have not joined, and could reasonably pin
it), and every write to `octo_project_member` is on the epoch-bumping path — a pin is not a
membership change and must not bump `member_epoch`.

### Also parked by P0/P1, still unowned at HEAD

P0 (`project-p0-foundation/brief.md:215`) and P1 (`project-p1-group-binding/brief.md:279-282`)
parked **seven** items as "P2". P2's brief reassigned only four of them to this task
(`project-p2-subsystem-integration/brief.md:1023-1024`). Of the remaining three, one is
decided here (the nested tree — D4) and one is PR-2 (`project_id` passthrough). **Two were
never given a home by any brief, and are recorded here so they stop being invisible rather
than because this task claims them:**

- **Project-level auto-join.** Named identically in P0 and P1, and *not* obviously the same
  thing as P2's 「`join_mode = 0` self-join」 (PR-3 / D7). Two readings: (a) self-service join,
  in which case PR-3 already covers it and the two names should be merged; (b) new Space
  members are auto-admitted to designated projects — the Project analogue of Space preset
  groups (`joinPresetGroups`, `modules/space/api.go:1366`), which is a different feature with a
  different consent question. **Q4 below.**
- **The member-picker data source.** I2 means the "add members to this project group" picker
  must be scoped to the project's roster, not the Space's. `GET /v1/projects/:project_id/members`
  (`modules/project/api.go:420`) is that roster and is already members-only — but it takes
  offset/limit and **no keyword** (`p.db.listMembers(row.ProjectID, offset, limit)`), so a
  picker over a 1000-member project has to page the whole roster client-side to search it. The
  gap is one filter parameter on an endpoint that already exists, not a new surface. Small
  enough to fold into PR-1's PR if the human confirms; listed, not claimed.

Two further P1 out-of-scope entries also remain unowned and are **not** this brief's:
read-path hardening (named `project-p2-read-path-hardening` by P2 but never written — the
`sidebar/sync` comment P2 said must be rewritten is unchanged at
`modules/message/api_sidebar.go:555-580`), and 「tightening the Space membership check into the
admission transaction」, which P1 called a separate task and nobody has opened.

## Open questions

- **Q4 — Which feature is 「Project-level auto-join」?** See above. If (a), delete the name and
  fold it into D7. If (b), it needs its own consent argument and does not belong in this brief.
- **Q1 — Who may set `is_official`, and what does it assert?** 「官方项目徽标」 reads as a
  platform assertion, not a Space one. If a project owner can set it on their own project the
  badge means nothing. Two candidates: the manager console (`api_manager.go` pattern, e.g.
  `modules/group/api_manager.go:50`), or a Space admin. **Recommendation: manager console**, on
  the grounding that the badge's value is that its subject cannot grant it to themselves. Gates
  PR-4 only.
- **Q2 — What is the `group` stub schema, exactly?** `reconcile_p1.go:191-198` asserts it exists
  and differs, but the brief's author did not locate its DDL (`grep -ri 'create table.*\`group\`'`
  over `modules/*/sql` finds only `modules/group/sql/20191106000002_group_legacy01.sql:5`; the
  Go module cache was unavailable in this environment, so `octo-lib` was not searched). Resolve
  before freezing D3's field set: if the stub carries only `group_no` / `name` / `status` /
  `space_id` / `project_id`, the avatar fields cannot be selected from `modules/project`, and D2
  is back on the table.
- **Q3 — Does the prototype's 群聊 tab need groups the caller is not in?** D1 says no and is
  reversible. Confirm with product before PR-1 merges, because widening later is additive while
  narrowing later is a breaking change.

## Out of scope

- **The all-hands group (全员群).** Creating a group with the project is P2 PR-0
  (`project-p2-subsystem-integration/brief.md` D10) and is unshipped at HEAD — there is no
  `全员群` / `all_hands` anywhere in `modules/`. PR-1 lists whatever groups exist; it does not
  create one. The prototype's first row stays empty until PR-0 lands, and that is expected, not
  a defect in this task.
- **Listing groups the caller is not a member of.** D1, pending Q3.
- **Nested threads in the group-list response.** D4.
- **Unread / badge state.** D5.
- **Read-path hardening** — `sidebar/sync`'s `ExistMembersActive` backstop, the deprecated
  `/v1/coversations` filter, the group-avatar enumeration oracle, `querySavedGroups` after
  leaving. Its own brief, named `project-p2-read-path-hardening` by
  `project-p2-subsystem-integration/brief.md:1015-1022`. PR-2 adds a field to an existing
  response; it does not touch those gates.
- **Re-parenting a group between projects.** I3 forbids it; the source guard
  (`TestNoProjectIDRewritesOutsideTheDetachStep`, `modules/group/admission_guard_test.go:134`) must keep passing untouched.
- **Pending invitations** (`octo_project_invitation`, P2 D8) and **ownership transfer** — P2's,
  not this brief's.
- **Making Project a read boundary.** `authtree.go:101-113`. No route here changes the gate;
  they all filter within a Space the caller already holds a seat in.
- **Any change to `group.project_id`'s two writers**, to the I2 admission entry, or to the
  cascade.

## Acceptance

**Gates (all must pass):**

```bash
go test ./modules/project/... ./modules/group/... ./modules/thread/... ./modules/space/...
make i18n-extract && make i18n-extract-check && make i18n-lint
golangci-lint run ./...
```

**PR-1 — `GET /v1/projects/:project_id/groups`:**

- `TestListProjectGroupsReturnsOnlyMyGroups` — project member in 1 of 3 project groups gets
  exactly 1 row.
- `TestListProjectGroupsExcludesDisbandedGroups` — a `status = 2` group the caller is still a
  `group_member` of does not appear.
- `TestListProjectGroupsExcludesSpaceDirectGroups` — a group with `project_id = ''` in the same
  Space does not appear.
- `TestListProjectGroupsUnknownProjectIsNotFound`,
  `...CrossSpaceProjectIsNotFound`, `...UnlistedNonMemberIsNotFound` — all three produce the
  **byte-identical** envelope, asserted against each other, not merely against a status code.
- `TestListProjectGroupsIsUIDRateLimited` — with `ratelimit:uid:*` reset in setup.
- `TestListProjectGroupsPaginationIsBounded` — reuses `pageParams`; `?page=9223372036854775807`
  returns an empty page, not a 5xx (the defect `pageParams`' comment records).
- An `EXPLAIN` assertion that the query uses `group_space_project`, in the shape of
  `TestReconcileQueriesAreBounded`.
- A collation-drift case extending `reconcile_p1_collation_test.go`'s rig: the new statement
  must survive a legacy side at `utf8mb4_0900_ai_ci`, i.e. fail without the `COLLATE` and pass
  with it.
- `TestProjectNoLegacyResponseError` still green (automatic — the file list is dynamic).

**PR-2 — `project_id` passthrough:**

- `project_id` present and correct on `GET /v1/group/my?space_id=`, on group detail, and on the
  sidebar payload, asserted for a project group **and** asserted as `""` for a Space-direct
  group.
- Both mappers covered: a test that would fail if only `from` or only `fromModel` were changed.
- `project_id` absent from the public invite-preview response (D6), pinned by a test.
- `TestGroupNoLegacyResponseError`'s fixed file list updated if `modules/group` gains a file.

**PR-3 — `join_mode = 0` self-join:**

- `join_mode` round-trips through create → read → update → read; a non-owner cannot set it.
- Self-join succeeds only for `join_mode = 0` **and** `discoverability = space_listed`; the
  unlisted case returns the same not-found envelope as PR-1's three refusals.
- Self-join respects the member cap and I1 (a caller whose `space_member` seat was closed
  between the middleware's cached verdict and the transaction is refused).
- `member_epoch` increases by exactly 1 per successful join and not at all on a rejected or
  idempotent one; `TestMemberEpochOnlyEverIncrements` still green.
- Every project row on disk before this PR remains `join_mode = 1` — a migration-free assertion
  that the semantics cannot open an existing project.

**PR-4 / PR-5:**

- `is_official` writable only from the surface Q1 settles, never by a project owner on their own
  project (asserted).
- Pinning: pinned projects sort first in `GET /v1/space/:space_id/projects` and the unpinned
  order is unchanged; a pin does **not** bump `member_epoch`; `(project_id, uid)` is unique and
  a repeated pin is idempotent.
- The new table's migration passes the module's migration test (mind the apostrophe-parity
  hazard documented in `modules/project/sql/20260906000001_project_group_binding.sql`'s Down
  section — no apostrophes in SQL comments).
