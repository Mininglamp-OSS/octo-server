---
type: Journal
title: "Journal: my-ai-team-sessions"
description: Record of isolated one-human/one-Bot AI session containers and thread-backed conversations
tags: [ai-team, bot, thread, isolation, acl, wukongim, i18n]
timestamp: 2026-09-07T15:42:31+08:00
task: my-ai-team-sessions
source: self
---

# Journal: my-ai-team-sessions

## What was done

- Added the feature-gated `/v1/ai-team` API and durable agent/session records.
- Lazily provisions one dedicated parent group per `(space_id,user_uid,bot_id)`;
  each parent contains exactly the owner and their active User Bot, while each
  conversation is a thread with idempotent DB and WuKongIM provisioning.
- Routes ordinary user messages in a ready AI thread to the persisted Bot target
  and emits a stable full-channel runtime session key and input ID.
- Protects dedicated parents from ordinary group, manager, thread and Bot API
  mutations, while preserving authoritative Space-member lifecycle cleanup.
- Filters both the dedicated parent and its topic channels from ordinary group,
  recent-conversation and follow/sidebar results.
- Persists the rollout-wide `thread.auto_archive_enabled=0` system setting.

## Structural learnings

- A dedicated AI container is a business purpose, not a new transport group type.
  Keeping `group_type` unchanged preserves WuKongIM protocol behavior while the
  server-owned `purpose` field drives ACL and visibility policy.
- Hiding only the parent is insufficient: WuKongIM exposes thread conversations as
  independent type-5 channel IDs, so recent/follow filtering must normalize each
  topic back to its parent before applying the purpose filter.
- The unique association constraint is necessary but not sufficient. Serializing
  the association row with `FOR UPDATE` ensures concurrent first-session requests
  converge before either allocates the parent group.
- DB, parent IM channel and thread IM channel cannot be one transaction. Persisting
  provisioning state and retrying create-or-update makes response loss converge
  without allocating a second parent or thread.

## Verification and boundaries

- Build, unit suite, four E2E/API shards, focused AI/message tests, `go vet`, i18n
  checks and direct WuKongIM type-17 persistence passed. Exact evidence is in
  `.octospec/tasks/my-ai-team-sessions/verification.md`.
- A blank-database migration was queried directly and returned
  `thread / auto_archive_enabled / 0`.
- The full pilote2e package still has an unrelated existing card-template catalog
  fixture failure; its direct WuKongIM persistence test passes.
- Client and external adapter E1-E5 remain deployment-level handoff checks; this PR
  does not claim those repositories were exercised.

## Upstream-main integration

- Merging `origin/main` at `96b3b926` brought in #846's single group-member
  admission funnel and its whole-tree source guard. The guard caught the AI
  container's intentionally atomic but direct two-row member insert.
- The integration keeps the AI association/group/session transaction intact via a
  narrow group-owned admission bridge. Before writing, the bridge locks and checks
  that the parent belongs to the requested Space and owner, has
  `purpose=ai_session_container`, and has no project binding; it then admits exactly
  the owner and Bot through the shared funnel.
- Conflict resolution in `modules/space/api.go` preserves both invariants: preset
  groups reject AI containers and project-bound groups, then use the upstream
  registered admission entry for ordinary Space-direct groups.
- Build, vet, the 52-package unit suite, all four E2E/API shards, i18n checks and the
  focused WuKongIM persistence test were rerun successfully after the merge.

## Final review hardening

- Kept mixed-collation conversion on the new AI tables, preserving indexed access
  on the legacy identity, Space and group columns; the regression fixture now uses
  realistic indexes, shipped service calls and an `EXPLAIN FORMAT=JSON` guard.
- Prevented org-sync events from adding or deleting AI-container members.
- Made add/re-add and session creation repair Space-cleanup membership loss through
  the shared admission funnel, reject any third member, and reconcile the parent
  plus retained session channels in WuKongIM before returning ready state.
- Prevented failed retries from overwriting ready session state and aligned rename
  locking with the agent -> session -> thread order.
- Separated product-visible group lookup from authoritative Bot lifecycle lookup,
  so deleting a User Bot also reaches its hidden AI container and is allowed to
  remove the protected Bot membership through the normal removal funnel.
- Re-ran build, vet, 52 unit packages, all four API/E2E shards, i18n checks and the
  focused WuKongIM persistence test successfully.
