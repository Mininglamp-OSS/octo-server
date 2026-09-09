---
type: Journal
title: "Journal: project-collaboration-roles"
description: Add project-scoped collaboration-role labels for human members while preserving owner/admin/member as the sole authorization model.
tags: ["project", "collaboration-role", "acl", "migration", "maintenance", "testing"]
timestamp: 2026-09-09T10:55:00+08:00
# --- octospec extension fields ---
task: project-collaboration-roles
upstream: Mininglamp-OSS/octo-server#870
source: self
---

# Journal: project-collaboration-roles

## What was done

- Added project-local collaboration-role definitions and many-to-many human-member
  bindings without changing `octo_project_member.role`. The existing
  owner/admin/member field remains the only project authorization input.
- Seeded the fixed V1 built-ins (`产品`, `前端`, `后端`, `HR`) transactionally for
  new Projects and through an idempotent, bounded backfill for existing Projects.
- Added authenticated, UID-rate-limited catalog CRUD and member-binding APIs.
  Owners manage custom definitions; owners and admins bind roles; ordinary members
  cannot write. Built-ins are immutable and AI-agent roster rows always expose an
  empty role list.
- Added an independent `collaboration_role_epoch`, lifecycle cleanup, audit events,
  bounded-cardinality metrics, configurable per-Project/per-member quotas, and a
  default-off write flag (`OCTO_PROJECT_COLLABORATION_ROLE_ENABLED`).

## Load-bearing boundaries

- Collaboration roles are descriptive metadata only: binding, renaming, or deleting
  one cannot alter permission roles or permission-derived capabilities.
- Writes follow the existing lock order (Space seats, active Project, membership and
  role rows), re-read the actor role under the Project lock, validate every selected
  role in the same Project, and replace the set atomically.
- Member removal clears bindings in the same lifecycle transaction; re-admission does
  not restore stale labels. Project disband removes definitions and bindings through
  the existing soft-close lifecycle.
- Normalized names are lower-cased by the application and stored with
  `utf8mb4_bin`: case-only variants collide, while accents remain distinct.

## Review findings closed

- The periodic integrity scan now keyset-pages the binding table before joining and
  classifying violations. A healthy table therefore costs at most one configured page
  per tick instead of a full-table scan.
- Case-only display renames compare the locked model in Go, so `designer` to
  `Designer` is a real update even under a case-insensitive display-name collation.
- `normalized_name` now pins an accent-sensitive binary collation, allowing
  `resume` and `résumé` to coexist while preserving case-insensitive uniqueness after
  normalization.

## Operational notes

- The write gate defaults off. Rollback is to disable
  `OCTO_PROJECT_COLLABORATION_ROLE_ENABLED`; reads and stored data remain available.
- Maintenance uses the existing reconcile interval/limit and keeps a composite cursor
  plus running total across ticks. Gauges are published only after a complete rotation.
- Role-name and role-count limits are enforced before unbounded request or catalog
  growth can reach storage.

## Verification

- `go test ./modules/project -run 'Test.*CollaborationRole' -count=1` and
  `go test -race ./modules/project -run 'Test.*CollaborationRole' -count=1`
  passed against local MySQL, Redis, and WuKongIM.
- `go test ./modules/project -count=1` passed (81.4s) after resetting the dedicated
  `test` schema in an otherwise idle shared test environment.
- `go build ./...`, `go vet ./modules/project`,
  `golangci-lint run ./modules/project/...`, `go test ./pkg/errcode ./pkg/i18n/...`,
  `make i18n-extract-check`, `make i18n-lint`, `gofmt`, and `git diff --check`
  passed. The live test-schema `normalized_name` column reports `utf8mb4_bin`.
