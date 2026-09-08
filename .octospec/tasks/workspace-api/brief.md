---
type: Task
title: "Task: workspace-api"
description: Implement the explicit Workspace metadata, membership, role, owner-transfer, and Group↔Workspace API contracts with isolated regression coverage.
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
active membership, Workspace roles, owner transfer/leave, lifecycle, and the
Group-owned Workspace relation APIs. Keep organization and resource boundaries
server-authoritative and fail closed. Preserve the existing `project` module and
existing Group API semantics as separate shipped contracts.

## Status

The Workspace server and group-owned relation/creation contracts described here are implemented. Isolated package/regression suites, repository build/vet, i18n/static checks, and real HTTP/DB/IM smoke validation passed; this is repository evidence only, not a deployment or production-acceptance claim.

## Background

Workspace owns only metadata, lifecycle, and membership. Group owns its paired
nullable `workspace_id`/`workspace_linked_by` relation and projects restricted
association metadata. Existing Group relation writes must not grant or revoke
native group membership or downstream ACLs. New Group creation may take a
one-time active Workspace-member snapshot, unioned with explicit members, in the
same transaction; subsequent Workspace membership changes do not synchronize to
that Group. API error responses use the registered localized error envelope and
the semantic status in `error.http_status`.

## Load-bearing list

- Explicit `space_id` selection for Workspace list/create; resource Space is
  derived from the authoritative resource ID, with optional `X-Space-Id` only as
  a consistency assertion.
- Active organization membership, banned/revoked membership, Workspace member
  status, and Workspace role checks on every read and write.
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
  Workspace is supplied; active snapshot members are unioned once, configured
  native system-account policy is enforced (enabled accounts may participate),
  and IM failure invokes atomic local compensation; if compensation itself fails,
  local state remains coherent and the underlying cleanup cause is returned
  without claiming rollback success.
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
  status codes, localized error envelope, header precedence, and auth behavior.
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
  and de-duplication, one-time snapshot behavior, configured native
  system-account policy enforcement, and rollback/compensation for IM failure
  including a compound cleanup failure without falsely reporting rollback
  success.
- `pkg/errcode/workspace.go` registers all Workspace semantic codes and
  `pkg/errcode/group.go` registers `err.server.group.workspace_conflict`; every
  code has the runtime zh-CN entry and internal 5xx codes are marked internal.
- `modules/group/api_i18n_test.go` scans every migrated Group production handler,
  including Workspace relation files, for legacy raw error responses.
- Central validation runs after all slices settle: fresh test DB per package
  using the CI reset procedure, bounded-pool focused tests plus the full package
  matrix, `go vet`, i18n extraction/check/lint, and the repository's formatter
  and static checks. No test claims depend on a production database.
