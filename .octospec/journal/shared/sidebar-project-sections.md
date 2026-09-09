---
type: Journal
title: "Journal: sidebar-project-sections"
description: Unified the Follow sidebar order for personal categories and Projects, synchronized Project entries with pin and unpin actions, and paired Sidebar-facing project IDs with names.
tags: ["sidebar", "project", "category", "space", "isolation", "migration", "testing"]
timestamp: 2026-09-09T20:17:10+08:00
# --- octospec extension fields ---
task: sidebar-project-sections
upstream: self
source: self
---

# Journal: sidebar-project-sections

## What was done

- Added `octo_sidebar_section` as the per-user, per-Space order authority for
  manual categories and Projects. The migration preserves category order,
  appends active Project memberships deterministically, and clears only manual
  assignments for Project-owned groups.
- Added authenticated, UID-rate-limited list/sort endpoints for unified sidebar
  sections. Existing category list/sort endpoints now read/write the same order.
- Added best-effort post-commit Project section provisioning for Project create,
  member admission, and successful #861 pin. The read path repairs missed hooks.
  A Space-listed Project pinned by a non-member appears with `groups: []`; pinning
  does not grant a Project seat or group access.
- Made explicit unpin a durable personal opt-out, including for active Project
  members. It retains the ordering row as hidden, prevents read repair from
  immediately restoring it, and lets a later pin reactivate the previous position.
- Added `project_name` wherever this task emits a Sidebar-facing non-empty
  `project_id`. `/v1/sidebar/sync` resolves distinct IDs with one Space-scoped
  query and fails soft by omitting names if that lookup fails.
- Rejected manual categorization of Project groups with a registered localized
  error, preserving the Project/category mutual-exclusion invariant.

## Load-bearing notes

- Space membership remains the security boundary. Project membership controls
  Project group contents; a Space-listed pin controls only personal visibility.
- Hook failures never roll back a committed Project, member admission, or pin.
  Idempotent reads are the repair path.
- A `pinned=0` row is meaningful sidebar state: it wins over active Project
  membership so an explicit unpin cannot be undone by the membership backstop.
- `SidebarItem.ProjectID` remains the existing string type. The pointer wire
  change in D6 is deferred pending client rollout confirmation.
- The new migration and joins use `utf8mb4_general_ci`, matching the Project and
  category tables involved in this task.

## Verification

- Local MySQL, Redis, and WuKongIM health checks passed.
- Against an isolated local MySQL 8.0.33 schema, with Redis/WuKongIM live:
  `go test -race -shuffle=on -count=1` passed for
  `./modules/category/...`, `./modules/project/...`, `./modules/space/...`, and
  `./modules/message/...` (each package reset independently, matching CI).
- `make i18n-extract`, `make i18n-extract-check`, and `make i18n-lint` passed.
- `golangci-lint run ./...` reported 0 issues; `git diff --check` passed.
- Standalone `octospec-lint` is not installed in this workspace. Frontmatter was
  written against existing valid Journal/Learning examples and checked manually.

## Structural learning

Stateful tests must both own their database schema and release deliberately
exhausted shared state. A test that assumes another package already migrated the
shared `test` schema fails under CI's per-package reset; a rate-limit test that
only clears its bucket on entry makes later shuffled tests order-dependent.
