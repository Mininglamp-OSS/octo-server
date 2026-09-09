---
type: Task
title: "Task: project-collaboration-roles"
description: Add project-scoped, multi-select collaboration-role labels for human members without changing project authorization roles.
tags: ["project", "space", "acl", "wire-contract", "migration", "testing"]
timestamp: 2026-09-09T00:00:00+08:00
slug: project-collaboration-roles
upstream: "Mininglamp-OSS/octo-server#870"
source: self
---

# Task: project-collaboration-roles

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Allow a Project to classify each **human** member with zero or more project-scoped
collaboration roles, for example `产品`、`前端`、`后端`、`HR`. A Project has a small
set of system-provided defaults and its owner may create custom roles. The roster
and member-role picker render those labels as the prototype shows.

This changes collaboration metadata only. It must not grant, revoke, or infer any
authorization.

## Background

The existing `octo_project_member.role` is an authorization enum, not a job-title
field:

- `0=RoleCommon`、`1=RoleAdmin`、`2=RoleOwner` in `modules/project/model.go`.
- It drives `canUpdateProject`、`canManageMembers`、`canChangeMemberRole`, owner
  transfer and disband behavior in `modules/project/service.go`.
- The existing member response exposes that integer as `role`; the project
  response exposes server-derived `capabilities`. Neither field may be renamed,
  repurposed, or derived from collaboration-role labels.
- An AI agent is already a project seat, but roster data distinguishes it with
  `robot` and `owner_uid`. The supplied prototype renders its collaboration role
  as `-` and separately shows its AI-state attributes.

Terminology is intentionally explicit:

| Term | Examples | Security meaning |
| --- | --- | --- |
| Project member permission | 成员、管理员、负责人 | Existing authorization role; security boundary |
| Collaboration role | 产品、前端、后端、HR | Project-local descriptive label only |
| AI-agent state | 项目分身、云端运行、不可移除 | Existing agent lifecycle and presentation |
| Platform-manager role | admin、superAdmin 等 | Outside this task |

## Decisions

### D1 — independent, project-scoped many-to-many model

Add an immutable-ID role-definition entity and a membership association. A member
may hold zero or more definitions from the same Project; a definition may be used
by many members. Never store a comma-separated role name list in
`octo_project_member`, and never use a role name as an identifier.

Proposed tables:

```text
octo_project_collaboration_role
  role_id, project_id, builtin_key, name, normalized_name, source, creator_uid,
  created_at, updated_at

octo_project_member_collaboration_role
  project_id, uid, role_id, created_at
```

- The association primary key is `(project_id, uid, role_id)`.
- A unique database constraint on `(project_id, normalized_name)` prevents
  same-Project duplicates under concurrent creates. The normalization policy is
  trim plus case-insensitive matching. The application stores the lower-cased
  value under `utf8mb4_bin`, so accents remain distinct while case-only variants
  collide; the collation is pinned rather than inherited from a deployment database.
- `source` is `builtin` or `custom`. It is descriptive API data, not an
  authorization grant.
- `builtin_key` is non-empty only for a system-provided definition and is the
  stable identity used by idempotent create/backfill; custom rows have an empty
  key.
- Add `octo_project.collaboration_role_epoch BIGINT NOT NULL DEFAULT 0` and
  increment it atomically for a changed definition or association. Do **not**
  reuse `member_epoch`: fleet/drive use it to detect membership changes.
- Do not add foreign keys to soft-closed Project/member rows. Application writes
  must instead validate active Project and active member state under the existing
  transaction/lock discipline.

### D2 — built-ins are templates, not RBAC presets

The first release seeds these four immutable defaults for every new Project:
`产品`、`前端`、`后端`、`HR`, matching the reviewed prototype. Each has a stable
system key and `source=builtin`.

Built-ins are selectable but cannot be renamed or deleted. A Project owner can
create, rename, and delete custom definitions. Deleting a custom definition
atomically removes its member associations; it does not affect the member's
existing permission role.

The V1 catalog is confirmed as exactly these four server-provided display names;
V1 does not localize them by client language. Adding or translating a future
built-in is a reviewed migration/backfill operation, not a client-side constant.

### D3 — scope and permission matrix

V1 permits role binding for humans only. A target whose roster row is `robot=1`
cannot receive collaboration roles, including an empty/clear request through the
new write endpoint. The roster returns an empty list for agents and the client
renders `-`.

| Operation | Project owner | Project admin | Common member | Space admin not in Project |
| --- | ---:| ---:| ---:| ---:|
| Read role catalog / roster labels | Yes | Yes | Yes | Existing roster-read widening only |
| Bind or clear a member's labels | Yes | Yes | No | No |
| Create, rename, delete custom definition | Yes | No | No | No |
| Change `owner/admin/member` permission role | Existing rule | Existing rule | Existing rule | Existing rule |

The handler must re-read the caller's Project permission role under the Project
lock. Middleware/cache results may only be a cheap early refusal. A Space admin's
existing read widening must not become Project write authority.

### D4 — wire contract

Keep existing fields byte-compatible. `MemberResp.role` remains the numeric
permission role, and `Resp.capabilities` remains the server verdict on
authorization.

Extend each roster row with:

```json
"collaboration_roles": [
  {"role_id": "...", "name": "前端", "source": "builtin"}
]
```

Add these authenticated, UID-rate-limited routes under the existing
`/v1/projects/:project_id` group:

| Method and route | Contract |
| --- | --- |
| `GET /collaboration-roles` | Current visible definitions plus `collaboration_role_epoch` |
| `POST /collaboration-roles` | Owner creates one custom definition |
| `PUT /collaboration-roles/:role_id` | Owner renames one custom definition |
| `DELETE /collaboration-roles/:role_id` | Owner deletes one custom definition and all its bindings |
| `PUT /members/:uid/collaboration-roles` | Owner/admin replaces the target human member's complete `role_ids` set; `[]` clears it |

Unknown, deleted, foreign-Project, duplicate, or built-in-as-mutable role IDs
must be rejected without partial writes. The target's membership/agent eligibility
should not disclose additional user facts to an unauthorized caller. New failures
use `pkg/errcode/project.go`, `httperr.ResponseErrorL`, and zh-CN translations;
no raw error responses.

The catalog read uses the same `canViewMembers` rule as the roster; an ordinary
Space member who is not in the Project cannot enumerate its role definitions.

### D5 — transaction, lifecycle, and concurrency requirements

- Every write takes locks in the established order: required active Space-member
  seats, then active Project row, then current Project membership/role state.
- Binding validates that the actor and target are active Project members and not
  `removing=1`, that the target is human, and that all requested role definitions
  are active definitions of this Project. It replaces the association set in one
  transaction, then increments `collaboration_role_epoch` only when data changed.
- Catalog mutation and binding serialize on the Project row. A role deleted while
  another request tries to bind it resolves as one successful transaction and one
  clean conflict/invalid-role response; it must never leave an orphan row.
- On direct remove, leave, owner-following agent removal, and Space-removal
  cascade, clear associations when the project seat starts closing, not after the
  asynchronous group-cleanup worker finishes. Re-adding a member starts with no
  collaboration roles.
- Project disband makes role data unreadable with the Project. Retaining
  historical rows is acceptable; this task does not introduce a Project restore
  path.
- Owner transfer and permission-role changes leave collaboration labels intact.

### D6 — validation, limits, and observability

- Names are required, trimmed, reject control characters, are bounded to 32
  Unicode code points, and are unique
  under the pinned normalization policy. Role definitions per Project and labels
  per human member have configuration-backed limits (defaults: 50 total
  definitions including built-ins, 8 labels per member).
- Emit structured audit actions for definition create/rename/delete and binding
  replace/clear. Record actor UID, Project/Space IDs, target UID where applicable,
  role IDs/counts, and a low-cardinality action/reason; never log role names or a
  request body.
- Add low-cardinality metrics for write outcomes/rejections and a bounded
  reconciliation scan for orphan associations or associations on inactive seats.
  A non-zero orphan result is data-integrity work, not permission evidence.
- Add `OCTO_PROJECT_COLLABORATION_ROLE_ENABLED`, default false. It gates only
  these new writes; existing Project reads and all existing Project writes retain
  their current behavior. Turning it off is the rollback: stored labels remain
  readable and no data is deleted.

## Rollout

1. Land DDL, DAO, read contracts, metrics, and disabled write paths.
2. Run an idempotent, bounded backfill that seeds the four built-ins for active
   Projects. It must be restart-safe and record/alert failures; reads must not
   create rows lazily.
3. Verify duplicate-name, orphan-association, and backfill-completeness queries
   in the target environment.
4. Enable writes in a canary environment, then increase deployment scope while
   monitoring rejection, audit, and reconciliation signals. The V1 switch is
   process-wide; it does not claim per-Space targeting.
5. Roll back by disabling only the new write flag. Do not drop the schema or
   erase labels during incident rollback.

## Load-bearing list

- `modules/project/model.go`: the existing numeric permission roles and roster
  response must remain semantically unchanged.
- `modules/project/service.go` / `middleware.go`: server-derived capabilities,
  in-transaction authorization re-read, cache invalidation, owner transfer, and
  lock ordering are security contracts.
- `modules/project/db.go`: active/`removing` predicates, member-epoch write
  discipline, and production collation compatibility are load-bearing.
- `modules/project/space_member_removal.go` and removal workers: a logically
  removed member must not retain project-visible labels or regain them on re-add.
- `modules/project/api.go` / `api_member.go`: AuthMiddleware then
  SharedUIDRateLimiter, Project middleware, localized error envelope, and the
  existing response fields are wire contracts.
- `modules/project/audit.go` and metrics: no high-cardinality labels or user
  content in operation telemetry.

## Out of scope

- Replacing platform-admin RBAC, `user.role`, manager-console admission, MFA, or
  platform capability checks.
- Granting data access, group membership, Bot permissions, or any automation
  authority through collaboration roles.
- Cross-Project, Space-wide, organization-wide, or external-member role
  catalogs.
- Assigning roles to AI agents, role-based mentions, role-based notification
  rules, or automatic task routing.
- Changing the existing owner/admin/member permission matrix or `member_epoch`
  semantics.
- A Project restore workflow or historical audit-search product.

## Acceptance

- [ ] A new Project has exactly the confirmed built-ins once; concurrent create
      or backfill cannot duplicate them.
- [ ] An owner can create a unique custom role, rename it, delete it, and bind it
      to multiple human members. An admin can bind existing roles but cannot
      mutate the catalog.
- [ ] A human member can hold multiple labels; an explicit empty `role_ids` set
      clears all labels. Repeating an identical set is an idempotent no-op.
- [ ] The member roster returns the old numeric `role` unchanged and returns
      complete `collaboration_roles` without N+1 database queries.
- [ ] Common members and Space-only admins cannot write. Permission changes
      between middleware and transaction commit are rechecked and refused.
- [ ] A bot/agent, a missing or closing member, and a foreign-Project/deleted
      role definition are rejected without partial association writes.
- [ ] Concurrent bind/delete and bind/member-remove paths leave no orphan
      association; re-admission does not restore old labels.
- [ ] Direct remove, leave, Space removal, and owner-following agent cleanup
      exercise every relevant association-clear path. Existing Project/group
      cascade behavior remains unchanged.
- [ ] `member_epoch` does not change for catalog or collaboration-label writes;
      `collaboration_role_epoch` advances exactly once for each changed write.
- [ ] All new failures use registered localized error codes; i18n extraction and
      lint checks pass.
- [ ] Focused Project tests cover the matrix and migration/backfill behavior;
      integration tests run with MySQL, Redis, and WuKongIM. Broader tests are
      selected after the final list of touched packages is known.
- [ ] `git diff --check` passes and the PR describes this as non-authorizing
      collaboration metadata, including the feature-flag rollback path.

## Confirmed V1 defaults

- Initial built-ins: `产品、前端、后端、HR`.
- Built-in names are returned as server-owned display strings without locale
  variants in this release.
- Limits default to 50 definitions per Project (including built-ins) and 8 labels
  per human member; both are environment-configurable.
- This server task exposes catalog management and picker APIs. A separate
  dedicated role-management screen is not required by the supplied prototype.
