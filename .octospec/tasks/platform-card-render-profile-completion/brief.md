---
type: Task
title: "Task: platform-card-render-profile-completion"
description: Make every newly sent server-authored platform card explicitly carry render_profile octo-chat/v1, without rewriting or changing already-sent messages.
tags: ["card", "wire-contract", "testing"]
timestamp: 2026-08-03T00:00:00+08:00
slug: platform-card-render-profile-completion
upstream: self
source: self
---

# Task: platform-card-render-profile-completion

## Goal

Every newly sent **platform-owned** type-17 card must carry these two
independent top-level wire fields:

```json
{
  "profile": "<existing octo/v1 or octo/v2>",
  "render_profile": "octo-chat/v1"
}
```

`profile` remains the card capability/interaction profile (`octo/v1` or
`octo/v2`). `render_profile` remains the client visual-compatibility selector.
The server owns `render_profile`; platform producers do not choose it.

The client already treats a missing historical `render_profile` as
`octo-chat/v1`. This task makes the same decision explicit for all new platform
messages so their rendering contract does not depend on the historical fallback.

## Current state

Current `main` has two different outcomes:

- Registry-backed new cards already propagate
  `manifest.renderProfileCompatibility = "octo-chat/v1"` through render,
  and internal dispatch. This includes
  `summary.completed`, `summary.failed`, `docs.commented`, `docs.shared`,
  `docs.access-request@0.3.0`, and `ai.reasoning-process`.
- Several platform-owned legacy builders still pass an empty
  `carddispatch.Card.RenderProfile`. Their newly sent payload therefore omits
  the field even though the client currently infers the same visual profile.

The observed production `summary.completed` payload without `render_profile`
does not match the current source path. Implementation verification must pin
the final transport payload and deployed server SHA; do not “fix” the summary
template blindly when the current manifest and dispatch chain are already
correct.

## In scope

### Platform new-send paths

The following newly sent cards must explicitly emit
`render_profile: "octo-chat/v1"`:

1. Docs legacy notification branches in `deliverDocsCardNotification`:
   - `access_requested` when the approval-card Registry gate is off;
   - `access_granted`;
   - `access_denied`.
2. The applicant outcome card sent by `DocsActionFinalizer`, including the
   user-facing “文档访问已获批准/拒绝” cards.
3. Generic approval request cards sent by `deliverApprovalCardNotification`.
4. Generic approval applicant outcome cards sent by
   `StandardActionFinalizer`.

## Proposed design

### 1. Author the default at the internal dispatch boundary

`internal/carddispatch.producerSender.Send` owns the final type-17 envelope for
all platform card new-sends. Resolve the outgoing render profile as follows:

1. non-empty `Card.RenderProfile` -> validate and preserve it;
2. empty `Card.RenderProfile` -> author `cardmsg.RenderProfileOctoChatV1`;
3. unsupported non-empty value -> fail closed as today.

Always write the resolved non-empty value into the final payload before
`cardmsg.Validate`, `Finalize`, the final payload-size check, and transport
serialization.

This is intentionally scoped to the internal platform dispatch boundary. It
avoids duplicating the same default across every notify caller and makes future
platform-card senders safe by construction. It does not change the meaning of
missing fields when reading historical messages.

### 2. Lock the contract with transport-level tests

Tests must cover the final envelope, not only manifest metadata or the
intermediate Adaptive Card document:

- internal dispatch authors `octo-chat/v1` when `Card.RenderProfile` is empty;
- explicit `octo-chat/v1` remains unchanged;
- an unsupported explicit value is rejected before transport;
- `summary.completed` and `summary.failed` Registry renders expose the field,
  and their internal-dispatch transport payload retains it;
- docs and generic approval tests assert newly sent request/outcome cards have
  the field;
- non-card payload behavior is unchanged.

## Load-bearing behavior

- **Wire contract:** `render_profile` is a top-level sibling of `profile`, not
  part of the Adaptive Card document or `metadata.octo`.
- **Profile separation:** this task must not rename `profile`, introduce a
  `card_profile` wire field, or change any `octo/v1`/`octo/v2` capability.
- **Server authority:** callers cannot select arbitrary render profiles.
  Unknown explicit values remain fail-closed through the accepted-profile
  allowlist.
- **Historical compatibility:** stored historical messages and their existing
  `content_edit` frames are not rewritten or changed for this task. Missing
  historical fields continue to use the client's existing `octo-chat/v1`
  fallback.
- **Frozen L1 contract:** do not modify
  `docs.access-request@0.2.0`. Its field-less historical artifact remains
  frozen; a later authoritative update may replace it with the existing
  `0.3.0/result` frame.
- **Mutation behavior:** existing-card updates, sender ownership,
  message/channel/Space binding, lifecycle checks, `card_seq` CAS, revision
  append, and CMD semantics are unchanged.
- **Validation and size:** the final enriched payload still runs
  `cardmsg.Validate`, `Finalize`, and the existing post-enrichment size check.
- **Failure behavior:** adding the field must not turn a render or dispatch
  error into a fabricated success or change text/card message types.

## Out of scope

- No historical message, `message_extra`, or revision backfill.
- No change to `content_edit` or any already-sent card update path.
- No database migration.
- No client changes; the client fallback is already implemented.
- No changes to card layout, copy, actions, inputs, template data schemas, or
  template versions.
- No change to the frozen `docs.access-request@0.2.0` assets.
- No new render profile generation such as `octo-chat/v2`.
- No runtime-catalog activation, producer-grant, or owner-policy change.
- No Incoming Webhook or legacy Robot API change in this task. Those are raw
  external ingress surfaces, not the platform-owned card paths requested here,
  and require their own compatibility/trust-boundary review.
- No change to the user message API, which continues to reject type-17 sends
  and edits.

## Acceptance

- Every platform new-send listed in **In scope** reaches transport with an
  explicit top-level `render_profile: "octo-chat/v1"`.
- The applicant-facing “文档访问已获批准/拒绝” cards contain the field.
- Current Registry-backed defaults continue to contain the field, including
  `summary.completed` and `summary.failed`.
- The canonical capability field remains `profile`; no `card_profile` field is
  introduced.
- Unsupported explicit render profiles remain rejected and never reach
  transport.
- Historical payload bytes and the frozen `docs.access-request@0.2.0` tree have
  no diff.
- Existing card authorization, action dispatch, mutation CAS, revision, CMD,
  localization, and text-fallback tests remain green and unchanged.
- Focused tests pass:

  ```bash
  go test ./internal/carddispatch/...
  go test ./pkg/cardtmpl/summary_completed/... ./pkg/cardtmpl/summary_failed/...
  go test ./modules/notify/... -run 'RenderProfile|Summary|DocsActionFinalizer|StandardActionFinalizer|Approval'
  ```

## Rollout and rollback

- No new rollout flag is required: clients already resolve missing historical
  fields to the same `octo-chat/v1` value, so this change makes an existing
  rendering decision explicit rather than selecting a new renderer.
- Before production rollout, capture at least one raw WuKongIM payload for
  `summary.completed`, docs granted/denied outcome, and generic approval result;
  record the deployed octo-server SHA with the evidence.
- Monitor existing card dispatch failure metrics and client rendering errors.
  The added bounded string must not create a new high-cardinality label.
- Rollback is an image rollback. No data rollback is required: clients already
  accept both explicit `octo-chat/v1` and missing historical fields.
