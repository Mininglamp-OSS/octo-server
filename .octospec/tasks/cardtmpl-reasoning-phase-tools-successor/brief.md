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
> card. None of them requires a consumer change. The rendered frame is bounded by
> the display ceiling regardless of input, but that alone does **not** bound the
> frame against the store: `thought` is not the dominant term (see D3a), so the
> storage budget is enforced at the write boundary instead.

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
  - `phases[].thought` gains two ceilings: `maxLength` (the *accept* ceiling) goes from `281` to `4001`,
    and a narrower display ceiling of `400` is declared in `x-octo-constraints.truncateStrings` and applied
    by the engine after validation (relaxation; see D3a);
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

#### Where the storage budget is actually enforced

**This subsection previously derived `400` from a measurement of the wrong bytes. Both the number and the
reasoning are corrected here; the correction was found in review (PR#712) and reproduced independently.**

The binding ceiling for this card is not `cardmsg.MaxPayloadBytes` (512 KiB, the only size gate a rendered
frame passes, `pkg/cardmsg/validate.go:30`). The authoritative bot-edit write stores the frame in
`message_extra.content_edit`, a MySQL **`TEXT` column = 65,535 bytes**
(`modules/message/sql/20220414000001_message_legacy01.sql:3`, never widened by any later migration;
`octo_message_card_revision.content` is `TEXT` too). That width is discovered only at `INSERT` — as
`Data too long for column` under `STRICT_TRANS_TABLES`, or a silent truncation into invalid JSON without it.

**The error was measuring `json.Marshal` of what `Render` returns.** That is the *pre-*`Finalize` envelope.
`cardmsg.Finalize` (`pkg/cardmsg/plain.go:30`) then adds a top-level `plain` built from every visible text
node, and *that* value is what `NormalizeContentEdit` canonicalizes and what gets written. `plain` inflates
this card's frame by **+47%**. Every byte figure in the earlier version of this subsection was therefore
low by roughly a third.

Re-measured on the bytes that are actually persisted (worst shape the schema admits — 6 phases / 13
aggregate actions / every other string at max — max across all five states):

| version | `thought` | encoding | persisted | share of the 64 KiB column |
| --- | --- | --- | --- | --- |
| `0.3.0` (live, frozen) | 281 (its own bound) | escaped | 79,678 B | **121.6%** |
| `0.4.0` | 400 (display ceiling) | CJK | 60,658 B | 92.6% |
| `0.4.0` | 400 | escaped | 98,998 B | **151.1%** |
| `0.4.0` | **1** | escaped | 70,270 B | **107.2%** |
| `0.4.0` | 400, `tool`/`detail` at 1 | escaped | 56,722 B | 86.6% |

**Two conclusions follow, and both contradict the earlier version of this decision.**

First, **`thought` is not the dominant term and never was.** At `thought = 1` the frame is still 107% of the
column; drop `tool`/`detail` to one code point instead and it falls to 87%. The frame is dominated by
13 actions × (`tool` 81 + `detail` 192) plus the `plain` copy. No value of the `thought` display ceiling
makes the worst case fit, so `400` cannot be — and is not — derived from the storage budget.

Second, **`0.3.0` is already over.** At its own bound, in the same encoding, the live version persists at
121.6%. This is pre-existing platform debt, it is not introduced by `0.4.0`, and it is unfixable in place
because `0.3.0`'s bytes are frozen. Any fix that lives inside one template version therefore leaves the
installed base exposed.

So the budget is enforced where it applies to every version at once: **a column-width check at the
persistence boundary.** `carddispatch.NormalizeFrameForPersistence` is now the single judge of "can this
frame be stored", and all three write paths that re-validate a frame go through it —
`CardMutator.Mutate`, `CardUpdater.Append`, `CardUpdater.ReplaceView`. Over-width frames get
`ErrCardMutationTooLarge` (which wraps `ErrCardMutationInvalid`, so existing error mapping is unchanged)
carrying the byte count, logged with the message and sender. What this buys:

- it covers `0.1.0`–`0.4.0` and every future template, including ones nobody has written yet;
- it costs nothing for realistic traffic — the five shipped samples persist at 10,310 / 10,724 / 12,069 /
  16,233 / 18,259 B, i.e. **15.7%–27.9%** of the column (measured through the production gate; an earlier
  revision of this line said "7–12 KB, under 20%", which was wrong for two of the five states — review P2-1);
- `Data too long` / silent truncation into a frame no client can render becomes a deterministic, typed,
  logged refusal. The card still cannot advance, but the failure names itself instead of surfacing as a 500.

`400` is therefore a **product decision** — the requirement was "≤ 400 is fine, truncate above it
server-side, don't error" — and the truncation engine's real contribution is that **frame size stops
depending on caller input** (400 and 4001 produce identical bytes). It is not a storage-budget derivation.
`TestSimplifiedSuccessorPersistedFrameIsMeasuredAndGated` asserts the corrected shape: that the persisted
encoding exceeds the rendered one (so nobody measures the wrong artifact again), that shipped samples keep
margin, and that the adversarial worst case is refused by the production gate rather than by MySQL.

**Open, and deliberately not fixed here.** A frame with every field legitimately filled to its schema
maximum in CJK persists at **92.6%** — inside the column, but with under 5 KB of margin, and an escape-heavy
mix (a `detail` carrying SQL or code, e.g. `a < 5 AND b > 3`) can cross it. The gate turns that into a clean
refusal rather than corruption, but the card still freezes. The durable fixes are widening `content_edit`
to `MEDIUMTEXT` (`TEXT`→`MEDIUMTEXT` needs `ALGORITHM=COPY, LOCK=SHARED` — measured; `message_extra` is a
large hot table, so this needs its own brief and a maintenance window) or extending `truncateStrings` to
`phases[].actions[].tool` / `.detail`, which is a visual change to how tool calls are displayed and so a
product decision rather than an engineering one. **Note for whoever plans that second option** (review P2-4):
`applyStringTruncations` cannot express it as written — `ArrayField` scopes `Field` to objects inside one
*top-level* array, and `tool`/`detail` live one level deeper, in `phases[].actions[]`. So the fields that
dominate the frame are exactly the ones the current mechanism structurally cannot clamp; that option needs an
engine change (nested path support) before it needs a product decision. Both are out of scope for `0.4.0`; neither is blocked by it.

One more thing whoever picks that up will be triaging (review P2-5): both new gates collapse "too large"
into `ErrBotAPICardInvalid`, deliberately, because a new public error code is out of scope here
(`ErrCardMutationTooLarge` wraps `ErrCardMutationInvalid` so the existing mapping absorbs it). The
observable consequence is that a producer hitting the column gets a generic "invalid card" with no hint
that shortening would help — the byte count exists only in the server log. If the column is widened or
`tool`/`detail` gain display ceilings, a distinguishable code for this case belongs in the same change.

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

### D4a — The chevrons are inlined as vetted `data:image/svg+xml` bytes, not fetched

**This decision reversed during review (PR#712). The earlier version accepted a third-party CDN and listed
inlining as *rejected*; the reverse shipped. What follows describes the code.**

The chevron icons were originally authored — in the front-end handoff — as fetches from `api.iconify.design`
(12 occurrences across the three V4 templates, expanding to 34 in the persisted goldens). That made them the
first outbound runtime dependency any built-in template in this repo carries: `@0.1.0`–`@0.3.0` contain
zero, their only `http(s)` strings being `$schema` meta-identifiers that are never fetched. And because a
published version's bytes are frozen, cards sent under `0.4.0` would have carried those URLs permanently.

Three things made that unacceptable rather than merely noted: `api.iconify.design` is not reliably reachable
from mainland China networks and this is a `zh-CN`-default enterprise product; every viewer of every
reasoning card would disclose IP, user-agent and timing to a third party, with the distinctive
`lucide/chevron-down` path fingerprinting *which* card is open; and third-party-controlled SVG bytes would
render in end-user clients with the server neither proxying nor pinning them.

**What ships instead.** The icon bytes are embedded in the templates as `data:image/svg+xml,` URIs
(345/349 B each, Lucide `chevron-down`/`chevron-up` at `#a1a6ab`). No outbound request, nothing to reach,
nothing to pin. `TestSimplifiedSuccessorHasNoOutboundRuntimeDependency` asserts every `http(s)` string in
the V4 templates and goldens is either the SVG namespace identifier or an unfetched `$schema`.

**The gate this needed, and the shape it settled on.** `cardmsg`'s `checkURL` is a positive allowlist that
admits only absolute `http`/`https`, so `data:` was refused — correctly, since an SVG is a classic XSS
carrier and the server cannot know whether a client renders it via `<img src>` (sandboxed, scripts inert) or
inlines it into the DOM (scripts execute). It must assume the latter.

The first implementation opened a *trusted-caller* mode: a `ValidateOption` the render path passed and other
callers did not, plus a substring denylist over the SVG content. Review broke both halves, and both breaks
were reproduced before being accepted:

- **The trust boundary was one-sided.** The option was applied at the render call site but not at the three
  places that re-validate the same frame before persisting it (`carddispatch` mutation, `CardUpdater.Append`,
  `CardUpdater.ReplaceView`). Since a reasoning card advances by repeated edits, every `0.4.0` card would
  have rendered its first frame and then frozen — the exact failure mode D3a warns about, reached from the
  other side. Tests missed it because the bot-edit tests inject a fake mutator that never re-validates.
- **The denylist was porous.** Five accepted bypasses: namespace-prefixed elements (`<s:script>`, `<s:use>`),
  CSS identifier escapes (`fill:\75 rl(...)`, `@\69 mport`), and the SVG 1.2 Tiny `<handler>` element. A
  substring denylist over two independently escapable syntaxes (XML and CSS) is structurally leaky.

So the mechanism changed rather than being patched: **an exact-byte allowlist** (`pkg/cardmsg/inline_image.go`).
Only the specific vetted byte strings pass — no decoding, no parsing, no filtering. This is smaller than what
it replaced and closes four things at once:

- Validation no longer depends on the call site, so no write path can disagree with the render that produced
  the frame. The P0 above is structurally impossible, not merely fixed.
- The sanitizer's holes stop being reachable: arbitrary SVG never matches, so the five bypasses and any
  sixth are all equally excluded.
- A template's `Image.url` must be a literal — an interpolated `${field}` cannot equal a constant — so a
  bot-supplied data field can never become a `data:` sink. No separate compile-time rule is needed.
- Adding an icon is an explicit source change to `cardmsg`, which puts a human in front of exactly the bytes
  that would be inlined into every client's DOM.

The cost is that template authors cannot freely use icons. That is intended.

**What did not change.** `data:` remains refused everywhere else: on non-image URL fields (`Action.OpenUrl.url`,
`iconUrl`, `backgroundImage`) even for vetted bytes, and on any unvetted bytes anywhere. `TestValidateURLAllowlist`
is untouched.

**What did widen, stated rather than denied** (review P2-5 — an earlier version of this section claimed raw bot
cards, incoming-webhook adapters and message edits were "unaffected", which is not accurate). Previously
`checkImageURL(u, false)` refused *every* `data:` URI for those callers; now `checkImageURL(u)` admits the two
vetted chevrons for **every** caller, because the allowance is a property of the bytes rather than of the caller.
That is the whole point of the redesign, and it cuts both ways. The widening is two fixed byte strings — a Lucide
chevron of two `<path>` elements, no script, no external reference, no interpolation point, matched by full-string
equality with no normalization — so a bot embedding that exact icon in a raw card is harmless. It is still a
widening of an anti-injection allowlist, and it is flagged for the human security reviewer, because the remaining
question is client-side and the server cannot answer it: `<img src>` sandboxes an SVG while inlining it into the
DOM does not, and the server cannot tell which a given client does.

**Not adopted.** A self-hosted icon endpoint (`OCTO_CARD_ASSET_BASE_URL` + a static-asset route) remains the
right shape if icons ever outgrow an allowlist; it needs a `BuildEnv` field and a route the server does not
have, and is deferred to its own brief. Reverting to a text affordance as V1–V3 used is not viable:
`selectAction` is not a `TextBlock` property, so each chevron needs a wrapping `Container` — 14 of them —
and the node budget already fails at 16 aggregate actions on 6 phases (Background § Node budget).

**Reported back.** The handoff's CDN choice is the front-end's, so the reversal is reported to them together
with the `render-profile/capabilities.json` `maxDepth: 24` divergence (D6). One consequence worth their
attention: `plain` (which feeds push previews and search) does not consider `isVisible`, and now renders six
`[图片]` placeholder lines for the chevrons. The collapsed *content* was already fully disclosed in `0.3.0`'s
`plain`, so that part is not a regression — but the placeholder noise is new, and suppressing `Image` nodes
in `BuildPlain` would change previews for every card type, so it is filed rather than fixed here.

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
- [ ] The worst case at the display ceiling (6 phases each carrying a 400-rune `thought`, 13 aggregate
  actions) renders in all five states and stays inside `cardmsg.MaxPayloadBytes`.
- [ ] **The storage budget is enforced by a test on the bytes that are actually persisted, not by prose.**
  The measurement is taken through the production check (`carddispatch.NormalizeFrameForPersistence`), so it
  includes the `plain` field `Finalize` appends — measuring the render output instead understates it by
  roughly a third, which is the specific mistake an earlier revision of this brief made. Asserted: the
  persisted encoding strictly exceeds the rendered one (so the wrong artifact cannot be measured again);
  shipped samples keep margin; and the adversarial escape-heavy frame at the accept ceiling is **refused by
  the persistence gate with a typed error and a byte count** rather than reaching MySQL. It is *not*
  asserted to fit — it does not, and neither does frozen `0.3.0` at its own bound (D3a). The column width
  is read out of the migration rather than hand-copied, in both directions.
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
- [ ] V4 carries **no** outbound runtime dependency: every `http(s)` string in its templates and goldens is
  either the SVG namespace identifier inside an inline icon or a never-fetched `$schema` identifier (D4a) —
  asserted, so an external dependency cannot be introduced without its own decision. The frozen V1–V3
  templates are checked to still carry none.
- [ ] The inline icon bytes are in `cardmsg`'s vetted allowlist, so a rendered V4 frame survives the
  **production persistence check** (`carddispatch.NormalizeFrameForPersistence`) and not merely the render
  path — a render/persist disagreement would send the first frame and freeze every edit after it (D4a).
- [ ] Unvetted `data:` bytes are refused on image fields, and vetted bytes are refused on non-image URL
  fields (`Action.OpenUrl.url`, `iconUrl`, `backgroundImage`); `TestValidateURLAllowlist` is unchanged.
- [ ] The persisted frame — `NormalizeContentEdit` output, i.e. **including** the `plain` that `Finalize`
  adds — is what the storage assertions measure; shipped samples keep margin against the 64 KiB column, and
  the adversarial worst case is refused by the persistence gate rather than by MySQL (D3a).

### G. Gates

- [ ] Focused suites pass: `pkg/cardtmpl/...`, `modules/bot_api/...`, `modules/card_template_catalog/...`,
  and the composition-root default test in `main_test.go`.
- [ ] Race lanes pass for cardtmpl, catalog, cardmsg, and the focused Bot capability/send/edit paths.
- [ ] `go build ./...`, `go vet ./...`, `golangci-lint run ./...`, and `git diff --check` pass.
- [ ] No `pkg/errcode` / i18n change is expected; if one appears, `make i18n-extract-check` and
  `make i18n-lint` pass.
