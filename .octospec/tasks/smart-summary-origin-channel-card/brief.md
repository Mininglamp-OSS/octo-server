---
type: Task
title: "Task: smart-summary-origin-channel-card"
description: Deliver a completed smart-summary card back to its bound origin group or thread with live authorization, bounded retries, and creator-DM fallback for invalid origins.
tags: ["summary", "card", "group", "thread", "space", "isolation", "acl", "rate-limit", "idempotency", "observability", "testing"]
timestamp: 2026-07-14T12:00:00+08:00
# --- octospec extension fields ---
slug: smart-summary-origin-channel-card
upstream: octo-server#571
source: self
---

# Task: smart-summary-origin-channel-card

## Goal

When a summary task was explicitly created from a group or thread, deliver its
terminal summary card back to that exact origin. The card is sent by the shared
`notification` Bot through the producer-bound dispatcher without adding the Bot
as a member. If the recorded origin is no longer an eligible target, deliver the
existing creator-DM notification instead.

This is a cross-repository change: smart-summary owns task-origin persistence,
delivery state, retries, and fallback selection; octo-server owns live Space/
group/thread authorization, the server-authored card, Bot sender authority,
outbound quotas, and transport.

## Background

The current `summary-notify` producer is DM-only. Origin group/thread delivery
was intentionally deferred because widening a card producer requires stronger
target provenance, member-exempt posting rules, per-channel traffic control,
and explicit retry/fallback behavior.

The origin is not a caller-selected destination at completion time. It is an
immutable task attribute captured from the authenticated creation action and
bound to the task, creator, and Space.

## Contract and decisions

1. **Origin binding.** At task creation, smart-summary persists the canonical
   `space_id`, `creator_uid`, origin `channel_id`, origin channel type, and, for
   a thread, its canonical parent group/channel identity. Completion delivery
   may use only this bound origin; a worker or retry request cannot replace it.
2. **Creation-time authorization.** The task-creation path verifies that the
   creator can read the source group/thread and that the source belongs to the
   declared Space before accepting the task. The stored binding is audit data,
   not permanent authorization.
3. **Live send-time authorization.** octo-server rechecks active Space, active
   creator membership, exact target Space, group lifecycle, and thread-parent
   validity immediately before dispatch. Thread access is derived through the
   parent group and must satisfy the repository's thread parent-access rule.
4. **Member-exempt Bot posting.** The `notification` Bot may post to a normal,
   same-Space origin group/thread without a membership row. Dispatch must not
   create/update group membership or affect member lists/counts. An explicit
   Bot blacklist/ban, disabled/disbanded group, archived/deleted/malformed
   thread, wrong Space, or DB error fails closed.
5. **Fallback.** A permanently invalid or unauthorized bound origin falls back
   once to the creator's DM using the existing summary notification contract.
   Rate-limit, Redis, DB, or transport failures are transient and must be
   retried; they must not be converted into surprising DM fallback.
6. **Traffic controls.** Before group/thread enablement, add a Redis-backed,
   cluster-wide token bucket keyed by producer + Space + canonical parent
   channel, plus a producer-wide cluster rate bucket. Keep the existing
   max-in-flight 20/process as the local concurrency guard. Rate/burst values
   are deployment configuration with validated positive bounds and must be
   chosen from observed task volume before rollout. A limiter-store failure
   fails closed for group/thread delivery and returns a retryable outcome.
   This task does not introduce a distributed concurrency semaphore; the
   producer-wide token bucket is the cluster cap.
7. **Retry and idempotency.** smart-summary owns a durable delivery row with a
   uniqueness key over task/version, terminal kind, and canonical destination.
   It retries retryable failures with bounded exponential backoff and jitter.
   octo-server performs one transport attempt per ingress request. Because the
   transport has no caller-controlled `client_msg_no`, an ambiguous timeout can
   still duplicate a card; exactly-once delivery is not claimed.
8. **Observability.** Emit bounded outcomes for `delivered_origin`,
   `fallback_creator_dm`, `target_denied`, `rate_limited`, `dispatch_failed`,
   and `retry_exhausted`; record latency and retry count. UIDs, channel IDs,
   task IDs, titles, excerpts, and card JSON are forbidden metric labels.
   Structured logs may carry identifiers under existing privacy/retention
   policy but never summary content or credentials.

## Load-bearing list

- Smart-summary task schema and immutable origin provenance.
- Authenticated task creation and summary visibility checks.
- `summary-notify` producer channel/profile/Space/group policies.
- `internal/carddispatch` group/thread authorizer, including explicit-ban and
  no-membership-side-effect invariants.
- Thread parent-channel access and lifecycle checks.
- Cluster-wide per-channel and producer-wide outbound rate limits.
- Durable delivery state, retry classification, idempotency key, and fallback.
- Bounded logs/metrics, rollout feature flag, and rollback path.

## Out of scope

- Allowing a user to choose another channel at completion time.
- User-initiated forwarding/sharing; that has its own brief.
- Person-to-person DM card delivery or OBO type-17 messages.
- Joining the Bot to a group or changing Bot presentation in member lists.
- Exactly-once transport semantics or automatic recall of a duplicate.
- Cross-Space origin delivery.

## Acceptance

- DB/schema tests prove the origin binding is immutable and tied to the task's
  authenticated creator and Space; legacy tasks without a valid origin remain
  creator-DM only.
- Authorization matrix covers active/removed creator, wrong Space, normal/
  disabled/disbanded/banned group, valid/archived/deleted/malformed thread,
  missing parent, explicit Bot blacklist, and DB errors; every denial produces
  zero group/thread transport calls and zero membership writes.
- A valid group and thread each persist one `octo/v1` card from
  `from_uid=notification` with the verified `space_id`.
- Permanent target denial triggers exactly one creator-DM fallback; transient
  limiter/storage/transport errors remain retryable and do not fallback.
- Concurrency tests across multiple simulated replicas prove both Redis buckets
  enforce cluster-wide limits. Configuration validation rejects zero, negative,
  or unbounded values; limiter-store failure fails closed.
- Retry tests prove one active logical delivery per unique destination and
  bounded backoff. Documentation and metrics explicitly retain the ambiguous-
  transport duplicate caveat.
- Feature flag can disable origin delivery without disabling creator-DM
  notifications. Rollback requires no schema downgrade and leaves pending
  delivery rows safely retryable after re-enable.
