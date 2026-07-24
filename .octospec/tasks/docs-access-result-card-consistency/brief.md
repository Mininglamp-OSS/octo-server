---
type: Task
title: "Task: docs-access-result-card-consistency"
description: Render clicked and sibling docs access decisions through the same V3 Registry result path without exposing raw operator UIDs.
tags: [card, wire-contract, trust-boundary, space, testing, commit]
timestamp: 2026-07-24T17:00:00+08:00
slug: docs-access-result-card-consistency
upstream: octo-server#635
source: self
---

# Task: docs-access-result-card-consistency

## Goal

When one docs approver decides an access request, render both the clicked card
and every sibling approver card through `docs.access-request@0.3.0/result`.
Never surface a raw `operator_uid` when the callback omits the operator's
display name.

## Load-bearing behavior

- The clicked-card finalizer and `POST /v1/internal/cards/mutate` share one
  bounded mapping into the published V3 result fields.
- Sibling context is recovered only from the stored, server-authored pending
  card action data; caller values cannot replace its request/document identity.
- The existing `docs.access_*` target-card gate, sender binding, lifecycle
  checks, channel/message coordinates, Space binding, and monotonic `card_seq`
  mutation path remain fail-closed.
- Approved maps to V3 `approved`; denied maps to V3 `rejected`, preserving the
  bounded denial reason.
- `operator_name` and `decided_at_display` are optional additive internal
  mutation fields. A missing operator name uses localized generic copy, never
  the raw UID.
- The frozen `docs.access-request@0.2.0` handoff is not modified.

## Out of scope

- No docs-backend change in this workspace.
- No database migration, new route, new public API, or generic template-mutate
  endpoint.
- No changes to applicant outcome-card delivery.
- No migration of unrelated notification card families.

## Acceptance

- Sibling mutations select `docs.access-request@0.3.0` and the V3
  approved/rejected state through `CardUpdater.ReplaceView`.
- Stored pending action data supplies request ID, document identity, requester,
  permission, reason, and display timestamps to the sibling result.
- Missing operator display name produces localized `审批人` / `Reviewer` and
  rendered fields never contain the operator UID.
- A mismatched Space or document ID fails before mutation.
- Existing target-family containment and mutation error envelopes remain
  unchanged.
- Focused notify tests cover approved, denied, V3 selection, context recovery,
  UID non-disclosure, and invalid stored context; relevant module tests pass.
