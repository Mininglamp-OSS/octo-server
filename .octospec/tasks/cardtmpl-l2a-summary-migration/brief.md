---
type: Task
title: "Task: cardtmpl-l2a-summary-migration"
description: Roadmap C PR-2 — migrate the remaining two legacy L2a summary cards (summary.completed, summary.failed) onto the cardtmpl Registry using the docs-notify pilot copy-directory shape. Stacked on PR-1 (#649). External NotifyReq shape unchanged (plan-B shell).
tags: ["wire-contract", "i18n", "error-response", "escape", "markdown", "url-destination", "trust-boundary", "testing", "commit"]
timestamp: 2026-07-23T14:00:00+08:00
# --- octospec extension fields ---
slug: cardtmpl-l2a-summary-migration
upstream: self (roadmap C, follows PR-1 #649)
source: self
---

# Task: cardtmpl-l2a-summary-migration

> Roadmap C PR-2. Same pattern as PR-1 (#649, `cardtmpl-l2a-migration`) applied
> to the summary-notify cards. Prior art: PR-1 brief
> `.octospec/tasks/cardtmpl-l2a-migration/brief.md`.

## Goal

Move `summary.completed` and `summary.failed` (the two remaining legacy L2a
display cards routed by `modules/notify/card.go.deliverCardNotification`) onto
`Registry.Render`, each as a `pkg/cardtmpl/<card>/` subpackage with its own
embedded `handoff/<id>@<ver>/` and 3-method Template — copy of the pilot shape
established by `docs.access-request` and reused by `docs.commented` /
`docs.shared` in PR-1.

After this PR the four legacy `deliver*` display branches
(`docs.commented`, `docs.shared`, `summary.completed`, `summary.failed`) are
all Registry-backed; `docs.access-request` v2 is the only non-legacy interactive
card. Roadmap C's L2a migration is complete except for `generic.approval`
(dynamic owner conflict, tracked separately in the parent brief).

## Background

- **`BuildSummaryResourceCardBodyWithLang`** was already added in PR-1 as a
  fragment-only helper that shares `assembleResourceCardBody` with the legacy
  `BuildSummaryResourceCard`. Body-level byte-equivalence is therefore
  infrastructural — PR-2's Go footprint is per-card i18n shim + fixture mapping,
  matching the docs display cards.
- Legacy summary card carries **facts** (time range / members / message count /
  generated at) not present on the docs display cards, plus a **completed vs
  failed** binary in `SummaryCardFields.Kind`. Modeling choice: one
  subpackage per variant (`summary_completed`, `summary_failed`) mirroring the
  L2a-per-variant pattern PR-1 established, rather than a single
  Kind-multiplexing Template. Trade-off: two schemas + two Templates but
  register-time freeze is uniform and each `Variant`/`FallbackText` stays
  simple.
- `SummaryCardFields.Reason` is only meaningful on failed; the completed schema
  omits it entirely, the failed schema requires it (schema is the enforcement
  point for the `Kind`-conditional shape).
- **F7 policy** (PR-1 established): Registry unwired = composition bug = 500,
  no legacy fallback in the migrated deliver path.
- **C1 policy**: schema field errors → 400 zero-delivery via preflight before
  member/gate checks.
- **#647 renderProfile**: each new manifest declares
  `renderProfile: octo-chat@1.2.0-rc.1` + `renderProfileCompatibility: octo-chat/v1`
  (same values PR-1 used, tracked by `cardmsg.IsAcceptedRenderProfile`).
- **G5 SanitizeLine** and the shared body helper are already in place from
  PR-1. This PR reuses them; no new base changes.

## Load-bearing list

- **L1 directory shape & freeze (`wire-contract`)**: two new subpackages
  `pkg/cardtmpl/summary_completed/handoff/summary.completed@0.1.0` and
  `pkg/cardtmpl/summary_failed/handoff/summary.failed@0.1.0` each with
  `manifest.json` + `contract/data.schema.json` + `samples/shown.json`,
  self-embedded via `//go:embed all:handoff` and frozen at register time.
- **#647 renderProfile declaration (`wire-contract`)**: both manifests declare
  `renderProfile: octo-chat@1.2.0-rc.1` + `renderProfileCompatibility: octo-chat/v1`;
  register-time `IsAcceptedRenderProfile` validates (illegal value panics).
- **notify deliver branch (`wire-contract`)**: `deliverCardNotification` moves
  the `Kind == completed|failed` branch onto `Registry.Render` via a new
  `buildSummaryCardViaRegistry` (mirrors `buildDocsDisplayCardViaRegistry`).
  `NotifyReq` / `SummaryCardFields` external shape unchanged.
- **C1 zero-delivery (`error-response`)**: schema-level field errors on the
  migrated summary cards → 400 zero-delivery via `preflightSummarySchema`
  ahead of member/gate checks, identical policy to PR-1 docs display cards.
- **Byte-equivalence baseline (`wire-contract`)**: canonical JSON diff before/after
  differs ONLY in `metadata.octo.{protocol,template}` + `render_profile`; body
  / facts / attribution / variant / source / webUrl bytes are identical
  (assembleResourceCardBody shared). 4 fixtures (completed × zh full/minimal,
  failed × zh with-reason / en no-reason).
- **Render safety (`escape`, `markdown`, `url-destination`)**: caller-controlled
  text (Title/Reason) goes through `escapeMarkdown`; deep-link is absolute
  https via `summaryDeepLink`. Enforced by base helpers, unchanged from PR-1.
- **FallbackText**: each Template's `FallbackText` matches legacy
  `buildSummaryFallbackText` byte-for-byte for its `Kind`. Uses the shared
  `cardtmpl.SanitizeLine` (G5, PR-1).
- **i18n (`i18n`)**: labels from `BuildEnv.Lang`; must byte-match legacy
  `summaryLabelsFor` (the same discipline PR-1 used against `docsLabelsFor`,
  where drift caught a fallback mismatch — see PR-1 journal).
- **trust-boundary**: caller-supplied `Reason` on failed goes through the
  schema allowlist + escape/sanitize, never trusted as card JSON.
- **Conformance / tests (`testing`)**: each new card passes register-time
  self-check; ≥80% -race coverage on new packages; notify baseline + C1 tests
  extend the existing pattern.
- **Git/PR (`commit`)**: English Conventional Commit + English PR + link this
  brief + link PR-1 (#649). PR-2 base = `cardtmpl-l2a-migration` (stacked);
  rebase to `main` after PR-1 merges.

## Out of scope

- `generic.approval` — same reason as PR-1 (dynamic `owner`/`action_type`
  conflicts with static `TemplateActionContract`); parent brief tracks the
  decision.
- No change to the migrated `docs.access-request` / `docs.commented` /
  `docs.shared`.
- No `NotifyReq` external-shape change / no envelope mode (E2).
- No `${}` JSON template engine (E1).
- No capability-discovery endpoint (B), L2b channel (D), or unfurl (H).
- No DB migration.
- No new kill switch (rely on register-time fail-close + existing card
  master switch + cardmsg.Enabled gate).
- No refactor of `BuildSummaryResourceCard` / `buildSummaryCard` — kept for
  the byte-equivalence baseline test and for the legacy fallback path outside
  the Registry-routed branch. Follow-up cleanup left for the roadmap C
  finalization PR once all deliver branches are Registry-backed.

## Acceptance

- `summary.completed@0.1.0` and `summary.failed@0.1.0` each have a
  `pkg/cardtmpl/<card>/` subpackage + `handoff/<id>@<ver>/`
  (manifest / contract / samples), a 3-method Template
  (Meta / Build / FallbackText), and are registered + `SetDefault` + `Freeze`d
  at the composition root (`main.installCardTmplRegistry`) alongside the
  existing pilot + PR-1 cards.
- Each new manifest declares `renderProfile` + `renderProfileCompatibility`
  and passes register-time self-check.
- `deliverCardNotification` (`modules/notify/card.go`) routes
  `SummaryCardKindCompleted` / `SummaryCardKindFailed` through
  `buildSummaryCardViaRegistry` (Registry.Render); the legacy `buildSummaryCard`
  is retained only as a byte-equivalence baseline (not on the live path).
- Byte-equivalence baseline: 4 fixtures pass canonical JSON byte-equality after
  stripping `metadata.octo.{protocol,template}` — `TestBuildSummaryCards_MigrationBaseline`.
- C1: a schema field error on a migrated summary card returns 400 zero-delivery
  (asserted via `TestPreflightSummarySchema_C1RejectsEmptyTaskNo`).
- F7: with an unwired Registry, `buildSummaryCardViaRegistry` returns
  `errCardTmplUnavailable` (composition-bug → 500, not silent legacy fallback).
- G5: no new `sanitizeLine` copy; both new Templates use `cardtmpl.SanitizeLine`.
- Green: `pkg/cardtmpl/summary_completed` + `pkg/cardtmpl/summary_failed`
  -race -cover ≥80%; `modules/notify` full package green with real
  MySQL/Redis/WuKongIM; `go vet ./...`; `make i18n-extract-check` +
  `make i18n-lint` clean.
