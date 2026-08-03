---
type: Task
title: "Task: cardtmpl-reasoning-controls-hidden-successor"
description: Publish ai.reasoning-process@0.3.0 without unsupported stop/retry controls while preserving the bounded server contract and historical exact-version edits.
tags: [card, cardtmpl, ai-reasoning-process, json-template, bot-api, wire-contract, trust-boundary, test, testing, observability, rollback]
timestamp: 2026-07-30T12:09:35+08:00
# --- octospec extension fields ---
slug: cardtmpl-reasoning-controls-hidden-successor
upstream: "roadmap E1d/E3 safety follow-up; server PRs #667/#675; source attachment sha256 db694427d79f5eb4b760993dca46eccb8b0f48620d4e82c00136e2c02c69ebe4"
source: user
---

# Task: cardtmpl-reasoning-controls-hidden-successor

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> Finish status (2026-07-30): `/octospec-go`, `/octospec-check`, and local
> `/octospec-finish` gates are complete. Focused, race, real-service integration,
> build, vet, lint, immutability, and diff-hygiene gates passed locally; Draft PR
> #681 records the evidence and rollout boundary. Deployment checks remain for
> the later operational go/no-go and are not claimed by this PR.

## Goal

Publish a new immutable built-in JSON artifact, `ai.reasoning-process@0.3.0`, whose rendered cards no
longer display the unsupported `reasoning_stop` or `reasoning_retry` `Action.Submit` controls. Preserve
the client-side `reasoning_toggle` action, all five states, the bounded `0.2.0` data contract, and the
existing state-to-view/wire mapping:

| View | States | Wire profile | `0.3.0` actions |
| --- | --- | --- | --- |
| `active` | `reasoning`, `answering` | `octo/v2` | `reasoning_toggle` only |
| `result` | `completed`, `stopped` | `octo/v1` | `reasoning_toggle` only |
| `error` | `error` | `octo/v2` | `reasoning_toggle` only |

Register `0.1.0`, `0.2.0`, and `0.3.0` together; make `0.3.0` the Registry default and the only Bot
version advertised/accepted for new `template_ref` sends; retain exact-version edit compatibility for
all three versions. `0.1.0` and `0.2.0` remain byte-for-byte frozen.

This is a server-only safety successor. It does not implement OpenClaw run control, change OpenClaw
fallback behavior, or open any production gate.

## Background

### Verified server baseline

- PR #657 introduced frozen `ai.reasoning-process@0.1.0` with active/error Submit controls.
- PR #667 introduced bounded `0.2.0`, registered both versions, moved Registry default and Bot new-send
  advertisement to `0.2.0`, and retained `0.1.0` for historical edits. Its acceptance deliberately kept
  templates/reports/samples/goldens visually identical to `0.1.0`, so the unsupported controls remained.
- PR #675 installed the RuntimeCatalog overlay and static reconciliation, but PR-C grants and Bot dynamic
  capability merging are not implemented. A dynamic publish cannot currently become an authorized Bot
  new-send path. This task therefore adds a new built-in static version and requires a server release.
- PR #675 also permits a validated static version to be persisted as an activation target. That pointer
  outlives the image and overrides `SetDefault`; if it names V3, a rollback binary without V3 classifies
  the orphan static active target as sticky catalog integrity failure. The V3 rollout therefore forbids
  using Activate/Rollback for routine built-in version selection and requires an activation-pointer check
  before binary rollback.
- Current package registration is in `main.go:installCardTmplRegistry`; Bot new-send and historical-edit
  policy is in `modules/bot_api/card_template_catalog.go`.
- Current `0.1.0/0.2.0` mapping is active=`octo/v2`, result=`octo/v1`, error=`octo/v2`. The result profile
  is load-bearing and remains unchanged.
- The shared validator accepts an explicit `payload.render_profile`, but raw Bot send/edit previously did
  not author one when the caller omitted it. That left the visual generation under producer control and
  made omitted raw frames use Legacy rendering. Registry `template_ref` sends already author the field
  from `renderProfileCompatibility`.
- Production `OCTO_BOT_CARD_ENABLED` and runtime dynamic new-send remain closed until their separate
  rollout/go-no-go. Merge is not authorization to enable either gate.

### Source attachment audit

The user supplied local artifact:

```text
.context/attachments/DaYAA4/ai.reasoning-process@0.2.0.handoff.zip
sha256 db694427d79f5eb4b760993dca46eccb8b0f48620d4e82c00136e2c02c69ebe4
```

All 22 JSON documents parse successfully. Its templates, interaction reports, and goldens contain only
`Action.ToggleVisibility`; there is no `reasoning_stop`, `reasoning_retry`, or template-level
`Action.Submit`. The generic render-profile capability still lists `Action.Submit` as an allowed platform
action; that is not evidence that this template uses it.

The ZIP is a UX input, not a directly registrable server bundle:

- it reuses the already published identity `0.2.0`, which cannot be overwritten;
- its schema is the old unbounded contract and lacks the #667 string/array/aggregate limits;
- its manifest lacks server-required `protocol`, `owner`, render-profile compatibility, and explicit
  state declarations;
- it changes result from the server contract’s `octo/v1` to `octo/v2`;
- it emits one interaction report per state, while the server artifact format requires one report per
  `octo/v2` view;
- it also renames render component IDs/badge IDs, which is unrelated to hiding stop/retry;
- its error sample removes “可以重试”, which is consistent with hiding the retry control.

The server adaptation therefore uses repository `0.2.0` as the security/wire baseline and imports only
the approved product deltas: remove stop/retry Submit controls and remove the retry invitation from the
error sample/golden. The source ZIP is gitignored; all accepted deltas must be represented by reviewed,
tracked `0.3.0` assets and tests.

## Contract decisions

### D1 — New immutable identity

- Add `pkg/cardtmpl/ai_reasoning_process/handoff/ai.reasoning-process@0.3.0/`.
- `id` remains `ai.reasoning-process`; `version` becomes `0.3.0`.
- Keep `contractVersion=1.1.0`, because the producer data/schema contract remains the bounded #667
  contract. The template version, not the data-contract version, carries the presentation/action change.
- Keep `protocol=octo-card@1.0`, `adaptiveCardVersion=1.5`, pinned
  `renderProfile=octo-chat@1.2.0-rc.1`, `renderProfileCompatibility=octo-chat/v1`, and `owner=ai`.
- Omit `actionType` from `0.3.0`. With no `Action.Submit`, `TemplateMeta.ActionContract` must be nil rather
  than advertising a callback contract that cannot be invoked.

### D2 — Minimal UX delta

- `active.template.json`: remove only `reasoning_stop`; retain `reasoning_toggle` and existing server
  component IDs/layout.
- `error.template.json`: remove only `reasoning_retry`; retain `reasoning_toggle` and existing server
  component IDs/layout.
- `result.template.json`: preserve the existing server template and `octo/v1` wire profile.
- `samples/error.json`: remove the “可以重试” invitation; preserve the rest of the safe error copy.
- Other samples remain byte-identical to server `0.2.0`.
- Do not import the attachment’s component-ID renames, `renderProfile=latest`, result=`octo/v2`, per-state
  report layout, or unbounded schema.

### D3 — Reports, schema, and goldens

- Copy the bounded `0.2.0` schema byte-for-byte, including `x-octo-constraints` aggregate limits.
- Keep server report layout: `active.interaction.json` and `error.interaction.json` only. Each report lists
  exactly one `Action.ToggleVisibility` (`reasoning_toggle`), zero inputs, and the existing two toggle
  targets. Result stays `octo/v1` and has no report document; the shared `cardmsg.Validate` profile gate
  rejects any `Action.Submit` in that view during registration before report-specific checks.
- Regenerate/review all five goldens from the selected templates and samples. Reasoning/answering/error
  goldens lose their Submit control; error also loses the retry invitation. Completed/stopped stay
  canonical-equal to the server `0.2.0` output.
- Full report-drift and golden self-check remain registration-time fail-close gates; no test-only bypass or
  hand-maintained manifest `submit_actions` list is introduced.

### D4 — Registry and Bot cutover

- Add explicit `HandoffRootV3` / `TemplateVersionV3`; current-version aliases point to V3.
- Register V1, V2, then V3 before `Registry.Freeze()`; set the default to V3.
- Bot `AdvertisedSend` contains only `ai.reasoning-process@0.3.0`.
- Bot `EditCompatible` contains V1, V2, and V3. Existing messages remain pinned to their stored exact
  version; there is no cross-version edit or migration.
- Capability for V3 reports empty `submit_actions` for active, result, and error. New sends using V1/V2
  fail before dispatch; historical V1/V2 edits keep their current authorization and validation order.
- RuntimeCatalog static reconciliation learns V3 from the frozen Registry; no dynamic artifact, grant,
  activation row, or migration is added by this task.

### D5 — Bot send render-profile contract

- Keep `render_profile` inside the final/effective raw type-17 `payload`; do not add a conflicting outer
  `/v1/bot/sendMessage` request field.
- Raw Bot send and raw Bot edit callers omit the field. After validating the caller-authored frame, the
  Bot API writes `render_profile="octo-chat/v1"` before final size validation and dispatch/persistence.
  A caller that already sends the accepted same value remains compatible, but it is not the authority;
  invalid explicit values still fail at the existing validation gate.
- Keep Registry mode server-authoritative: a `template_ref` caller still cannot supply `render_profile`,
  while rendered V3 messages receive `octo-chat/v1` from the manifest.
- This adds no outer request field, new profile value, or capability-manifest field.

## Load-bearing list

- **L1 immutable artifact identity (`cardtmpl`, `wire-contract`)** — published `0.1.0/0.2.0` bytes, manifests,
  samples, reports, templates, and goldens cannot change. Hiding controls requires a new exact version.
- **Bounded JSON producer contract (`cardtmpl`, `trust-boundary`)** — all #667 max lengths, phase/action
  bounds, aggregate action cap, template budgets, URL/cardmsg validation, and fail-close classification
  remain intact; the attachment’s unbounded schema cannot replace them.
- **Interaction truth (`cardtmpl`, `wire-contract`)** — rendered templates, reports, goldens,
  `TemplateMeta.ActionContract`, and Bot `submit_actions` must agree that V3 has no Submit controls.
- **View/state/profile compatibility (`wire-contract`)** — five states and active/error=`octo/v2`,
  result=`octo/v1` remain stable. Hiding actions must not silently change view selection or wire profile.
- **Registry multi-version/default (`cardtmpl`)** — three exact versions coexist; only V3 is the default;
  `Freeze()` and global exact-key uniqueness remain unchanged.
- **Bot capability/new-send/edit boundary (`bot-api`, `wire-contract`)** — only V3 is discoverable/sendable,
  while historical V1/V2 exact edits remain possible without allowing cross-version overwrite or existence
  probing.
- **Visual-profile ownership (`bot-api`, `wire-contract`)** — callers do not choose `render_profile`.
  Raw Bot send/edit use the deployment-owned stable key; Registry frames use the selected immutable
  manifest. Both paths continue through final `cardmsg` validation and size gates.
- **Runtime catalog reconciliation (`cardtmpl`, `testing`)** — V3 becomes a new static claim on startup;
  a pre-existing dynamic claim for the same exact key must fail readiness rather than select by replica.
- **Rollout/rollback (`rollback`)** — old binaries cannot edit a V3 card. Production gates stay closed until
  all replicas and the compatible consumer are verified; rollback cannot rewrite V3 messages to V2/raw.
  V3 is selected only by the image Registry default: normal operations must not persist a static V3
  activation pointer that an older rollback image cannot validate.
- **Regression evidence (`test`, `testing`)** — conformance, report drift, golden rendering, Bot policy,
  historical edits, runtime startup/reconciliation, focused race, build, vet, and diff hygiene are required.

## TDD implementation checkpoints

1. **RED — immutable/version contract**: add tests expecting V1/V2/V3 registration, V3 default, old exact
   versions still renderable, and frozen V1/V2 directories unchanged.
2. **RED — control removal**: assert V3 active/error rendered output and reports contain no
   `Action.Submit`, `reasoning_stop`, `reasoning_retry`, or retry invitation; toggle remains valid in all
   five states; V3 `ActionContract` is nil.
3. **GREEN — V3 artifact**: adapt the approved attachment delta onto the bounded V2 server baseline and
   make all five self-check/golden fixtures pass.
4. **RED→GREEN — Registry/composition**: add V3 constants, registration, and default without changing
   freeze/order guarantees.
5. **RED→GREEN — Bot policy**: advertise/send V3 only, retain V1/V2/V3 historical edits, assert empty
   Submit capability and zero dispatch/side effects for old-version new sends or cross-version edits.
6. **RED→GREEN — RuntimeCatalog**: cover V3 static reconciliation/exact lookup and prove no interactive
   RouteSpec is required for a no-Submit V3 artifact; retain source-sensitive V2 static acceptance and
   the dynamic interactive RouteSpec guard.
7. **Verify**: focused tests first, then cardtmpl/Bot/catalog race and integration lanes, build/vet, and
   `git diff --check`. No error-code/i18n change is expected.

## Out of scope

- Modifying any file under frozen `ai.reasoning-process@0.1.0` or `@0.2.0`.
- OpenClaw source, selector, package/release, local Model B behavior, or deployment configuration.
- Implementing `reasoning_stop`/`reasoning_retry`, active-run authority, event polling, queue ACK, or E1e.
- PR-C grants, trusted producer provenance, B1/B2 discovery/export, dynamic Bot capability, or a dynamic
  producer pilot.
- Publishing this version through the dynamic catalog; without PR-C grants it would remain unusable for
  Bot new-send.
- Importing attachment-only component-ID/badge-ID renames, result=`octo/v2`, `renderProfile=latest`,
  render-profile package files, or the unbounded source schema.
- Removing `stopped` state. It remains part of the producer contract for cancellation initiated outside
  these hidden card controls and for historical compatibility.
- Adding routes, DB migrations, error codes, localized error responses, callback RouteSpecs, or client
  rendering changes.
- Adding `render_profile` as an outer Bot request field, introducing a new render-profile value, or changing
  the `/v1/bot/card/profile` capability response.
- Enabling production Bot/runtime gates or claiming OpenClaw E2E/release completion.
- Redesigning PR #675's global startup integrity classification or removing its future dynamic-to-static
  recovery mechanism. This PR documents and gates the safe V3 operational path; broader isolation of an
  incompatible active target belongs in a separate reviewed RuntimeCatalog hardening change.
- Retrofactively rewriting already delivered V1/V2 payloads; those cards remain immutable historical
  messages and may still display their original controls.

## Acceptance

### A. Artifact identity and immutability

- [x] A tracked `ai.reasoning-process@0.3.0` server handoff exists with exact `id/version`, explicit states,
  pinned protocol/render-profile metadata, `owner=ai`, no `actionType`, and `contractVersion=1.1.0`.
- [x] `git diff origin/main --` for both V1 and V2 handoff directories is empty; no formatting or generated
  byte drift is accepted.
- [x] V3 schema is byte-identical to bounded V2 schema and still rejects every string/array/aggregate
  limit+1 case before template expansion while accepting exact-limit and worst-case valid fixtures.
- [x] Reasoning/answering/completed/stopped samples remain byte-identical to V2; error differs only by
  removing “可以重试”.

### B. No unsupported controls

- [x] V3 templates, reports, and goldens contain no `Action.Submit`, `reasoning_stop`, `reasoning_retry`,
  `stop_reasoning`, `retry_reasoning`, or “可以重试”.
- [x] Every V3 state renders exactly one `reasoning_toggle`; its targets resolve to `trace_panel` and
  `collapsed_panel`, with no input or server callback payload.
- [x] V3 active/error per-view reports exactly match rendered actions; result remains `octo/v1` with no
  report document; registration-time report/golden drift checks pass, and a mutation test proves an
  `octo/v1` Submit is rejected by `cardmsg.Validate` even when its golden matches.
- [x] V3 `TemplateMeta.ActionContract == nil`; V1/V2 action contracts and exact rendered Submit payloads
  remain unchanged for historical messages.

### C. Registry and runtime catalog

- [x] Registry lists V1, V2, V3, resolves empty-version/default lookup to V3, and continues exact lookup
  and rendering for V1/V2 after `Freeze()`.
- [x] Composition root registers all three before installing the RuntimeCatalog; no duplicate registry,
  post-freeze mutation, or initialization-order fallback is introduced.
- [x] Startup static reconciliation records V3’s exact identity with `source=static` (the existing static
  claim model does not persist built-in hashes). Static/dynamic V3 source conflict and malformed artifact
  still fail readiness; explicit historical static exact lookup remains available during dynamic DB outage
  under the existing catalog contract.
- [x] V3 activation/validation does not require a `reasoning.control` RouteSpec because there is no Submit;
  existing V2 static-interactive tests retain the source-sensitive built-in bypass, while dynamic
  interactive artifacts continue requiring a matching route.

### D. Bot capability, send, and edit

- [x] `/v1/bot/card/profile` advertises exactly one `ai.reasoning-process` entry, V3, and all three views
  expose `submit_actions: []` even when top-level card delivery is disabled.
- [x] A V3 new send renders through server `template_ref + state + data`, writes server-authored V3
  provenance, and emits no Submit action. V1/V2 new sends fail with zero message/dispatch side effects.
- [x] V1, V2, and V3 historical same-version edits remain allowed with the existing owner/Space/lifecycle/
  CAS checks. Cross-version edits and raw overwrite remain rejected before mutation.
- [x] Disabled gate, malformed data, stale/out-of-order `card_seq`, and identical replay behavior are
  unchanged; errors do not disclose catalog membership or historical message existence.
- [x] Raw Bot type-17 send and edit requests may omit `render_profile`; their dispatched/persisted frames
  contain server-authored `octo-chat/v1`. A valid explicit same value remains compatible, invalid values
  fail validation, and Registry manifest ownership remains unchanged.

### E. Verification evidence

- [x] Focused package tests pass for `pkg/cardtmpl/ai_reasoning_process`, `pkg/cardtmpl`,
  `modules/bot_api`, and `modules/card_template_catalog`.
- [x] Targeted race tests pass for reasoning registration/render, Bot template capability/send/edit, and
  RuntimeCatalog/static reconciliation paths.
- [x] Clean MySQL/Redis/WuKongIM integration lane passes for affected Bot/catalog behavior before finish;
  no shared-DB migration pollution is reported as success.
- [x] `go build ./...`, affected-package `go vet`, relevant source guards, and `git diff --check` pass.
- [x] V3 legacy `reasoning_stop`/`reasoning_retry` IDs do not resolve through `cardmsg.SubmitAction` or
  catalog/message `ActionContext` (`ErrActionUnknown` before enqueue), and Registry callers supplying
  server-owned `render_profile` fail before dispatch.
- [x] Handler-level raw send and real MySQL/Redis/WuKongIM raw-edit tests prove the Bot API authors
  `render_profile` when callers omit it; edit normalization preserves `card_seq` integer precision.
- [x] PR evidence records the source attachment SHA, exact server adaptation decisions, focused/race/
  integration results, and confirms that no i18n extraction was required because no error code or
  user-facing server error changed.

### F. Rollout and rollback

- [x] The production RuntimeCatalog runbook forbids routine static-to-static Activate/Rollback, explains
  the sticky-readiness failure mode, and requires checking active-pointer compatibility before binary
  rollback. `ai.reasoning-process@0.3.0` is selected by image `SetDefault`, not a MySQL activation row.
- [ ] First deployment keeps `OCTO_BOT_CARD_ENABLED=false` and dynamic new-send closed; all replicas reach
  ready after V3 static reconciliation before any experimental enablement.
- [ ] With the Bot gate closed, live profile inspection shows V3 only and empty Submit actions. A
  compatible consumer/release and server-rendered send/edit E2E are separate go/no-go evidence; this PR
  does not assert them without execution.
- [ ] Before enabling new sends, operators verify whether any V1/V2 experimental/production messages are
  still active. They are not migrated: old payloads can retain their original controls until naturally
  terminal/frozen.
- [ ] Target clients have the `octo-chat/v1` host generation before Bot card traffic is enabled; the
  server-authored raw Bot key applies to every raw type-17 Bot send/edit, not only reasoning cards.
- [ ] Before binary rollback, operators prove the activation target is absent or resolvable by the rollback
  image; no row may point to static V3. Before any V3 send, the previous binary is then safe. After a V3
  message exists, rollback must retain a binary that can resolve/edit V3 or accept freezing the last
  successful frame; it must not raw overwrite or mutate the immutable version.
- [ ] Reintroducing stop/retry in the future requires another immutable template version plus real E1e
  semantics and joint E2E; it cannot modify V3 in place.

## Human confirmation before `/octospec-go`

- [x] Accept `ai.reasoning-process@0.3.0` with `contractVersion=1.1.0`; V1/V2 remain byte-frozen.
- [x] Accept the minimal attachment delta only: hide stop/retry and remove retry invitation; preserve
  existing server component IDs, bounded schema, pinned render profile, and result=`octo/v1`.
- [x] Accept `owner=ai` with no `actionType`, making V3 `ActionContract=nil` while toggle remains a
  client-local action.
- [x] Accept V3-only Bot advertisement/new-send and V1/V2/V3 same-version historical edit compatibility.
- [x] Accept that already delivered V1/V2 cards are not migrated and can retain their old rendered
  controls; production gates remain closed while this is audited.
- [x] Accept server-only scope: no OpenClaw, E1e, PR-C grants/discovery, DB migration, or production enable.
