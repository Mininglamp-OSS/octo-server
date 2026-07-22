---
type: Task
title: "Task: cardtmpl-interaction-closure"
description: Close the post-633 interactive-card loop with versioned result rendering, authoritative message updates, callback envelopes, and bounded metrics.
tags: [wire-contract, trust-boundary, error-response, i18n, space, auth, testing, commit, callback, card-update]
timestamp: 2026-07-22T16:03:51+08:00
# --- octospec extension fields ---
slug: cardtmpl-interaction-closure
upstream: octo-server#633
source: self
---

# Task: cardtmpl-interaction-closure

> Post-#633 roadmap group A: A1 `CardUpdater`, A2 standardized callback
> envelope, A3 `docs.access-request@0.3.0` result view, and A4 callback/update
> metrics. PR #633 merged as `2917e6a6`; its `docs.access-request@0.2.0`
> contract is published and immutable.

## Goal

Close the first platform-template interaction loop:

1. a `docs.access-request@0.2.0` or `@0.3.0` pending card is clicked;
2. the existing authenticated `/v1/message/card/action` ingress validates the
   effective frame and enqueues one authoritative action event;
3. a callback consumer receives either the byte-compatible legacy request or
   the opt-in `octo-card@1.0` nested envelope;
4. the docs finalizer renders `docs.access-request@0.3.0` in the
   `result` view (`approved` or `rejected`, `octo/v1`) and replaces the
   original message through the existing card mutation CAS/revision/CMD path;
5. callback and update outcomes are observable through bounded Prometheus
   counters.

The result visual is implemented as Go `Template.Build` code using the attached
backend handoff's `result.template.json`, approved/rejected samples, reports,
and goldens as the design input. This task does not introduce the `${}` JSON
template engine.

## Background

- PR #633 published `docs.access-request@0.2.0` with only the interactive
  `pending` view. The original design handoff also contained a `result` view,
  but it was deliberately excluded because the pilot had no updater/finalizer
  integration and no JSON expression engine.
- `modules/notify.DocsActionFinalizer` already rebuilds a terminal card and
  calls `internal/carddispatch.CardMutator.Mutate`, but it bypasses
  `Registry.Render`, manually builds the type-17 envelope, fixes the profile to
  `octo/v2`, and emits no cardtmpl update metric.
- `CardMutator` already owns sender binding, active-message lifecycle checks,
  card normalization, `card_seq` CAS, idempotent replay, revision append, and
  CMD sync. `CardUpdater` must compose this path, not create another write
  transport.
- The current callback HTTP body is the flat `DecisionRequest`. The public
  client action request remains unchanged; the standardized envelope is a
  server-to-server callback format and must be route-versioned so an upgrade
  cannot silently break an existing strict callback consumer.
- The merged `0.2.0` action data contains enough authoritative identity for a
  safe terminal update (`doc_id`, `request_id`, `doc_title`, requester display
  name), but not every decorative result field from the design sample. The
  `0.3.0` result renderer therefore treats avatar/reason/time/permission copy as
  optional and omits unavailable blocks for in-flight `0.2.0` cards.

## Load-bearing list

- **Published L1 wire contract (`wire-contract`)**: do not modify any byte under
  `pkg/cardtmpl/docs_access_request/handoff/docs.access-request@0.2.0/`.
  Register `0.2.0` and `0.3.0` concurrently. New sends may move to the `0.3.0`
  default only after its register-time self-check passes.
- **Template identity/version transition (`wire-contract`)**: an in-flight
  `0.2.0/pending` message may be authoritatively replaced by
  `0.3.0/result`; the message ID and channel binding stay unchanged while
  `metadata.octo.template.version` becomes `0.3.0`.
- **State vocabulary (`wire-contract`)**: callback `StateApproved` maps to
  template state `approved`; callback `StateDenied` maps to template state
  `rejected`. `cancelled` is not declared by the `0.3.0` result contract and
  retains the existing legacy terminal fallback.
- **Card mutation authority (`trust-boundary`, `auth`, `space`)**:
  `sender_uid`, `message_id`, channel, channel type, Space, and monotonic
  `card_seq` come from the stored message/action event, never callback display
  fields. Wrong sender, revoked/deleted message, non-card source, stale sequence,
  or channel mismatch fails closed through `CardMutator`.
- **Effective-frame semantics (`wire-contract`)**: Append reads the current
  effective card (`message_extra.content_edit` when present, otherwise original
  payload), appends one element to `card.body`, then runs the same
  `cardmsg.Validate/Finalize` and CAS mutation path. It never edits the original
  payload directly and never bypasses revision history.
- **Revision/CMD semantics**: authoritative `content_edit` persistence remains
  primary; revision append and CMD sync remain best-effort exactly as in the
  existing `CardMutator`. Replays do not append a duplicate revision or send a
  duplicate CMD.
- **Client action ingress (`wire-contract`, `error-response`, `i18n`, `auth`,
  `space`)**: `POST /v1/message/card/action` request and response shapes,
  anti-IDOR checks, visibility gates, input validation, UID rate limiting, and
  Redis idempotency are unchanged.
- **Callback trust boundary (`trust-boundary`, `wire-contract`)**: action data
  is extracted from the effective server-authored frame; inputs are the
  existing allowlisted/typed map; template ID/version come from
  `metadata.octo.template`; view is resolved from the registered interaction
  report and matching action ID. Client-supplied `data`, template identity, or
  view are never trusted.
- **Callback compatibility (`wire-contract`)**: existing routes default to the
  legacy flat request. A validated route option explicitly selects the nested
  `octo-card@1.0` payload. HMAC continues to cover the exact request body and
  event ID headers. Queue rows created before this change still decode.
- **Callback envelope**: the new format contains `protocol`,
  `type=card.action`, `action.{id,data,inputs}`,
  `card.{template_id,template_version,view,message_id,channel_id,channel_type}`,
  `actor.uid`, and an opaque `trigger_id` derived from the durable event ID.
  `response_url` remains optional/reserved because §7 defines no authenticated
  response request body; no unusable URL is emitted.
- **Localization (`i18n`)**: result labels are selected from
  `BuildEnv.Lang`; business/display fields are escaped, rune-bounded, and never
  treated as trusted card JSON. Existing applicant notification localization
  and denial-reason behavior remain intact.
- **Metrics cardinality**: `template_id`/`version` come from registered metadata,
  `action_id` comes from a declared interaction, and result values are closed
  enums. Metrics do not record arbitrary user input, UID, message ID, document
  ID, URL, or error text.
- **Testing (`testing`)**: use TDD; unit tests cover render/update/envelope/error
  branches, integration tests cover finalizer-to-CAS behavior, and the existing
  full card-action orchestration remains green.
- **Git/PR (`commit`)**: English Conventional Commit messages and English PR
  description; link this brief in the PR.

## Out of scope

- No modifications to the frozen `docs.access-request@0.2.0` assets or behavior.
- No `${}` JSON template engine and no runtime loading of `templates/*.json`.
- No capability-discovery/export endpoints (roadmap B).
- No migration of docs shared/commented, summary, or generic approval cards
  (roadmap C).
- No `ext.*`/L2b production channel, owner registry, or private visibility
  rollout (roadmap D).
- No explicit notify envelope replacing `NotifyReq` (roadmap E2).
- No public/signed `response_url` execution endpoint. The field remains absent
  until its request schema, authorization, TTL/replay semantics, and operational
  ownership are specified in a separate brief.
- No change to generic `StandardActionFinalizer` rendering; only the docs
  binding adopts the registry-backed `0.3.0` result view.
- No database migration. Existing `message_extra` and
  `octo_message_card_revision` storage remains authoritative.
- No change to applicant outcome-card delivery semantics; callback retries may
  still retry the applicant notification under the existing at-least-once
  boundary.
- G1/G2/G3 technical-debt refactors remain separate unless a minimal local
  adjustment is required to compile the A implementation.

## Acceptance

- A new `pkg/cardtmpl/docs_access_request` version `0.3.0` is registered beside
  `0.2.0`; its manifest declares `pending: octo/v2` and
  `result: octo/v1` with states `approved`/`rejected`.
- `0.3.0` carries hardened schema/samples/reports for all three states; every
  sample passes register-time self-check and the shared conformance suite.
- Approved/rejected `Registry.RenderCard` output has
  `metadata.octo.template={id:"docs.access-request",version:"0.3.0"}`, the
  expected result variant, no `Action.Submit`/`Input.*`, and passes
  `cardmsg.Validate` under `octo/v1`.
- The `0.2.0` handoff tree has no diff against merge commit `2917e6a6`.
- `CardUpdater.ReplaceView` accepts an authoritative update target containing
  sender UID, message ID, channel, channel type, Space, message sequence
  (optional lookup optimization), and positive card sequence; it renders via
  Registry, persists through `CardMutator`, preserves message ID, and returns
  typed render/mutation errors without fallback writes.
- `CardUpdater.Append` reads the effective card, appends exactly one JSON object
  to `card.body`, preserves template metadata/profile/Space, uses the supplied
  positive `card_seq`, validates the full envelope, and persists through the
  same mutator. Non-object elements, non-card sources, stale sequences,
  revoked/deleted targets, and invalid post-append cards fail closed.
- Repeating an identical ReplaceView/Append frame is an idempotent replay with
  no duplicate revision/CMD. Competing different frames with the same or lower
  `card_seq` return the existing conflict error.
- Docs approved/denied finalization calls `CardUpdater.ReplaceView` with target
  version `0.3.0`; approved maps to `approved`, denied maps to `rejected`, and
  the denial reason is bounded before rendering. Existing cancelled/unavailable
  behavior stays on the legacy fallback.
- A `0.2.0` pending event with only the currently published action-data keys can
  be finalized into a valid `0.3.0/result` card; missing decorative fields are
  omitted rather than fabricated.
- The action ingress enriches internal callback events only from the effective
  card and Registry. Invalid/unknown template metadata or an action not declared
  for the resolved view is rejected before enqueue for the registry-backed
  route; legacy cards without template metadata retain the existing path.
- Route config accepts only `legacy` (default) or `octo-card-v1` callback
  formats. Legacy callback body tests remain byte-compatible; the new format
  matches the nested envelope above, preserves HMAC verification, and carries a
  stable string trigger ID without treating it as authorization.
- `dmwork_cardtmpl_callback_total{template_id,version,action_id,result}` records
  one of `ok|rejected|error`; `dmwork_cardtmpl_update_total{template_id,version,result}`
  records `ok|error`. Tests prove registration, increments, and bounded labels.
- Focused RED tests fail for missing `0.3.0`, updater, envelope, and metrics
  behavior before production implementation is added; the same tests pass
  after implementation.
- Required verification passes: focused `pkg/cardtmpl`,
  `internal/carddispatch`, `internal/cardactiondispatch`, `modules/message`, and
  `modules/notify` tests; `go test -race -cover` for the new updater/template
  packages with at least 80% statement coverage for new production code;
  `go vet ./...`; `golangci-lint run ./...`; `make i18n-extract-check`;
  `make i18n-lint`; and `go test ./...` when MySQL/Redis/WuKongIM are available.
