---
type: Task
title: "Task: project-group-category"
description: Revoke the Project-group ↔ manual-category mutual exclusion (sidebar-project-sections D3). Project groups — associated groups and the dedicated all-member group alike — may be filed into the caller's manual categories, created with a project_id+category_id pair, and appear in the Follow tab alongside their auto-provisioned Project section.
tags: ["wire-contract", "error-response", "i18n", "testing"]
timestamp: 2026-09-21T00:00:00Z
# --- octospec extension fields ---
slug: project-group-category
upstream: self
source: self
---

# Task: project-group-category

> **Status: IMPLEMENTED (server).** Owner decision 2026-09-21; diagnosis context
> recorded in the project-group-follow thread. The prior exclusion was a
> requester-confirmed contract (sidebar-project-sections D3, PR #878); this task
> supersedes that single decision. Everything else in D3's successor surface —
> Project sections ordering, pin/unpin lifecycle, relation metadata semantics —
> is unchanged.

## Goal

Users can add Project groups to their personal Follow/categories: a Project
group moves into or out of a manual category exactly like any other group, can
be created with `project_id` and `category_id` in one request, and its Project
section placement is untouched by either operation.

## Load-bearing surface

- `PUT /v1/groups/:group_no/category` (`modules/category/api.go`
  `moveGroupToCategory`): the `group.project_id` rejection is removed; the
  handler only needs the group's home Space for the effective-space
  computation. Membership-check ordering (anti-enumeration) is preserved.
- `POST /v1/group/create` (`modules/group/api.go` `groupReq.Check`): the
  `ProjectID`/`CategoryID` mutual exclusion is removed.
- `modules/group/service_project_create.go` `CreateProjectGroup`: the
  service-level `CategoryID` refusal is removed; the finish path now writes
  the creator's category through the shared
  `applyCreatorCategoryBestEffort` helper (same best-effort policy as
  `CreateGroup`; extracted into one implementation, not a copy).
- `modules/category/db_group_setting.go` `queryUserGroupsInSpace`: the
  `g.project_id = ''` filter becomes
  `g.project_id = '' OR (gs.category_id IS NOT NULL AND gs.category_id <> '')`
  — an explicitly categorized Project group is visible in the
  category-management tree, while never-categorized ones (NULL *or* the
  legacy empty string, which the Go side already treats as uncategorized)
  stay exclusive to their Project section. The manual-category view is
  opt-in; without the narrowing, uncategorized rows fell into the default
  category and dual-listed every Project group with no user action.
- `modules/category/model.go` + `api.go`: `groups[]` entries of
  `/v1/spaces/:space_id/categories` (and the embedded `category` sections of
  `/sidebar-sections`) gain an additive `project_id` (`omitempty`). A
  categorized Project group is intentionally present in both its Project
  section and a manual category; without this field a client cannot tell
  which category entries are Project groups, so the contract's
  "de-duplicate client-side" instruction was not actionable.
- `modules/category/sql/20260909000001_sidebar_project_sections.sql`: the
  destructive `UPDATE group_setting ... WHERE g.project_id <> ''` cleanup is
  **retired to a no-op** (`SELECT 1`) with the full rationale inline. After
  the reversal that statement would delete valid user data on any replay
  (down/up cycle, migration-ledger rebuild), and `group_setting` carries no
  timestamp the WHERE could be narrowed by. Already-applied environments
  keep their result; the ledger row stays.
- Error envelope: `err.server.category.project_group_cannot_categorize`
  removed from `pkg/errcode/category.go` and both locale files as part of the
  feature removal (clients must no longer branch on it).

## Out of scope

- Server-side dual-display de-duplication: a categorized Project group
  legally appears in both its Project section (`groups[]` relation metadata)
  and the caller's Follow tab. Rendering is the client's two independent
  views; no server change attempts to merge or de-prioritize one. The
  category-side entry carries `project_id` so a client CAN reconcile or
  de-duplicate across the two views if it chooses to.
- Historical data: rows created under the exclusion era are valid either
  way; no data migration is needed. The D3 cleanup statement is retired to a
  no-op (see Load-bearing surface) so a replayed migration cannot delete
  assignments that are legal again; assignments it already cleared in
  applied environments stay cleared, and re-categorizing is a user action.
- Project-section visibility predicates, pin/unpin semantics (D4), and
  relation-metadata perms — untouched.

## Acceptance

- `TestMoveGroupToCategoryAllowsProjectGroup` pins the move endpoint for a
  Project group (move into a category is 200, persists, and bumps
  follow_version exactly once; clear is 200) plus the tree read-back: the
  categorized group renders under its manual category, carries its
  `project_id`, and is absent from the default bucket; a never-categorized
  Project-group member (NULL or legacy `''` category) renders in neither;
  a plain group's entry carries no `project_id` (manual-category view stays
  opt-in).
- `TestGroupReqCheckAllowsProjectAndCategoryTogether` pins the create
  request gate;
  `TestGroupCreateProjectGroupWithCategoryPersistsCreatorSetting` pins the
  HTTP create path end-to-end, asserting the creator's persisted
  `group_setting.category_id` for a `project_id + category_id` request.
- `TestSidebarSectionMigrationBackfillsOrderAndPreservesProjectGroupCategories`
  replays the migration file and asserts the retired cleanup no longer
  touches Project-group assignments.
- `make i18n-extract-check` and `make i18n-lint` pass with the removed
  error code not registered anywhere.
- `go test ./modules/category/... ./modules/group/...` green; gofmt clean
  on all changed Go files.
- `docs/sidebar-project-sections-api.md` §3/§7 and the
  sidebar-project-sections brief describe the reversed contract.
