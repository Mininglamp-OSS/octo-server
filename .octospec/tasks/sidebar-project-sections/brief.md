---
type: Task
title: "Task: sidebar-project-sections"
description: Make the 关注 tab's top-level list one user-orderable sequence mixing manual categories and actively joined Projects as peers, with Project entries provisioned on create or admit and removed on explicit unpin; every Sidebar-facing project_id ships its paired project_name; Project groups remain relation metadata, mutually exclusive with manual categorization, and never grant native chat access.
tags: ["space", "isolation", "acl", "wire-contract", "error-response", "i18n", "rate-limit", "testing", "commit", "migration"]
timestamp: 2026-09-09T00:00:00Z
# --- octospec extension fields ---
slug: sidebar-project-sections
upstream: self
source: self
---

# Task: sidebar-project-sections

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> **Status: SERVER IMPLEMENTED; CLIENT CUTOVER PENDING.** D1-D5 and the additive
> `project_name` contract are implemented in the server working tree. Project
> visibility is membership-only: Space-listed pinning without a Project seat does
> not create or restore a Sidebar entry. D6 remains deferred pending Q2; D7 remains
> a client-side ordering concern; native group membership synchronization is outside
> this Sidebar task and is not changed by its relation projection.
> Client integration contract: [`docs/sidebar-project-sections-api.md`](../../../docs/sidebar-project-sections-api.md);
> the coordinated client deployment is not claimed here.
>
> | | |
> |---|---|
> | **Confirmed** (requester, 2026-09-11) | The prototype mapping (Background §1); Project-group ↔ manual-category **mutual exclusion** (D3); category stays a per-user private view, never shared/admin-managed; Project creation and admission add the Project to the caller's 关注; only an active Project member may pin; explicit unpin removes it even while the caller remains a Project member; every Sidebar-facing `project_id` in this task is paired with `project_name`; relation metadata never grants native chat permissions |
> | **Implemented in this task** | D1 (new ordering table), D2 (reuse the final Project-group relation projection with SQL per-Project cap), D3 (mutual exclusion), D4 (auto-provision on create+admit and hide on unpin), D5 (old endpoints re-point their sort source), plus paired `project_name` on Sidebar-facing payloads |
> | **Deferred / unchanged** | D6 (`SidebarItem.ProjectID *string`) remains gated on Q2; D7 (全员群首位) remains a client concern; native group membership synchronization is outside this Sidebar task; ordinary Project-created and associated groups retain their existing independent semantics |
> | **Open** | Q2–Q4 and coordinated client rollout |
>
> Line references measured at `98d2092` unless marked otherwise.
## Goal

The 关注 (Follow) tab's top-level list becomes **one user-orderable sequence
whose entries are of two kinds**: the caller's manually-created categories,
and Projects in which the caller has an active membership seat. Both are draggable
against each other in one order. An eligible Project's group contents are the
Project's live associated groups, regardless of whether the caller has a native
`group_member` seat, including non-native or blacklisted native relations; these
groups are metadata only and do not grant chat or subarea access. A group that
belongs to a Project can never also be filed into one of the caller's own categories.

Creating or joining a Project auto-adds its entry to the caller's list, the way a
default category is auto-provisioned today. Only an active Project member may use
#861 pinning to add or restore the entry. A Space member without a Project seat
cannot make a Space-listed Project visible by pinning, and a historical non-member
pin is ignored by the read backstop. Pinning never grants Project or group membership.
Explicitly unpinning is a durable personal opt-out: it removes the Project from
关注 even if the caller still has an active Project seat, and a later pin restores
the retained ordering entry.

## Background

### 1. The prototype, and what each row is

Confirmed against the 2026-09-09 prototype screenshot by the requester:

```
关注 | 最近                      <- POST /v1/sidebar/sync, tab=follow|recent
─────────────────────────────
⊞ 供应链运营协同        📋      <- a PROJECT entry (section_type=project).
   全员群                          The ⊞ / 📋 affordances are Project-only;
   采购与招投标  @我 4  ˅          a category header has no equivalent.
      本季度间接采购需求  3        <- 话题/threads inside a group. Existing,
      供应商询价与比价   1            separate concern; not this task.
      招投标文件评审
   质量与排产      1     ˅
   合规与合同            ˅
˄ 其他会话 (9)          3  ⋯   <- a manual CATEGORY (section_type=category)
```

- `供应链运营协同` is a **Project**, rendered as a top-level entry in the same
  list as categories — not a switcher above the list, and not a drill-down
  context the rest of the list is filtered by.
- `其他会话` is a **manual category**, i.e. today's `group_category` row.
- The rows under the Project (`全员群`, `采购与招投标`, …) are its **groups**;
  the rows under those are **threads**, which already have their own endpoint
  and gate (`GET /v1/groups/:group_no/threads`) and are out of scope.

### 2. What the code does today

- `modules/category` (`group_category`, `group_setting.category_id`) is a
  **per-user private folder**, scoped `(uid, space_id)` —
  `modules/category/model.go:5`, 「群组类别（用户个人视图）」. Ordering lives in
  `group_category.sort`, written by `PUT /v1/spaces/:space_id/categories/sort`
  (`modules/category/api.go:418`).
- It has no Project dimension at all: `queryUserGroupsInSpace`
  (`modules/category/db_group_setting.go:54-75`) pulls every group the caller
  is an active member of in the Space and buckets by `category_id`, with no
  `project_id` predicate. Project groups therefore already appear in the
  category tree today, undifferentiated.
- `group.project_id` (`modules/space/sql/20260906000001_group_project_binding.sql:58-59`)
  is the attribution column — `VARCHAR(40) NOT NULL DEFAULT ''`, sentinel `''`
  = 直属 Space, never `NULL`. Index `group_space_project (space_id, project_id)`
  at `:78`.
- `GET /v1/projects/:project_id/groups` (`project-p2-product-surfaces` D1-D3,
  `modules/project/api_group.go:41`) answers which live groups are associated
  with this Project: Project-relation-scoped, disbanded-excluded, pinned-first,
  and independent of the caller's native `group_member` seat. Its narrow
  `ProjectGroupRelation` field set is the shared Sidebar contract.
- `POST /v1/sidebar/sync` (`modules/message/api_sidebar.go:189-196`, `tab` ∈
  {`follow`,`recent`}) is the endpoint behind the screenshot. Its `SidebarItem`
  (`:107-152`) carries `CategoryID *string` and `ProjectID string` as two
  **parallel, unrelated** fields. This task adds `ProjectName string` whenever
  a Space-scoped item carries a non-empty `project_id`; the name is resolved in
  one Space-constrained batch, never once per item.

### 3. Correction record

An earlier draft of this brief read the prototype as a per-Project drill-down
and proposed a `?project_id=` filter on `GET /v1/spaces/:space_id/categories`.
That was wrong and is recorded rather than deleted, because the wrong reading
is the intuitive one: the two structures *look* nested (a Project above,
groups below) when they are actually **peers in one flat, ordered list**. The
distinguishing question, for anyone re-reading this later: can the user drag a
Project entry to sit *between* two of their own categories? Yes — which is
what makes it a peer and not a filter.

## Load-bearing list

- **`space` / `isolation` / `acl`.** `pkg/authtree/authtree.go:101-113`:
  「Project is explicitly not a read boundary — Space remains the only
  security boundary」. The unified list is gated by the same
  `spacepkg.CheckMembership` `category.list` already performs
  (`modules/category/api.go:133-142`); a Project entry's *contents* use the
  Project-owned relation-only batch reader. It requires an active Project seat
  (and active account/Space), but does not require native `group_member`
  membership for each related group.
  Nothing here turns Project into a read boundary.
- **`migration` — the ordering table is authoritative for BOTH entry kinds.**
  See D1. The backfill must seed a row per existing category (carrying its
  current `group_category.sort`) *and* a row per existing active Project
  membership. Model it on
  `modules/category/sql/20260428000001_category_legacy01.sql` — the GH #1228
  default-category backfill — including its `INSERT IGNORE` + unique-index
  idempotency and its `NOT EXISTS` guard.
- **`wire-contract`.**
  - New endpoint pair for the unified list (naming not final; placeholder
    `GET/PUT /v1/spaces/:space_id/sidebar-sections(/sort)`). Every Project
    object in the list carries `project_id` and its paired `project_name`.
    Additive.
  - `GET/PUT /v1/spaces/:space_id/categories(/sort)` keep their response
    shapes but change where `sort` is read from and written to (D5). This is
    the most invasive change in the task: already-shipped, already-called code.
  - `PUT /v1/groups/:group_no/category` gains a rejection branch (D3) — a
    previously-legal request becomes an error.
  - `SidebarItem.ProjectID string` → `*string` (D6) is a real breaking wire
    change on `POST /v1/sidebar/sync`, one of the hottest read payloads in the
    product. Independent of D1-D5, gated on Q2. Until and after that decision,
    every non-empty, Space-scoped sidebar `project_id` must carry the matching
    `project_name`; both fields are omitted on the unscoped path.
- **`error-response` / `i18n` — the module guard discovers source files.**
  `modules/category/api_i18n_test.go:33` reads the category directory and scans every
  production Go file for legacy response calls, so the Sidebar handler files are
  covered without a stale hand-maintained filename list. Keep the directory
  discovery when adding future handlers.
- **`rate-limit`.** `SharedUIDRateLimiter` is already mounted after
  `AuthMiddleware` on both category route groups (`modules/category/api.go:40-53`);
  new routes mount the same way. Tests hitting them must reset
  `ratelimit:uid:*` in setup — `CleanAllTables` does not clear it.
- **Collation.** The new table is pinned `utf8mb4_general_ci`, matching
  `group_category` (converted by
  `modules/category/sql/20260416000001_category_legacy01.sql`) and
  `octo_project*`. Every read this task adds is same-family: new table ⋈
  `group_category`, new table ⋈ `octo_project`, and `octo_project_member` in
  the backfill. **No cross-collation join is introduced** — unlike the
  `group`/`group_member` legacy side (production `utf8mb4_0900_ai_ci`), which
  this task only touches through `modules/project`'s existing statement, whose
  own header explains why it correctly carries no `COLLATE`
  (`modules/project/db_group.go:27-42`).
- **Import direction.** Rendering a Project entry needs `octo_project` (name,
  avatar) and the project group list. `modules/category` currently imports
  `modules/space` and `modules/conversation_ext`, and reads the `group` table
  without importing `modules/group`. Adding `category → project` is a new edge;
  verify `modules/project` does not import `modules/category` (nothing found
  suggests it does) before relying on it. Note the *inverse* ban is pinned by
  a test — `pkg/project/import_guard_test.go` holds `modules/project →
  modules/group` at zero — so this task must not accidentally give
  `modules/project` a new dependency either.
- **Mutual-exclusion enforcement point.** `moveGroupToCategory`
  (`modules/category/api.go:510`) currently accepts any `group_no` with no
  `project_id` awareness. Without D3's rejection branch, a caller can
  re-create by hand exactly the state D3's cleanup migration removes.
- **Pre-existing data violates the new invariant.** Nothing has ever stopped a
  user filing a Project group into a category, so production may already hold
  such rows. D3 part 2.
- **The provisioning-hook precedent, and the bug it already caused once.**
  `modules/space/hooks.go` defines `DefaultCategoryProvisioner`; `modules/category`
  injects into it at `init` (`modules/category/1module.go:28`) to avoid a
  `space → category → space` cycle; `modules/space` calls it at four sites
  (`api.go:369,861,1357`, `api_manager.go:666`) and the read path calls it
  again defensively (`modules/category/api.go:147-149`). **GH #1228 is what
  happens when a call site is missed** — the create/join paths did not provision,
  users got an empty list, and a backfill migration was needed. D4 therefore
  requires Project-side hooks at `createProjectOnce` (`modules/project/service.go:371`)
  and `admitMemberTx` (`modules/project/db.go:837`). A successful #861 setting
  update may add or restore the entry only for an active Project member; the
  read-path backstop uses the same membership-only predicate and never treats a
  `pinned=1` row from a non-member as an admission path.
- **There is no third Project admission path today.** `join_mode = 0`
  self-join is unimplemented at HEAD — the column exists with no writer and no
  reader (`modules/project/db.go:48-50`). Recorded so nobody hooks a path that
  does not exist, and so whoever ships PR-3 later knows to add the hook.
- **All-member-group position is explicitly NOT a contract.** See D7.
- **DM categorization is untouched.** `user_conversation_ext.dm_category_id`
  (`modules/conversation_ext/db.go:33-53`) points into the same
  `group_category.category_id` space; DMs have no Project dimension and stay
  wherever the user filed them.
- **Known pre-existing bug, do not absorb it.** `queryUserGroupsInSpace`'s own
  comment (`modules/category/db_group_setting.go:54-75`) records the issue
  #151 dangling-`category_id` follow-up. Out of scope.

## Decisions

**D1 — A new table, `octo_sidebar_section (uid, space_id, section_type,
ref_id, sort, …)`, is the single authoritative order for both categories and
Project entries. `group_category.sort` stops being authoritative.
RECOMMENDED.**

Two entries of different kinds cannot share one drag order unless the order
lives in one place. Keeping `group_category.sort` for categories and adding a
Project-only sort elsewhere forces a merge of two independently-numbered
sequences on every render, and leaves a drag that moves a Project *between*
two categories with nowhere consistent to write.

Shape: `section_type` (`1=category, 2=project`), `ref_id` (a `category_id` or a
`project_id`), `sort`, `uid`, `space_id`, unique
`(uid, space_id, section_type, ref_id)`. `octo_` prefix per this repo's
convention. Application-written UTC timestamps, no MySQL-side defaults —
matching `octo_project_user_setting`'s rule in
`project-p2-product-surfaces` D8.

*Rejected: add `project_id` to `group_category` and let some rows mean
"Project".* Three concrete costs, not stylistic ones:
1. `group_category.name` would either duplicate the Project's name — drifting
   silently the moment someone renames the Project, with nothing to re-sync it
   — or be a column that lies for one whole row class.
2. `is_default` and `uk_uid_space_is_default` exist purely for the manual
   "未分类" bucket. Project rows have no default-bucket meaning and would need
   a type check at every site that touches them, forever.
3. `moveGroupToCategory` writes `group_setting.category_id = <whatever the
   caller passed>` with no type awareness. Sharing one ID space means every
   present and future caller must remember to reject Project-typed IDs; one
   miss reintroduces the state D3 exists to remove. A separate table makes
   rejection the *default* — a Project's `ref_id` is simply never a valid
   `category_id`.

**D2 — A Project entry's group list reuses the final Project relation semantics
through a Project-owned batch reader. Not re-implemented in category, not
proxied over HTTP. RECOMMENDED.**

`modules/project/db_group.go` owns the relation-only projection used by
`GET /v1/projects/:project_id/groups` and the unified Sidebar. It returns the
Project's live associated groups rather than filtering by the caller's native
`group_member` seat, excludes disbanded groups, carries only
`ProjectGroupRelation` fields, and preserves the endpoint's pinned-first order
and 50-row default page. Active Space and Project membership authorize the read;
a non-member Project receives no Sidebar entry, even if a historical pin exists.

The unified Sidebar calls `ListProjectGroupRelationsByProjectIDs` once for all
visible Projects. The Project module batches the relation read, applies a
SQL `ROW_NUMBER()` cap per Project, and retains an in-memory defensive cap. It
does not duplicate the endpoint's relation predicate in category, and it does
not load native chat member counts or avatar fields. Sort validation uses section
metadata only and never renders Project contents.

The project-owned legacy native-membership reader and `modules/project.GroupResp`
projection were removed after their production callers disappeared. Native chat
surfaces continue to use their own `modules/group` readers; they are not a
Sidebar compatibility source.

Sub-question to settle in implementation, not a product call: **which module
owns the unified-list handler.** `modules/category` is the natural home (it
owns the ordering/listing concept being generalized) but its name stops
fitting once it lists Projects too. Flagged so it is decided deliberately
rather than by whichever file gets opened first.

**D3 — Mutual exclusion is enforced, not assumed. CONFIRMED (requester,
2026-09-09).**

A group belonging to a Project appears **only** under that Project's entry.
It cannot be dragged into a manual category. Three parts:

1. **Create**: `POST /v1/group/create` rejects a request that supplies both
   non-empty `project_id` and `category_id` before either resource is queried.
2. **Move**: `PUT /v1/groups/:group_no/category` rejects a non-empty category
   target when the group has `project_id != ''`, via a new `pkg/errcode` code
   and the module's `respondCategoryXxx` helper pattern. An empty
   `category_id` remains allowed so historical violating rows can be repaired.
3. **Existing data**: a migration clears `category_id` and `category_sort` on
   any `group_setting` row whose group has `project_id != ''`. It must not
   touch rows for 直属 Space groups — over-clearing would silently destroy real
   user organization, so that non-effect needs its own test, not just the
   positive case.

The user-visible effect on the day this ships: a Project group someone had
filed under a manual category moves to its Project entry. It does not vanish.
That is the argument for doing it silently (Q3).

**D4 — Auto-provision the Project entry at create and admit; allow active-member
#861 pin to restore it; hide it on explicit unpin; and retain a read-path
backstop. CONFIRMED (requester, 2026-09-11).**

Without it, 「新建 Project 后要出现在分组列表」— the ask that started this task —
does not hold. Hook points: `createProjectOnce`
(`modules/project/service.go:371`) for the creator and `admitMemberTx`
(`modules/project/db.go:837`) for everyone admitted afterwards. The post-commit
`pinned=true` branch in #861's `updateSettingHandler` may add or restore the
entry only after the caller's active Project membership is checked. A Space
member without a Project seat cannot create a visible entry by pinning a
Space-listed Project. Failure degrades to a warn and never rolls back the
committed Project, membership, or pin write —
`modules/space/hooks.go`'s own reasoning transfers verbatim: 「生产 flow（创建/
加入空间）绝不能因 category 初始化失败而回滚」.

The read path calls the same ensure-function inline, as `category.list` does at
`api.go:147-149`, because GH #1228 proved a best-effort hook can miss a site and
the read is the backstop that keeps the list correct anyway. It repairs only
active Project members; a historical `pinned=1` setting from a non-member is
ignored and cannot resurrect an ordering row.

An explicit `pinned=false` is also an explicit removal from 关注, including for
an active Project member. The setting row is the durable opt-out used by both
the render predicate and the repair predicate; the ordering row is retained as
`status=2` so unpin is not confused with departure/disband and a later pin can
reactivate the previous position. The post-commit hide hook remains best-effort:
if it fails, `pinned=0` still suppresses the Project on the next read. A later
eligible `pinned=true` reactivates the row, and the read backstop repairs a
missed reactivation hook.

**D5 — The old `GET/PUT /v1/spaces/:space_id/categories(/sort)` stay, but
source their ordering from `octo_sidebar_section`. RECOMMENDED — and this is
the task's largest blast radius.**

Not optional once D1 makes the new table authoritative: if the old endpoints
keep reading and writing `group_category.sort`, a drag in the unified list and
a drag in any surviving category-only surface write two different numbers and
the views desync permanently. Concretely: `category.list`'s ordering moves to a
join against the new table; `category.sort` (`modules/category/api.go:418`)
writes `section_type=category` rows instead of `group_category.sort`. Because
the legacy request cannot name Project entries, it reassigns categories only
within their currently occupied unified-order slots; Project slots stay fixed.

They stay rather than being deleted because a category-only list is still the
right data source for a category-only surface — notably the picker behind
`PUT /v1/groups/:group_no/category`, which should not have to filter Project
entries out of its options.

`group_category.sort` is **not** dropped as a column (column removal is its own
migration hazard). It simply stops being read. Recorded explicitly so a future
contributor does not "restore" it as the source of truth.

**D6 — `SidebarItem.ProjectID` becomes `*string`, reversing
`project-p2-product-surfaces`'s declined decision. RECOMMENDED, gated on Q2,
independently shippable.**

That brief's Out of scope records the decline and its two grounds: (1) the
"not evaluated" vs "直属 Space" ambiguity already has a client-side
disambiguator — the client knows whether it sent `X-Space-ID`; (2) the
ambiguity was judged temporary, pending `project-p2-read-path-hardening`.

Reversal rationale: (1) still holds technically, but this task's whole point is
a client bucketing sidebar items by Project, which would have to thread "did I
send the header" through its grouping logic as a second piece of state — the
exact ergonomic reason `CategoryID` is a pointer two lines below it. (2) has
not resolved: `project-p2-read-path-hardening` is still unwritten, so
"temporary" has already outlasted a full sibling task.

Shape: `nil` = not evaluated (no `X-Space-ID`), pointer-to-`""` = 直属 Space,
pointer-to-value = the Project. `omitempty` keeps omitting only on `nil`.

`project_name` is additive and follows the same security gate: for a non-empty
Project ID on the Space-scoped path it is the current active Project's `name`,
loaded once for the distinct IDs; it is omitted if there is no Project ID, the
request is unscoped, or the Project lookup fails. A failed display-name lookup
must not fail the hot sidebar response, but it must never manufacture a name or
query outside `space_id`.

**D7 — 全员群's position stays a client-side concern; the backend does not
promise it is first. CONFIRMED (requester, 2026-09-11).**

The relation projection orders pinned rows first, then by stable group identity;
the all-member group may therefore move after a rebuild or re-association. The
endpoint deliberately ships no "is the all-member group" flag: the client holds
`all_member_group_no` from the Project payload and compares two strings.
Position is a convenience, never the contract.
The client may compare `group_no` against `all_member_group_no` to label or place
the all-member group. This Sidebar task does not promise backend ordering or
native membership synchronization; the relation projection leaves ordinary
Project-created and associated groups under their existing independent semantics.

## API surface

| Endpoint | Change | Risk |
|---|---|---|
| `GET /v1/spaces/:space_id/sidebar-sections` *(placeholder name)* | **New** — unified ordered list, both entry kinds; only active Project members see Project entries; each Project object pairs `project_id` with `project_name` and relation-only `groups[]` | low, additive |
| `PUT /v1/spaces/:space_id/sidebar-sections/sort` | **New** — accepts typed items `[{type,id}]`, not the old bare `category_ids[]` | low, additive |
| `GET /v1/spaces/:space_id/categories` | Ordering source moves to the new table; response shape unchanged (D5) | **medium** — shipped, in use |
| `PUT /v1/spaces/:space_id/categories/sort` | Write target moves to the new table (D5) | **medium** — authority handover, ordering can scramble if done wrong |
| `PUT /v1/groups/:group_no/category` | New rejection branch for Project groups (D3) | low — previously-legal request now errors; needs release-note mention |
| `GET /v1/projects/:project_id/groups` | **Reused relation projection** (D2) — active Project members receive pinned-first `groups[]`, at most 50 rows per Project; no native member fields or chat permission | low, additive |
| `POST /v1/sidebar/sync` | `SidebarItem.ProjectID` → `*string` (D6); add paired `project_name` only for a non-empty, Space-scoped Project ID | **high** for D6, gated on Q2; `project_name` is additive and fails soft |
| *(internal, no HTTP surface)* `createProjectOnce`, `admitMemberTx`, #861 `updateSettingHandler` | Provision on create/admit; active-member pin may restore; retain-and-hide on unpin (D4) | low — pin remains personal preference, not membership |

## Open questions

- **Q2 — Rollout sequencing for D6.** Which clients parse `SidebarItem` today,
  and can they be made pointer-tolerant first? Product/client-team question.
  Gates D6 only.
- **Q3 — D3's cleanup: silent, or does the user get a signal?**
  Recommendation: silent, because the group moves to its Project entry rather
  than disappearing. Still a product call — it changes what a real user sees on
  ship day.
- **Q4 — Do the old `categories(/sort)` endpoints get marked legacy once the
  unified list ships?** D5 keeps both working either way; this only decides
  what new client work is told to build against.

## Out of scope

- **Threads** under a group. Existing endpoint, existing gate
  (`GET /v1/groups/:group_no/threads`), own pagination — nesting them is
  `project-p2-product-surfaces` D4's settled "no".
- **Unread / badge counts** on an entry. Same reasoning as that brief's D5: a
  second source of truth for a number that changes on every message.
- **`project-p2-read-path-hardening`** — referenced by D6's rationale, still
  unwritten, not closed here.
- **`join_mode = 0` self-join** (PR-3, unimplemented). Whoever ships it wires
  D4's hook into that path.
- **Dropping `group_category.sort`** as a column. D5 stops reading it; removal
  is a later cleanup.
- **Any change to `group.project_id`'s writers, I2, I3, or the cascade.**
- **Making Project a read boundary.** `authtree.go:101-113` stands.
- **Client-side rendering.** Backend contract only — including D7(a), whose
  implementation is a client rule this brief only records the need for.
- **A repository-wide `project_id` rename or redundant name rollout.** The
  `project_id`/`project_name` pairing is limited to this task's Sidebar-facing
  payloads (`GET /v1/spaces/:space_id/sidebar-sections` and
  `POST /v1/sidebar/sync`). Database columns, route parameters, and unrelated
  Project resource responses keep their existing contracts.
- **Swagger.** Pre-existing gap for `modules/category`; this task updates only
  what it touches.

## Acceptance

**Gates (all must pass):**

```bash
go test -race ./modules/category/... ./modules/project/... ./modules/space/... ./modules/message/...
make i18n-extract && make i18n-extract-check && make i18n-lint
golangci-lint run ./...
```

**D1 / D2 — the unified list**

- `TestSidebarSectionsListsJoinedProjectsAndOwnCategories` — a caller in 2
  Projects with 3 categories gets 5 entries, correctly typed.
- Every Project entry carries both `project_id` and the current `project_name`.
- `TestSidebarSectionProjectContentMatchesProjectGroupsEndpoint` — a Project
  entry's relation groups are identical to `GET /v1/projects/:project_id/groups`
  for the same caller, including a related group outside the caller's native
  roster (pins D2's Project-wide relation semantics).
- `TestTheBatchProjectGroupListReachesItsRowsByAnIndex` — the batched relation
  query keeps one group read, applies a SQL per-Project row bound, and preserves
  the relation projection's explainable group/setting indexes.
- `TestSidebarSortValidationDoesNotRenderProjectContents` — sorting validates
  only visible section metadata and cannot invoke the full Project-group render.
- `TestSidebarSectionsOrderInterleavesTypesAndOldCategoriesKeepRelativeOrder`
  — sort a Project between two categories, re-read, order holds; the legacy
  category-only view preserves the remaining categories' relative order.
- `TestSidebarSectionsIsUIDRateLimited` — `ratelimit:uid:*` reset in setup.
- `TestSidebarSectionMigrationBackfillsOrderProjectsAndProjectGroupCleanup`:
  existing categories keep their relative order; every active Project
  membership gains an entry; re-running the migration is a no-op.

**D3 — mutual exclusion**

- `TestMoveGroupToCategoryRejectsProjectGroup` — new error envelope,
  `group_setting.category_id` unchanged for a non-empty target; an empty target
  successfully clears a historical assignment.
- `TestGroupCreateRejectsProjectAndCategoryTogether` and
  `TestGroupReqCheckRejectsProjectAndCategoryTogether` — the create endpoint
  rejects simultaneous non-empty `project_id` and `category_id` before writes.
- `TestSidebarSectionMigrationBackfillsOrderProjectsAndProjectGroupCleanup` also
  proves a Project group's assignment is cleared while a 直属 Space group's
  assignment in the same category is **untouched**.

**D4 — provisioning**

- `TestProjectCreateProvisionsSidebarSection` and
  `TestProjectAdmitProvisionsSidebarSection` — both hook points, separately.
- `TestPinnedSpaceListedProjectIsNotVisibleWithoutMembership` — a Space member
  with no Project seat and a historical `pinned=1` row does not receive a
  Sidebar entry, and the read backstop does not materialize one.
- `TestSidebarSectionsListsJoinedProjectsAndOwnCategories` — the read-path
  backstop covers members whose hook did not run.
- `TestUnpinningProjectRemovesItFromFollowAndRepinningRestoresIt` — an explicit
  unpin hides the retained section even while the caller remains an active
  Project member; repeated reads do not undo the opt-out, and re-pin restores it.
- `TestUnpinningSomethingNeverPinnedPersistsOptOut` and
  `TestUnpinningNeverPinnedMemberStaysOutOfFollow` — the first explicit
  `pinned=false` from an auto-provisioned member writes the durable tombstone;
  two consecutive reads cannot resurrect the section.
- A hook failure warns and does **not** roll back the Project write.

**D5 — old endpoints stay consistent**

- `TestSidebarSectionsOrderInterleavesTypesAndOldCategoriesKeepRelativeOrder`
  — after a drag through the new endpoint, the old list reflects the same
  relative order; a later legacy category sort keeps Project slots unchanged.
- `TestOldCategoriesSortWritesSidebarSectionTable` — the old sort endpoint
  updates the new table without densifying the existing category slots.

**D6 — `SidebarItem.ProjectID *string`** (only once Q2 is answered)

- `nil` on the no-`X-Space-ID` path; pointer-to-`""` for 直属 Space;
  pointer-to-value for a Project group; `COMMUNITY_TOPIC` resolves to its
  parent group's value.
- Every non-empty, Space-scoped `project_id` is paired with `project_name`;
  no-Space and 直属 Space items omit it. The name lookup is one distinct-ID,
  Space-constrained batch; no new per-item query.

**Guard hygiene**

- `TestCategoryNoLegacyResponseError` scans every production Go file in the module
  for legacy response calls, so new Sidebar handlers are covered automatically;
  retain directory discovery when adding future handlers.
