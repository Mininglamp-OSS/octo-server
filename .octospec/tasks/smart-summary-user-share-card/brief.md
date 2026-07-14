---
type: Task
title: "Task: smart-summary-user-share-card"
description: Let an authenticated user share a visible summary to a group or thread as a Bot-authored card with authoritative forwarder attribution.
tags: ["summary", "card", "share", "auth", "space", "isolation", "acl", "rate-limit", "idempotency", "audit", "testing"]
timestamp: 2026-07-14T12:00:00+08:00
# --- octospec extension fields ---
slug: smart-summary-user-share-card
upstream: octo-server#571
source: self
---

# Task: smart-summary-user-share-card

## Goal

Add an authenticated summary-api operation that lets a user share a summary
they are authorized to view into a group or thread they are authorized to post
to. The resulting `octo/v1` card is sent by the shared `notification` Bot and
includes a server-authored attribution header such as “Alice shared a smart
summary”.

The user triggers the share but does not author the type-17 wire message. This
preserves the platform rule that rich cards originate only from reviewed Bot
producers.

## Background

Today web forwards summary content as user-authored plain-text chunks. The web
client is intentionally receive-only for type-17. A card share therefore needs
two independent authorization decisions: summary-api verifies access to the
summary content, and octo-server verifies live permission to the selected chat
target.

The target model is intentionally split. Group/thread shares can use a
Bot-authored card with user attribution. A person-to-person DM cannot contain a
third-party Bot message in the same human conversation, so it keeps the current
user-authored plain-text flow.

## Contract and decisions

1. **Authenticated content-owner endpoint.** summary-api exposes a user-auth
   share endpoint following that repository's versioning conventions. It takes
   a summary identifier, canonical group/thread target, and an idempotency key;
   it never accepts card JSON, sender UID, attribution name/avatar, `plain`, or
   arbitrary payload fields.
2. **Summary authorization.** summary-api resolves the authenticated actor and
   verifies current visibility of the requested summary before reading or
   relaying any content. Deleted, expired, revoked, cross-Space, or unauthorized
   summaries fail closed with the service's localized error contract.
3. **Target authorization.** The service relays structured summary fields,
   `actor_uid`, summary Space, target channel/type, and idempotency key through
   its existing authenticated S2S boundary. octo-server independently verifies
   active actor membership and post access in the exact same-Space group or
   thread, including thread-parent access and live lifecycle state.
4. **Bot sender and attribution.** A distinct `summary-forward-card` producer is
   bound to `notification`, group/thread only, `octo/v1` only, and
   member-exempt. octo-server resolves the actor's current profile and builds
   the visible attribution; caller-supplied display data is ignored/rejected.
   The Bot is never added to the group and explicit Bot bans remain effective.
5. **DM split.** A person target is rejected by the card-share endpoint. Web
   keeps the existing user-authored plain-text forward for human-to-human DMs.
   A Bot DM is not substituted because it would create a different conversation.
6. **No OBO or generic card relay.** No path may send type-17 as the actor, add
   OBO fields, accept a free-form card/document, or expose “send as any UID”.
   The server-owned summary template is the only accepted card body.
7. **Rate limits.** Apply authenticated per-user share limits at summary-api,
   plus the same Redis-backed cluster-wide per-channel and producer-wide
   outbound buckets required by the origin-delivery brief. Limit exhaustion is
   retryable only when the API explicitly returns a safe retry-after; it never
   silently falls back to another target or message type.
8. **Idempotency and audit.** Require an opaque idempotency key scoped to actor
   and endpoint. summary-api persists the authorized request/result so client
   retries return the original outcome and do not intentionally re-dispatch.
   Record an audit event containing actor, summary reference, Space, canonical
   target, timestamp, outcome, and request correlation ID. Do not store card
   JSON or summary text in the audit event. An ambiguous transport failure can
   still duplicate at WuKongIM and is not represented as exactly-once.

## Load-bearing list

- Summary visibility, retention, and same-Space data-isolation rules.
- User authentication and anti-enumeration error behavior in summary-api.
- Existing S2S token boundary and structured ingress contract.
- Actor membership/post permission and thread-parent access in octo-server.
- Producer-bound sender, member-exempt Bot policy, template ownership, and
  type-17 no-bypass guard.
- Per-user, per-channel, and producer-wide rate control.
- Durable idempotency, privacy-safe audit events, metrics, and rollback flags.

## Out of scope

- Rich-card shares to person-to-person DMs.
- OBO/user-authored type-17 messages or arbitrary client-authored cards.
- Cross-Space sharing, public links, or changing summary visibility grants.
- Editing/revoking a card after the underlying summary changes or is deleted.
- Interactive `octo/v2` actions, comments, reactions, or card revisions.
- Automatically sharing on task completion; origin delivery has its own brief.

## Acceptance

- Unauthenticated, unauthorized, revoked/deleted, and cross-Space summary
  requests return the service's safe error envelope and produce zero S2S calls.
- Group/thread authorization matrix covers active/non-member/removed actor,
  wrong Space, disabled/disbanded group, invalid/archived/deleted thread,
  missing parent, explicit Bot ban, and storage error; all failures produce zero
  transport calls and zero membership mutations.
- Success persists one `octo/v1` card from `from_uid=notification`; attribution
  is derived from the authenticated actor's live server profile and cannot be
  spoofed by request fields.
- Person targets are rejected by the card endpoint, while the existing web
  user-authored plain-text DM flow remains covered by a regression test.
- Source/contract guards reject OBO fields, sender UID, free-form type-17/card
  payloads, and arbitrary attribution fields at both service boundaries.
- Repeated requests with the same actor/idempotency key return the recorded
  result without a second intentional dispatch; keys cannot collide across
  actors. Ambiguous transport duplicates remain documented and observable.
- Per-user and cluster-wide outbound limit tests pass across multiple simulated
  replicas, with bounded retry-after behavior and no fallback sends.
- Audit tests prove success and denial outcomes are recorded without summary
  text/card JSON/secrets; metric labels stay bounded and contain no identifiers.
- Independent feature flags can disable card sharing and fall the UI back to
  the existing plain-text behavior without disabling summary notifications.
