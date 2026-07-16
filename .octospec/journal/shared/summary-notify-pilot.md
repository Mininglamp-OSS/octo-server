---
type: Journal
title: "Journal: summary-notify pilot (card-message-internal-dispatch P2)"
description: Record of enabling the first internal card producer — the summary-notify DM card path in modules/notify on top of the dormant internal/carddispatch foundation.
tags: ["card", "internal", "trust-boundary", "notify", "producer", "pilot", "cross-repo"]
timestamp: 2026-07-13T15:44:34Z
# --- octospec extension fields ---
task: card-message-internal-dispatch
upstream: octo-server#571
source: self
---
# Journal: summary-notify pilot (card-message-internal-dispatch P2)

## What was done

Enabled the first production `internal/carddispatch` producer (`summary-notify`)
on top of the dormant P1 foundation (PR #577 lineage), turning the summary
completion/failure DM from plain text into a display-only (`octo/v1`)
ResourceCard. Scope is octo-server only; DM (person) targets only.

- **2a — summary bot.** Added a dedicated `summary` User Bot
  (`user`+`app`+`robot.status=1`) provisioned via the same idempotent pattern as
  `ensureNotifyBot`. The `notification` bot keeps the generic text path
  untouched; identities stay decoupled.
- **2b — producer wiring.** Registered the `summary-notify` `ProducerSpec`
  (DM-only, `octo/v1`, `SpacePolicySystemNotification`, `MaxInFlight` 20) in
  `installCardDispatch`; `modules/notify` obtains its bound `Sender` from the
  registry already installed in `*config.Context` via `SenderFromContext` —
  no `register.AddModule` change, no package-global registration (Decision 1/11).
- **Card path.** `NotifyReq` gained an optional structured `Card` field
  (`SummaryCardFields`, not a type-17 map). When present, notify builds the card
  once via `cardtmpl.BuildSummaryResourceCard` and dispatches per verified
  recipient through the bound `Sender`, reusing the existing member-verify →
  actor-exclude → fan-out machinery. `Payload` and `Card` are mutually
  exclusive.
- **Contract.** `.octospec/tasks/card-message-internal-dispatch/summary-notify-contract.md`
  captures the cross-repo wire contract for octo-smart-summary.

## Structural learnings / gotchas

- **Reuse the existing ingress, do not add a route.** The brief locks "no new
  HTTP endpoint" and points the pilot at `NotifyReq`/notify's `memberCache`.
  The card path therefore extends `NotifyReq` (a structured `Card` field), it
  is NOT a new `/v1/internal/notify/summary-card` route. Decision 14's raw-card
  rejection is unaffected because `Card` is structured fields, not a type-17
  payload map.
- **Preserve `NotifyResp{delivered,filtered}`.** octo-smart-summary's entire
  per-recipient dedup/retry/sweep state machine keys off this response. The
  card path maps `Sender.Send` success → `delivered[uid]`, `target_denied` /
  `dispatch_failed` → `filtered[uid]=reason`, so the sidecar needs zero
  response-handling change and its silent-success guard still holds.
- **Template ownership is server-side.** The sidecar sends raw fields
  (`task_no`, title, time range, counts, failure reason); octo-server composes
  labels + the `/s/{task_no}?sp={space_id}` deep link. Deep-link identifier is
  `summary_task.task_no` (opaque, unique), never the autoincrement `id`.
- **Dual-mode request needs an explicit both-present reject.** A request struct
  with two mutually-exclusive modes (`Payload` vs `Card`) must 400 when BOTH are
  set, not only when both are absent — otherwise one mode is silently dropped.
  Caught in review (`err.server.notify.card_invalid`).
- **Deep link requires https.** `cardtmpl` fail-closes on non-https; the base
  URL source (`External.WebLoginURL` origin vs a new `External.WebBaseURL`) is
  deferred to the enablement config review, and prod must be https.

## Verification

`go vet` clean; `internal/carddispatch` race+cover 92.4%; `modules/notify`,
`pkg/cardtmpl`, `pkg/cardmsg` tests green; `lint-card-dispatch` guard green;
`make i18n-extract-check` + `make i18n-lint` green; `git diff --check` clean.

## Rollout / dependencies

Stacked on the P1 foundation branch (`feat/card-message-internal-dispatch`),
NOT main — the `internal/carddispatch` foundation is not yet merged to main.
End-to-end requires two other repos (out of scope here): octo-web must ship the
`/s/:taskId` route (else the deep link is dead), and octo-smart-summary must
switch its terminal-state notification from the text payload to the structured
`Card` field per the contract. Until then this producer is inert (only the
holder of `NOTIFY_INTERNAL_TOKEN` can reach it, and it still sends text).
Rollback: global `OCTO_CARD_MESSAGE_ENABLED=false` or remove the one spec.
