---
type: Learning
title: "Widening a whitelist means widening every mirror surface — drive them from one predicate, not copies"
description: When a value/type whitelist is enforced at several points (send-time validation, consumer collection, action dispatch, capability manifest), extending it at one point silently desynchronizes the others. Route every surface through a single shared predicate/authority and pin the symmetry with a test — otherwise you ship validated-but-undispatchable inputs (dead buttons) or a manifest that advertises capabilities the enforcement path rejects.
tags: ["wire-contract", "trust-boundary", "whitelist", "testing", "review"]
timestamp: 2026-07-09T01:00:00Z
# --- octospec extension fields ---
source: self
origin_task: card-message-p3-rich-inputs
origin_pr: self
status: pending
candidate_rule: error-handling
---

# Widening a whitelist means widening every mirror surface — drive them from one predicate, not copies

## Context

P3-3 added `Input.Number/Date/Time` to the card octo/v2 input whitelist. The
same whitelist is consulted at **four** places in `pkg/cardmsg` +
`modules/bot_api`:

1. send-time validation (`validate.go element()`),
2. submit-time input collection (`inputs.go collectInputSpecs`),
3. action dispatch (`interactive.go findSubmitInElements` — walks `inlineAction`),
4. the D12 capability manifest (`card_profile.go` — advertises the set).

Extending only (1) shipped two latent defects caught in review:

- Dispatch (3) still hardcoded `t == "Input.Text" || "Input.Toggle" ||
  "Input.ChoiceSet"`, so a new-type `inlineAction` Submit **validated at send but
  was undispatchable** → the button renders and clicking it returns invalid: a
  "send-ok, click-invalid" dead button. Security invariant (dispatch ⊆
  validation) still held; the bug was the inverse — validated-but-unreachable.
- The manifest (4) would have re-typed the list literal, drifting from the
  validator on the next change.

## Principle

A whitelist enforced at N surfaces is one fact with N copies. **Make the copies
one:** define the set once (`inputElements` + `isInputElement()` /
`InputElements()`), and have validation, collection, dispatch, and the manifest
all derive from it. Then pin the symmetry with tests:

- `TestInputElementsAuthority` — every member is accepted by the validator,
  non-members rejected (validation == the list).
- `TestSubmitActionDispatchRichInputInlineAction` — every member's `inlineAction`
  Submit is both send-valid and dispatchable (dispatch == validation).

## Applicability

Any type/format/capability whitelist with more than one consumer: element
whitelists, accepted MIME/scheme sets, feature-flag manifests, protocol
version/profile sets. If you cannot route every surface through one authority,
at minimum ship a drift-guard test asserting the surfaces agree.

## Related

- `verify-io-before-latency-metric` (pending) — similar "the guard must cover
  the whole surface, not the happy path" theme.
- Sibling: `card-message-p2-capability-manifest` learning (manifest fields must
  source from a single in-package authority).
