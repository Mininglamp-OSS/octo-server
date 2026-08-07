---
type: Journal
title: "Journal: cardtmpl-reasoning-phase-tools-successor"
description: Published ai.reasoning-process@0.4.0 — the front-end's per-phase collapsible tool panels and simplified header, adapted onto the bounded #667/#681 data contract rather than the handoff's unbounded schema. Registry default and Bot new-send cut to 0.4.0 via image release; 0.1.0-0.3.0 stay frozen and exact-version editable. thought's ceiling was deliberately widened 281 to 4001, breaking its old habit of mirroring the producer's truncation length.
tags: ["card", "cardtmpl", "ai-reasoning-process", "json-template", "bot-api", "wire-contract", "trust-boundary", "test", "testing", "rollback"]
timestamp: 2026-08-07T12:00:00Z
# --- octospec extension fields ---
task: cardtmpl-reasoning-phase-tools-successor
upstream: "front-end handoff `ai.reasoningprocess0.3.0.handoff_1.zip`; predecessors PR #657 (0.1.0) / #667 (0.2.0) / #681 (0.3.0) / #675 (RuntimeCatalog overlay)"
source: self
---
# Journal: cardtmpl-reasoning-phase-tools-successor

## What was done

A new immutable built-in artifact `ai.reasoning-process@0.4.0` carries the front-end's
redesigned presentation — per-phase collapsible tool panels
(`reasoning_tools_panel_${$index}`, collapsed by default), chevron `Image` +
`selectAction` toggles replacing the footer `ActionSet`, and a header that drops the
`${timerText}` line. `Registry.SetDefault` and Bot `AdvertisedSend` moved to `0.4.0`;
`EditCompatible` covers V1–V4 so stored messages keep rendering under their exact pinned
version. `0.1.0`/`0.2.0`/`0.3.0` are byte-for-byte untouched.

The handoff declared itself `version: 0.3.0`, colliding with the already-live `0.3.0`
from #681. Publishing in place would have re-rendered delivered historical cards with
different content, so the delta landed as a new exact version instead.

Three defects in the handoff were corrected rather than adopted: per-view
`submit_actions` in the manifest (it is *derived* here from the interaction report —
declaring it reintroduces the hand-maintained list #681 refused), missing
`owner`/`protocol`, and a schema stripped of every `maxLength`/`maxItems`/
`x-octo-constraints`. The `render-profile/` package was left out (no Go consumer, and its
`capabilities.json` claims `maxDepth: 24` against this server's authoritative
`cardmsg.MaxDepth = 16`).

## Load-bearing decisions

- **New exact version, never an in-place edit** (L1). Rendered cards are persisted
  immutably and Bot edits resolve by exact version, so the one-permanent-source-per-key
  invariant makes a redesign under an existing version unrepresentable.
- **Bounded contract restored, not the handoff's unbounded schema** (L2, trust-boundary).
  Every `maxLength`, `phases.maxItems: 6`, `actions.maxItems: 12`, and the
  `x-octo-constraints` aggregate cap of 13 came back verbatim — with one deliberate
  exception, `thought` (below). Verified by a bound-by-bound comparison against the frozen
  V3 schema rather than a file diff, since the two legitimately differ in `required`,
  descriptions, and examples.
- **`thought` 281 → 4001** (D3a). The old value existed only because it mirrored the
  producer's observed output (`THOUGHT_MAX = 280` + `…`); see the learning below. It stays
  bounded on both ends — an unbounded string fails
  `DefaultCompileLimits().RequireBoundedSchema`, and widening a ceiling is not removing
  it. Measured on the `0.4.0` template at the worst case the schema admits (6 phases / 13
  aggregate actions / every other string at max, max across five states): 35,076 B (6.69%
  of `cardmsg.MaxPayloadBytes`) at 281 → 102,036 B (19.46%) at 4001. `MaxNodes` /
  `MaxDepth` are *structural*, so longer text costs no nodes and the aggregate cap of 13
  was untouched.
- **`timerText` optional but retained in `properties`** (D3/D8). No template binds it, but
  the root is `additionalProperties: false` and the producer sends it unconditionally, so
  deleting the property would have rejected every payload the current plugin emits.
- **`${statusGlyph}` rebound** (D7). The handoff hardcoded `•`; the glyph is the only
  per-call success/failure marker in the tool rows (producer sends `Attention` tone with
  it on a failed call). Its `maxLength: 2` is therefore load-bearing again — the value now
  lands in TextBlock text, and 2 runes is too short to forge markdown.
- **Server-release cutover, not a hot update** (L12). Bot advertisement is a compile-time
  constant resolved by exact version, so a runtime activation pointer cannot change what
  the plugin sees; the runbook also forbids Activate/Rollback for built-in version
  selection by name for this template (an orphan pointer takes replicas out of readiness
  on binary rollback).

## Gotchas worth remembering

- **An interaction report cannot honestly declare a data-generated action id.** The
  handoff's reports enumerated `reasoning_tools_toggle_action_0/1`, but those ids are
  produced per phase by `${$index}` — the report was true only for a 2-phase card and
  under-declared the 3-phase `answering` sample the same `active` view serves. Nothing
  catches this: `assertInteractionReport` compares `Action.Submit` and `Input` ids only,
  and the Bot capability skips non-Submit entries, so an indexed id is a published claim
  no gate verifies. Fixed by declaring only the data-independent subset
  (`reasoning_toggle`, `reasoning_toggle_expanded_action`).
- **`${$index}`-generated toggle ids bypass the static target check** (L6).
  `jsontmpl.ValidateToggleTargets` can only see literal ids, so `cardmsg.Validate`'s
  whole-card `resolveTargetRefs` becomes the *only* enforcement of dangling targets and
  frame-unique ids. Covered by rendering phase counts 1…6 across all five states;
  `phases.minItems: 1` rules out the empty case.
- **`selfCheckCompiledJSON` runs before `checkArtifactGoldens`.** That ordering is what
  makes "a matching golden cannot launder a forged control" true: injecting an
  `Action.Submit` into the `octo/v1` `result` template *and* updating both result goldens
  to match still fails at the wire-profile check, because the golden gate is never
  reached. This was asserted for the generic compiler path but not for this artifact;
  a V4-specific mutation test now covers it.
- **Shared cross-version test constants become an obstacle the moment one version
  diverges.** `worstCasePhases()` hardcoded 281 and the bounds table held one `limit` per
  field for all bounded versions, so widening V4 alone required a `perVersion` seam and a
  `reasoningThoughtMax(version)` helper. Worth building that seam when a bound is first
  pinned to an external value, not when it first diverges.
- **`thought`'s bound was only ever exercised with ASCII.** The bounds table rotates
  `{"x", "界", "😀"}` by field index, and `thought` happened to land on `"x"`. The
  code-point (not byte, not UTF-16 unit) counting semantics *are* covered — by `title`
  (CJK) and `statusLabel` (emoji) — but the field whose production content is Chinese was
  not one of them.

## Verification

- Focused suites green: `pkg/cardtmpl/...` (8 packages), `modules/bot_api/`,
  `modules/card_template_catalog/`, and the composition-root default test in
  `main_test.go`.
- Race lanes green: `pkg/cardtmpl/...` + `pkg/cardmsg`, `modules/card_template_catalog/`,
  and the focused Bot capability/send/edit paths.
- `go build ./...`, `go vet ./...`, `gofmt` on every changed file,
  `golangci-lint run ./...` (2.12.2, CI's version) = **0 issues**, `git diff --check`.
- `make i18n-extract-check` + `make i18n-lint` green — no `pkg/errcode` / `pkg/i18n`
  change was needed or made.
- `CompileJSONArtifact` succeeds under **both** `staticCompileLimits()` and
  `DefaultCompileLimits()`, the latter proving `RequireOwner` / `RequireProtocol` /
  `RequireBoundedSchema` are satisfied on the runtime-publish path that static
  registration skips.
- Empirically confirmed (throwaway probe, not committed) that `maxLength` counts Unicode
  **code points**: 281 CJK runes = 843 bytes passes, 281 BMP-outside emoji = 562 UTF-16
  units passes, 282 of either is rejected — while 280 `é` (2 cp each) and 280 `👨‍👩‍👧`
  (5 cp each) are rejected outright.

## Follow-ups / notes

- **Consumer change is optional and maintainer-owned.** Widening `thought` cannot reject
  anything the plugin sends today, so no plugin release is required; the new headroom
  simply goes unused until `openclaw-channel-octo` raises `THOUGHT_MAX` to 4000 (keeping
  the `+…` at exactly 4001). Server-first is the safe order and is the order taken. Two
  hazards for that change: switching to grapheme-aware truncation would reintroduce the
  overflow (one grapheme can be many code points), and changing the ellipsis from `…`
  (1 cp) to `...` (3 cp) would break the arithmetic.
- **`tool: 81`, `detail: 192`, `errorMessage: 121` still carry the zero-headroom design**
  and were knowingly left alone. `tool` is the most exposed — MCP tool names can exceed
  80 characters. Recorded in D3a and Out of scope so it reads as a decision, not an
  oversight.
- `contractVersion` stayed at `1.2.0` rather than bumping to `1.3.0`: `1.2.0` has never
  shipped, so both relaxations (`timerText` `required`, `thought` ceiling) belong to the
  same first-published delta. Bumping would invent a version history.
- The `octo-surface-*` / `octo-badge-*` primitives stay out (D8b), so this card renders
  status as plain toned text where `docs.access-request` V3 renders a pill. Accepted as a
  genre difference; revisiting it means a new template version, since sent cards are
  persisted immutably.
- The handoff's `render-profile/capabilities.json` claims `maxDepth: 24` against
  `cardmsg.MaxDepth = 16`. Reported to the front-end rather than reconciled here.
- Runtime catalog reconciles V4 as a new *static* claim; no dynamic artifact, grant, or
  activation row was created. The "a dynamic claim on the same exact key must fail
  readiness" invariant is covered by version-agnostic tests
  (`TestInitializeRuntimeCatalogFailsClosedOnInvalidActiveTarget`,
  `TestAPIStartPoisonsReadinessOnPersistentIntegrityFailure`) rather than a V4-specific
  assertion — the mechanism does not depend on the version string.
