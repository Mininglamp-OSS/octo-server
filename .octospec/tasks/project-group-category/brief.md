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
  `g.project_id = ''` filter is dropped so categorized Project groups are
  visible in the category-management tree; pre-reversal behavior restored.
- Error envelope: `err.server.category.project_group_cannot_categorize`
  removed from `pkg/errcode/category.go` and both locale files as part of the
  feature removal (clients must no longer branch on it).

## Out of scope

- Server-side dual-display de-duplication: a categorized Project group now
  legally appears in both its Project section (`groups[]` relation metadata)
  and the caller's Follow tab. Rendering is the client's two independent
  views; no server change attempts to merge or de-prioritize one.
- Historical data: rows created under the exclusion era are valid either
  way; no migration needed. The one-way D3 cleanup migration
  (`20260909000001_sidebar_project_sections.sql`) is not reversed — rows it
  cleared were historical assignments, and re-categorizing is a user action.
- Project-section visibility predicates, pin/unpin semantics (D4), and
  relation-metadata perms — untouched.

## Acceptance

- `TestMoveGroupToCategoryAllowsProjectGroup` pins the move endpoint for a
  Project group (move into a category is 200 and persists; clear is 200).
- `TestGroupReqCheckAllowsProjectAndCategoryTogether` pins the create
  request gate.
- `make i18n-extract-check` and `make i18n-lint` pass with the removed
  error code not registered anywhere.
- `go test ./modules/category/... ./modules/group/...` green; gofmt clean
  on all changed Go files.
- `docs/sidebar-project-sections-api.md` §3/§7 and the
  sidebar-project-sections brief describe the reversed contract.
