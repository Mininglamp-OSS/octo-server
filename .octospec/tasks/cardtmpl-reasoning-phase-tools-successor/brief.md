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
> keeps the `octo-*` design-system primitives out, and D3a gives
> `phases[].thought` two ceilings — accept 4001, display 400 with server-side
> truncation — so an over-long summary renders truncated instead of failing the
> card. None of them requires a consumer change, and because the rendered frame is
> now bounded by the display ceiling regardless of input, no caller can produce a
> frame the persistence layer cannot hold.

## Goal

Publish a new immutable built-in JSON artifact, `ai.reasoning-process@0.4.0`, carrying the front-end's
redesigned presentation:

- per-phase collapsible tool panels (`reasoning_tools_panel_${$index}`), collapsed by default;
- chevron `Image` + `selectAction` toggles replacing the footer `ActionSet` toggle button;
- a simplified header that drops the `${timerText}` line.

Adapt that presentation onto the **existing bounded server data contract** (#667/#681), not onto the
attachment's unbounded schema — with one deliberate change, `phases[].thought` gains a display ceiling of
400 enforced by server-side truncation plus an accept ceiling of 4001 (D3a), so long summaries degrade
instead of erroring. Preserve all five states and the existing state-to-view/wire mapping:

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
| `THOUGHT_MAX = 280` + `…` | `thought: 281` → **accept 4001 / display 400, truncated; see D3a** |
| `TOOL_NAME_MAX = 80` + `…` | `tool: 81` |
| `REASONING_ID_MAX_LENGTH = 512` | `reasoningId: 512` |
| `MAX_RENDERED_PHASES = 6` | `phases.maxItems: 6` |
| `SUMMARY_MAX 64` + `ERROR_MAX 120` joined | `detail: 192` |

Deleting them is not a gap in the handoff; it removes the contract both repos were built against. Every
bound is therefore restored verbatim **except `thought`** (D3a), which stops being a shared constant
altogether: the server accepts up to 4001 and clamps to 400 at render time, so the consumer no longer has
to match a server number at all.

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

D3a does not change that: the accept ceiling rises to 4001 and anything above the 400 display ceiling is
truncated, so nothing the plugin sends at `THOUGHT_MAX = 280` is affected. If the consumer ever raises that
constant it no longer needs to match the server's number — any value up to 4001 is accepted and clamped —
which removes the cross-repo arithmetic that caused two rounds of correction here.

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
  - `phases[].thought` `maxLength` goes from `281` to `400` (relaxation; see D3a);
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

### D3a — `thought` gets two ceilings: accept 4001, display 400 with server-side truncation

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

**The fix is not a bigger number, it is separating two different ceilings** — an accept ceiling and a
display ceiling. Rejecting an over-long summary fails the whole card; truncating it costs the reader some
tail text. For a *display* field the second is obviously right, and it was the requirement: "≤ 400 is
fine, truncate above it server-side, don't error."

| | value | enforced by | behaviour |
| --- | --- | --- | --- |
| **display ceiling** | **400 code points** | `x-octo-constraints.truncateStrings`, applied in `jsonTemplate.Build` after validation | over-long text renders as 399 runes + `…` |
| **accept ceiling** | **4001 code points** | schema `maxLength` | above this, `ErrFieldsInvalid` — truncation is not a licence for unbounded input, and `RequireBoundedSchema` needs a bound |

Measured behaviour (`TestSimplifiedSuccessorTruncatesThought`):

| input | result |
| --- | --- |
| ≤ 400 | rendered untouched, no ellipsis |
| 401 … 4001 | clamped to 399 runes + `…`; **frame size becomes constant** regardless of input length |
| ≥ 4002 | `ErrFieldsInvalid` |

#### Why this is the persistence-safe shape, not just the friendlier one

The binding ceiling for this card is not `cardmsg.MaxPayloadBytes` (512 KiB, the only size gate a rendered
frame passes, `pkg/cardmsg/validate.go:30`). The authoritative bot-edit write stores the whole marshalled
frame in `message_extra.content_edit`, a MySQL **`TEXT` column = 65,535 bytes**
(`modules/message/sql/20220414000001_message_legacy01.sql:3`, never widened by any later migration;
`octo_message_card_revision.content` is `TEXT` too). That width is discovered only at `INSERT` — as
`Data too long for column` under `STRICT_TRANS_TABLES`, or a silent truncation into invalid JSON without it.

Earlier attempts got this wrong twice. First `4001` was set as a plain `maxLength` on the theory that
512 KiB left ~4× headroom; against the real column the frame was 1.56× over with CJK and **2.84× over
(186,024 B)** with every free string escaped. Then `400` was set as a plain `maxLength`, which was safe but
turned every long summary into a failed card.

With a display ceiling the guarantee is stronger than either: **the rendered frame's size no longer depends
on what the caller sends.** Worst case the schema admits (6 phases / 13 aggregate actions / every other
string at max, max across five states), with every free string escaped to 6 bytes per code point:

| `thought` input | rendered frame | share of the 64 KiB column |
| --- | --- | --- |
| 281 (V3's bound) | 52,104 B | 79.5% |
| **400 (display ceiling)** | **56,388 B** | **86.0%** |
| 4001 (accept ceiling) | **56,388 B — truncation makes it identical** | 86.0% |

So a bot cannot produce a frame the store cannot hold, whatever it sends. That is what
`TestSimplifiedSuccessorCeilingIsPersistenceSafe` asserts, and it now feeds the *accept* ceiling in to
prove it — the earlier version could only assert it for the one input length the schema happened to allow.

Note the escaped baseline at `thought = 1` is already **42,024 B, 64% of the column**: 13 actions ×
(`tool` 81 + `detail` 192) is most of the frame. That is why 400 rather than something larger — 654 is the
arithmetic limit for the display ceiling and would leave no headroom, which is the mistake this decision
exists to correct. A display ceiling in the thousands still needs the `MEDIUMTEXT` widening first (own
brief; `message_extra` is a large hot table) and then a `0.5.0`, because `0.4.0`'s bytes are immutable.

`MaxNodes = 200` / `MaxDepth = 16` are *structural* limits — text length adds no nodes, so the node budget
in the section above is unaffected and the aggregate cap of 13 stands unchanged.

#### The engine capability this needs

`x-octo-constraints.truncateStrings` is new in `pkg/cardtmpl` (parsed alongside the existing
`aggregateArrayLimits`, applied in `jsonTemplate.Build` before expansion). Deliberate properties:

- **Opt-in per field.** Anything not named keeps the fail-close behaviour, so this cannot silently soften
  a bound nobody asked to soften. V1–V3 declare nothing and are unchanged.
- **Counted in code points**, matching `maxLength`'s unit — cutting by byte would split a rune.
- **The ellipsis counts toward the ceiling**, so the result is never longer than declared.
- **Compile-time validation** refuses a declaration whose display ceiling exceeds the field's `maxLength`
  (the value would be rejected before truncation could apply), that points at a missing or non-string
  field, whose ellipsis alone would fill the budget, or that carries an unknown key.

#### Consumer impact

None required. The plugin keeps truncating at `THOUGHT_MAX = 280` and nothing changes for it. If it ever
raises that constant it no longer has to match the server's number at all — anything up to 4001 is accepted
and clamped, so the two repos stop being coupled through this bound. That is the actual win over a plain
`maxLength`: the cross-repo arithmetic that caused all of this goes away.

Only `thought` is declared truncatable. `tool: 81`, `detail: 192`, and `errorMessage: 121` keep the
zero-headroom fail-close design and are knowingly left alone — `tool` is the most exposed (MCP tool names
can exceed 80). Extending truncation to them is a follow-up decision, not an unstated part of this change.

### D4a — The chevrons fetch icons from a third-party CDN; accepted here, disclosed and owned

Found in review, and it is the first outbound runtime dependency any built-in template in this repo has
carried, so it is recorded as a decision rather than left as an implementation detail.

**What ships.** All three V4 templates hardcode the chevron icons to a third party — 12 occurrences:

```
"url": "https://api.iconify.design/lucide/chevron-{down,up}.svg?color=%23a1a6ab"
```

It is not a build-time asset reference. The URL expands into all five **persisted goldens** (34
occurrences), i.e. it is card content, stored immutably and shipped to every client. And because
`0.4.0`'s bytes are frozen once published, cards sent under it carry these URLs permanently; changing
this later means a `0.5.0`.

**No precedent, and only a scheme gate.** `@0.1.0`/`@0.2.0`/`@0.3.0` contain zero external runtime URLs —
their only `http(s)` matches are the `$schema` meta-identifiers, which are never fetched. Nothing
structurally prevents this: `cardmsg`'s `checkURL` (`pkg/cardmsg/validate.go:808`) is a positive allowlist
on the **scheme** only (`http`/`https` + non-empty host); it does not constrain the host, and template-
authored URLs are server-authored and therefore trusted by construction — unlike data-supplied URLs such
as `docs.access-request`'s `avatarUrl`, which are additionally pattern-constrained in schema and
preflighted through `cardtmpl.AbsoluteHTTPSURL`.

**Risks accepted.** Availability: `api.iconify.design` is not reliably reachable from mainland China
networks, and this is a `zh-CN`-default enterprise product. Privacy: every viewer of every reasoning card
issues a request to a third party, disclosing client IP, user-agent and timing, and the distinctive
`lucide/chevron-down` path fingerprints *which* card is being viewed. Supply chain: third-party-controlled
SVG bytes render in end-user clients, with the server neither proxying nor pinning them.

**Why it is accepted rather than fixed here.** The severity is bounded by a property of the markup: every
chevron carries `altText` ("展开"/"收起"/"展开工具"/"收起工具") and its `selectAction` is on the `Image`
element itself, not on the fetched resource. So when the icon fails to load, clients render the alt text
and the toggle still works — the per-phase panels stay operable and discoverable, rather than becoming
unreachable. Each alternative also has a real obstacle:

- **Inline SVG data URI** — rejected by `cardmsg.Validate`. `checkURL`'s positive allowlist admits only
  absolute `http`/`https`; `data:` is explicitly refused (`pkg/cardmsg/cardmsg_test.go:142` pins it), and
  that refusal is a load-bearing anti-injection guard, not an oversight to work around.
- **Self-hosted icon endpoint** — needs a static-asset route the server does not have. `BuildEnv`
  (`pkg/cardtmpl/template.go:130`) does give a precedent for injecting a deployment URL into a template
  (`WebLoginURL`, used for deep links), so it is feasible — but adding an asset endpoint plus a new
  `BuildEnv` field is out of scope here.
- **Text affordance, as V1–V3 used** — `selectAction` is not a `TextBlock` property in Adaptive Cards, so
  a reliable version needs each chevron wrapped in a `Container` carrying the action: 2 in the header plus
  2 × 6 per phase = 14 new containers. The node budget already fails at 16 aggregate actions on 6 phases
  (see Background § Node budget), so this trades a network dependency for a structural-limit risk.

**Preconditions and ownership.** Deploying V4 requires confirming that client CSP and egress policy allow
`api.iconify.design`; where they do not, the cards degrade to alt-text toggles rather than breaking, but
that should be a known state, not a discovery. The underlying choice is the front-end's — it came in with
the handoff — so it is reported back to them together with the `render-profile/capabilities.json`
`maxDepth: 24` divergence (D6), with self-hosting or inlining as the preferred long-term shape. Server-side
this stays as-is for `0.4.0`.

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
  `thought`'s accept/display split (D3a) as the sole exception. The attachment's *unbounded* schema must not
  replace them: raising an accept ceiling while clamping the rendered value is not the same as removing a
  bound, and every field — `thought` included — stays bounded on both ends.
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
  raising `cardmsg.MaxNodes`. The `thought` accept/display split is in scope and specified in D3a.
- Widening `message_extra.content_edit` / `octo_message_card_revision.content` to `MEDIUMTEXT`. That is
  what a `thought` ceiling in the thousands would require, and `message_extra` is a large hot table, so
  the `MODIFY COLUMN` rebuild needs its own brief and rollout plan. D3a records it as the precondition for
  a future `0.5.0`, not as work deferred inside this change.
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
  deltas are `timerText` optionality, `thought`'s accept ceiling plus its `truncateStrings` declaration, and
  documentation text. A test asserts bound-by-bound equality with the V3 schema rather than comparing whole
  files, and asserts V3 declares no `truncateStrings` (it stays frozen fail-close).
- [ ] `thought` declares an accept ceiling of 4001 (schema `maxLength`) and a display ceiling of 400
  (`x-octo-constraints.truncateStrings`), V3 is still 281 with no truncation, and the display ceiling is
  asserted to sit under the accept ceiling so the value cannot be rejected before truncation applies.
- [ ] Rendering asserts the three-way behaviour: at or below 400 untouched, 401…4001 clamped to
  399 runes + `…` (never rejected), ≥ 4002 rejected with `ErrFieldsInvalid`. V2/V3 are asserted to still
  reject one rune over their own ceiling.
- [ ] The engine refuses malformed `truncateStrings` declarations at compile time — display ceiling above
  the accept ceiling, missing or non-string target, ellipsis filling the whole budget, non-positive
  `maxRunes`, unknown keys.
- [ ] Every string/array/aggregate `limit+1` case is rejected with `ErrFieldsInvalid` before template
  expansion; every exact-limit case renders. The `thought` limit is resolved per version, so V2/V3 keep
  asserting 281 while V4 asserts 400.
- [ ] The worst case at the new ceiling (6 phases each carrying a 400-rune `thought`, 13 aggregate
  actions) renders in all five states and stays inside `cardmsg.MaxPayloadBytes`.
- [ ] **Contract ≤ storage is enforced by a test, not by prose.** At the schema's own ceiling, with every
  free string driven to its worst-case byte encoding (6 bytes per code point), the rendered frame fits
  `message_extra.content_edit`. The column width is read out of the migration rather than hand-copied, so
  widening it to `MEDIUMTEXT` is observable here instead of leaving the ceiling silently
  over-conservative; and the escape-heavy frame is asserted to exceed the CJK frame, so a future edit
  cannot quietly reintroduce a CJK-only "worst case".
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
- [ ] Every outbound `http(s)` URL in the V4 templates and goldens is either the chevron icon host
  disclosed in D4a or a never-fetched `$schema` identifier — asserted, so a second external dependency
  cannot be added without its own decision. The frozen V1–V3 templates are checked to still carry none.

### G. Gates

- [ ] Focused suites pass: `pkg/cardtmpl/...`, `modules/bot_api/...`, `modules/card_template_catalog/...`,
  and the composition-root default test in `main_test.go`.
- [ ] Race lanes pass for cardtmpl, catalog, cardmsg, and the focused Bot capability/send/edit paths.
- [ ] `go build ./...`, `go vet ./...`, `golangci-lint run ./...`, and `git diff --check` pass.
- [ ] No `pkg/errcode` / i18n change is expected; if one appears, `make i18n-extract-check` and
  `make i18n-lint` pass.
