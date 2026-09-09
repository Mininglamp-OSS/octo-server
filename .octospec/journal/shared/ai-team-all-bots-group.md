---
type: Journal
title: "Journal: ai-team-all-bots-group"
description: Projects each user's active AI Team Agent roster into protected two-member containers and one visible owner-plus-all-agents group, including automatic Bot creation hooks and reconciliation.
tags: ["ai-team", "group", "space", "isolation", "acl", "bot", "testing"]
timestamp: 2026-09-08T19:20:00+08:00
# --- octospec extension fields ---
task: ai-team-all-bots-group
upstream: "User request; design informed by server PR #855"
source: user
---

# Journal: ai-team-all-bots-group

## Result

Added one visible `purpose=ai_team_group` group named “我的 OPT” per
`(space_id, user_uid)`, stored directly in `group`. Its exact live roster
is the owner plus every eligible `ai_team_agent.is_added=1` Bot whose private
two-member container is ready. Removing an Agent keeps its private container and
session history but removes it from the team group; reactivation reuses both
group identities.

AI Team listing now acts as the legacy-data convergence point: it creates Agent
rows only for owned Bots that never had one, repairs all active pair containers,
and then projects the complete team group. BotFather conversation creation,
`POST /v1/user/bots`, and `MintBotOBO` call the same idempotent provisioner after
the Bot's Space membership is durable. MySQL deadlocks (1213) retry the whole
idempotent add/remove/list reconciliation up to three times; lock-wait timeouts
return immediately because repeating a full timeout is not useful backoff.

Ordinary web, manager, service, Bot API, scan/invite, org-directory, ownership,
and disband paths cannot mutate either managed group. Both managed parent groups
and their threads are hidden from ordinary recent, follow, contacts, and group
lists; the dedicated AI Team surface can still open them by authoritative ID.
The aggregate team group allows ordinary subareas, while the private pair parent
only allows AI Team session creation. Lifecycle cleanup can still remove deleted
or departed identities from both managed group types.

An AI assistant starts with no child session. The dedicated UI creates a
`新对话` thread only when the user requests one. On the first non-empty owner
message, the shared AI Team helper atomically replaces that default title with a
100-rune-bounded message summary; manual and later titles are not overwritten.

## Structural learnings / gotchas

- The Agent table must remain the only roster authority. Accepting a Bot directly
  into the team group would create a second source of truth and make repair
  direction ambiguous.
- The safe convergence order is pair containers first, then one exact team-roster
  projection. Projecting only the newly added Bot misses inactive, invalid, or
  stale rows and cannot repair old data.
- The active owner's existing `space_member` row serializes first creation;
  `group` stores the stable identity and `group_member` stores the projected
  roster, so no parallel team-registration table is needed. The entire
  orchestration remains idempotent with bounded deadlock retries.
- The repository's test packages share a hard-coded MySQL database. Running
  `go test ./...` in package-parallel mode lets schema-owning fixture tests race
  migrations and table cleanup; target integration tests must be rerun serially
  after rebuilding that disposable database.
- Message delivery bypasses the HTTP application API and reaches the server via
  WuKongIM's message notification. Session-derived writes such as first-message
  naming belong on the Robot listener's persisted AI-target branch, where they
  avoid adding an AI-only query to every ordinary thread message and retain the
  existing non-text display-title fallback.
- WuKongIM channel upsert adds subscribers but does not replace the subscriber
  set. Exact roster projection therefore requires explicit removal from the
  parent and every existing subarea, plus explicit addition to old subareas for
  newly activated Bots.
- A reconciliation-triggering GET must not become an availability dependency on
  the external IM service. A stable DB roster skips replay; explicit Agent
  mutations replay idempotent IM writes and surface retryable failures.
- External projection must not hold a transaction or pooled connection across
  WuKongIM I/O because the legacy IM client has no deadline. The projection
  snapshot uses the existing monotonic `group_member.version`; a short
  post-projection transaction rechecks both that version and current Agent
  eligibility, replaying the latest snapshot when either changed.
- Joined eligibility checks that need a row lock use `FOR UPDATE OF` on the
  module-owned authority table only. A bare locking join silently adds Space,
  membership, Robot, and User rows to the lock graph.
- Authorization compares Bot identities as a set rather than trusting database
  result ordering; application byte order and MySQL collation order are not the
  same contract.
- Lifecycle cleanup deactivates affected Agents in the same transaction as
  protected membership removal. Existing `group_member.version` plus an
  effective-eligibility recheck prevents an older external projection from
  winning after that removal.
- System-managed groups must not consume user-action quotas. Daily group-create
  accounting now includes only ordinary `purpose=''` rows.
- Durable removal and external revocation are separate outcomes. If subscriber
  reconciliation fails, the removal remains durable but the API returns the
  retryable error instead of reporting complete success. Explicit mutation
  retries replay revocation rather than acknowledging a still-degraded
  projection.
- The owner is part of the projected authority, not an unconditional member.
  Aggregate snapshots and ready CAS checks gate the owner through one shared
  Space-liveness predicate and explicitly revoke the owner when it fails.
  Private-container writes validate the same authority before and after IM I/O;
  a lifecycle race is compensated by removing both participants from the parent
  and every session channel while preserving the Agent's durable rejoin intent.

## Verification

See `.octospec/tasks/ai-team-all-bots-group/verification.md` for passing focused,
race, vet, i18n, and whitespace checks plus the documented shared-database limits
on full-suite execution.
