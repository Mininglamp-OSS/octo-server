---
type: Journal
title: "Journal: cardtmpl-l2a-migration (PR-1: docs.commented + docs.shared)"
description: Roadmap C first slice — migrate docs.commented and docs.shared display cards onto the cardtmpl Registry using the pilot copy-directory shape. Together with the already-migrated docs.access-request they reach the §2.2-5 L2b hard gate ② (≥3 L2a cards, v1 + v2 coverage). Also extracts the shared SanitizeLine helper (G5) and lays a summary-side fragment helper for PR-2.
tags: ["cardtmpl", "platform", "docs-commented", "docs-shared", "l2a-migration", "wire-contract", "i18n", "error-response", "escape", "markdown", "url-destination", "testing"]
timestamp: 2026-07-23T12:30:00+08:00
# --- octospec extension fields ---
task: cardtmpl-l2a-migration
upstream: self (roadmap C)
source: self
---

# Journal: cardtmpl-l2a-migration (PR-1)

## Review round (PR #649, 3× APPROVE + 1 CHANGES_REQUESTED → addressed)

`lml2468` / `Jerry-Xin` / `mochashanyao` approved; `yujiawei` requested changes
on one blocking item. Addressed in-branch:

- **P1 (blocking, yujiawei) — over-length display fields hard-rejected (400,
  zero delivery) where legacy truncated & delivered.** The new schemas cap
  `excerpt`/`actorName`/`updatedAt`/`title`; `preflightDocsDisplaySchema` runs
  before member/gate checks, so a 301-rune comment (realistic) now 400s and
  *every* recipient loses the notification — not even the text fallback fires.
  Legacy `buildDocsCard` never rejected on length (it truncated at render).
  This is the "unchanged wire shape ≠ unchanged accepted-input behavior" trap
  and the baseline can't catch it (all fixtures in-bounds). Fix, per the brief's
  own G9 wording ("truncate before render"): `mapDocsCardFieldsToDisplayJSON`
  now server-side truncates `title`→`cardtmpl.MaxTitleRunes`,
  `actorName`→120, `excerpt`→`cardtmpl.MaxExcerptRunes`, `updatedAt`→80 before
  preflight, preserving delivery. `docId` is **not** truncated — it's the
  deep-link key, so an over-length docId stays a genuine C1 400 (same handling
  as summary `taskNo`). Exported `cardtmpl.MaxTitleRunes` + `cardtmpl.TruncateRunes`
  so ingress and render share one cap/impl (G9). Within-budget inputs unchanged
  → canonical-JSON baseline still holds;
  `TestMapDocsDisplayFields_TruncatesDisplayFields` locks the new behavior
  (huge input delivers, over-length docId 400s).
- **P2 — dangling reference to a non-existent "docs_commented G9 field-cap
  test".** `resource.go` claimed the excerpt cap and schema `maxLength` "never
  drift (see the ... test)" but no such test existed. Added
  `TestSchemaCapsMatchRenderCaps` to both `docs_commented` and `docs_shared`
  (assert schema `excerpt.maxLength == MaxExcerptRunes`, `title.maxLength ==
  MaxTitleRunes`) and reworded the comment to name the real test.
- **P2 — "byte-equivalence" over-claim.** The baseline unmarshal→canonical→
  remarshal proves *canonical-JSON / semantic* equality, not literal wire-byte.
  Reworded the test comment to say so (kept the function name; it's the right
  drift guard, just not literal bytes).
- **P2 (mochashanyao/Jerry-Xin) — fallback delegation gap** (commented/shared
  text fallback still legacy, not `Template.FallbackText`) and **Nit — no
  `docs_shared` en test**: added `TestRenderEnglishSourceLocalized` for
  `docs_shared`; the fallback-delegation refactor is deferred to the roadmap C
  finalization PR (copy is byte-identical today, non-blocking).
- **Nit — gofmt drift** in `docs_shared/template.go`: `gofmt -w`.
- Also made the docs build-error log label kind-generic (+`zap kind`); it
  previously hard-coded "docs access-request card" for commented/shared too.

Dismissed (yujiawei's own): F7-fail-open-to-text is intended F6/§10 for v1
display cards (matches legacy); actor-hydration-after-preflight residual (a
>120-rune hydrated name) collapses into the P1 truncation fix.

## What was done

First slice of roadmap group C — two new L2a display cards migrated onto
`Registry.Render` behind the docs-notify plan-B shell, plus one shared helper
and one PR-2 scaffold:

1. **`pkg/cardtmpl/docs_commented@0.1.0`** — new subpackage with self-embedded
   handoff (`manifest.json` incl. `renderProfile`/`renderProfileCompatibility`,
   `contract/data.schema.json`, `samples/shown.json`) and a 3-method Template
   (Meta/Build/FallbackText). Build composes the base
   `BuildDocsResourceCardBodyWithLang` (already extracted upstream), so the
   rendered body is byte-identical to legacy `buildDocsCard` for the same
   inputs. 85.1% coverage; -race green.
2. **`pkg/cardtmpl/docs_shared@0.1.0`** — symmetric subpackage, differs from
   commented only in `Variant` + attribution copy. 80.9% coverage; -race green.
3. **notify wiring** (`modules/notify/card_via_registry.go` +
   `modules/notify/card.go`): new `buildDocsDisplayCardViaRegistry` +
   `preflightDocsDisplaySchema` helpers; `deliverDocsCardNotification` routes
   `DocsCardKindCommented` / `DocsCardKindShared` through Registry with F7
   fail-close (Registry-unwired → 500, not silent legacy). The
   `NotifyReq`/`DocsCardFields` external shape is unchanged (plan B).
4. **`main.installCardTmplRegistry`** registers both new cards + `SetDefault` +
   `Freeze` alongside the pilot; `modules/notify` gets a `TestMain` that
   mirrors the production wiring so preflight tests inherit the same registry.
5. **G5**: `SanitizeLine` exposed on `pkg/cardtmpl/resource.go`; both prior
   copies (`modules/notify` + `pkg/cardtmpl/docs_access_request`) become
   thin wrappers. Word list drift is now impossible — single source.
6. **PR-2 scaffold**: `BuildSummaryResourceCardBodyWithLang` added so the
   summary migration in PR-2 can compose the base body without touching the
   legacy summary path.

## Byte-equivalence baseline (the core acceptance)

`modules/notify/card_via_registry_display_baseline_test.go` runs 4 fixtures
(`commented` × zh full/anon, `shared` × zh full/anon) through both **legacy
`buildDocsCard`** and **`buildDocsDisplayCardViaRegistry`**, strips the two
newly-injected metadata fields (`metadata.octo.protocol` +
`metadata.octo.template`), and asserts canonical JSON byte-equality. Everything
else — body, facts, ActionSet, attribution, variant, source, webUrl — is
identical. Any future drift (copy, ordering, alias, extra field) fails this
test immediately.

`Action.OpenUrl.id` is intentionally NOT added on v1 cards (only the v2
`access_requested` pilot adds `view_document` because its interaction report
requires it). No id to strip on v1.

## Structural learning

- **The base already anticipated this migration.** `assembleResourceCardBody`
  was extracted upstream so legacy and Registry paths share the exact same
  body assembler; `BuildDocsResourceCardBodyWithLang` had a comment naming
  `pkg/cardtmpl/docs_commented` explicitly. PR-1's Go footprint was therefore
  minimal — each Template is a thin i18n + fixture-mapping shim on top of the
  shared body. Confirms the "copy pilot directory" pattern scales; the
  remaining L2a cards are similarly cheap.
- **§2.2-5 L2b hard gate ② is now met.** Three L2a cards (`docs.access-request`
  v2 + `docs.commented` v1 + `docs.shared` v1) run the full Registry path
  covering both wire profiles. This is prerequisite, not sufficient — D
  (`ext.*` production channel) still needs the visibility/token plumbing.

## Gotchas worth remembering

- **`TestMain` global Registry mirrors production wiring.** Any test that runs
  `deliverDocsCardNotification` for the commented/shared/access_requested
  kinds now needs a wired Registry (F7). A single `TestMain` under
  `modules/notify` beats retrofitting Registry setup into 10+ tests
  individually. Tests that intentionally verify unwired behavior can
  `SetDefaultRegistry(nil)` locally.
- **Cross-package migration ledger still bites `-count=1` runs.** After
  running any test package that uses `testutil.NewTestServer`, another package
  may hit `panic: Table 'seq' already exists ... sql-migrate`. Recovery is
  documented in memory `local_test_infra.md`: `docker exec octo-test-mysql
  mysql -uroot -pdemo -e "DROP DATABASE IF EXISTS test; CREATE DATABASE test
  ..."`. This is not introduced by C; it's the CI lane's per-package
  drop-and-recreate mirror running locally.
- **English copy alignment.** The migrated card's per-language `labelSet` must
  match `modules/notify.docsLabelsFor` byte-for-byte (banner / actor / time
  labels / kvSep), or the byte-equivalence baseline fails. Caught one during
  PR-1 (initial `bannerAnon` / `actor` / `updatedAt` drift). Copying the exact
  entries from the legacy label set before hand-editing prevents the miss.

## Out of scope (deliberate, deferred to later PRs)

- `summary.completed` / `summary.failed` (PR-2 — scaffold in place).
- `generic.approval` — dynamic caller-supplied `owner`/`action_type` conflicts
  with the static `TemplateActionContract`; needs a separate adaptation
  decision (fixed-owner template vs preserved dynamic special-case) before it
  can move to the Registry.
- No `NotifyReq` external shape change / no envelope mode (roadmap E2).
- No `${}` JSON engine (E1), no capability-discovery endpoint (B), no L2b
  channel (D), no unfurl (H).
- No DB migration.

## Verification (this run, real infra)

Locally green with MySQL 8 (docker :3306) / Redis 7 (:6379) / WuKongIM (:5001):

- `go build ./...`, `go vet ./...`
- `pkg/cardtmpl/docs_commented` -race -cover → **85.1%**
- `pkg/cardtmpl/docs_shared` -race -cover → **80.9%**
- `pkg/cardtmpl/...` (base + pilot + new subpackages) all green
- `modules/notify` full package (after `DROP DATABASE test; CREATE DATABASE
  test` to reset the cross-package migration ledger) — all green including
  the 4-case byte-equivalence baseline and the C1 preflight assertion
- `make i18n-extract-check`, `make i18n-lint` — clean
- `golangci-lint` not installed on this host; the CI lint lane covers it.
- Message `//go:build integration` orchestration e2e — pre-existing import
  cycle, not run locally nor in the CI default lane (unchanged by this PR).
