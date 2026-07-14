---
type: Task
title: "Task: smart-summary-user-share-card"
description: Onboard smart-summary as the first provider on the generic user-resource-share-card platform for user-authored shares to DM, group, and thread targets.
tags: ["summary", "card", "resource", "share", "auth", "space", "isolation", "testing"]
timestamp: 2026-07-14T12:00:00+08:00
# --- octospec extension fields ---
slug: smart-summary-user-share-card
upstream: octo-server#571
source: self
---

# Task: smart-summary-user-share-card

> Provider onboarding brief. The generic sender, target authorization, trusted
> human-card provenance, quotas, idempotency, audit, and rollback contract is
> `.octospec/tasks/user-resource-share-card/brief.md`.

## Goal

Register smart-summary as the first resource provider on the generic user
resource-share-card platform. An authenticated user can select one or more DMs,
groups, or threads and share a summary card as themselves into those existing
conversations. `notification` is not the sender and no separate Bot DM is
created.

This task supplies only summary-specific content authority, intent claims,
template mapping, disclosure policy, deep link, and tests. It must not create a
second summary-specific message endpoint or duplicate the platform's target,
sender, proof, rate-limit, idempotency, audit, or transport code.

## Distinction from origin delivery

| Capability | Trigger | Destination | Sender |
| --- | --- | --- | --- |
| Summary origin delivery | Automatic on terminal task state | Immutable task-origin group/thread | `notification` Bot |
| Summary user share | Explicit authenticated action | User-selected DM/group/thread | Sharing user |

They may target the same group/thread, but do not share provenance, fallback,
idempotency, or sender semantics and remain independently deployable.

## Provider contract

- **Resource type:** stable `smart_summary` provider ID (final wire spelling is
  pinned once in the generic provider registry and cannot be caller-selected).
- **Issuer:** summary-api, using its user-authenticated visibility/share
  endpoint and provider-specific signing key/audience configuration.
- **Resource identity:** non-enumerable task identifier plus immutable summary
  result revision/version. Database auto-increment IDs are not exposed.
- **Space:** the summary task's authoritative Space. Cross-Space targets are
  rejected.
- **Template:** the existing display-only summary ResourceCard mapping in
  `pkg/cardtmpl`; no provider-authored Adaptive Card JSON.
- **Deep link:** octo-server builds `/s/{taskId}?sp={spaceId}` from registered
  External web origin and signed identifiers; the intent carries no arbitrary
  URL.
- **Structured claims:** bounded title, terminal kind, excerpt, time range,
  participant/message counts, generated time, and sanitized failure reason.
  Labels, truncation, layout, actions, URL, and `plain` remain server-owned.
- **Targets:** bounded DM/group/thread list signed into the intent and passed to
  the generic target authorizer. Per-target partial results are returned.
- **Sender:** the generic endpoint's authenticated login UID. No `actor_uid`,
  `from_uid`, Bot identity, OBO grant, or attribution profile is accepted from
  summary-api or web.

## Summary visibility and disclosure gate

Before implementation, the summary owner must explicitly confirm one of these
provider policies and encode it in the provider spec/tests:

1. every active member of the summary's Space may already view the result; or
2. sharing atomically creates a provider-owned access grant for the exact DM
   peer/group/thread audience before intent issuance.

The default is fail-closed: if summary-api cannot prove that the target audience
may receive both the card excerpt and deep-link result, it does not mint an
intent for that target. octo-server does not infer summary visibility from chat
membership and never creates summary access grants itself. Revocation/deletion
after sending is enforced when the deep link is opened; this task does not
rewrite already-persisted card text.

## Load-bearing list

- Summary-api user authentication, visibility/shareability, result revision,
  retention/deletion, and same-Space rules.
- Provider signing/audience configuration and structured intent claims.
- Summary ResourceCard template, localization, truncation, failure-reason
  sanitization, and `/s/{taskId}?sp={spaceId}` route contract.
- Generic platform provider registration, target authorization, provenance,
  rate limiting, idempotency, audit, feature flags, and rollback.
- Web share UI target selection, per-target outcomes, and generic card proof
  verification/rendering.

## Out of scope

- Building a summary-only card transport or weakening generic provider gates.
- Automatic origin group/thread delivery or creator notification fallback.
- Cross-Space/public sharing or an implicit summary access grant not confirmed
  by the provider policy above.
- Bot-authored user shares, arbitrary card JSON, generic user type-17, Bot OBO,
  interactive actions, or card edits.
- Onboarding docs, tasks, approvals, or other resource types; each gets its own
  provider brief without changing the generic transport authority.

## Acceptance

- Provider registry tests accept the pinned smart-summary issuer/type/template/
  intent version and reject unknown issuer, stale result revision, wrong Space,
  arbitrary URL/card fields, oversized claims, or unsupported template version.
- Summary-api tests prove unauthenticated, unauthorized, deleted/expired,
  revoked, cross-Space, and disclosure-policy-denied requests mint no intent.
- An issued intent is bound to the authenticated actor, exact result revision,
  Space, bounded target list, nonce, expiry, and idempotency key.
- DM, group, and thread integration tests each persist one summary card with the
  sharing user as `from_uid` in the selected conversation; no `notification`
  Bot conversation or group membership change is created.
- Card snapshot/validation tests pin localized completed/failed variants,
  truncation, sanitized reason, authoritative `plain`, and
  `/s/{taskId}?sp={spaceId}`. Provider claims cannot change layout/actions/URL.
- Mixed-target results, retry, rate-limit, audit, proof verification, key
  rotation, feature-flag, and rollback cases pass the generic platform suite
  plus smart-summary provider fixtures.
- The feature can be disabled per `smart_summary` provider without disabling
  origin notifications, generic platform support, or any other provider.
