---
type: Task
title: "Task: summary-notify-sender-unification"
description: Reuse the existing notification User Bot for summary cards and their text fallback so users see one system-notification DM conversation.
tags: ["notify", "card", "bot", "trust-boundary", "wire-contract", "testing"]
timestamp: 2026-07-14T12:00:00+08:00
# --- octospec extension fields ---
slug: summary-notify-sender-unification
upstream: octo-server#571
source: self
---

# Task: summary-notify-sender-unification

## Goal

Bind the existing `summary-notify` producer to the already-provisioned
`notification` User Bot. Summary cards, legacy notifications, and summary-card
plain-text fallback must all arrive in the same system-notification DM
conversation instead of creating a second `summary` Bot conversation.

This decision supersedes the dedicated-summary-Bot choice recorded on
2026-07-13. Producer capability isolation remains unchanged: only the
`summary-notify` producer can originate this card template, even though it
shares a sender identity with non-card notification traffic.

## Background

PR #579 introduced the first internal card producer with a dedicated static
`summary` User Bot. Product review found that a second system conversation in
the user's DM list is confusing: the message is a system notification, not a
human-authored share and not a separate assistant relationship.

The existing `notification` identity is already provisioned as an active User
Bot (`user` + `app` + `robot.status=1`) and is trusted by `botidentity` and the
card dispatcher. Reusing it therefore changes presentation and provisioning,
not the card authorization boundary.

## Load-bearing list

- **Producer sender binding** (`main.go`) — `summary-notify` stays DM-only,
  `octo/v1`, system-notification Space policy, and max-in-flight 20; only its
  bound UID changes to `NotifyBotUIDValue`.
- **Bot lifecycle/readiness** (`modules/notify`) — startup, legacy text, card,
  and fallback paths share one idempotent, retryable, race-safe readiness gate.
- **Fallback identity** — card construction/dispatch failure must not silently
  switch conversations; fallback text uses the same `notification` sender.
- **Trust boundary** — callers still cannot choose a sender or submit an
  arbitrary type-17 payload; the producer-bound `carddispatch.Sender` remains
  the only card path.
- **Existing installations** — no migration or destructive cleanup removes an
  already-provisioned `summary` identity; cleanup, if desired, requires a
  separately reviewed operational plan.
- **Tests** — source wiring, captured WuKongIM requests, DB-backed HTTP pilot,
  and pilot E2E assertions pin the shared sender UID.

## Out of scope

- Renaming or rebranding the `notification` Bot.
- Deleting, disabling, or merging a previously provisioned `summary` user/app/
  robot record.
- Smart-summary origin group/thread delivery.
- User-initiated sharing of a summary card.
- Changing templates, deep links, card profile, Space policy, retry semantics,
  per-channel quotas, or the global card feature flag.

## Acceptance

- `summaryNotifyProducerSpecs()` binds `summary-notify` to
  `notify.NotifyBotUIDValue`; a source guard fails if a dedicated summary UID is
  reintroduced into the production producer wiring.
- No production startup path provisions a dedicated `summary` Bot or maintains
  a separate summary-Bot readiness state.
- Legacy text notifications, summary cards, and summary-card fallback text all
  use `from_uid=notification`.
- The shared readiness helper is safe under concurrent card/text delivery and
  remains retryable after a provisioning failure.
- Card capability, target authorization, server-owned template/finalization,
  and the type-17 internal-notify gate remain unchanged.
- Focused race tests, pilot compile tests, vet, card-dispatch lint, i18n checks,
  and `git diff --check` pass.
