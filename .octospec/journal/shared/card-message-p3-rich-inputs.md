---
type: Journal
title: "Journal: card-message-p3-rich-inputs (card message P3-3)"
description: Record of P3-3 — extend the octo/v2 input whitelist with Input.Number/Date/Time (all AC 1.0, within the pinned card_version "1.5"; additive, no version bump), add submit-time value validation (finite Number + declared min/max, Date YYYY-MM-DD, Time HH:MM), refactor the whitelist into a single pkg/cardmsg authority that validator + collector + dispatcher + D12 manifest all derive from, and additively advertise elements/inputs on GET /v1/bot/card/profile. No new errcode/DB/endpoint.
tags: ["card", "wire-contract", "trust-boundary", "bot-api", "testing"]
timestamp: 2026-07-09T01:00:00Z
# --- octospec extension fields ---
task: card-message-p3-rich-inputs
upstream: card-message-interaction P3-3
source: self
---

# Journal: card-message-p3-rich-inputs (card message P3-3)

## What was done

Delivered the server half of P3-3 (richer card inputs). Octo's octo/v2 whitelist
accepted only 3 of Adaptive Cards' 6 input elements; added the missing three.

- **`pkg/cardmsg/whitelist.go` (new)** — the single authority for the element
  whitelists: `displayElements` (octo/v1 display set, shared) and `inputElements`
  (octo/v2 interactive inputs), plus `DisplayElements()` / `InputElements()`
  accessors. `isInputElement` moved here. The validator, the submit-time inputs
  collector, the dispatch walker, and the D12 manifest now all derive from these
  two slices — no re-typed literals anywhere.
- **`validate.go`** — send-time whitelist extended to `Input.Number/Date/Time`.
  The `element()` type switch no longer enumerates input literals; unknown types
  fall through to `default` and are accepted iff `isInputElement(t)`, so adding
  an input element is a one-line change in `whitelist.go`.
- **`inputs.go`** — submit-time value validation for the new types (trust
  boundary D11): Number = finite numeric + declared `min`/`max`; Date =
  `YYYY-MM-DD` + declared range; Time = `HH:MM` (24h) + declared range; `""`
  passes as "unfilled". `isRequired`/`regex` remain client-UX + bot concerns.
- **`interactive.go`** — dispatch (`findSubmitInElements`) walks `inlineAction`
  via the same `isInputElement` predicate as validation.
- **`modules/bot_api/card_profile.go`** — D12 manifest additively advertises
  `elements` and `inputs` (from the cardmsg authority) so producers can
  feature-detect at element granularity even while `card_version` stays "1.5".
- **`docs/card-protocol.md`** kept a faithful mirror (§2 whitelist, §3 manifest,
  §7.1 inputs trust boundary).

No new errcode / i18n / DB / migration / endpoint; wire contract additive-only.
Verified live against adaptivecards.io that all three new inputs are AC 1.0 and
`Data.Query` (dynamic typeahead) is AC 1.6 — so this stays inside the pinned 1.5.

## Structural learnings

- **Widening a whitelist means widening every surface that mirrors it — and the
  cleanest way to guarantee that is one predicate/authority all surfaces derive
  from.** This change touches four surfaces of the same whitelist: send-time
  validation, submit-time input collection, action dispatch, and the capability
  manifest. Two review bugs came precisely from surfaces that were *not* driven
  by a shared authority: (1) dispatch still hardcoded the old 3 input types, so
  `Input.Number/Date/Time.inlineAction` validated at send but was undispatchable
  → a "send-ok, click-invalid" dead button; (2) the manifest would have re-typed
  the input list. Routing validation + collection + dispatch + manifest all
  through `isInputElement` / `InputElements()` makes the four physically
  incapable of drifting. Guard tests (`TestInputElementsAuthority`,
  `TestSubmitActionDispatchRichInputInlineAction`) pin the symmetry.
- **`strconv.ParseFloat` accepts non-finite tokens without error.** `"NaN"`,
  `"Inf"`, `"+Inf"`, `"Infinity"` (case-insensitive) all parse to a valid
  `float64` with `err == nil`, and `NaN` compares `false` against every bound —
  so a naive `ParseFloat` + `min/max` check silently accepts non-finite values.
  Any numeric wire-input validator must add an explicit
  `math.IsNaN || math.IsInf` reject. This is the numeric analogue of the
  "validation surface must cover the whole value space" discipline.

## Gotchas

- Additive-within-1.5 is invisible to version-based negotiation. Because the
  three new inputs are additive to octo/v2 and `card_version` stays "1.5", a
  version-only capability gate cannot distinguish a deployment that accepts them
  from an older 1.5 deployment that 400s them. The manifest's new `elements` /
  `inputs` arrays are exactly the element-granularity probe that closes this gap.
- Host has no `go` binary; unit tests were run via a workspace-local
  `.context/go` (go1.25.12) reusing the host module cache. `pkg/cardmsg` is pure;
  `modules/bot_api` / `modules/message` card tests need MySQL (`octo-test-mysql`)
  + `OCTO_MASTER_KEY` (exactly 32 bytes).

## Open (deferred, in brief)

- Whether server-enforced `min/max` (chosen default, consistent with ChoiceSet /
  Toggle declared-value checks) should instead be delegated to bot business
  validation — flagged for maintainer confirmation.
- AC 1.6 `Data.Query` (dynamic typeahead) is a separate XL item: it needs a new
  synchronous client→bot query channel that Octo's async event-queue model does
  not have. Sequenced after modal forms (Goal 4), gated on a real
  remote/huge-choice-set need.
