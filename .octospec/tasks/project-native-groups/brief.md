---
type: Task
title: "Project ordinary linked groups"
description: Remove automatic Project group creation and preserve native group membership.
tags: [project, group, space, migration, wire-contract]
timestamp: 2026-09-24T00:00:00Z
slug: project-native-groups
source: self
---

# Project ordinary linked groups

## Goal

Implement the confirmed design in `docs/specs/2026-09-10-project-prd-alignment-design.md`: a Project has no special group; creation does not automatically create one; historical automatically created groups remain ordinary Project-linked native groups with their messages, members, owner and relation.

## Load-bearing list

- Project creation and membership/Owner/name write paths; Project and Sidebar read DTOs.
- Group and Bot native authorization, group relation and explicit group creation.
- Project removal jobs must finalize seats without changing ordinary group membership; Space revocation must continue native group cleanup; prior `rejoined` projection jobs must safely terminate.
- Operator-run SQL in `scripts/project-native-groups.sql` removes `all_member_group_no` and `all_member_group_lease_until` only after compatible application instances replace old readers/writers and in-flight work drains; automatic startup migrations keep the columns during the rollout.
- HTTP/i18n contract, client documentation, runtime DB migration compatibility and existing DB/Redis/IM tests.

## Out of scope

- Destroying historical groups, messages, group-member rows or `group.project_id`.
- Changes to Drive provisioning, unrelated Space revocation, ordinary group ACL or user-facing Project roles.

## Acceptance

- Creating a Project yields no native group and no `all_member_group_no` response field; a user-created Project group keeps the existing member snapshot behavior.
- Historical Project-linked groups remain readable and mutable through ordinary native group rules; Project membership, Owner and name changes leave native group membership, owner, name and IM subscription unchanged.
- A pending Project removal closes the intended Project seat without removing the native group member; Space revocation still removes native group access; pending legacy projection jobs do not mutate the group.
- Sidebar returns existing linked-group metadata without a dedicated-group identifier. New-instance startup retains old columns for rolling deployment; the post-drain operator SQL preserves group data and relation. Narrow cross-module tests and HTTP smoke exercise the changed path.
