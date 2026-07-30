---
type: Journal
title: "Journal: cardtmpl-reasoning-controls-hidden-successor"
description: Publish ai.reasoning-process@0.3.0 without unsupported stop/retry controls, cut new Bot sends to V3, and preserve exact-version historical edits.
tags: [cardtmpl, ai-reasoning-process, json-template, bot-api, wire-contract, trust-boundary, testing, rollback]
timestamp: 2026-07-30T13:02:11+08:00
# --- octospec extension fields ---
task: cardtmpl-reasoning-controls-hidden-successor
source: user
---

# Journal: cardtmpl-reasoning-controls-hidden-successor

## What was done

- Added immutable built-in `ai.reasoning-process@0.3.0` from the bounded server
  `0.2.0` baseline. The V2 schema and four unchanged samples remain
  byte-identical; the only product deltas are removal of `reasoning_stop` and
  `reasoning_retry` Submit controls plus the obsolete retry invitation.
- Preserved all five states and the existing wire mapping: reasoning,
  answering, and error use `octo/v2`; completed and stopped use `octo/v1`.
  Every state keeps the client-local `reasoning_toggle`; V3 has no server
  action contract, inputs, or Submit actions.
- Registered V1, V2, and V3 together, moved the Registry default and Bot
  new-send advertisement to V3, and retained same-exact-version historical
  edits for all three versions. V1/V2 new sends and cross-version edits remain
  fail-closed.
- Covered RuntimeCatalog static reconciliation for the new exact V3 identity,
  including a real-MySQL dynamic-claim collision. Because V3 has no Submit,
  it needs no `reasoning.control` RouteSpec; dynamic interactive artifacts
  retain their existing route requirement.
- Made Bot visual-profile ownership server-authoritative. Raw type-17 send and
  edit callers omit `render_profile`; the Bot API writes `octo-chat/v1` after
  validating the caller frame and before final dispatch/persistence. Registry
  `template_ref` callers still omit the field because the server authors it
  from the immutable manifest.

## Review closure

- Documented the inherited RuntimeCatalog rollback trap: persisting a built-in
  V3 activation pointer makes a rollback image without V3 classify the active
  static claim as sticky catalog integrity failure. The production runbook now
  forbids routine static-to-static Activate/Rollback, requires active-pointer
  compatibility proof before binary rollback, and reserves static fallback for
  a separately reviewed future dynamic pilot.
- Restored the composition-root comment for the unrelated
  `docs.access-request` V2→V3 finalizer path at the correct registration site.
- Added explicit regressions proving legacy reasoning stop/retry IDs do not
  resolve from V3 at either stored-frame Submit lookup or catalog/message
  `ActionContext` (`ErrActionUnknown` before enqueue), and Registry-mode callers
  cannot inject the server-owned `render_profile` field.
- Retargeted the V3 RouteSpec unit test to call `validateInteractiveTarget`
  directly; the separate startup test is now accurately named as a static
  registry compatibility test instead of claiming to cover a branch bypassed
  by static resolution.
- Mutation-tested the reported `octo/v1` registration gap. It is already
  closed by `renderCore -> cardmsg.Validate` before the report-only branch:
  even with a matching golden, an `Action.Submit` in the V1 result profile
  fails compilation. A dedicated regression now preserves that guarantee;
  no duplicate production walker was added.
- Expanded the tracked card protocol with the Bot `templating` capability and
  empty V3 `submit_actions`. The gitignored OpenClaw execution handoff was
  separately re-signed to V3 and now explicitly forbids falling back to a local
  raw-card/template version; it is operational context, not part of the PR.
- Corrected the raw Bot render-profile interpretation after product review:
  handler and real-service tests now prove omitted send/edit inputs produce a
  server-authored `octo-chat/v1` effective frame rather than requiring producer
  passthrough.

## Verification

- Focused suites passed serially for `pkg/cardtmpl/...`, `modules/bot_api/...`,
  `modules/card_template_catalog/...`, and the composition-root V3 default.
- The real-MySQL V3 static-claim/collision test executed and passed (not
  skipped) against the local container environment.
- Race suites passed for cardtmpl, catalog, cardmsg, card dispatch/action
  dispatch, the focused Bot capability/send/edit/raw-render-profile paths, and
  the V3 legacy-action message context rejection. The raw Bot edit lane also
  passed under `-race` against real MySQL/Redis/WuKongIM without skipping.
- `go build ./...`, `go vet ./...`, and `golangci-lint run ./...` passed with
  zero lint issues. `git diff --check`, all V3 JSON parsing, V1/V2 zero-diff,
  V2/V3 schema byte equality, and the forbidden-control scan passed.
- Rechecked statement coverage: `pkg/cardtmpl` 80.8% and
  `modules/card_template_catalog` 81.3%.

## Structural learnings / gotchas

- `profile` and `render_profile` are orthogonal. The former is the message
  capability tier; the latter selects a client visual compatibility tier. A
  card may legitimately remain `octo/v2` after removing every Submit action.
- Exact Forge provenance (`renderProfile=octo-chat@1.2.0-rc.1`) belongs in the
  immutable manifest; the message envelope carries only the stable
  `render_profile=octo-chat/v1` compatibility key.
- Bot callers never own the visual key. Raw-card frames receive the deployment
  default in the Bot API; Registry frames receive the manifest compatibility
  key. Adding an outer request field would create a competing authority.

## Remaining rollout boundary

- This is server-only. It does not implement OpenClaw selection/release,
  active-run authority, stop/retry semantics, dynamic catalog grants, or joint
  E2E.
- Production Bot and dynamic new-send gates remain closed for the first
  deployment. Operators must verify every replica reconciles V3, confirm no
  activation row pins static V3, and audit any still-active V1/V2 messages
  before enablement.
- The server-owned raw Bot visual key affects every raw type-17 Bot send/edit,
  not only reasoning cards. Rollout must verify target clients have the
  `octo-chat/v1` host generation before enabling Bot card traffic.
- A previous binary cannot resolve/edit V3. After the first V3 message exists,
  rollback must retain a V3-capable binary or explicitly accept freezing its
  last successful frame; immutable V3 must never be rewritten in place.
