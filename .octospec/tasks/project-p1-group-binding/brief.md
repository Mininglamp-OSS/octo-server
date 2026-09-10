---
type: Task
title: "Task: project-p1-group-binding"
description: P1 of the Project collaboration layer — bind groups to projects via group.project_id, converge the eleven group-membership write paths onto one admission entry that enforces invariant I2 (a project group's active members are a subset of that project's active members), add the reverse-registered project→group cascade and disband detach, and extend the reconcile job to I2/I3, plus the point-query read contract other systems judge against. P0 shipped the member pool with no consumer; P1 is what gives a project seat its meaning, which is also what makes P0's tolerated windows stop being tolerable and what makes an authoritative external read path load-bearing rather than speculative.
tags: ["space", "isolation", "acl", "error-response", "i18n", "rate-limit", "wire-contract", "testing", "commit", "migration"]
timestamp: 2026-09-06T00:00:00Z
# --- octospec extension fields ---
slug: project-p1-group-binding
upstream: self
source: self
---

# Task: project-p1-group-binding

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Make a Project own groups, and make invariant **I2** real:

> **I2** — for a group with `group.project_id != ''`, every active
> `group_member` row belongs to a uid that is an active member of that project
> (system bots exempted by whitelist).

I2 is the entire security mechanism of the Project layer. There is no read-path
filter behind it: if a non-member gets a `group_member` row, they see the group in
`/v1/sidebar/sync`, they receive its messages over WuKongIM, and they can post.
The design brief says so in as many words — 「只要所有"加人入群"的入口都守住 I2…
不需要在 sidebar/search/message 额外加过滤」. That trade buys P1 a small diff on the
read paths and charges it a very high price on the write paths: **one missed
admission path is not a cosmetic bug, it is the whole hole.**

So the deliverable is not "add a check". It is:

1. one admission entry every path goes through,
2. a **source guard** that fails CI when a new path does not,
3. a per-entry-point rejection metric so a path that silently stopped enforcing
   is visible in production rather than in the next review,
4. a reconcile scan that reports violations that got in anyway.

Plus the two structural pieces I2 needs to survive membership churn: the
project→group cascade when a member leaves a project, and the detach when a
project is disbanded.

### What P0 already built, and how that moves the P1 boundary

P0 (`modules/project`, PR #841) shipped `octo_project` + `octo_project_member`,
CRUD, membership under I1, `member_epoch`, the Space-removal cascade step, quotas,
audit, metrics and a read-only reconcile job — and touched no group code. Five
consequences for P1:

- **`member_epoch` already exists** (P0 decision D2). The design brief assigns it
  to P1; it is done. P1 item 6 shrinks to *extending the same rule to the write
  paths P1 adds* and adding the reconcile guard.
- **A lock order is already declared, and P1 has to fill in its elided middle.**
  `modules/project/db.go:344-345` states the module's order as
  `space_member → space → project → … → octo_project_member`, and every P0 write
  path takes `space_member` (`FOR SHARE`) before `space` before the project row.
  The `…` is not a reservation — nothing in P0 names the group positions — so P1
  **chooses** them, and the only choice compatible with the declared ends is
  `space_member → space → project → group → group_member → octo_project_member`.
  Pin it in a comment where P0 pinned its own, and cover the crossing paths with a
  deadlock test: the group-side admission path takes `group` first today, so
  "project before group" is a change to the order that path uses.
- **The ownerless-project question is closed, and it created new P1 work.** P0's
  round-2 review made the Space cascade hand ownership to the senior remaining
  member, and **disband the project** when there is no successor
  (`project_cascade_ownerless_disbands_total`). So a *background worker* can now
  disband a project — which in P1 means the group-detach step must be reachable
  from the cascade, not only from the disband handler.
- **P0's reconcile discipline is a constraint, not a suggestion.** Every scan is
  cursor-paged with bounds on rows *examined*, pinned by
  `TestReconcileQueriesAreBounded` and `TestReconcilePageQueriesExamineBoundedRows`.
  P1's I2/I3 scans read `group` and `group_member`, the two largest tables in
  scope. They inherit the discipline and the guard.
- **Two existing source guards enumerate P0's write paths** —
  `TestWritePathsRevalidateTheActorSpaceSeatInTx` and
  `TestEveryWritePathEmitsAnAuditEntry`. Every write path P1 adds to
  `modules/project` must be added to both, or the guards quietly shrink in
  coverage while still passing.

### P0's tolerated window stops being tolerable here

P0's `space_member_removal.go` states it plainly: the Space→Project cascade is
asynchronous because the machinery is (a 10-second poller, a 10-minute lease,
backoff, a terminal `abandoned` state), and *"P0 tolerates that window because a
project seat grants nothing yet: no group, no channel, no message. **The tolerance
expires in P1**, where the same row gates group admission."*

That is now due. In the window between a Space removal committing and the cascade
running, `octo_project_member` holds `status = 1` rows for a uid with no Space
seat. If P1's admission gate asks only "is this uid an active project member?",
those rows admit a removed Space member into a project group.

**Resolution: the gate is a conjunction, and the Space half is not new work.**
Every in-module admission path that P1 converges (A1 `addMembersTxWithSpace`, A2
`AddGroupMembers`, A5 `groupScanJoin`) already calls `pkg/space.CheckMembership`.
The single entry keeps that check and adds the project check beside it, so the
composite predicate is "active Space member **and** (project is `''` **or** active
project member)". The window then costs nothing: the Space half fails first.

Two things this does *not* fix, both of which must be written down rather than
assumed away:

- `pkg/space.CheckMembership` takes a `*dbr.Session`, not a `*dbr.Tx`
  (`pkg/space/membership.go:9`), so today every group-side Space check is a read
  **outside** the admission transaction. P1's project check must not copy that: it
  takes the tx and a shared lock, the way P0's own
  `checkSpaceMembershipForWriteTx` / `lockSpaceSeatRowTx` do. Tightening the
  *Space* check the same way is a strict improvement but is **out of scope** — it
  changes behaviour on every group join in the product.
- The reverse window (project seat closed, group rows not yet removed) is the
  cascade's problem, and is addressed by D4 below.

## Background

The design source is the git-ignored `.context/[octo-server]brief-project.html`
(v9), §6–§9. P0 is `.octospec/tasks/project-p0-foundation/`; its `verification.md`
records seven deviations from that design and the four review rounds that produced
them. This brief is written against the **shipped P0 code**, not against the design
document, wherever the two disagree — and they disagree in several places, each
recorded as a decision below.

### Why the write-path convergence is the hard part

Today group membership is admitted by **eleven** distinct code paths and removed
by **seven**, and there is no shared admission funnel. There are two parallel
near-duplicate implementations (`api.go addMembersTxWithSpace` and
`service.go AddGroupMembers`), nine sites calling the DAO primitives directly, and
two sites doing raw DML from **outside** `modules/group` entirely. The code itself
concedes this: `service.go:1151`, `api.go:1678`, `:1686`, `:1758`, `:1968` all
carry comments of the form 「与 Service.AddGroupMembers 保持一致」 — the invariant is
currently maintained by hand-copied comments, not by a function.

P0's own journal already recorded what that costs: I1 was checked from day one,
but per path, and the paths were added one review round at a time — the owner-seat
check landed after the add-member check, the successor recheck landed rounds after
the direct role change. The brief predicted it and it still happened. The recorded
counter-move is exactly what this task is: **a guard that enumerates the paths**.

## Load-bearing list

- **The eleven admission paths, verified against HEAD.** A1
  `addMembersTxWithSpace` (`modules/group/api.go:1881,1883`) · A2
  `AddGroupMembers` (`service.go:1598,1600`) · A3/A4 `CreateGroup` initial members
  and `req.BotUID` (`service.go:1303`, `:1333`) · A5 `groupScanJoin`
  (`api.go:2684,2686`) · A6 `handleRegisterUserEvent` (`event.go:169`) · A7
  `handleOrgOrDeptCreateEvent` (`event.go:316,341`) · A8
  `handleOrgOrDeptEmployeeUpdate` add branch (`event.go:522,524`) · A9
  `IService.AddMember` (`service.go:380`) · A10 `joinPresetGroups`
  (`modules/space/api.go:1366`) · **A11 the un-blacklist branch of `blacklist`
  (`modules/group/api.go:3757`)**.
- **A11 is not in the design brief's list of eleven, and it is the one a naive
  gate misses.** `blacklist` with `action != "add"` calls
  `updateMembersStatus(version, groupNo, GroupMemberStatusNormal, uids)` — it
  restores a uid to the active member set, re-subscribes them to the IM channel
  (`api.go:3813`) and to the group's threads (`api.go:3820`), and it touches
  **neither** `InsertMemberTx` nor `recoverMemberTx`. A gate installed only in the
  two admission primitives is bypassed by it, with no transaction anywhere on the
  path. Treat A11 as an admission path, because that is what it is.
- **`recoverMemberTx` is the reactivation half of admission**
  (`modules/group/db.go:285`). `group_member` has
  `CREATE unique INDEX group_no_uid on (group_no, uid)`
  (`20191106000002_group_legacy01.sql:49`) and rows are soft-deleted, so re-joining
  is an UPDATE. A gate in `InsertMemberTx` alone covers first joins only.
- **A10 is the reference case for how this fails.** `joinPresetGroups` is a bare
  `INSERT INTO group_member (group_no, uid)` with **no transaction**, launched as
  `go s.joinPresetGroups(...)` from `afterJoinSpace` (`modules/space/api.go:1313`),
  every failure a `Warn` + `continue`. Its presence check filters `is_deleted=0`
  (`:1356`), so a uid who previously left passes the check, the INSERT hits the
  unique index, MySQL returns 1062, the error is logged, and **the user is
  permanently never re-added.** Two places in the tree already document this
  defect (`modules/project/sql/20260904000001_project_core.sql:45`,
  `modules/space/db_manager.go:219`). It also writes only two columns — no
  `version`, `role`, `invite_uid`, `is_external`, and no `IMAddSubscriber` — so
  incremental member sync misses the row.
- **A1's entry point is an unauthenticated public route.**
  `groupMemberInviteSure` is mounted on the `openGroup` group with no
  `AuthMiddleware` (`modules/group/invite.go:327`, route at `api.go:~176`). The
  most-exposed admission path in the product is one of the paths that must enforce
  I2.
- **A9 `IService.AddMember` has zero non-test callers and zero validation.**
  `service.go:380` does `s.db.InsertMember(&MemberModel{GroupNo, MemberUID})` — no
  transaction, no Space check, no `version`, no `vercode`. It is exported on
  `IService`, so any future module can bypass every gate through it.
- **`RemoveGroupMembers` is already the good removal funnel, and it does far more
  than delete a row.** `modules/group/service.go:1743` (delete at `:1857`) handles
  IM unsubscribe, the removal system message, `CMDGroupMemberUpdate`, the
  invited-bot cascade, `thread_member` + `thread_setting` cleanup, Space-scoped
  pinned/conversation-extra cleanup, and external-group flag reclamation. It sorts
  targets by uid for lock ordering (`:1830`) and takes `LockRemovableMemberTx`
  (`db.go:1134`, `SELECT role … FOR UPDATE`) per target. It **silently skips
  `role = creator`**. Reimplementing any part of this is how omissions happen —
  `modules/group/space_member_removal.go:88` says so explicitly.
- **Because the cascade reuses `RemoveGroupMembers`, `thread_member` comes for
  free.** `removeUserFromGroupThreadsCleanup`
  (`modules/group/thread_cleanup.go:50`) already deletes the uid's `thread_member`
  and `thread_setting` rows for every thread of the group. The design brief leaves
  the thread cascade unspecified; the answer is that it is already covered, and
  the reason is worth writing down so nobody adds a second one.
- **The IM-unsubscribe leak is a known, open, separately-tracked P0 — and P1 must
  not re-solve it.** `modules/group/space_member_removal.go:96-133` records the
  verified finding: `IMRemoveSubscriber` failing leaks **permanently** (the member
  keeps receiving pushes and can still send, because WuKongIM's send-side permission
  check reads `Store.ExistSubscriber`), and the `IMDatasource` callbacks that were
  believed to self-heal are dead code in the pinned broker `v2.2.4-20260313`. There
  are **seven** leak sites. Issue #797 carries the triage and
  `.octospec/tasks/im-pending-outbox/brief.md` carries the design **plus the six
  defects a first implementation shipped and had reverted** (a permanent tombstone
  from `UNIQUE` + `INSERT IGNORE`, firing without re-validating membership, and
  four more). See D6.
- **The Space cleanup-step contract.** Steps must be idempotent, must decide
  "nothing to do" themselves and return `nil`, must not assume any other step
  succeeded, and a returned error reruns the **whole** job including
  already-successful steps (`modules/space/member_removal.go:56-64`). Any new
  registry P1 defines should state the same contract, because the same worker shape
  produces the same hazards.
- **Import direction, measured at HEAD.** `go list -deps`: `thread → group → space`
  and `project → space`; `project → group` is 0 and `group → project` is 0. So
  `group → project` is available and `project → group` must stay forbidden. The
  cascade therefore has to be reverse-registered, exactly as
  `modules/group/space_member_removal.go:22` and
  `modules/project/space_member_removal.go:59` already do into `modules/space`.
- **`modules/space` reads the `group` table by raw SQL and must keep doing so.**
  `modules/space/api.go:1349` is `SELECT status FROM \`group\` WHERE group_no=? AND
  space_id=?`. Reading `project_id` there is one more column on an existing query,
  not a new dependency.
- **`group.space_id` was added by `modules/space`'s migration**
  (`modules/space/sql/20260307000004_space_legacy03.sql:4`), as
  `VARCHAR(40) DEFAULT ''` — **nullable**, no `NOT NULL`, no `COLLATE`. That is the
  precedent for where `project_id` lives, and an example of what not to copy.
- **`group` has exactly two indexes, and `space_id` is in neither.**
  `group_groupNo` UNIQUE(`group_no`) and `group_creator`(`creator`). Queries that
  already filter on `space_id` (`db.go:675 queryGroupsWithMemberUIDAndSpaceID`,
  `modules/bot_api/groups.go:51`) have no index behind them today. Adding
  `(space_id, project_id)` therefore **changes existing query plans** — a welcome
  change, and a non-regression item, not a no-op.
- **`group` declares no explicit `CHARSET`/`COLLATE`**
  (`20191106000002_group_legacy01.sql:5`), so it inherits the database default,
  while `octo_project*` pin `utf8mb4_general_ci`. Two places in the tree already
  work around the resulting error 1267 with an explicit `COLLATE` in a JOIN
  (`modules/message/db_reminders.go:113`, `modules/bot_api/resolve_targets.go:148,165`).
  P1's reconcile JOINs must be verified against a real 8.0 database, not assumed.
- **Group disband leaves `group_member` rows intact** (`UpdateStatusTx` only), and
  two modules depend on that (`modules/message/1module.go:280`,
  `modules/messages_search/authz.go:35`). So "detach on project disband" is a
  `group.project_id` write and nothing else — and option (b), disbanding the
  groups, would not clean up members either.
- **`modules/project/db.go:532 admitMemberTx` is the reference implementation** of
  a transactional, idempotent, affected-rows-reporting admission primitive
  (`INSERT … ON DUPLICATE KEY UPDATE`, with the assignment-order defect found and
  documented at `db.go:515-531`). `group_member` has no equivalent; the current
  `ExistMemberDelete` + `InsertMemberTx` / `recoverMemberTx` triple is a racy
  three-statement stand-in, and that raciness is exactly what A10 falls into.
- **Three DAO methods are dead and unguarded.** `DeleteMember` (`db.go:72`),
  `deleteMembersWithGroupNOTx` (`db.go:78`), `UpdateMemberTx` (`db.go:263`) — zero
  callers, and each one writes `is_deleted` without any gate.
- **`CheckForbiddenLoop` is not loop detection.** It is the unmute-expiry poller
  (`api.go:4024`, started unconditionally as `go g.CheckForbiddenLoop()` at
  `api.go:193`). Its source query `queryForbiddenExpirationTimeMembers`
  (`db.go:689`) does **not** filter `is_deleted=0`, and `DeleteMemberTx` does not
  clear `forbidden_expir_time`, so removed-and-muted members are rewritten
  forever by a full-row `UpdateMember` outside any transaction (`api.go:4047`).
  Two independent fixes, both cheap: filter the query, and clear the column on
  delete.
- **i18n, rate limiting, migrations, dbr quoting, authtree census** — the same
  constraints P0 listed, unchanged: `httperr.ResponseErrorL` with codes in
  `pkg/errcode/project.go` (extended) and `pkg/errcode/group.go`; never
  `c.ResponseError` / `AbortWithStatusJSON` / non-OK `c.JSON`; 5xx ⟺
  `Internal=true`; `SharedUIDRateLimiter` after `AuthMiddleware`; `octo_` prefix on
  anything new; MySQL 8.0 only; `Update`/`InsertInto`/`DeleteFrom` take bare table
  names while `From`/`Select` need backticks; a census entry per affected route.
  Note `TestGroupNoLegacyResponseError` (`api_i18n_test.go:30`) scans a **fixed
  file list** — any new group file must be added to it.

## Out of scope

- **`GET /v1/projects/:project_id/groups`**, the nested Project → Group → Thread
  response, `project_id` passthrough in sidebar / group detail / group list,
  project pinning, the `is_official` management endpoint, Project-level auto-join,
  and the member-picker data source — all **P2**.
- **Read-path hardening** — the `/v1/sidebar/sync` `ExistMembersActive` fallback,
  the deprecated `/v1/coversations` filter, the group-avatar enumeration oracle,
  `querySavedGroups` after leaving — **P2**. P1's obligation on the read side is
  **observation only**: the stale-subscriber reconcile scan.
- **Fixing the IM-unsubscribe leak.** Tracked in #797 and
  `.octospec/tasks/im-pending-outbox/brief.md`. See D6.
- **Re-parenting a group between projects.** I3 makes attribution immutable in v1.
  No endpoint, and the source guard should forbid an `UPDATE` that writes
  `project_id` outside the create path and the disband-detach step.
- **Tightening the Space membership check into the admission transaction.** A
  strict improvement, and a behaviour change on every group join in the product.
  Separate task.
- **Deleting the three org-directory event listeners.** See D7 — P1 converges them
  instead.
- **Subsystem integration beyond the read contract in D11.** No
  `/v1/internal/membership/epochs` endpoint, no removal log, no full-reconcile
  member endpoint, no replica report, no callback registry, no fleet/drive code.
  Those wait for a named consumer with a known polling period (the epoch endpoint's
  QPS is `consumers × spaces ÷ interval`, which cannot be estimated without one),
  and the replica machinery waits for a subsystem that meets all six Tier-1 entry
  conditions. The full consumer-facing contract, including the API reference, is
  `.context/project-membership-integration.md` (local-only, alongside this brief).
- External members, cross-Space or nested projects, Project as a read boundary.

## Decisions

Recorded because each one deviates from the design brief, or resolves an ambiguity
it left open.

**D1 — `project_enforce_membership` does not ship as a flag.** The design brief
specifies it as 「默认 true、不可动态关闭」, and a flag that must never be off is a
constant with extra failure modes: an operator surface implying a lever that must
not be pulled, a config read on the hottest write path in the product, and a
reviewer's assumption that someone thought about the off case. The brief's own
warning is the argument — 「回滚只能"停止产生新 project 群"，不能放松已有 project 群
的约束」. So: I2 enforcement is **unconditional**, and the rollback lever is the flag
that already exists, `OCTO_PROJECT_CREATE_ENABLED`, extended to gate *creating a
group with a non-empty `project_id`*. Off ⇒ no new project groups are produced and
existing ones keep enforcing. This continues P0's D1 reasoning rather than
contradicting it. (`modules/featuregate` exists on unmerged branches only — 0 hits
at HEAD — so it is not an option here either way.)

**D2 — `group.project_id` is `VARCHAR(40) NOT NULL DEFAULT ''`; `''` is the
sentinel, never `NULL`.** Every predicate in the design, every source guard and
every reconcile query is written as `project_id != ''` / `= ''`. `NOT NULL`
deliberately diverges from `group.space_id`'s nullable-with-default shape: a
three-valued column turns each of those predicates into a bug waiting for the
first `NULL` row. The migration lives in `modules/project/sql/`, following the
precedent that `modules/space` added `group.space_id` from its own directory —
the module that owns the concept owns the column. `ADD COLUMN … NOT NULL DEFAULT ''`
is INSTANT on MySQL 8.0; the `(space_id, project_id)` index is a separate,
online, INPLACE statement whose duration must be **measured on production row
counts before rollout**, not asserted here.

**D3 — one admission entry, and it takes the transaction.**
`AdmitOrRestoreMemberTx(tx, groupNo, spaceID, projectID, uid, opts)` in
`modules/group`: it decides insert-vs-restore internally, enforces the composite
predicate on **both** branches, and writes the full column set every current path
writes (`version`, `vercode`, `role`, `invite_uid`, `is_external`,
`source_space_id`, the `bot_admin = 0` reset that `recoverMemberTx` performs, and
`IMAddSubscriber`). `InsertMemberTx`, `InsertMember` and `recoverMemberTx` become
unexported and callable only from it. That the entry fixes A10's four historical
defects as a side effect is the point: the defects exist *because* admission was
reimplemented.

**D4 — the cascade closes the seat first and detaches groups after, and
re-admission cancels an in-flight cascade.** This resolves an ambiguity the design
brief leaves open, and it is the most consequential decision in P1.

`octo_project_member.removing TINYINT` is set to 1 **in the same transaction** that
begins removal, while `status` stays 1. Every authorization read — the project
member list, the group admission gate, the project middleware's role resolution —
treats `removing = 1` as a non-member. Keeping `status = 1` until the detach
finishes means **I2 is never literally violated by the removal itself**: the
`group_member` rows that remain still belong to a member of record. The worker
flips `status` to 0 and clears `removing` in its final transaction. `member_epoch`
is bumped when `removing` is set, because that is a membership change from every
consumer's point of view.

The interesting case is re-admission during the window, and the answer is **cancel,
not reject**. Adding the uid back inside a transaction that holds the project row
lock clears `removing`, leaves `status = 1`, and marks the outstanding cascade job
cancelled. Rejecting instead — the obvious alternative — is worse in a specific
way: the cascade can legitimately stall for hours, because `RemoveGroupMembers`
skips `role = creator` and a project member who owns a group cannot be detached
until a human hands that group over. A rejection would make an unrelated admin
action fail for as long as that takes, with no self-service remedy.

The worker must therefore re-read the member row **under lock** before each batch
and stop when `removing = 0` — the same shape as P0's
`checkSpaceSeatForCleanupTx` re-check inside `deactivateSeatForCascade`, and the
same shape as `cleanupSpaceMemberGroups`'s re-check. A cancellation that lands
mid-batch leaves the member in the project but out of some of its groups. That is
**not** an invariant violation (the subset relation still holds), it is visible in
the member lists, and an admin can re-add. Say so in the code, or the next reader
will "fix" it.

**D5 — the project-side cascade is its own outbox, not another step on the Space
job.** The Space job is keyed `(space_id, uid)` and shares one 10-minute lease
across the group, conversation and project steps; #797 documents that a single job
on a large Space can already outrun that lease. Project removal is keyed
`(project_id, uid)` and can fan out over every group in the project. Hanging it
off the Space job would (a) mis-key the work, (b) put project-sized fan-out inside
a lease sized for Space cleanup, and (c) make a project-side failure re-drive the
Space-side steps, which the step contract explicitly warns about. So:
`octo_project_member_removal_cleanup`, copied structurally from
`space_member_removal_cleanup` (`modules/space/sql/20260821000001_*.sql`) — lease
owner + `lease_until`, `attempts`, `next_attempt_at`, backoff, terminal
`abandoned`, `(status, next_attempt_at, lease_until)` and `(status, finished_at)`
indexes, retention purge — with three corrections that table's own history earned:
application-written **UTC** columns instead of `CURRENT_TIMESTAMP(3)` (P0's
migration explains why: a session-timezone column compared against a Go UTC clock
already shipped a metric reading −28799 seconds), a **lease heartbeat** from the
job body (#797's open P1: without it the abandon sweep marks a still-running final
attempt as `abandoned`), and a purge loop that drains rather than a fixed
`limit = 1000` per hour (#797: 24k rows/day is below realistic churn).

**D6 — P1 does not create `octo_project_member_removal_im_pending`.** The design
brief's §6 assigns it to P1, and that assignment predates
`.octospec/tasks/im-pending-outbox/brief.md`, which owns this exact table for
**all seven** leak sites and records six defects a first implementation shipped and
had reverted — including a permanent tombstone (`UNIQUE` + `INSERT IGNORE` + no
purge, reproduced on MySQL 8.0.46) and a worker that fired without re-validating
membership. Building a project-private copy from the design brief's four-column
sketch would re-derive those defects at exactly the site with the least production
exposure, and would leave the other six leaking. P1 therefore: reuses
`RemoveGroupMembers` and **inherits** its leak; asserts nothing about IM
convergence it cannot deliver; and ships the **stale-subscriber reconcile scan**
so the leak is measurable at project granularity before anyone claims it is fixed.
If `im-pending-outbox` lands first, P1 inherits the fix for free — which is the
whole argument for one table instead of two.

**D7 — the three org-directory listeners are converged, not deleted.** The design
brief says 「P1 直接删除」 on the grounds that they have no publisher.
`AddEventListener` exists for `OrgOrDeptCreate`, `OrgOrDeptEmployeeUpdate` and
`OrgEmployeeExit` and no publisher exists **in this repository** — but #797's own
inventory classifies two of those handlers as 「HR / org-directory offboarding
paths — arguably the highest-stakes callers」. Two documents in the same
`.octospec/` tree disagree about whether this code is dead, and P1 should not
settle that by deleting it: the event table is a database queue, `Wait` rows can
predate the deploy, and a deleted listener drops them silently. So route A7/A8 and
the R4 removals through the new entries — mechanical, since they already do the
same work by hand — and add a metric that fires if they ever execute. Deletion
becomes a separate change once production shows zero rows of those types over a
window.

**D8 — the source guard is written first, and it is the tree-walking kind.** The
model is `internal/msgextraseq/source_guard_test.go:42`
(`TestNoLegacyMessageExtraGenSeqOutsideAllocator`): `filepath.WalkDir` from the
module root, skip `vendor` / `.git` / `node_modules` / `.octospec`, skip `_test.go`,
allowlist by path prefix, collect **all** offenders and fail with the full list.
Not the `strings.Contains`-over-a-fixed-file-list shape that
`TestGroupNoLegacyResponseError` uses — a fixed list cannot see a new file, which
is the failure mode that matters. Forbidden outside the allowlist:
`InsertInto("group_member"`, `Update("group_member"`, `DeleteFrom("group_member"`
and the raw-SQL equivalents. Allowlist: `modules/group/db.go` primitives plus the
compensating `DeleteFrom` at `service.go:1404`. Run today it flags exactly
`modules/botfather/command.go:658` and `modules/space/api.go:1366` — so the guard
is what proves those two got converged, and P0's journal is explicit that
"a comment describing a guarantee is a specification without a test".

**D9 — `AssertMembersInProject` lives in a new `pkg/project`, with a Tx variant.**
`modules/group` needs a predicate, not a module. `pkg/space` is the precedent
(`CheckMembership`, `MemberRole` — plain functions over a session, no module
import, no init-order coupling), and it is what lets `modules/group` and
`modules/project` both depend on the same fact without either importing the other.
The registry that carries the cascade in the other direction —
`RegisterProjectMemberRemovalStep` / `RegisterProjectDisbandStep` — stays in
`modules/project`, mirroring `modules/space`'s. Unlike `pkg/space`, the predicate
takes a `*dbr.Tx` and a shared lock on the member row: the check has to be inside
the transaction that commits the admission, or it is a TOCTOU with a comment.

**D10 — batch admission asks once per project, not once per uid.** `CreateGroup`'s
initial member list and `AddGroupMembers`' batch both validate the whole set in one
query (`WHERE project_id = ? AND uid IN (?) AND status = 1 AND removing = 0`),
returning the missing uids. Per-uid checks would put N queries inside the admission
transaction and lengthen it in proportion to batch size — and the batch cap is 200.

**D11 — the read contract for other systems lands in P1, not P3, and it is a point
query.** The design brief schedules subsystem integration for P3 and shapes it as a
`projects[]` array of everything the caller belongs to. Both parts change here.

*Why P1 and not P3.* This is the same argument P0 used to pull `member_epoch`
forward (D2), applied to the read side. P1 is where a project seat first **means**
something: before P1 it grants no group, no channel, no message, so a subsystem
asking "is X in project P" gets an answer with no consequence attached. After P1 it
is a live authorization fact — and the moment it is, every service that needs it
will improvise a predicate from whatever octo-server happens to expose. That is not
hypothetical: a subsystem shipped a Space gate built out of *the HTTP status code of
`GET /v1/space/:space_id`*, fail-open when no token was present. Shipping the
permission model without the one authoritative way to read it is what produces
those. "Wait for a consumer" is also a deadlock — they cannot integrate against an
endpoint that does not exist — and it does not apply to an additive field on an
endpoint fleet and drive already call on **every request**: no new route, no new
secret, no new attack surface, no new rate-limit dimension. The things that *do*
carry rot risk (a new internal route with a per-consumer secret and an
unestimatable QPS; machinery nobody drives) stay out of scope.

*Why a point query and not a list.* The caller already knows which project it cares
about — the `project_id` is on the resource row it is serving. So the request names
the projects and the response answers only those:

```http
POST /v1/auth/verify?include=context
{ "token": "…", "space_id": "…", "project_ids": ["p_1", "p_2"] }
→ { …, "projects": [ { "project_id", "member", "role", "capabilities", "member_epoch" } ] }
```

A `member: false` item carries **only** `project_id` and `member` — no role, no
capabilities, no epoch — so "not a member", "no such project" and "a project in
another Space" are one indistinguishable answer. Handing a non-member the epoch would
leak that the project exists and how often its membership has changed.

Returning every project the user belongs to would force a truncation contract
(`truncated` + cursor) for a list bounded only by the 1000-per-Space quota. A point
query makes the response `O(asked)` instead of `O(user's projects)`, so **truncation
stops being a case that has to be designed correctly**. Callers that genuinely need
a list use the existing user-facing `GET /v1/space/:space_id/projects`, which is
paginated; the judgment path never reads a member list. Cap `project_ids` at 50 and
reject an over-long request with `err.shared.param.invalid`; a project id outside
`space_id` is answered `member: false` rather than rejected, because rejecting it
would turn the endpoint into a way to probe which Space a project lives in.

*What must be fixed in the same PR, because it is the same function.*
`queryUserSpaceContext` has two defects that Project fields would otherwise inherit:
`spaces` truncates at 100 with only a server-side `Warn` and **no wire signal**
(`modules/user/api.go:4848-4867` — the over-fetch to `LIMIT 101` already computes
the fact, it is simply not returned), and on a query error the handler returns an
empty `spaces` while still setting `context_included = true`
(`:4803-4813`), making "belongs to no Space" and "the lookup failed" identical on
the wire. Both are live defects today, independent of Project. If PR-3 slips, split
these two out and ship them alone.

*What stays sealed in octo-server.* The response carries `member`, `role` **and**
explicit `capabilities`, because a consumer that derives permissions from a role
number re-implements the permission matrix and drifts the first time it changes.
Transitive owner/admin protection, the system-bot exemption and "does `removing = 1`
count as a member" are answered here and never exported as rules.

*What the field may not be used for.* Only to narrow. A consumer must keep its
existing Space check and layer the project answer on top, because the Space→project
cascade is asynchronous (D4): between a Space removal committing and the cascade
running, the project row still exists. The Space half fails first, so the conjunction
is safe and the disjunction is not.

## Reconcile additions

All four are read-only, cursor-paged, bounded on rows examined, and subject to
`TestReconcileQueriesAreBounded`.

- **I2** — active `group_member` rows in a group with `project_id != ''` whose uid
  is not an active project member. Driven **group-first** (the new
  `(space_id, project_id)` index), then `group_member` by `group_no`: no index
  leads with `is_deleted`, so a member-first scan would walk the table.
  **Exemptions, each for the same reason P0's I1 scan has exemptions —** an alert
  that fires on normal operation is noise before the feature has a user: uids with
  `removing = 1` and a pending cascade job; uids with a pending Space-removal
  cleanup job; members of a **banned** Space (`space.status = 2`), because
  `CheckMembershipForCleanup` deliberately leaves their seats alone; whitelisted
  system bots.
- **I3** — `group.project_id` pointing at a disbanded project, a project in a
  different Space, or a project that does not exist.
- **`removing` stalled** — `removing = 1` past a configured age. A **separate**
  alert from I2, with the opposite meaning: I2 says the invariant broke, this says
  the machinery stopped. The group-creator case (D4) surfaces here, so the alert
  text must name it or on-call will chase a phantom.
- **Stale subscriber** — WuKongIM's subscriber set versus `group_member`, per
  `(channel, uid)`. This is the observable face of D6's inherited leak, and the
  only P1 answer to it.

Metrics: admission rejections **labelled by entry point** (the design brief is
right that this is what exposes a path someone forgot to convert), cascade backlog
and abandoned counts, I2/I3 violation gauges, `removing` stall count. Gauges
publish only on a completed rotation, as P0's do — a truncated tick has counted
part of the keyspace.

## Implementation slices

Three PRs. The ordering is load-bearing: **no PR may leave a state where a
project group can exist without a cascade behind it.**

**PR-1 — schema, predicate, single entry, guard, reconcile (inert in production)**

- Migration: `group.project_id` + `(space_id, project_id)`, in
  `modules/project/sql/`.
- `pkg/project` with the session and Tx predicates.
- `AdmitOrRestoreMemberTx` + the removal funnel wrapper; all eleven admission
  paths and the seven removal funnels converged; `botfather` and `joinPresetGroups`
  brought inside; A9 `IService.AddMember` deleted (zero non-test callers) and the
  three dead DAO methods deleted.
- The D8 source guard, the reconcile I2/I3 scans, the metrics.
- `validatePresetGroupIds` gains a semantic check rejecting groups with
  `project_id != ''`, and `joinPresetGroups` re-checks at execution time. Note the
  current `validatePresetGroupIds` (`modules/space/api.go:554`) is a pure string
  validator with no database access — the semantic half belongs at the call site
  (`:474`) or the function needs a session.
- `CheckForbiddenLoop`: filter `is_deleted = 0` in
  `queryForbiddenExpirationTimeMembers`, clear `forbidden_expir_time` in
  `DeleteMemberTx`.
- **No path accepts a `project_id` from a client in PR-1.** The column exists, the
  gate exists and is tested, and the only way a row gets `project_id != ''` is a
  test fixture. This is what makes PR-1 a pure refactor in production and lets it
  be reverted independently.

**PR-2 — the cascade, and only then the create parameter**

- `RegisterProjectMemberRemovalStep` / `RegisterProjectDisbandStep` in
  `modules/project`; `modules/group` registers "detach this uid from this project's
  groups" and "revert this project's groups to Space-direct" at construction.
- `octo_project_member.removing` + `octo_project_member_removal_cleanup` + the
  worker, per D4/D5.
- The P0 Space cascade's ownerless-disband branch reaches the detach step.
- `POST /v1/group/create` accepts optional `project_id`, validated against
  `space_id` and gated by `OCTO_PROJECT_CREATE_ENABLED`.

**PR-3 — the read contract for other systems** (`modules/user`, per D11)

Independently revertible and not on PR-1/PR-2's critical path, so it can land in
parallel. Three changes to one function pair (`authVerifyToken` /
`queryUserSpaceContext`):

- Fix the silent `spaces` truncation: return the fact the over-fetch already
  computes.
- Stop setting `context_included = true` when the context lookup failed.
- Accept `space_id` + `project_ids[]` (cap 50) in the request body and answer only
  those projects, with `member` / `role` / `capabilities` / `member_epoch`.

The first two are live defects; if this PR slips, split them out and ship them
alone. Nothing here creates a route, a secret, or a rate-limit dimension.

**PR-4 — the observation scans** (stale subscriber, `removing` stall) if PR-2 grew
too large to review; otherwise fold into PR-2.

## Non-regression: Space, group, thread, DM and IM transport

**P1 is not P0.** P0 could promise it changed no existing messaging behaviour
because it touched no existing file. P1 rewrites the admission and removal paths
of every group in the product, adds a column and an index to the `group` table,
and changes what `botfather` does when it deletes a bot. The honest statement is
therefore not "nothing changes" but **"the following things change, and here is how
each is bounded and verified"**.

**Unchanged by construction:** DM (including `dm_space_presence` and the symmetric
fake channel IDs), every channel-ID derivation in `pkg/space/channel.go`,
`category`, message storage and search, the sidebar and conversation-sync
endpoints' request and response shapes, `space_member` and its middleware, and
`group_member`'s schema (no column added, no row rewritten by the migration).

**Changed, deliberately, and each with its own verification:**

- **C1 — Space-direct groups must take the same path they take today, and must not
  pay for the gate.** `project_id = ''` short-circuits *before* any project query.
  Verified by counting queries on an admission into a Space-direct group, not by
  reading the code: a gate that runs and passes is a latency regression on every
  group join in the product.
- **C2 — the single entry must write every column the eleven paths wrote.** This
  is the largest regression surface in P1. A missing `version` breaks incremental
  member sync; a missing `vercode` breaks invite links; a missing `is_external` /
  `source_space_id` breaks external-member display; a lost `bot_admin = 0` reset
  lets bot-admin survive leave-and-rejoin; a missing `IMAddSubscriber` means the
  member joins and receives nothing. Verified per column, per branch
  (insert *and* restore), against the pre-change behaviour of each path.
- **C3 — `botfather`'s bot deletion gains side effects.** Converging
  `command.go:658` onto `RemoveGroupMembers` means a bot removal now emits the
  removal system message, the `CMDGroupMemberUpdate`, the invited-bot cascade and
  the IM unsubscribe that the raw `UPDATE` skipped. Some of that is the fix; the
  system message is a **user-visible change** and needs the existing
  `SuppressRemoveNotice` / `BotCascadeTipAction` treatment
  (`modules/group/service.go:1045,1054,1058`) rather than being discovered in
  production.
- **C4 — the `group` table's query plans change.** `(space_id, project_id)` is the
  first index to serve `space_id`, so `queryGroupsWithMemberUIDAndSpaceID` and
  `modules/bot_api/groups.go:51` get new plans. Expected to be faster; verified
  with `EXPLAIN` before and after, and the index build timed against production row
  counts.
- **C5 — the existing Space→group cascade must keep working.** P0 already ships
  the test for this shape
  (`TestSpaceRemovalStillRemovesFromGroupsWhenProjectStepFails`, driven through the
  real endpoint and asserted on the `group_member` row). P1 adds the mirror: the
  Space cascade still removes the user from their groups with the **new project
  detach step deliberately failing**.
- **C6 — thread behaviour is unchanged, and that is a claim about existing code.**
  Thread admission already gates on `ExistMemberActive` against the parent group
  (`modules/thread/api.go:349,429,519,643,941`, `service.go:193`), so I2 reaches
  threads transitively with no new gate; thread cleanup on removal already runs
  inside `RemoveGroupMembers`. P1 adds a defensive assertion in thread's
  `InsertMemberTx` and nothing else.
- **C7 — `modules/base/event` is not used as a carrier for anything in P1.** A
  listener error only sets the row to `Fail`, and `QueryAllWait` selects `Wait`
  only, so `Fail` rows are never retried
  (`modules/base/event/api.go:58-70`). The cascade is an outbox for this reason.
- **C8 — module registration still boots the whole server.** `modules/project` and
  `modules/group` are blank-imported from `internal/modules.go`, and
  `mustLookupSharedCode` panics at init by design, so one unregistered code is "no
  IM service at all". P0 carries a real boot smoke test
  (`TestServerBootsWithProjectRegistered`); P1 extends it.
- **C9 — `/v1/auth/verify` is a hot gateway path, and PR-3 touches it.** It runs on
  every request of every subsystem that fronts octo-server, its limiter is per-IP at
  1000/min so all gateway pods share a bucket, and the three verify routes carry no
  `AuthMiddleware`. Two rules follow: the response shape for callers that do **not**
  pass `include=context` stays byte-identical (golden-payload assertion), and the
  added project lookup is one bounded query, not a per-project loop.

### Non-regression acceptance

- [ ] `go test ./modules/group/... ./modules/thread/... ./modules/space/... ./modules/message/... ./modules/botfather/... ./modules/bot_api/...`
      passes with **no existing test file edited** (new files are fine). An edited
      assertion is a changed contract and must be justified in the PR body.
- [ ] Admission into a Space-direct group issues the **same number of database
      round-trips** as before the change (C1).
- [ ] For each of the eleven admission paths, a test asserts the full written
      column set on **both** the insert and the restore branch (C2).
- [ ] A member who left a preset group can be re-added by `joinPresetGroups` —
      the 1062 defect at `modules/space/api.go:1366` is gone, verified against a
      real MySQL 8.0, not mocked.
- [ ] Bot deletion via botfather produces exactly the intended notice behaviour,
      asserted on emitted messages (C3).
- [ ] `EXPLAIN` before/after for `queryGroupsWithMemberUIDAndSpaceID` and
      `modules/bot_api/groups.go:51`, plus a measured index-build time on
      production-scale row counts (C4).
- [ ] The Space member-removal cascade still removes the user from their groups
      with the new project detach step deliberately failing (C5).
- [ ] `git diff --stat` touches no file under `modules/message/`,
      `pkg/space/channel.go`, `modules/category/`, and no existing migration.
- [ ] The reconcile JOINs across `group` / `group_member` / `octo_project*` run on
      a database created with the CI collation without error 1267.

## Acceptance

**I2 enforcement**

- [ ] For each of the eleven admission paths, a test admits a non-project-member
      into a project group and asserts rejection — including A10
      (`joinPresetGroups`, which must skip) and **A11** (the un-blacklist branch).
- [ ] The rejection is enforced on the **restore** branch, not only the insert
      branch.
- [ ] The composite predicate refuses a uid who is an active project member but
      has lost their Space seat (the P0 cascade window).
- [ ] A whitelisted system bot is admitted; a non-whitelisted bot is not.
- [ ] The check runs **inside** the admission transaction and takes a shared lock,
      proven by a concurrent test where the project seat is removed between the
      check and the commit.
- [ ] The D8 source guard fails when a new file writes `group_member` outside the
      allowlist, and its failure message lists every offender.
- [ ] `pkg/project` does not import `modules/project`; `go list -deps
      ./modules/project | grep modules/group` is 0.

**Schema and I3**

- [ ] `group.project_id` is `NOT NULL DEFAULT ''`; no code path writes `NULL`.
- [ ] Creating a group with a `project_id` from another Space is rejected.
- [ ] No endpoint can change a group's `project_id` once set; the guard forbids
      such an `UPDATE` outside the create path and the detach step.
- [ ] Existing groups are unaffected: `project_id = ''`, no gate, no behaviour
      change.

**Cascade**

- [ ] Removing a member from a project detaches them from every group of that
      project, verified on `group_member` rows and across more than one worker
      batch.
- [ ] `removing = 1` is treated as a non-member by the project member list, the
      admission gate and the middleware role resolution, **while `status` is still
      1**.
- [ ] Re-adding during the window clears `removing`, cancels the job, and the
      worker — re-reading under lock — stops (D4).
- [ ] A group whose creator is the departing member blocks that group's detach,
      leaves `removing = 1`, and raises the stall alert rather than force-transferring.
- [ ] Disbanding a project reverts its groups to `project_id = ''` and leaves
      `group_member` untouched; reachable from **both** the disband handler and the
      P0 ownerless-disband cascade branch.
- [ ] The cascade step is idempotent: a rerun affects zero rows and does **not**
      bump `member_epoch` (the rule P0 established and nearly broke).
- [ ] `member_epoch` increments in the same transaction as `removing = 1` and as
      the final `status` flip, and only ever by `+1`.
- [ ] The outbox worker heartbeats its lease, and a long final attempt is not
      marked `abandoned` (D5).

**Reconcile and observability**

- [ ] Every new scan is cursor-paged and bounded on rows **examined**; the existing
      guards cover the new queries.
- [ ] The I2 scan reports zero violations during a normal member removal (all four
      exemptions), and reports one when a violation is seeded directly in SQL.
- [ ] `removing` stall is a distinct alert from I2, and its text names the
      group-creator case.
- [ ] Admission rejections are labelled by entry point, and every entry point's
      label is emitted at least once by the test suite — a label never emitted is a
      path that is not enforcing.
- [ ] Cursors persist across ticks and gauges publish only on a complete rotation.

**The read contract for other systems (PR-3, D11)**

- [ ] A request naming N project ids returns exactly those N answers — never the
      caller's other projects — so the response size is `O(asked)`.
- [ ] `member: false` is returned for a project the caller is not in, for a project
      id that does not exist, and for one that lives in another Space — and that item
      carries **no** `role`, `capabilities` or `member_epoch`. All three cases are
      byte-identical on the wire; the distinguishing reason appears in the log only.
- [ ] More than 50 project ids is rejected with `err.shared.param.invalid` and
      `details.field = "project_ids"`.
- [ ] The default response shape (no `include=context`) is **byte-identical** to
      today's, asserted against a golden payload — IM clients and admin tools depend
      on it.
- [ ] `capabilities` is emitted explicitly; no consumer needs `role` to compute it.
- [ ] The top-level `role` (platform role, string) and `projects[].role` (project
      role, int) are pinned by a test as distinct fields — same name, different type,
      different meaning.
- [ ] `spaces` truncation is visible on the wire, and a seeded >100-Space user
      proves it.
- [ ] A forced context-lookup failure yields `context_included = false`, and is
      distinguishable from a user who belongs to no Space.
- [ ] The added lookup is bounded and adds at most one query to the request; the
      endpoint's per-IP limiter (1000/min, burst 100 — gateway traffic) is unchanged.
- [ ] Nothing in the response exposes anything beyond what the token holder may
      already know — these three routes carry no `AuthMiddleware`.
- [ ] `member_epoch` returned here equals the column, and a membership write is
      observed to change it.

**Conventions**

- [ ] Every new user-facing error goes through `httperr.ResponseErrorL` with a
      registered code; `make i18n-extract-check` and `make i18n-lint` pass; zh-CN
      translations added.
- [ ] New group files are added to `TestGroupNoLegacyResponseError`'s file list.
- [ ] New/changed routes have an `authtree` census entry, including an explicit
      "deliberately not Project-scoped" record where that is the answer.
- [ ] New write paths in `modules/project` are added to
      `TestWritePathsRevalidateTheActorSpaceSeatInTx` and
      `TestEveryWritePathEmitsAnAuditEntry`.
- [ ] The lock order `space_member → space → project → group → group_member →
      octo_project_member` holds on every new path, is pinned in a comment the way
      `modules/project/db.go:344-345` pins P0's, and is covered by a deadlock test
      over the crossing paths.

## Rollback

Asymmetric, and worth being blunt about.

`OCTO_PROJECT_CREATE_ENABLED=false` stops new project groups from being created
and keeps existing ones enforcing — which is the only rollback the design brief
permits (「不能放松已有 project 群的约束」). It does **not** undo PR-1: the write-path
convergence is a refactor of code every group in the product runs, and it is
revertible only by reverting the PR. That is the honest cost of the convergence,
and it is why PR-1 ships with no way to produce a project group.

Dropping `group.project_id` and its index reverts the schema. `octo_project*`
tables and the outbox are P1-owned and droppable. Rolling back PR-2's migration
discards pending cascade jobs — check the pending count first, the same warning
`space_member_removal_cleanup`'s migration carries.

## Open questions (product, not blocking the schema)

- **Disbanding a project that owns groups.** The design recommends (a) groups
  revert to Space-direct with members intact, over (b) disbanding them. This brief
  implements (a); (b) destroys data and, because group disband leaves
  `group_member` rows, would not even clean up. Needs a product confirmation, not a
  technical one.
- **The system-bot exemption whitelist.** The design proposes `botfather` /
  `fileHelper` / `u_10000` exempt, ordinary bots requiring explicit project
  membership. `pkg/space.SystemBotList()` / `IsSystemBot()` already exist; confirm
  the list is the right one for this purpose.
- **A group creator blocking their own detach.** D4 raises an alert and waits for a
  human handover. Auto-transferring to another project admin is the alternative,
  and it changes who controls a group without anyone asking — the same shape as the
  ownerless-project question P0 escalated and product answered for projects. Worth
  answering the same way, for groups.
- **Whether the org-directory listeners are live.** D7 defers deletion pending a
  production observation window. Someone has to look at the `event` table.
- **The 「客户联合交付」 narrative.** Still unresolved from P0: the prototype copy
  promises external parties in a project, and v1 has no external members and
  Project is not a read boundary. P1 makes project groups real, which is when
  users will start forming that expectation.
