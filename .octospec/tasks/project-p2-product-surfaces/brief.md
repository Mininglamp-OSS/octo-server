---
type: Task
title: "Task: project-p2-product-surfaces"
description: The four product surfaces P0, P1 and P2 all deferred to a sibling brief that was never written — the project-scoped group list a client needs to render the 群聊 tab, `project_id` on the sidebar (the half #855 does not cover), `join_mode = 0` self-join, `is_official` management, and per-user project pinning. P1 gave a Project groups and enforced I2 on the write side; #855 gives it an all-member group; neither ships a way to ask which groups a project has.
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
>
> **Depends on #855** (`feat(project): give every project an all-member group (P2)`,
> OPEN, in review, base `5dd80d4`). Every line reference below is measured at
> `main@5dd80d4`; #855 touches 61 files including `modules/group/service.go`,
> `modules/group/db.go`, `modules/project/{api,db,model,service}.go`, so they will
> move. Two of its changes are load-bearing for this brief and are folded in below:
> it ships the all-member group, and it ships **most of PR-2 already**.

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
  serialized. (#855 fixes exactly this one — see below. The sidebar's own item struct,
  which #855 does not touch, still cannot say it.)
- The query the endpoint needs already exists and is unexported:
  `queryProjectGroupNosWithActiveMember` (`modules/group/db.go:1316`), whose only caller
  is the member-removal cascade worker (`modules/group/project_cascade.go:101`).

So a client cannot list a project's group chats, and cannot even tell that a group it
*can* already see belongs to a project. The 群聊 tab of the 2026-09-07 prototype is not
expressible against HEAD. That is the gap this task closes.

Four surfaces, in the order they unblock things:

1. **`GET /v1/projects/:project_id/groups`** — the caller's groups within one project (PR-1).
2. **`project_id` on the sidebar** — the last hop of the passthrough #855 starts (PR-2). SHIPPED.
3. **`join_mode = 0` self-join** — a column P0 shipped with no writer and no reader (PR-3).
4. **`is_official` management** (PR-4) and **per-user project pinning** (PR-5).

PR-1 and PR-5 ship together in #861 at the requester's direction (2026-09-08). They are
independent features sharing a branch, which is a review cost paid deliberately, not a
dependency.

PR-1 and PR-2 are independently shippable and are the whole of the unblock. PR-3 to PR-5
carry their own product questions and must not hold PR-1 up.

### What #855 already does, so this brief does not

- **`GroupResp.ProjectID`** — added, with the disclosure argument written out in the field's
  own comment (「客户端要靠它把项目群归到项目名下展示」), and populated in **both** mappers
  (`from` and `fromModel`). That covers `GET /v1/group/my` on both branches and the group
  detail. Nothing is left for this brief there.
- **The all-member group** — provisioned with the project, `octo_project.all_member_group_no`
  on the project `Resp`, invariant I4 (the group's active member set *equals* the project's)
  and two reconcile scans behind it. So the prototype's 全员群 row is real, and PR-1 will list
  it like any other project group. The client can label it by comparing `group_no` against the
  `all_member_group_no` it already has from the project detail — PR-1 adds no flag for that.
- **The import ban is now a test.** `pkg/project/import_guard_test.go` pins
  `modules/project → modules/group` at zero, and #855's own group-side work is registered
  *into* project through `modules/project/all_member_group_registry.go`. D2 below is
  unchanged by this — it gets stricter.

What #855 leaves for PR-2 is the **sidebar**: `modules/message/api_sidebar.go` is not in its
61 files, and `SidebarItem` (`:107-137`) carries `space_id` / `my_source_space_id` and no
`project_id`. So a client rendering the 消息 list still cannot tell which of those
conversations belong to a project — the one remaining half of 「拿到一个群，问它属于哪个项目」.

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
- **`wire-contract` — `SidebarItem` gains a field (PR-2), and so does `group.InfoResp`.**
  `InfoResp` is the cross-module batch read shape (`GetGroups`), and adding `project_id` there is
  what makes the sidebar's copy free: the value was already in the row `QueryWithGroupNos`
  selects, it was simply never mapped out. Without it the sidebar would need a second batch on
  the hottest read path for a field already in the response.
- **`wire-contract` — `SidebarItem` gains a field (PR-2).** `modules/message/api_sidebar.go:107-137`
  is one of the hottest read payloads in the product, and its existing `SpaceID` comment
  (`:112-119`) already fixes the per-target-type contract a new `project_id` must match. #855
  makes the equivalent change on `GroupResp` and shows the failure mode to avoid: a field
  populated in one mapper and not the other reads `""` on one path and correctly on the other,
  which is worse than absent.
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
is nothing they could do with that — there is no self-join path into a group. The
all-member group is not a counter-example: every project member is already in it (#855's I4),
so it shows up in a membership-scoped list for everyone anyway, without the list having to
disclose anything. A name is the most sensitive field a group has at
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

Proposed field set, to confirm against the prototype: `group_no`, `name`, `is_named`,
`avatar_text`, `avatar_color`, `is_upload_avatar`, `member_count`. Anything beyond that needs an
argument. Q2 is resolved and does not narrow this — the columns are all on the one real `group`
table, and the test suite is what proves it.

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

**D6 — PR-2 is the sidebar only, and it inherits #855's disclosure argument rather than
re-deciding it.**

#855 already settled who may learn a group's `project_id`: a member of the group, who by I2 is
already an active member of that project, so the field tells them nothing their own project
roster does not. The sidebar is the same population — `SidebarItem`s are built from the
IM-returned conversation list, which only contains channels the caller is a current member of
(`modules/message/api_sidebar.go:560-578`). So the field carries no new disclosure there.

It must still stay off every unauthenticated or pre-admission surface. The one to check is the
public invite preview (`GET /v1/group/invite/detail`, `modules/group/api.go:191`), where a
`project_id` would leak project existence to an anonymous caller holding only an invite code.
At `5dd80d4` that handler builds its response as a hand-written `gin.H`
(`modules/group/api.go:5040-5100`) rather than through `GroupResp`, so it is not exposed by
#855's change and must not be "tidied up" onto `GroupResp` by this one. A test pins that.

**D6 amendment (post-review, requester-approved).** The paragraph above is wrong about one
population and the review found it. `SidebarItem`s are NOT uniformly built from channels the
caller is a current member of: without an `X-Space-ID` the handler skips Space filtering
entirely, and only `COMMUNITY_TOPIC` items get the fail-closed parent-membership check
(`filterThreadConvsByParentMembership`, which runs on every path). Plain `GROUP` items get no
membership predicate there, so a removed member whose IM conversation row outlives the removal
still receives the item. The disclosure argument D6 inherited from #855 therefore does not
cover that path.

Two options were put to the requester: keep the field everywhere (the marginal disclosure is an
opaque id no route will resolve for that caller), or ship it only where Space filtering ran.
The requester chose the gate. So `project_id` is emitted **only on the Space-scoped path**, for
every target type including the one that was checked — gating per type would let a topic and its
own parent group disagree inside one response, which is the property PR-2 exists to preserve.

Consequence to hand to the client teams: on the no-`X-Space-ID` path `""` means "not
evaluated", not "直属 Space", and the two are indistinguishable in the payload. It is written
into the `SidebarItem.ProjectID` comment. The underlying hole is NOT closed here — closing it
means a membership predicate for every group item — and belongs to
`project-p2-read-path-hardening`.

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

**D8 — Per-user project pinning (PR-5) is a new table, not a column. SHIPPED, with a cap.**

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

*As implemented*, three things the draft did not settle:

- **The write is a settings bag, not `/pin` + `/unpin`.** `PUT /v1/projects/:project_id/setting`
  with a pointer field, following `PUT /v1/groups/:group_no/setting` (top / mute / save /
  remark through one route onto `group_setting (group_no, uid)`). Two verb routes are simpler
  only while there is one preference; the second costs two routes and two handlers where this
  costs a key. PUT is also idempotent by contract, so "pin an already-pinned project" needs no
  answer invented for it.
- **The read is a field on the existing list, not a second endpoint.** A separate "my pinned
  projects" route would make the client fetch twice and merge — and the *order* has to be the
  server's, because OFFSET pagination needs a total order that a client-side merge cannot
  reconstruct. `ORDER BY IFNULL(s.pinned,0) DESC, s.pinned_at DESC, p.id DESC` keeps the
  pre-existing tail order byte-identical.
- **A cap of 6 pinned projects PER SPACE** (`OCTO_PROJECT_MAX_PINNED`, requester's call,
  2026-09-08), refused with `err.server.project.quota_pinned` carrying `max` in details.
  Per Space rather than per user because the pinned section is rendered inside one Space's
  list: a global budget would refuse a pin in the Space the caller is looking at because of
  pins in a Space they cannot see. Unpinning is never refused (it is the operation that frees
  a slot), re-pinning something already pinned is never refused (it adds no row), and a
  disbanded project releases its slot (its owner can no longer see it to unpin it).

The count and the write share a transaction so the check reads the snapshot the write lands in.
That is NOT atomicity against a concurrent pin by the same uid: two requests that both count 5
both insert, leaving 7. The window is a double-click inside a 2 rps shared bucket and its worst
outcome is one extra row the user can remove, so it does not justify serialising every pin
behind a lock. Recorded because "there is a transaction here" reads like "this is atomic".

### Also parked by P0/P1, still unowned at HEAD

P0 (`project-p0-foundation/brief.md:215`) and P1 (`project-p1-group-binding/brief.md:279-282`)
parked **seven** items as "P2". P2's brief reassigned only four of them to this task
(`project-p2-subsystem-integration/brief.md:1023-1024`). Of the remaining three, one is
decided here (the nested tree — D4) and one is `project_id` passthrough, which #855 lands for
`GroupResp` and PR-2 finishes on the sidebar. **Two were
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
- ~~**Q2 — What is the `group` stub schema?**~~ **RESOLVED: there is no stub in this repo, and
  it does not constrain D3.** `reconcile_p1.go:191-198` claims that 「binaries whose migration
  set does not include `modules/group` get a `group` STUB instead, and that one declares status
  nullable」, and adds a defensive `g.status IS NOT NULL` on the strength of it. Chased down:

  - The repo contains exactly **one** `group` DDL — `modules/group/sql/20191106000002_group_legacy01.sql:5`
    — and it declares `status smallint not null DEFAULT 0`.
  - `octo-lib@v0.0.0-20260811160929` contains **no `.sql` files at all** and no table-creating Go
    code.
  - Tests DO run migrations, per test binary, from whatever module set is registered.
    `testutil.NewTestServer` sets `cfg.DB.Migration = false`, but that flag gates nothing here:
    `module.Setup` calls `executeSQL` unconditionally (octo-lib `module/module.go:29`). An
    earlier draft of this entry read the flag and concluded the opposite; running the suite
    refuted it directly. **The per-binary divergence is real and observable**: running
    `./modules/group/` against a database that `./modules/project/` had just migrated fails at
    startup with `Unable to create migration plan because of 20191106000001_event_legacy01.sql:
    unknown migration in database` — modules/project's test binary registers all 40 modules (its
    external test package blank-imports `octo-server/internal`), modules/group's registers
    fewer, and sql-migrate refuses a database carrying records its own source does not know.
    Each package therefore needs a fresh `test` database.
  - But that divergence produces a MISSING TABLE, not a different one. A binary without
    modules/group's migrations has no `group` table at all, so a query against it fails loudly
    with 1146 rather than quietly loading a NULL into a Go bool. The stub the comment describes
    is a third thing, and nothing in this repo creates it.
  - Production is a single binary: `main.go` → `internal/modules.go` blank-imports 40 modules, so
    every migration always runs together.

  The only place such a divergence could live is **another repository** — `init-db.sql` in
  `octo-deployment` (`tools/migrate-rename/rewrite_initdb.go:15`), which is outside this repo and
  was not inspected. If it turns out to define `group` differently from the migrations, that is a
  deployment-seed drift bug and its own task, not a constraint on a response shape.

  **Consequence for D3:** pick the field set from what the client needs, and let the test suite
  prove the columns exist. It now has: PR-1 selects `is_named` / `avatar_text` / `avatar_color` /
  `is_upload_avatar` from `modules/project` and `go test -race -shuffle=on ./modules/project/`
  is green. Leave `reconcile_p1.go`'s defensive predicate alone; it is harmless and removing it
  is not this task's business.
- ~~**Q3 — Does the prototype's 群聊 tab need groups the caller is not in?**~~ **RESOLVED
  2026-09-08: no — the list is the caller's own groups.** D1 stands as written and is
  implemented as the query predicate rather than as a filter over a wider result. Still
  reversible in the additive direction only: a `?scope=all` variant can be argued later on its
  own merits; narrowing after shipping the wider set could not.

## Out of scope

- **The all-member group (全员群) itself.** #855 creates it, owns invariant I4, and exposes
  `all_member_group_no` on the project `Resp`. PR-1 **lists** it — it is a project group like
  any other — and creates, names, protects and repairs nothing about it. In particular the five
  operations #855's D7 refuses on an all-member group (disband, exit, member removal, owner
  transfer, blacklist) are refused in `modules/group`'s handlers and stay there; PR-1 must not
  grow a second copy of that verdict just because it happens to be listing the group.
- **Listing groups the caller is not a member of.** D1, pending Q3.
- **Nested threads in the group-list response.** D4.
- **Unread / badge state.** D5.
- **Read-path hardening** — `sidebar/sync`'s `ExistMembersActive` backstop, the deprecated
  `/v1/coversations` filter, the group-avatar enumeration oracle, `querySavedGroups` after
  leaving. Its own brief, named `project-p2-read-path-hardening` by
  `project-p2-subsystem-integration/brief.md:1015-1022`. PR-2 adds a field to the sidebar
  payload; it does not touch the `ExistMembersActive` question, and must not be read as having
  settled it.
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

**PR-2 — `project_id` on the sidebar** (the rest landed in #855) — SHIPPED as implemented below:

- **On a request carrying `X-Space-ID`**: `project_id` present and correct on the
  `/v1/sidebar/*` payload for a project group, `""` (omitted) for a Space-direct group, and
  `""` for a DM — the same three-way split `SidebarItem.SpaceID` already documents.
- **On a request WITHOUT `X-Space-ID`**: `""` for every item, deliberately. See the amendment
  to D6 — this was not in the brief as written and is a requester-approved narrowing, added
  after review. Stated here rather than only in D6's rationale, because this section is what a
  future contributor implements against, and read without it the gate looks like a bug to
  remove.
- For a COMMUNITY_TOPIC item on the Space-scoped path, `project_id` is the **parent group's**,
  matching how `SpaceID` is resolved for that target type. Asserted, not assumed.
- No extra per-item query: the value must come from the batch the sidebar already runs, not a
  per-group round-trip on the hot read path.
- `project_id` still absent from the public invite-preview response (D6), pinned by a test.
- Regression: `GroupResp.project_id` from #855 still correct on both mappers after the rebase.
- ~~`TestGroupNoLegacyResponseError`'s fixed file list updated~~ — **no longer applies.** That
  guard now DISCOVERS its file list from the package directory (`modules/group/api_i18n_test.go:45`),
  changed with the note that "a hard-coded list cannot see a new file — which is the failure mode
  that matters, because nobody adds a handler by editing api.go". The brief's load-bearing list
  said otherwise and was correct when written; recorded here rather than silently dropped.

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
