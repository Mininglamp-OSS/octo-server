---
type: Journal
title: "Journal: cardtmpl-reasoning-progress-card (roadmap E1a)"
description: Onboard the ai.reasoning-process handoff as the first live JSON-mode card on the E1 engine (#654). Decision A — keep the producer's action buttons (reasoning_stop / reasoning_retry Submit + toggle), fixed owner "ai" / action_type "reasoning.control", active/error octo/v2, result octo/v1. Registered + renderable; the stop/retry handler + RouteSpec + bot streaming delivery are downstream.
tags: ["cardtmpl", "platform", "ai-reasoning-process", "json-template", "wire-contract", "trust-boundary", "testing", "e1a"]
timestamp: 2026-07-24T04:33:57Z
# --- octospec extension fields ---
task: cardtmpl-reasoning-progress-card
upstream: self (roadmap E1a; depends on #654)
source: self
---

# Journal: cardtmpl-reasoning-progress-card (roadmap E1a)

## What shipped

The first live JSON-mode card on the E1 engine (#654): `ai.reasoning-process@0.1.0`
registered via `Registry.RegisterJSON`, no hand-written Go `Build()`.

- **`pkg/cardtmpl/ai_reasoning_process/`** — embeds the handoff (manifest +
  contract + templates + view-named reports + samples + goldens) and exposes
  `Assets` / `HandoffRoot` / `TemplateID` / `TemplateVersion`. No Template struct.
- **`registry.go`** — `ai` added to the L2a owner allowlist (new platform card
  family).
- **`main.go`** — `RegisterJSON` + `SetDefault` in the composition root.

## Decision A — keep the buttons (translate the handoff faithfully)

The handoff ships `reasoning_stop` (active) and `reasoning_retry` (error)
`Action.Submit` buttons plus `reasoning_toggle`. Per the user's decision the card
is translated **as delivered** (buttons kept), not stripped to a v1 display card.
Consequences resolved here:

- **Fixed owner `ai` / action_type `reasoning.control`.** Not the C3 dynamic-owner
  problem — the template is platform-owned, stable; `reasoning_id` is a Submit
  data field, not the routing key.
- **`Submit.data` carries `owner` + `action_type`.** The base ActionContract
  self-check asserts every `Action.Submit.data.{owner,action_type}` matches the
  manifest contract (same as docs.access-request). This is the one deviation from
  byte-identical-to-as-delivered; goldens carry the same two static keys so
  `engine(template) == golden` stays self-consistent.
- **`active`/`error` = octo/v2, `result` = octo/v1.** A v2 view with no Submit
  fails the self-check (`seenSubmit == 0`), so the display-only result view is
  v1 — mirroring the docs.access-request 0.3.0 pending-v2 / result-v1 split. The
  two v2 views carry view-named interaction reports; the v1 result view needs none.

## Out of scope (downstream)

The button **handler + `cardactiondispatch` RouteSpec** (actually stop / retry a
reasoning run) and **bot streaming delivery** (send + `ReplaceView`
reasoning→answering→result). This PR registers a card whose buttons render and
carry a routable contract, but a click has no route until the handler lands —
deliberate, documented in the PR body so a reviewer doesn't read it as a gap.

## Tests

`TestRegisters` (RegisterJSON success == ActionContract self-check passing),
`TestRenderAllStates` (5 states, correct v2/v1 profiles), `TestConformance`
(RenderCard minus metadata == golden minus $schema, 5/5). `go build ./...` +
`go test ./pkg/cardtmpl/... -race` green.

## Git

Built on the E1 branch; after #654 squash-merged to main, rebased
`--onto origin/main` so the PR diff is reasoning-card-only (single commit, engine
commits dropped into main). Mirrors the #650-onto-#649 rebase pattern.
