---
type: Task
title: "Task: workspace-api"
description: Implement the explicit Workspace metadata, membership, role, owner-transfer, and Group↔Workspace API contracts with isolated regression coverage; the Workspace resource-level DELETE route is intentionally unregistered.
tags: ["workspace", "acl", "space", "wire-contract", "i18n", "test"]
timestamp: 2026-09-08T00:00:00Z
# --- octospec extension fields ---
slug: workspace-api
upstream: self
source: self
---

# Task: workspace-api

## Goal

Implement the wire and service contracts in `.local/assets/workspace-api.md` and
`.local/assets/workspace_design.md` for explicit Workspace creation, metadata,
active membership, Workspace roles, owner transfer/leave, and the supported
Workspace lifecycle surface (create/list/detail/update; no resource-level
DELETE), plus the Group-owned Workspace relation APIs. Keep organization and
resource boundaries server-authoritative and fail closed. Preserve the existing
`project` module and existing Group API semantics as separate shipped
contracts.

## Status

The Workspace server and group-owned relation/creation contracts described here are implemented, including the stability behavior recorded below. This is repository status only; it is not a deployment or production-acceptance claim.

### Stability status

- Read authorization, results, counts, pagination, and projections use one explicit RR read-only transaction. Unbound Group GET also checks current Space, user/seat, and native membership inside that read transaction.
- Writes lock the exact seat/user keys, then the shared Space and exclusive Workspace rows in the established order. Group prefetch/current recheck does not require an extra connection while the bounded write pool is held.
- Group creation prepares request configuration and every required `GenSeq` value before opening its write transaction. `MemberUIDs` is the complete active current-read Workspace set; `EligibleMemberUIDs` is the prepared active destination-seat and eligible-account subset used for the final one-time group membership. Candidate expansion is bounded at three total attempts, including the initial attempt.
- Creator and explicit candidates retain native Group policy, including valid explicit external members; an absent, inactive, or otherwise invalid explicit member fails the request atomically. Workspace changes after creation do not synchronize the group membership.
- Workspace and Group relation keyword searches use SQL-mode-independent literal matching for `%`, `_`, and `!`.
- Space-seat removal asynchronously deactivates non-owner Workspace memberships through the shared durable cleanup registry; restoring a Space seat does not restore prior Workspace roles without explicit re-admission.
- Ordinary Group creation retains its existing missing/destroyed-member filtering and batched Space-membership classification; strict all-or-none candidate validation remains scoped to Workspace-backed creation.
- Outbox enqueue persists every registered target independent of current credentials. Delivery waits while a target is unavailable, and Redis stream appends do not evict older entries by raw length.
- Verification passed on an isolated candidate: full `modules/workspace` and `modules/group` package suites, repository build/vet, i18n extraction and lint checks, and real TCP HTTP/DB/IM smoke. Group suite and HTTP smoke were rerun after moving relation reads into `Service`. Dependencies were dedicated MySQL 8.0.46, Redis 7, and WuKongIM instances. The full Space suite still reports member-removal cleanup-worker failures, also observed in the pre-fix snapshot; individual cases can pass independently and the failing set varies. Candidate Space source matches that snapshot. The Space suite is not reported as passing.
- Sequence smoke passed all four ordinary/Workspace × cold/refill scenarios, each in a fresh process and schema. Creation ran with `MaxOpenConns=1`, including Bot. Cold runs observed absent `group`/`groupMember` sequence rows followed by `min_seq=1001000`, step `1000`; refill runs exhausted each block and observed both rows advance to `1002000`. Persisted group/member versions, membership, Bot policy, external-member mapping, and the real IM channel were checked.
- Cross-module lock smoke passed all three scenarios with exactly two application-pool connections: a real Workspace member write interleaved with Space metadata PUT; unrelated Space-member removal completed while prepared Workspace seats remained locked; and Space dissolution waited on the observed seat lock, then completed with subsequent Workspace access denied. `EXPLAIN` selected `spacemember_spaceid_uid`; an independent connection queried only `performance_schema` to observe lock waits.

## Background

Workspace owns only metadata, active membership, roles, and its status field. Group owns its
paired `workspace_id`/`workspace_linked_by` relation and projects restricted
association metadata. Existing Group relation writes must not grant or revoke
native group membership or downstream ACLs. New Group creation may take a
one-time active eligible Workspace-member snapshot, unioned with explicit
members, in the same transaction; creator and explicit members retain native
Group policy, valid external explicit members remain allowed, and invalid
explicit members fail the request atomically. Subsequent Workspace membership
changes do not synchronize to that Group. API error responses use the
registered localized error envelope and the semantic status in
`error.http_status`.

## Load-bearing list

- Explicit `space_id` selection for Workspace list/create; resource Space is
  derived from the authoritative resource ID, with optional `X-Space-Id` only as
  a consistency assertion.
- Active organization membership, banned/revoked membership, Workspace member
  status, and Workspace role checks on every read and write; Space-seat removal
  deactivates non-owner Workspace memberships through the durable cleanup chain.
- Exactly one active Workspace Owner; Owner transfer is atomic and leaves the
  previous Owner as active Admin. Owner cannot be removed or self-exit.
- Partial metadata PUT (submitted fields only), read-only field protection,
  Unicode name limits, duplicate-name allowance, idempotent same-value writes.
- All-or-none member batches of 1–100, same-role active re-add as a no-op,
  conflicting-role rejection, and Admin management of any non-Owner member.
- Stable direct JSON DTOs and `{count,list}` pagination with true totals,
  non-nil lists, sane defaults/caps, and overflow-safe out-of-range pages.
- Group-owned single relation and reverse lookup: same-Space source/target
  checks, target/source active membership, native Group owner/admin checks,
  atomic rebind, `linked_by` preservation on no-op, and unbind without native
  membership changes.
- New Group creation Workspace snapshot: empty explicit members is valid when a
  Workspace is supplied; the one-time snapshot uses the active eligible
  Workspace-member subset, then unions it with creator and explicit members
  under native Group policy. Valid explicit external members remain allowed;
  an absent or invalid explicit member fails the whole request atomically.
  Configured native system-account policy is enforced, and IM failure invokes
  atomic local compensation; if compensation itself fails, local state remains
  coherent and the underlying cleanup cause is returned without claiming
  rollback success. Later Workspace membership changes do not synchronize to
  that Group.
- Domain-event intent is transactionally persisted for every registered target;
  runtime credentials gate delivery rather than enqueue, and the Redis stream is
  not truncated independently of consumer progress.
- Registered server error codes and runtime zh-CN translations, including safe
  internal/dependency handling; group relation fields must not leak through
  ordinary `GroupResp`.
- Regression tests use bounded database pools and isolated test-only services;
  MySQL test schemas must use `utf8mb4_general_ci` consistently so deliberate
  cross-table joins do not reintroduce the 0900-vs-general collation failure.

## Out of scope

- Migrating or aliasing the existing `project` module into Workspace.
- Default, kind-based, automatic, background, or quota-driven Workspace
  creation; business version fields; new personnel-search endpoints.
- Workspace-owned Drive, Docs, Loop, or Group relation tables, resource mapping,
  downstream ACL decisions, or automatic ACL/member synchronization.
- Changing native Group role/is_external/status policy except where the optional
  Workspace snapshot is explicitly part of Group creation.
- Service-identity or `X-UID` authorization in place of the user's session;
  pure backend Workspace credentials are not defined by this task.
- Changes to the original design/API documents, except factual implementation-status synchronization requested by this task, production data, or destructive use of `testutil.CleanAllTables` against a non-test database.

## Acceptance

- Workspace HTTP routes return the documented methods, paths, direct DTOs,
  status codes, localized error envelope, header precedence, and auth behavior;
  Workspace resource-level DELETE is unregistered while member-removal,
  self-exit, and Group-relation DELETE routes remain available.
- Tests cover owner/admin/member boundaries, owner-protected actions, partial
  updates/no-op, active/revoked/banned organization isolation, header mismatch,
  pagination totals and large-page overflow, atomic member batches/re-add, and
  transfer/remove/leave races.
- Group tests cover `group/my` query combinations and numeric native roles;
  relation metadata visibility versus native content authorization; source and
  target permission checks; cross-Space rejection; atomic bind/rebind; no-op
  `linked_by`; unbind preservation of native membership; and no relation-field
  leakage in `GroupResp`.
- Group-create tests prove snapshot-only requests (no explicit members), union
  and de-duplication, active eligible snapshot behavior, configured native
  system-account policy, native handling of valid explicit external members,
  atomic failure for invalid explicit members, and rollback/compensation for IM
  failure including a compound cleanup failure without falsely reporting
  rollback success.
- `pkg/errcode/workspace.go` registers all Workspace semantic codes and
  `pkg/errcode/group.go` registers `err.server.group.workspace_conflict`; every
  code has the runtime zh-CN entry and internal 5xx codes are marked internal.
- `modules/group/api_i18n_test.go` scans every migrated Group production handler,
  including Workspace relation files, for legacy raw error responses.
