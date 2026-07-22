---
type: Journal
title: "Journal: cardtmpl-registry-pilot"
description: Introduced the octo-card@1.0 platform base (Template interface + Registry + Render pipeline + fail-close composition) and migrated docs.access-request@0.2.0 as the first L2a pilot. Two commits ship a byte-equivalent production switch (only metadata.octo.{protocol,template} added) plus a C1 policy change that turns schema-level DocsCard field errors into 400 zero-delivery instead of silent text degradation.
tags: ["cardtmpl", "platform", "docs-access-request", "wire-contract", "trust-boundary", "auth", "error-response", "i18n", "testing"]
timestamp: 2026-07-22T00:00:00Z
# --- octospec extension fields ---
task: cardtmpl-registry-pilot
upstream: self
source: self
---

# Journal: cardtmpl-registry-pilot

## What was done

Two commits on `docs-access-request-card-inspiration` shipping the L0 platform
base + the first L2a pilot card end-to-end:

1. **feat(cardtmpl): L0 platform base + docs.access-request@0.2.0 pilot scaffold**
   (`e4cc10f9`, 23 files +2597/-91)
   - `pkg/cardtmpl/template.go` — 3-method `Template` interface
     (`Meta`/`Build`/`FallbackText`) + immutable `TemplateMeta` carrying
     `Views`/`ActionContract`/`InputSchema`/`Source` + private `stateIndex`
     and `interactions`. `BuildResult` only carries the business fragment
     (`Body`/`Actions`/`Variant`/`DeepLink`/`Source`); the base is the one
     that marshals AC top-level and injects `metadata.octo.*` — templates
     cannot forget nor override.
   - `pkg/cardtmpl/registry.go` — `Register`/`SetDefault`/`Freeze`/`Lookup`/
     `List`/`RegisteredForTest`; asset loading from `embed.FS` at registration
     time (manifest + `data.schema.json` compile + `reports/*.interaction.json`
     + samples), with fail-close panics on any inconsistency (dup key, state
     across views, missing v2 view report, sample failing its own schema).
   - `pkg/cardtmpl/render.go` — the single 8-step `Registry.Render` pipeline
     that every producer must go through.
   - `pkg/cardtmpl/metrics.go` + `default_registry.go` — Prometheus
     `dmwork_cardtmpl_build_total{template_id, version, view, result}` and a
     package-scoped default registry that `main.installCardTmplRegistry()`
     wires + freezes at boot (symmetric to `main.go:521` docs approval
     callback assert).
   - `pkg/cardtmpl/docs_access_request/{template.go, labels.go}` — pilot
     Template implementation with an embedded `handoff/` (manifest with 5
     backfilled base fields, `contract/data.schema.json`, `samples/*.json`,
     `reports/pending.interaction.json`).
   - `pkg/cardtmpl/docs_action.go` — extracted
     `BuildDocsAccessRequestBodyWithLang` returning body/actions/deepLink
     fragment; legacy `BuildDocsAccessRequestCard` preserved
     byte-equivalent as a thin wrapper for non-Registry callers.
   - `docs/platform-card-base.md` moved into version control as the L0
     authoritative contract; `docs/l2b-owners.md` (empty) and
     `pkg/cardtmpl/ext/README.md` reserve the L2b placeholder without
     wiring the `ext.*` owner branch to production (rejected at register
     until channel opens; §2.2-5).

2. **feat(notify): route docs access_requested via cardtmpl Registry + C1 policy**
   (`1bf38edb`, 15 files +819/-110)
   - `modules/notify/card_via_registry.go` +
     `modules/notify/card.go` — the `access_requested + gate on` branch now
     goes through `Registry.Render`. If the default registry is unwired
     (test isolation without composition root), it falls back to the legacy
     builder via a typed `errCardTmplRegistryUnwired` sentinel so a rollout
     bisect stays viable.
   - **C1 policy change**: `cardtmpl.ErrFieldsInvalid` (schema violation)
     is now converted to `errNotifyCardInvalid` → **400 zero delivery**
     rather than degraded to a plain-text DM. Other build errors (render,
     marshal) keep the historical text degradation.
   - Pilot L1 contract adjusted to match the caller shape actually landed
     in production: `requester.name`/`avatarUrl` and `permission.*` made
     optional (legacy allows empty actor/avatar), `document.url` no longer
     required (server-composed), `document.docId` added so the mapping
     layer can carry `DocsCardFields.DocID` through to the deep-link.
   - `pending.interaction.json` rewritten to mirror the actual Go const IDs
     (`docs-access-approve`/`docs-access-deny`/`DocsDenyReasonInputID`) and
     the real `data` keyset — the interaction lock is now real-code-vs-report,
     not aspirational.
   - Pilot label defaults ("查看者"/"viewer") aligned with the legacy
     `docsLabelsFor` wording so the migration byte-equivalence baseline
     passes.
   - `pkg/cardtmpl/conformance_test.go` — generic conformance across
     `RegisteredForTest()`, enforcing A15c four sub-assertions
     (Submit id set equal, hidden inputs included, `data` keys equal,
     `associatedInputs` matched) + A15d/e/f metadata + A16 ActionContract
     three-way consistency.
   - `modules/notify/card_via_registry_baseline_test.go` — DeepEqual proof
     that stripping the two new `metadata.octo.{protocol,template}`
     subfields makes the new payload byte-equivalent to legacy. **Zero
     drift outside the two new fields**.

Deps: `github.com/santhosh-tekuri/jsonschema/v6 v6.0.2` (draft-07 +
`additionalProperties:false` + `allOf/if-then`; only new transitive is
`regexp2`; `golang.org/x/text` already direct).

## Structural learnings worth remembering

### 1. A handoff schema is a **wire-shape** contract, not a **caller** contract

The `docs.access-request@0.2.0` handoff shipped a `data.schema.json` designed
for the *full compiled card data* — required `requester.name minLength=1`,
required https `requester.avatarUrl`, required `document.url` etc. But the
production DocsCard callers legitimately submit anonymous requests
(`ActorName=""`) and don't have avatar URLs; the deep-link URL is
server-composed. If we had wired the handoff schema unchanged as the caller
input contract, every legacy-shape request would 400 under C1 policy.

The right split — the pilot L1 schema (validated by Registry) is a
**caller-input** contract (nullable optional server-filled fields relaxed);
the handoff schema stays as reference for the eventual full-card renderer.
Broke this out into `.octospec/learnings/pending/`.

### 2. A machine-readable interaction report only locks reality when it mirrors code IDs

The handoff `reports/pending.interaction.json` used display-oriented action
IDs (`approve`/`deny`/`view_document`) and short `dataKeys` (`[action,
request_id]`). The Go implementation had shipped with `docs-access-approve`/
`docs-access-deny`/`DocsDenyReasonInputID` and a much wider `dataKeys` set
(`owner, action_type, doc_id, request_id, doc_title, actor, decision`).
Wiring the conformance test to the handoff-shape report would have made A15c
red on day one.

We rewrote the pilot's `pending.interaction.json` to match the actual Go
constants and dataKeys. Lesson: **the interaction contract lock is only
credible if it is authored against the code that already ships, not
against a design-phase draft** — otherwise the test is either always red or
gets weakened to `⊆` semantics that hide drift.

### 3. `metadata` injection responsibility must live in the base, not in each template

Early designs had each template call a `FinalizeCard()` helper. Peer review
correctly pointed out that this only enforces the invariant "metadata is
always injected" by convention — the fourth L2a template author would forget.
Making `Registry.Render` the only path that produces a final payload, and
having `Template.Build` return only a fragment (`BuildResult`), moves the
guarantee from convention to type system.

### 4. `//go:embed` restrictions force one embed root per subpackage

Go's `//go:embed` rejects `..` and cannot climb parents. This means one
central `pkg/cardtmpl/testdata/handoff/<id>@<ver>/` cannot serve every L2a
template through a single top-level embed — each subpackage must embed its
own copy. The pilot's `handoff/` therefore lives under
`pkg/cardtmpl/docs_access_request/handoff/`. This is fine (per-template
ownership), but if the L2a fleet grows past ~5 cards, we may want to
consolidate by extending the composition root to load `embed.FS`
programmatically from a walk over `pkg/cardtmpl/*/handoff/*`.

## Gotchas worth remembering

- **`cardmsg.Validate` validates the whole payload envelope** (`{type:17,
  card, card_version, profile}`), not the bare AC card. `Registry.Render`
  therefore composes the envelope internally and calls `cardmsg.Validate` on
  it — then extracts `payload["card"]` back out via `RenderCard()` for
  callers that need `carddispatch.Card.Document`. Do not push the envelope
  concern to callers.
- **The pilot `Template.ActionContract{Owner, ActionType}` is server-owned
  and immutable**; the callback-route registration in `cardactiondispatch`
  (`main.go:521-523` asserts `Resolve(NotifyBotUID, "docs",
  "access_request.decision")` on boot) is the anchor. The A16 three-way
  consistency test asserts `Meta.ActionContract == Action.Submit.data.
  {owner,action_type}` in the rendered card, so a rogue owner rename in
  either place is caught at CI.
- **`NotifyCapability{SenderUID, Owner}`** (only two fields;
  `internal/cardactiondispatch/registry.go:79`) is orthogonal to
  `TemplateActionContract{Owner, ActionType}` — an earlier brief revision
  incorrectly conflated them, and Capability's `CanNotify` is only invoked
  from the `ApprovalCard` path. Any future review that touches "owner/
  action_type" identity must remember there are two separate models.
- **Downstream doc `.octospec/tasks/card-message-internal-dispatch/
  docs-notify-contract.md` was NOT updated in this PR** (deferred as PR-2
  in `docs/platform-card-base.md` §15-3). Client teams that read that doc
  will not see the new `metadata.octo.{protocol,template}` fields, nor the
  C1 policy change, until PR-2 lands.

## What is deliberately left for later PRs

- `CardUpdater` (§8) — needs the outcome-view (approved/rejected) migration
  to have a real caller.
- `GET /v1/message/card/templates` public endpoint (§9) — internal
  `Registry.List` is ready.
- Unified callback envelope (§7) — client contract PR, not internal.
- Remaining L2a migrations: `summary.completed/failed`,
  `docs.shared/commented`, `generic.approval`, `docs.access.outcome`.
- L2b `ext.*` channel opening — behind a documented four-condition gate
  (§2.2-5).
- JSON template engine, envelope-mode `NotifyReq` — later still.
