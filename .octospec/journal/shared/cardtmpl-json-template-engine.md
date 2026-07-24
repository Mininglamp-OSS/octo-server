---
type: Journal
title: "Journal: cardtmpl-json-template-engine (roadmap E1)"
description: Add a second runtime render path to the cardtmpl base — a bounded Adaptive Card Templating engine that compiles JSON handoff templates (`.template.json`) against caller data, so a card can register and render via Registry.Render without a hand-written Go Build(). Validated byte-for-byte against the ai.reasoning-process@0.1.0 goldens. The engine is card-agnostic; the reasoning card's productionization (drop Submit → octo/v1, register, bot delivery) is a separate downstream task.
tags: ["cardtmpl", "platform", "json-template-engine", "adaptive-card-templating", "wire-contract", "trust-boundary", "escape", "testing", "e1"]
timestamp: 2026-07-23T12:58:36Z
# --- octospec extension fields ---
task: cardtmpl-json-template-engine
upstream: self (roadmap E1)
source: self
---

# Journal: cardtmpl-json-template-engine (roadmap E1)

## What shipped

A JSON-template render path for `pkg/cardtmpl`, additive and parallel to the
existing hand-written Go `Template.Build()` cards (the 5 L2a cards are untouched).

- **`pkg/cardtmpl/jsontmpl/`** — a dependency-free evaluator for the bounded ACT
  subset the shipped handoff templates actually use: single-identifier field
  binding, `$index`, `if($index == N, a, b)`, string interpolation, typed
  whole-value binding (`"${bool}"` → real JSON bool), and `$data` array
  repetition. Anything outside the subset is a hard error at parse/expand, never
  a silent passthrough (D4: freeze the subset, extend via L0 PR). `expr.go` +
  `expand.go` + `toggle.go` (static ToggleVisibility-target existence check).
- **`pkg/cardtmpl/json_template.go`** — a generic `jsonTemplate` implementing
  `Template` + `metaSetter`, with per-view template ASTs parsed once at register
  time (no per-frame JSON parse — progress cards re-render often). `RegisterJSON`
  loads `manifest.views[].template`, validates toggle targets, and delegates to
  the existing `Register` so JSON cards get identical fail-close guarantees.
  `FallbackText` reuses `cardmsg.BuildPlain`.
- **`render.go` (D7)** — `BuildResult.DeepLink` is now optional: empty skips the
  absolute-https check and omits `metadata.webUrl`. Cards that ship a deep link
  behave exactly as before (existing docs/summary conformance stays green).
- **`registry.go`** — extracted a named `manifestView` type and parses the
  previously-dropped `template` / `samples` view fields.

## Structural learnings / gotchas

- **The goldens bind data literally — no markdown escaping (D6).** The authoring
  tool substitutes `${...}` verbatim (`run_sql`, `funnel_definition.sql` keep raw
  underscores). Escaping in the engine would break byte-equivalence. The trust
  boundary is not markdown-escaping; it is `cardmsg.Validate`, whose positive URL
  allowlist covers TextBlock markdown links (`cardmsg.go`) plus the element
  whitelist and node/size caps. Proven: a caller-injected `[x](javascript:…)` in
  literal-bound data is rejected → `ErrRenderFailed`. The engine keeps an
  injected `EscapeFunc` seam (identity by default) so a future card can opt in.
- **Conformance lives at the expander level, not renderCore.** Goldens are the
  full pre-metadata card (`{$schema,type,version,body}`); `renderCore` adds
  `metadata` and drops `$schema`. So the byte oracle compares
  `jsontmpl.Expand(template,sample)` to the golden (canonical JSON, sorted keys —
  key order is not semantic and Go maps are unordered). Registry integration is
  proven separately with a minimal registrable v1 fixture.
- **The as-delivered reasoning handoff does not register as-is** (diagnostic, not
  a gate): its `reports/*.interaction.json` are named by **state**
  (`reasoning`/`answering`/…) while the Registry convention is by **view**
  (`active.interaction.json`). Dropping Submit → octo/v1 (the product decision)
  removes the need for reports entirely, dissolving the mismatch. This is the
  downstream "translate the reasoning card" task, kept out of scope here.

## Out of scope (→ downstream tasks)

Translating `ai.reasoning-process` into a live product card (drop Submit →
octo/v1, re-issue handoff + regenerate goldens, register a subpackage, wire
"wave A" bot delivery), the capability-discovery endpoint additions
(`/v1/bot/card/profile` templating advertise), multi-locale template selection,
and migrating the 5 Go cards to JSON.
