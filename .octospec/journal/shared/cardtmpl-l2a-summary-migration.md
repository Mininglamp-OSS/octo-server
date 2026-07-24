---
type: Journal
title: "Journal: cardtmpl-l2a-summary-migration (PR-2: summary.completed + summary.failed)"
description: Roadmap C second slice — migrate summary.completed and summary.failed onto the cardtmpl Registry using the same copy-directory shape PR-1 established. Together with PR-1 the four legacy display-card deliver branches are Registry-backed; only generic.approval remains legacy (dynamic owner, out of scope).
tags: ["cardtmpl", "platform", "summary-completed", "summary-failed", "l2a-migration", "wire-contract", "i18n", "error-response", "escape", "markdown", "url-destination", "testing"]
timestamp: 2026-07-23T14:20:00+08:00
# --- octospec extension fields ---
task: cardtmpl-l2a-summary-migration
upstream: self (roadmap C, follows PR-1 #649)
source: self
---

# Journal: cardtmpl-l2a-summary-migration (PR-2)

## Review round (PR #650, 4× APPROVE)

4 members approved (`lml2468` / `Jerry-Xin` / `mochashanyao` / `yujiawei`), zero
P0/P1. Addressed the actionable P2s in-branch:

- **P2-1 — C1 schema-cap was a delivery regression.** Legacy `buildSummaryCard`
  never validated length; an over-length `title`/`reason`/`timeRange`/
  `generatedAt` still delivered (truncated card or text DM). Under the new C1
  preflight it would flip to a 400 zero-delivery. Fix:
  `mapSummaryCardFieldsToJSON` now server-side truncates the display fields to
  the schema/render budgets (`title`→`cardtmpl.MaxTitleRunes`,
  `reason`→`cardtmpl.MaxExcerptRunes`, `timeRange`/`generatedAt`→local consts
  mirroring schema `maxLength`) and clamps negative `members`/`msgCount` to 0.
  `taskNo` is **not** truncated — it's the deep-link key, so an over-length
  taskNo stays a genuine C1 400. Exported `cardtmpl.MaxTitleRunes` +
  `cardtmpl.TruncateRunes` so ingress and render share one cap/impl (G9).
  Within-budget inputs are unchanged, so the byte-equivalence baseline still
  holds; `TestMapSummaryFields_TruncatesDisplayFields` locks the new behavior
  (huge input delivers, over-length taskNo 400s).
- **P2-2 — misleading log label.** The docs display-card build-error branch
  logged `"docs access-request card ..."` for `docs.commented`/`docs.shared`
  too and omitted `kind`. Made kind-generic + added `zap.String("kind", ...)`.
  (PR-1-introduced; fixed here since reviewers flagged it on #650.)
- **Nit — `docs_shared` en test.** Added `TestRenderEnglishSourceLocalized`
  (the other three new packages already had it).

Deferred (non-blocking, → roadmap C finalization PR): P2-3 (summary text
fallback doesn't delegate to `Template.FallbackText` — copy identical today),
P2-4 (build-site nil-registry diagnostic log). Refuted (yujiawei): the
"excerpt prefix inside the 300-rune budget" concern — legacy does the identical
`prefix + reason` concat then truncates at the same cap; changing it would
*break* byte-equivalence. Pre-existing, out of scope.

Also updated `docs/platform-card-base.md` §2.2 gate ② + §15.4 to record that
#649/#650 put 5 L2a cards on the Registry (register/Render/conformance/baseline
dimensions met; production-canary dimension still pending, so gate ② not yet
整体 satisfied).

## What was done

Second slice of roadmap group C — two more L2a display cards migrated onto
`Registry.Render` behind the summary-notify plan-B shell, reusing every helper
PR-1 introduced:

1. **`pkg/cardtmpl/summary_completed@0.1.0`** — new subpackage with
   self-embedded handoff (`manifest.json` incl.
   `renderProfile`/`renderProfileCompatibility`, `contract/data.schema.json`,
   `samples/shown.json`) and a 3-method Template (Meta/Build/FallbackText).
   Build composes the base `BuildSummaryResourceCardBodyWithLang` (the PR-1
   scaffold), so the rendered body is byte-identical to legacy
   `buildSummaryCard` for the same inputs. 91.7% coverage; -race green.
2. **`pkg/cardtmpl/summary_failed@0.1.0`** — symmetric subpackage. Differs from
   completed in `attribution` / `variant` / `excerpt` only; facts / deep-link /
   source come from the same shared body helper. Schema adds an optional
   `reason` field (maxLength = `cardtmpl.MaxExcerptRunes` = 300, G9 single
   source). 91.1% coverage; -race green.
3. **notify wiring** (`modules/notify/card_via_registry.go` +
   `modules/notify/card.go`): new `buildSummaryCardViaRegistry` +
   `preflightSummarySchema` + `mapSummaryCardFieldsToJSON` helpers.
   `deliverCardNotification` routes `SummaryCardKindCompleted` /
   `SummaryCardKindFailed` through Registry with F7 fail-close
   (Registry-unwired → 500, not silent legacy) and C1 preflight (schema →
   400 zero-delivery ahead of member/gate checks). `NotifyReq` /
   `SummaryCardFields` external shape unchanged.
4. **`main.installCardTmplRegistry`** registers both new cards + `SetDefault` +
   the existing `Freeze` covers them; `modules/notify` `TestMain` mirrors the
   wiring so preflight tests inherit the same registry.
5. **`renderProfile`** now flows through the send path for summary cards too —
   `deliverCardNotification` passes `carddispatch.Card{RenderProfile: ...}`
   from `buildSummaryCardViaRegistry`, matching the docs display card path
   PR-1 introduced.
6. **Legacy summary path retained for the baseline only.** `buildSummaryCard`
   and `buildSummaryFallbackText` remain in-tree so
   `TestBuildSummaryCards_MigrationBaseline` can prove byte-equivalence; they
   are no longer on the live production path. Deletion is deferred to the
   roadmap C finalization PR once every deliver branch is Registry-backed and
   the baselines are stable across a release.

## Byte-equivalence baseline (the core acceptance)

`modules/notify/card_via_registry_summary_baseline_test.go` runs 4 fixtures
(`completed_zh_full`, `completed_zh_minimal`, `failed_zh_with_reason`,
`failed_zh_full`) through both **legacy `buildSummaryCard`** and
**`buildSummaryCardViaRegistry`**, strips the two newly-injected metadata
fields (`metadata.octo.protocol` + `metadata.octo.template`), and asserts
canonical JSON byte-equality. Everything else — body, facts, attribution,
variant, source, webUrl, ActionSet — is identical. Any future drift (copy,
ordering, alias, extra field) fails this test immediately.

`Action.OpenUrl.id` is intentionally NOT added on v1 cards (only the v2
`access_requested` pilot adds `view_document` because its interaction report
requires it). No id to strip on v1.

## Structural learning

- **The PR-1 scaffold covered PR-2 automatically.**
  `BuildSummaryResourceCardBodyWithLang` (added in PR-1 to prepare this
  migration) plus the shared `assembleResourceCardBody` mean PR-2's body
  layer needed zero new base code. Each Template is a thin i18n +
  fixture-mapping shim on top of the shared body — same pattern as PR-1's
  docs display cards. Confirms the "copy pilot directory" template scales
  linearly.
- **Baseline stays zh-CN — same reason as PR-1.** Legacy `buildSummaryCard`
  routes the AC button label through `i18n.OutboundLanguage(ctx)` (background
  ctx → zh default), while the Registry path takes `env.Lang` directly. A
  cross-lang byte-equivalence would fail on button copy even though every
  other bit lines up. The Template unit tests
  (`TestRenderEnglishSourceLocalized`) cover the en source-label path
  independently.
- **Two subpackages beat one Kind-multiplexer.** Modeling `completed` and
  `failed` as separate L2a subpackages (each with its own manifest / schema /
  Variant / FallbackText) matches the L2a-per-variant pattern PR-1 already
  used. Trade-off is two `handoff/` trees, but register-time freeze is uniform
  and each `FallbackText` stays a short branch — no `switch card.Kind`
  spillage into the Template.
- **§2.2-5 L2b hard gate ② now exceeded.** After PR-1 (3 L2a cards), PR-2
  brings the total to 5 (docs.access-request v2 + docs.commented v1 +
  docs.shared v1 + summary.completed v1 + summary.failed v1). All four
  legacy display-card deliver branches (`docs.commented`, `docs.shared`,
  `summary.completed`, `summary.failed`) now Registry-backed. Only
  `generic.approval` remains legacy — parent brief tracks the dynamic-owner
  vs static-`TemplateActionContract` decision separately.

## Gotchas worth remembering

- **`buildSummaryCard` legacy is now baseline-only.** It still compiles and
  runs in tests but the live `deliverCardNotification` path never calls it.
  Reviewers may misread this as "dead code left behind"; comment on
  `buildSummaryCard` should be updated in the finalization PR when it can be
  deleted without losing the baseline.
- **`summary_failed` schema accepts the same facts fields as `summary_completed`,
  plus `reason`.** Legacy `buildSummaryCard` doesn't Kind-gate the facts
  branch, so producers that historically sent facts on a failed card would
  still see them rendered. `additionalProperties: false` on both schemas
  guards against silent shape drift.
- **`summary.failed.reason` maxLength ↔ `MaxExcerptRunes`.** Both are 300;
  bumping one without the other reintroduces the G9 double-write hazard.
  `TestSchemaC1RejectsReasonTooLong` guards this at the 301-rune boundary.
- **`generic.approval` still deferred.** Same reason as PR-1 (dynamic
  `owner`/`action_type`). Its migration needs a separate design decision, not
  another copy of this Template shape.

## Out of scope (deliberate, deferred to later PRs)

- `generic.approval` — dynamic caller-supplied `owner`/`action_type` conflicts
  with the static `TemplateActionContract`; needs a separate adaptation
  decision (fixed-owner template vs preserved dynamic special-case) before it
  can move to the Registry.
- Removal of legacy `buildSummaryCard` / `buildSummaryFallbackText` — retained
  for the byte-equivalence baseline; deletion belongs to the roadmap C
  finalization PR (once all migrated cards are stable across a release).
- No `NotifyReq` external shape change / no envelope mode (roadmap E2).
- No `${}` JSON engine (E1), no capability-discovery endpoint (B), no L2b
  channel (D), no unfurl (H).
- No DB migration.

## Verification (this run, real infra)

Locally green with MySQL 8 (docker :3306) / Redis 7 (:6379) / WuKongIM (:5001):

- `go build ./...`, `go vet ./...`
- `pkg/cardtmpl/summary_completed` -race -cover → **91.7%**
- `pkg/cardtmpl/summary_failed` -race -cover → **91.1%**
- `pkg/cardtmpl/...` (base + pilot + PR-1 subpkgs + new summary subpkgs) all green
- `modules/notify` full package -race — all green including the 4-case
  summary byte-equivalence baseline, C1 preflight assertion, and F7 unwired
  assertion (also PR-1's docs baseline still green — no cross-migration
  regression)
- `make i18n-extract-check`, `make i18n-lint` — clean
- `golangci-lint` not installed on this host; the CI lint lane covers it.
- Message `//go:build integration` orchestration e2e — pre-existing import
  cycle, not run locally nor in the CI default lane (unchanged by this PR).
