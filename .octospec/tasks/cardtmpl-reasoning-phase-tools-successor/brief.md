---
type: Task
title: "Task: cardtmpl-reasoning-phase-tools-successor"
description: Publish ai.reasoning-process@0.4.0 with per-phase collapsible tool panels and a simplified header, adapted onto the bounded server contract, and cut Registry default and Bot new-send to it via a server release.
tags: [card, cardtmpl, ai-reasoning-process, json-template, bot-api, wire-contract, trust-boundary, test, testing, rollback]
timestamp: 2026-08-07T09:59:46+08:00
# --- octospec extension fields ---
slug: cardtmpl-reasoning-phase-tools-successor
upstream: "front-end handoff attachment `ai.reasoningprocess0.3.0.handoff_1.zip` sha256 dd83e4dc89a296fe8409f6ab0543c2cf78d0cf451ee18fd93dd6f701da77e374; server PRs #657 (0.1.0) / #667 (0.2.0) / #681 (0.3.0) / #675 (RuntimeCatalog overlay)"
source: user
---

# Task: cardtmpl-reasoning-phase-tools-successor

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> Status: **draft, awaiting human confirmation of the brief as a whole.** The
> product decisions it depended on are resolved: D7 keeps `${statusGlyph}` bound,
> D8 adopts the simplified header (so `timerText` is no longer rendered), D8b
> keeps the `octo-*` design-system primitives out, and D3a widens
> `phases[].thought` to 4001. None of them *requires* a consumer change; D3a
> only takes visible effect once the consumer raises `THOUGHT_MAX`, which the
> maintainer owns separately.

## Goal

Publish a new immutable built-in JSON artifact, `ai.reasoning-process@0.4.0`, carrying the front-end's
redesigned presentation:

- per-phase collapsible tool panels (`reasoning_tools_panel_${$index}`), collapsed by default;
- chevron `Image` + `selectAction` toggles replacing the footer `ActionSet` toggle button;
- a simplified header that drops the `${timerText}` line.

Adapt that presentation onto the **existing bounded server data contract** (#667/#681), not onto the
attachment's unbounded schema — with one deliberate widening, `phases[].thought` 281 → 4001 (D3a), which
raises a ceiling rather than removing it. Preserve all five states and the existing state-to-view/wire
mapping:

| View | States | Wire profile | `0.4.0` actions |
| --- | --- | --- | --- |
| `active` | `reasoning`, `answering` | `octo/v2` | `Action.ToggleVisibility` only |
| `result` | `completed`, `stopped` | `octo/v1` | `Action.ToggleVisibility` only |
| `error` | `error` | `octo/v2` | `Action.ToggleVisibility` only |

Register `0.1.0`–`0.4.0` together; make `0.4.0` the Registry default and the only Bot version advertised
for new `template_ref` sends; retain exact-version edit compatibility for all four. `0.1.0`, `0.2.0`, and
`0.3.0` remain byte-for-byte frozen.

This is a **server-release** cutover. Hot-publishing through the runtime catalog is explicitly not the
delivery mechanism (see Background § Why a release).

## Background

### The attachment collides with an already-published version

The handoff declares `version: 0.3.0`, but `ai.reasoning-process@0.3.0` was already published by PR #681
and is live: registered in `main.go:installCardTmplRegistry`, advertised as the sole Bot new-send version,
and pinned by stored messages for exact-version historical edits. The attachment is a *different* artifact
under the same identity (`contractVersion` 1.1.0→1.2.0, `renderProfile` rc.1→rc.2, templates ~2× larger).

Overwriting `0.3.0` in place would re-render already-delivered historical cards with different content and
break the one-permanent-source-per-exact-key invariant. The delta therefore lands as `0.4.0`.

### Measured compile results against the current engine

The attachment bundle was compiled against `CompileJSONArtifact` on both limit profiles. Three defects
block it; everything else already passes:

| Defect | Error | Path affected |
| --- | --- | --- |
| per-view `"submit_actions": []` in manifest | `view "active": unknown key "submit_actions"` | both |
| missing `owner` / `protocol` | `owner is required` | runtime publish only |
| schema stripped of all `maxLength`/`maxItems`/`x-octo-constraints` | `$.properties.collapsedSummary string is unbounded` | runtime publish only |

With those three corrected, **both** `staticCompileLimits()` and `DefaultCompileLimits()` compile clean:
templates expand, all five goldens match canonically, samples validate, `jsontmpl.ValidateToggleTargets`
passes, and interaction-report conformance passes. The template's `${$index}` and `${if($index == 0, …)}`
usage is already inside the frozen `jsontmpl` subset — **no expression-engine change is required.**

`submit_actions` is a *derived* field in this repo (`modules/bot_api/card_template_catalog.go:170`
computes it from the interaction report). A manifest must not declare it; doing so would introduce the
hand-maintained list that #681 deliberately refused.

### The dropped bounds are a co-designed cross-repo contract

The consumer's constants and the server's schema bounds line up exactly:

| `openclaw-channel-octo/src/reasoning-process.ts` | server schema bound |
| --- | --- |
| `THOUGHT_MAX = 280` + `…` | `thought: 281` → **widened to 4001, see D3a** |
| `TOOL_NAME_MAX = 80` + `…` | `tool: 81` |
| `REASONING_ID_MAX_LENGTH = 512` | `reasoningId: 512` |
| `MAX_RENDERED_PHASES = 6` | `phases.maxItems: 6` |
| `SUMMARY_MAX 64` + `ERROR_MAX 120` joined | `detail: 192` |

Deleting them is not a gap in the handoff; it removes the contract both repos were built against. Every
bound is therefore restored verbatim **except `thought`, which is deliberately widened** (D3a) — a widening
is a relaxation, so it cannot reject a payload the consumer already sends.

### Node budget under the new presentation

The redesign costs materially more nodes per phase. Measured through `renderCore → cardmsg.Validate`
(`MaxNodes = 200`, `MaxDepth = 16`):

- new template, 6 phases: **13 aggregate actions OK, 15 OK, 16 fails** (`卡片节点数超过上限`);
- worst case admitted by the retained schema (6 phases / 13 actions / every string at its max) renders
  successfully in all five states;
- the only production producer already self-caps at 6 phases / **12** aggregate actions
  (`MAX_RENDERED_PHASES` / `MAX_RENDERED_ACTIONS` + `trimForRender`).

The aggregate cap of 13 therefore stays; it must not be relaxed as part of this change.

### Consumer impact: none required, one optional follow-up

`openclaw-channel-octo` deliberately carries **no local version allowlist**
(`selectReasoningProcessTemplate`, asserted by `it.each(["0.1.0","0.2.0","0.3.0","1.0.0","9.8.7"])`), and
`reasoning_template_ref` is derived server-side from `AdvertisedRef()` rather than stored per bot
(`modules/bot_api/card_profile.go:149`). Moving `AdvertisedSend` to `0.4.0` propagates to the plugin with
no plugin release, because the view/wire/state/`submit_actions` shape is unchanged.

D3a does not change that: widening `thought` cannot reject anything the current producer sends, so the
plugin keeps working untouched at `THOUGHT_MAX = 280`. It does, however, mean the extra capacity sits
unused until the consumer opts in by raising that constant — a separate, maintainer-owned change with no
ordering dependency on this one (server first is the safe order, and it is the order being taken).

### Why a release, not a hot update

The runtime control plane (`/v1/manager/card-templates/…`) exists but cannot deliver this change:

- it is dark by default (`OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED` /
  `..._NEW_SEND_ENABLED` both `false`);
- publish/activate is not a producer grant (`docs/card-template-runtime-catalog-runbook.md:14`);
- Bot advertisement is a compile-time constant resolved by **exact** version
  (`defaultBotTemplateRefs()`, `CatalogExactRequest{ID, Version}`), so an activation pointer cannot change
  what the plugin sees;
- the runbook explicitly forbids Activate/Rollback for built-in static version selection and names this
  template: *"do not Activate or Rollback `ai.reasoning-process` … as a shortcut for the image cutover"* —
  an orphan activation pointer takes replicas out of readiness on binary rollback.

Making this hot-updatable is PR-C scope (`card_template_grant`, dynamic Bot capability merging, producer
provenance) and is out of scope here.

## Contract decisions

### D1 — New immutable identity

- Add `pkg/cardtmpl/ai_reasoning_process/handoff/ai.reasoning-process@0.4.0/`.
- `id` stays `ai.reasoning-process`; `version` becomes `0.4.0`.
- Adopt the attachment's `contractVersion=1.2.0` and `renderProfile=octo-chat@1.2.0-rc.2` (provenance
  only — the server never validates it and the wire carries `renderProfileCompatibility=octo-chat/v1`
  instead); keep `renderProfileCompatibility=octo-chat/v1`, `adaptiveCardVersion=1.5`,
  `defaultLocale=zh-CN`. `1.2.0` covers **two** data-contract relaxations relative to `1.1.0`: D8's
  `timerText` `required` relaxation and D3a's `thought` widening. It is not bumped to `1.3.0` for the
  second one because `1.2.0` has never shipped — `0.4.0` is the first artifact to carry it, so both
  relaxations land as one published delta rather than as a fabricated version history.
- Restore `owner=ai` and `protocol=octo-card@1.0`. Omit `actionType` — with no `Action.Submit`,
  `TemplateMeta.ActionContract` must stay nil.

### D2 — Manifest normalization

- Remove per-view `submit_actions` from the manifest. Capability stays derived from the interaction
  reports; no hand-maintained Submit list is introduced.
- Views, states, wire profiles, template paths, and sample paths are otherwise taken as-is.

### D3 — Schema: bounded contract preserved

- Start from the frozen `0.3.0` schema and apply only these deltas:
  - `timerText` moves from `required` to optional (relaxation; see D8);
  - `phases[].thought` `maxLength` goes from `281` to `4001` (relaxation; see D3a);
  - descriptions/examples updated to match the new presentation.
- **`timerText` must stay in `properties` even though no template binds it.** The schema root is
  `additionalProperties: false` and the producer sends the field unconditionally
  (`ReasoningProcessData.timerText` is non-optional), so deleting the property would reject every
  payload the current plugin emits. Optional-but-present is the only safe shape: the template stops
  consuming it, existing producers keep validating, and future producers may omit it.
- Restore verbatim: every other `maxLength`, `phases.maxItems: 6`, `actions.maxItems: 12`, and
  `x-octo-constraints.aggregateArrayLimits` (`phases[].actions` `maxTotalItems: 13`).
- **Drop `phaseState`.** The attachment adds it, but no template in any view binds it and the producer
  does not send it. A required-nothing/read-by-nobody field is contract debt.
- Retain the `traceExpanded`/`traceCollapsed` mutual-exclusion `oneOf` and the state-conditional
  `required` blocks unchanged.

### D3a — `thought` ceiling is raised to 4001 and stops tracking the producer

`0.2.0`/`0.3.0` pinned `thought: 281` because that was the producer's *observed* output at the time
(`THOUGHT_MAX = 280` + `…`, per the `cardtmpl-reasoning-schema-successor` brief's D2 table, sourced from
consumer snapshot `530bc2dc`). That brief explicitly declined to present these numbers as a product
contract. Two problems follow from keeping it:

- **Zero headroom.** `280 + 1 = 281` is exactly the ceiling. The consumer currently truncates with
  `slice(0, 280)` (UTF-16 code units) and JSON Schema `maxLength` counts code points, and since code
  points ≤ code units that direction is safe — but any of these breaks it with no warning: switching to
  grapheme-aware truncation (one grapheme can be many code points — combining marks, ZWJ emoji, flags),
  or changing the ellipsis from `…` (1 code point) to `...` (3).
- **It caps a product surface at an implementation detail.** 280 characters is roughly two sentences;
  the field is the user-facing summary of a reasoning phase.

`4001` is chosen as a platform product cap (`4000 + …`, the same `N + ellipsis` shape as the old `281`),
not a mirror of any producer constant. Budget is not the constraint. Measured on the **`0.4.0` template**
at the worst case the schema admits (6 phases / 13 aggregate actions / every other string at max), so the
two rows isolate the bound change rather than conflating it with the template redesign — max across all
five states:

| `thought` data | worst-case payload | share of `cardmsg.MaxPayloadBytes` (512 KiB) |
| --- | --- | --- |
| 281 runes (what `0.3.0`'s bound allowed) | 35,076 B | 6.69% |
| 4001 runes (the new ceiling) | 102,036 B | 19.46% |

Even fully saturated the card uses under a fifth of the payload budget, leaving ~4× headroom.
`MaxNodes = 200` / `MaxDepth = 16` are *structural* limits — longer text adds no nodes, so the node
budget in the section above is unaffected and the aggregate cap of 13 still stands unchanged.

The jump is deliberately large (14×) rather than a token widening: the point is to stop the ceiling from
being a number that has to be revisited every time the producer's truncation changes. It is affordable
here specifically because V4 puts the tool rows — and with them the long text — behind per-phase
collapsible panels that default to collapsed (D4), so a 4000-character phase summary does not expand the
card on arrival. A larger cap would start to matter for payload budget under 6 saturated phases; a much
smaller one would leave the same "revisit it later" problem in place.

The field stays explicitly bounded on both ends: an unbounded string fails
`DefaultCompileLimits().RequireBoundedSchema`, and the `trust-boundary` rule requires an explicit
ceiling. Widening is safe in the rollout direction that matters — a relaxation cannot reject a payload
the current consumer already sends, so no plugin release is *required* by this change.

**Realising the longer text requires a consumer change, which is out of scope here.** Actual output
length is decided by the consumer's `THOUGHT_MAX` truncation; until that constant is raised
(`THOUGHT_MAX = 4000` to keep the `+…` at exactly 4001), server-side capacity is headroom the producer
does not use. That change is owned separately by the maintainer, in `openclaw-channel-octo`, and is not
part of this task's diff.

Only `thought` is widened. `tool: 81`, `detail: 192`, and `errorMessage: 121` carry the same zero-headroom
design and are knowingly left alone — `tool` is the most exposed of them (MCP tool names can exceed 80),
and revisiting them is a follow-up decision, not an unstated part of this change.

### D4 — Templates

- Adopt the attachment's three templates: per-phase `Container id=reasoning_phase_${$index}` with a
  chevron pair (`reasoning_tools_toggle_collapsed_${$index}` / `…_expanded_${$index}`) driving
  `reasoning_tools_panel_${$index}` (`isVisible: false` by default), and the header chevron pair driving
  `trace_panel` / `collapsed_panel`.
- `errorTitle`/`errorMessage` stay at body top level with **no** `isVisible` binding, as in `0.3.0`, so the
  failure reason is visible while the trace is collapsed.
- `result` stays `octo/v1`; `Action.ToggleVisibility` is permitted there per the card protocol §2.2.

### D5 — Reports

- Ship `reports/active.interaction.json` and `reports/error.interaction.json` only.
- **Do not ship `reports/result.interaction.json`.** `result` is `octo/v1`; `LoadJSONBundle` reads reports
  only for `octo/v2` views, and a runtime-assembled bundle carrying it would fail `unreferenced`.
- **Declare only data-independent action ids** — `reasoning_toggle` and
  `reasoning_toggle_expanded_action`. Found during Verify: the attachment's reports enumerate
  `reasoning_tools_toggle_action_0/1`, but those ids are generated per phase by `${$index}`, so the
  delivered report is true only for a 2-phase card. It under-declares the `answering` sample (3 phases →
  8 toggles) that the same `active` view serves. Nothing catches this — `assertInteractionReport`
  compares `Action.Submit` and `Input` ids only, and the Bot capability skips non-Submit entries — so an
  indexed id is a published claim no gate verifies. A report that cannot enumerate every instance must
  declare the stable subset instead of an arbitrary two.

### D6 — Exclude the render-profile package

Do not import `render-profile/` (manifest/host-config/theme.css/styles.css/tokens/capabilities/checksums).
No Go code consumes it; `//go:embed all:handoff` would only grow the binary. Its
`capabilities.json` also declares `maxDepth: 24` against this server's authoritative
`cardmsg.MaxDepth = 16`; the divergence is reported to the front-end rather than tracked here.

### D7 — `statusGlyph` stays data-driven (**resolved**)

The attachment hardcodes `•` in the tool rows and no longer binds `${statusGlyph}`. **Bind it again.**

The glyph is the only per-call success/failure marker in the tool list — the producer sends `Attention`
tone with it on a failed call — and freezing it to a lighter `•` weakens the failure signal for no gain.
This is orthogonal to the header simplification (D8): the tool rows are behind a chevron and are the one
place a reader is deliberately inspecting detail. `statusGlyph` therefore stays `required`.

Note the earlier "two render lanes would disagree" concern does **not** apply:
`renderReasoningProcessCard` is dead code in the consumer — no production call site and not exported from
`index.ts`, only its own test references it. No consumer change is implied either way.

### D8 — Header simplification is intentional; `timerText` is not rendered (**resolved**)

The redesigned header deliberately drops the `${timerText}` line to slim the card down. Adopt it: do not
restore the line, and do not restore the two-column header that carried it.

Consequences accepted with this:

- Server-rendered cards lose `12.3s · 3 phases · 9 tool calls` and the `stopped` variant
  `6.1s · stopped at phase 1`. Elapsed time survives in `collapsedSummary`; the phase/tool counts do not.
  They are largely redundant with the phase list the reader can expand.
- The failure reason is **not** lost: `errorTitle`/`errorMessage` are top-level with no `isVisible`
  binding in both `0.3.0` and `0.4.0`, so the consumer's belt-and-braces copy inside `timerText` was
  already redundant.
- `timerText` becomes optional but stays in the schema — see D3 for why deleting it is unsafe.
- This relaxation of `required` is what justifies `contractVersion=1.2.0` in D1.

### D8b — `octo-*` design-system primitives stay out (**resolved**)

The attachment drops every `octo-surface-accent-header-*`, `octo-badge-*-status`, and
`octo-surface-default-footer-*` id, so octo-web's `styles.css` no longer paints an accent header, a pill
status badge, or a footer surface on this card. **This is intentional and is kept.**

- It is pure presentation: no Go code reads these ids, `cardmsg` treats them as ordinary element ids
  subject only to frame-uniqueness, and nothing on the wire or in the schema depends on them.
- `octo-badge-*` is also used by `docs.access-request` V3 (`docs_action_v3.go`), so the reasoning card's
  status will render as plain toned text where the docs card renders a pill. That cross-card difference is
  accepted: the two are different genres (an in-place-updating progress card vs a static action card), and
  a pill is heavier than the simplification is aiming for.
- Because the rendered card is persisted immutably, revisiting this later means a new template version;
  cards sent under `0.4.0` keep this look permanently.

### D9 — Registry and Bot cutover

- Add `HandoffRootV4` / `TemplateVersionV4`; current-version aliases point to V4.
- Register V1→V4 before `Registry.Freeze()`; `SetDefault` to V4.
- Bot `AdvertisedSend` = `{ai.reasoning-process@0.4.0}` only.
- Bot `EditCompatible` = V1, V2, V3, V4. Stored messages stay pinned to their exact version; no
  cross-version edit or migration.
- `botTemplateSwitchFor` must keep covering the advertised set (`TestBotTemplateSwitchCoversAdvertisedSet`).
- RuntimeCatalog learns V4 as a new static claim at startup; no dynamic artifact, grant, or activation row
  is added.

## Load-bearing list

- **L1 immutable artifact identity (`cardtmpl`, `wire-contract`)** — published `0.1.0`/`0.2.0`/`0.3.0` bytes
  cannot change. The redesign requires a new exact version, never an in-place edit of `0.3.0`.
- **L2 bounded producer contract (`cardtmpl`, `trust-boundary`)** — every `maxLength`/`maxItems` value, the
  `phases[].actions` aggregate cap of 13, and fail-close schema classification survive verbatim, with
  `thought`'s single deliberate widening to 4001 (D3a) as the sole exception. The attachment's *unbounded*
  schema must not replace them: widening a ceiling is not the same as removing it, and every field —
  `thought` included — stays bounded on both ends.
- **L3 card node/depth budget (`wire-contract`, `trust-boundary`)** — the richer per-phase markup must stay
  inside `cardmsg.MaxNodes = 200` / `MaxDepth = 16` at the worst case the schema still admits.
- **L4 interaction truth (`cardtmpl`, `wire-contract`)** — templates, reports, goldens,
  `TemplateMeta.ActionContract`, and Bot `submit_actions` must all agree V4 has zero `Action.Submit`.
- **L5 view/state/profile stability (`wire-contract`)** — five states, `active`/`error` = `octo/v2`,
  `result` = `octo/v1`. The consumer's compatibility filter depends on this shape being unchanged.
- **L6 toggle-target integrity (`cardtmpl`, `wire-contract`)** — data-driven `${$index}` toggle ids skip
  `jsontmpl.ValidateToggleTargets`' static check, so `cardmsg.Validate`'s whole-card `resolveTargetRefs`
  becomes the only enforcement of dangling targets and frame-unique ids.
- **L7 Registry multi-version/default (`cardtmpl`)** — four exact versions coexist; only V4 is default;
  `Freeze()` and global exact-key uniqueness unchanged.
- **L8 Bot capability/new-send/edit boundary (`bot-api`, `wire-contract`)** — only V4 discoverable and
  sendable; V1–V3 exact edits still possible without cross-version overwrite or existence probing.
- **L9 derived capability (`bot-api`)** — `submit_actions` and `reasoning_template_ref` stay derived from
  the interaction report and `AdvertisedRef()`; neither becomes manifest- or config-declared.
- **L10 cross-repo producer contract (`wire-contract`, `trust-boundary`)** — the consumer's sanitizer
  ceilings must remain ≤ the server bounds, and its no-version-allowlist selector must still resolve V4.
- **L11 runtime catalog reconciliation (`cardtmpl`, `testing`)** — V4 becomes a new static claim; a
  pre-existing dynamic claim on the same exact key must fail readiness, not select per replica.
- **L12 rollout/rollback (`rollback`)** — an older binary cannot edit a V4 card. V4 is selected by the image
  Registry default only; no static activation pointer may be persisted for it.
- **L13 regression evidence (`test`, `testing`)** — conformance, report drift, goldens, bounds, worst-case
  node budget, Bot policy, historical edits, runtime startup, race, build, vet, lint, diff hygiene.

## Out of scope

- Modifying any file under frozen `ai.reasoning-process@0.1.0`, `@0.2.0`, or `@0.3.0`.
- Any change to `openclaw-channel-octo`. The plugin requires no release for this cutover, and none of the
  resolved decisions (D7/D8/D8b/D3a) implies one — D3a is a relaxation, so the current plugin keeps
  validating unchanged. Raising `THOUGHT_MAX` there to use the new headroom is tracked as the separate
  maintainer-owned follow-up noted in D3a.
- Restoring the `octo-*` primitives or the two-column header (D8b/D8). Revisiting the simplified look is a
  future template version, not this one.
- Hot update: enabling the runtime catalog gates, publishing V4 as a dynamic artifact, persisting an
  activation pointer, or using Activate/Rollback for built-in version selection.
- PR-C scope — `card_template_grant`, grant/revoke APIs, dynamic Bot capability merging, trusted producer
  provenance, B1/B2 discovery/export.
- Making `AdvertisedSend` runtime-resolvable. That moves wire-contract authority from the image to the
  database and needs its own reviewed brief.
- Importing `render-profile/` package files or reconciling its `maxDepth: 24` with `cardmsg.MaxDepth = 16`.
- Relaxing any bound other than `thought` (`phases.maxItems`, `actions.maxItems`, aggregate 13, or any
  other `maxLength` — including the same-shaped `tool: 81` / `detail: 192` / `errorMessage: 121`), or
  raising `cardmsg.MaxNodes`. The `thought` widening is in scope and specified in D3a.
- Raising the consumer's `THOUGHT_MAX` so the widened ceiling is actually used. That is an
  `openclaw-channel-octo` change owned by the maintainer; this task only removes the server-side cap.
- Adding `phaseState` rendering, reintroducing `reasoning_stop`/`reasoning_retry`, or adding any
  `Action.Submit`, RouteSpec, or callback.
- Adding routes, DB migrations, error codes, localized error responses, or rate-limit surfaces.
- Retroactively rewriting delivered V1–V3 payloads; they remain immutable historical messages.
- Enabling production Bot/runtime gates or claiming cross-repo E2E completion.

## Acceptance

### A. Artifact identity and immutability

- [ ] Tracked `ai.reasoning-process@0.4.0` handoff exists with exact `id`/`version`, `owner=ai`,
  `protocol=octo-card@1.0`, `contractVersion=1.2.0`, `renderProfile=octo-chat@1.2.0-rc.2`,
  `adaptiveCardVersion=1.5`, and no `actionType`.
- [ ] `git diff origin/main -- pkg/cardtmpl/ai_reasoning_process/handoff/ai.reasoning-process@0.{1,2,3}.0`
  is empty.
- [ ] No manifest view declares `submit_actions`; no `render-profile/` directory and no
  `reports/result.interaction.json` are added.
- [ ] `phaseState` appears in neither the schema nor any template.

### B. Bounded contract preserved

- [ ] V4 schema retains every `0.3.0` bound and the `x-octo-constraints` aggregate cap of 13; the only
  deltas are `timerText` optionality, `thought`'s widening to 4001, and documentation text. A test asserts
  bound-by-bound equality with the V3 schema rather than comparing whole files.
- [ ] `thought` is `4001` in V4 and still `281` in V3, is a strict widening, and remains bounded on both
  ends (D3a) — so a later edit cannot narrow it back or drop the bound unnoticed.
- [ ] Every string/array/aggregate `limit+1` case is rejected with `ErrFieldsInvalid` before template
  expansion; every exact-limit case renders. The `thought` limit is resolved per version, so V2/V3 keep
  asserting 281 while V4 asserts 4001.
- [ ] The worst case at the widened ceiling (6 phases each carrying a 4001-rune `thought`, 13 aggregate
  actions) renders in all five states and stays inside `cardmsg.MaxPayloadBytes`.
- [ ] `CompileJSONArtifact` succeeds under **both** `staticCompileLimits()` and `DefaultCompileLimits()`
  (the latter proving `RequireOwner` / `RequireProtocol` / `RequireBoundedSchema` are satisfied).

### C. Node budget

- [ ] Worst case admitted by the schema (6 phases, 13 aggregate actions, every string at max) renders
  through `cardmsg.Validate` in all five states.
- [ ] A regression asserts the ceiling: 6 phases / 13 actions renders, and a deliberately over-cap payload
  is rejected — so a future template edit that inflates per-phase markup fails the build rather than
  production.

### D. No unsupported controls

- [ ] V4 templates, reports, and goldens contain no `Action.Submit`, `reasoning_stop`, `reasoning_retry`,
  `stop_reasoning`, `retry_reasoning`, or "可以重试".
- [ ] `TemplateMeta.ActionContract == nil` for V4; V1–V3 contracts unchanged.
- [ ] Every rendered `Action.ToggleVisibility` target resolves to an element id present in the same frame,
  for every state and for phase counts 1…6 (covering the `${$index}` expansion).
- [ ] `active`/`error` reports declare no index-suffixed id, and every id they do declare is present in
  every state that view serves at every phase count 1…6. (Revised during Verify: "exactly match rendered
  actions" is unsatisfiable once toggle ids are `${$index}`-generated — see D5.)
- [ ] `result` has no report document and its `octo/v1` frame still rejects an injected `Action.Submit` at
  `cardmsg.Validate` even with a matching golden (mutation test, per #681).

### E. Registry, Bot, and runtime catalog

- [ ] Registry lists exactly `0.1.0`, `0.2.0`, `0.3.0`, `0.4.0`; `Lookup(id, "")` returns `0.4.0`.
- [ ] `/v1/bot/card/profile` advertises exactly one `ai.reasoning-process` entry at `0.4.0`, three views,
  empty `submit_actions` on all three, and `reasoning_template_ref = {id, 0.4.0}`.
- [ ] New sends at V1–V3 fail before dispatch with zero side effects; exact-version edits at V1–V4 succeed.
- [ ] `TestBotTemplateSwitchCoversAdvertisedSet` still passes.
- [ ] RuntimeCatalog startup reconciles V4 as a static claim; a dynamic claim on the same exact key fails
  readiness. No activation row is created.

### F. Cross-repo consistency

- [ ] A test (or documented check) proves the plugin's compatibility filter still resolves V4: three views
  named `active`/`result`/`error` with wire profiles `octo/v2`/`octo/v1`/`octo/v2`, states as mapped, and
  `submit_actions` empty.
- [ ] Every tool row binds `${statusGlyph}`; no template hardcodes a glyph (D7), and `statusGlyph`
  remains `required`.
- [ ] No template binds `${timerText}`, yet a payload carrying it still validates — proving the property
  survived the `required` relaxation under `additionalProperties: false` (D3/D8).
- [ ] No template declares an `octo-surface-*` or `octo-badge-*` id (D8b), and a rendered card is checked
  to contain none.

### G. Gates

- [ ] Focused suites pass: `pkg/cardtmpl/...`, `modules/bot_api/...`, `modules/card_template_catalog/...`,
  and the composition-root default test in `main_test.go`.
- [ ] Race lanes pass for cardtmpl, catalog, cardmsg, and the focused Bot capability/send/edit paths.
- [ ] `go build ./...`, `go vet ./...`, `golangci-lint run ./...`, and `git diff --check` pass.
- [ ] No `pkg/errcode` / i18n change is expected; if one appears, `make i18n-extract-check` and
  `make i18n-lint` pass.
