---
type: Task
title: "Task: cardtmpl-l2a-migration"
description: Migrate the remaining L2a notification cards (docs.shared/commented, summary.completed/failed) onto the cardtmpl Registry using the docs.access-request copy-directory shape, clearing the legacy builders and reaching the ≥3-card L2b gate with both v1/v2 view modes covered.
tags: ["wire-contract", "i18n", "error-response", "escape", "markdown", "url-destination", "trust-boundary", "testing", "commit"]
timestamp: 2026-07-23T09:30:00+08:00
# --- octospec extension fields ---
slug: cardtmpl-l2a-migration
upstream: self (roadmap C)
source: self
---

# Task: cardtmpl-l2a-migration

> Roadmap group C. Migrate the remaining L2a notification cards onto the
> cardtmpl Registry, copying the `docs.access-request` pilot directory shape.
> Prior art: `.octospec/tasks/cardtmpl-registry-pilot/brief.md` (#633),
> `.octospec/tasks/cardtmpl-interaction-closure/brief.md` (#641),
> #647 (renderProfile). Authoritative contract: `docs/platform-card-base.md`.

## Goal

Move the notification cards still built by legacy builders — **docs.shared,
docs.commented, summary.completed, summary.failed** (all `octo/v1` display
cards) — onto the cardtmpl Registry, each as a `pkg/cardtmpl/<card>/` subpackage
with its own embedded `handoff/<id>@<ver>/` and a 3-method Template, exactly like
the `docs.access-request` pilot.

Two payoffs:
1. **Clear the legacy builders**: the corresponding variants in
   `pkg/cardtmpl/{resource,...}.go` and the deliver branches in
   `modules/notify/card.go` route through the single `Registry.Render` entry.
2. **Reach the L2b hard gate** (§2.2-5 gate ②): together with the already-migrated
   `docs.access-request` (v2 interactive), ≥3 L2a cards run the full Registry
   path covering **both** view modes — v1 display (these new cards) and v2
   interactive (docs.access-request).

The external `NotifyReq` shape does not change (plan-B shell unchanged; only the
internal `deliver*` path moves to Registry).

## Background

- The pilot (#633) established the shape: each card lives in
  `pkg/cardtmpl/<card>/` with its own `handoff/<id>@<ver>/`
  (manifest / contract / samples / reports) + Template impl; new cards copy it.
- #647 introduced **renderProfile / renderProfileCompatibility** (dual profile:
  `profile` = message capability, `render_profile` = which HostConfig/CSS
  generation the web should use). **Every new card's manifest must declare
  `renderProfile` (exact Forge artifact) + `renderProfileCompatibility` (stable
  key, e.g. `octo-chat/v1`)**, or it emits no `render_profile` and renders as
  Legacy forever (contract §3; register-time `cardmsg.IsAcceptedRenderProfile`).
- Current state: `docs.access-request` is migrated (v2). `docs.shared/commented`
  and `summary.completed/failed` are still legacy (variants in
  `modules/notify/card.go` + `pkg/cardtmpl/resource.go`).
- **`generic.approval`** (`pkg/cardtmpl/approval_request.go`) takes a **dynamic
  `owner`/`action_type`** from the caller, which conflicts with the Registry's
  **static `TemplateActionContract`** model → see Out of scope; not migrated
  here.

## Load-bearing list

- **L1 directory shape & freeze (`wire-contract`)**: new cards self-embed
  `pkg/cardtmpl/<card>/handoff/<id>@<ver>/`; a published version is frozen
  (§2.1). The published `docs.access-request@0.2.0/0.3.0` trees are not touched.
- **#647 renderProfile declaration (`wire-contract`)**: each new manifest
  declares `renderProfile` + `renderProfileCompatibility`; register-time
  `IsAcceptedRenderProfile` validates (illegal value panics); the send/update
  envelope emits `render_profile` accordingly.
- **notify deliver branches (`wire-contract`)**: `modules/notify/card.go`
  currently routes `access_requested` through the Registry and the rest through
  legacy. Move the `docs.shared/commented` + `summary.*` branches to
  `Registry.Render`; the `NotifyReq` / internal notify token shell is unchanged.
- **C1 zero-delivery (`error-response`)**: schema-level field errors on the
  migrated cards → HTTP 400 zero-delivery (`ErrFieldsInvalid`), no fabricated
  fallback text; preflight moved ahead of member/gate checks (as the pilot F1).
- **Byte-equivalence baseline (`wire-contract`)**: canonical JSON diff before/after
  migration may differ ONLY in `metadata.octo.{protocol,template}` +
  `render_profile` + `Action.OpenUrl.id` (as pilot A11/F4).
- **Render safety (`escape`, `markdown`, `url-destination`)**: caller-controlled
  text goes through `escapeMarkdown`; all URLs absolute https; enforced by L0
  `Registry.Render`, not relaxed.
- **Fallback text**: each new Template's `FallbackText` is the L0 authoritative
  fallback; the master-switch / gate / render-failure degrade paths are unchanged.
- **i18n (`i18n`)**: labels from `BuildEnv.Lang`; reuse the pilot Source
  localization. **G5**: `sanitizeLine` currently duplicated (notify + pilot) —
  extract a shared helper this round (≥3 cards amplify the duplication).
- **Field-cap single source**: **G9** — finalizer/mapping-layer rune caps are
  double-written against each card's schema `maxLength`; unify this round
  (schema is the source of truth, truncate before render).
- **trust-boundary**: caller-supplied business fields on summary/docs cards go
  through the schema allowlist + escaping, never trusted as card JSON.
- **Conformance / tests (`testing`)**: each new card passes register-time
  self-check + the shared conformance suite (interaction-contract equality;
  v1 cards have no interaction report).
- **Git/PR (`commit`)**: English Conventional Commit + English PR + link this brief.

## Out of scope

- **`generic.approval` is NOT migrated**: its dynamic caller-supplied
  `owner`/`action_type` conflicts with the static `TemplateActionContract`;
  needs a separate decision (fixed-owner template vs retained dynamic special
  case) before it can move to the Registry.
- No change to the migrated `docs.access-request` or its frozen versions.
- No `NotifyReq` external-shape change / no envelope mode (E2).
- No `${}` JSON template engine (E1).
- No capability-discovery endpoint (B), L2b channel (D), or unfurl (H).
- No DB migration.
- No new kill switch (rely on register-time fail-close + existing card master switch).

## Acceptance

- `docs.shared`, `docs.commented`, `summary.completed`, `summary.failed` each
  have a `pkg/cardtmpl/<card>/` subpackage + `handoff/<id>@<ver>/`
  (manifest / contract / samples [/ reports]), a 3-method Template, and are
  registered + frozen at the composition root.
- Each new manifest declares `renderProfile` + `renderProfileCompatibility` and
  passes register-time self-check.
- Together with `docs.access-request`, the Registry has **≥3 L2a cards** covering
  **both** v1 (the new display cards) and v2 (docs.access-request) → satisfies
  §2.2-5 gate ②.
- The matching `modules/notify/card.go` deliver branches route through
  `Registry.Render`; the corresponding legacy builder variants become
  fallback-only or are removed as dead code.
- Each card's canonical JSON is byte-equivalent before/after (only
  `metadata.octo` + `render_profile` + `OpenUrl.id` diff allowed).
- C1: a schema field error on a migrated card returns 400 zero-delivery (asserted).
- G5: a single shared `sanitizeLine` helper (notify + cardtmpl reuse one copy).
- G9: field caps have a single source of truth aligned to each card's schema
  `maxLength` (test asserts no double-write drift).
- Green: focused `go test` (`pkg/cardtmpl/...` + `modules/notify`),
  `go test -race -cover` (new card packages ≥80%), `go vet ./...`,
  `golangci-lint run ./...`, `make i18n-extract-check` + `make i18n-lint`,
  the conformance suite; `go test ./...` when MySQL/Redis/WuKongIM are available.
