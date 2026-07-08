---
type: Journal
title: "Journal: card-message-p2-action-loop (PR-B, card message P2 interaction)"
description: Record of PR-B — the card message P2 interaction closed loop (D3–D9/D11 + octo/v2 whitelist) harvested onto the merged P1 pkg/cardmsg, including a real concurrency bug caught in verify.
tags: ["message", "card", "wire-contract", "trust-boundary", "bot-api", "rate-limit", "i18n", "error-response", "space", "isolation"]
timestamp: 2026-07-08T09:30:00Z
# --- octospec extension fields ---
task: card-message-p2-action-loop
upstream: self
source: self
---

# Journal: card-message-p2-action-loop (PR-B, card message P2 interaction)

## What was done

Shipped the first PR of card message P2 — the interaction closed loop
(brief `.octospec/tasks/card-message-p2-action-loop/brief.md`, contract
`card-message-interaction` D3–D9/D11 + octo/v2 whitelist D1/D2):

- **`pkg/cardmsg`**: extended the merged-P1 `validate.go` seams (octo/v2 accepts
  `Action.Submit` + `Input.*`, frame-unique ids per D1, `data` must be object);
  new `interactive.go` (`SubmitAction` extracts the matched action's static
  `data` from the effective frame, `NormalizeContentEdit`, `CardSeq`) and
  `inputs.go` (`ValidateInputs` D11 trust boundary).
- **`POST /v1/message/card/action`** (`modules/message/api_card_action.go` +
  `card_action_claims.go`): authz + anti-IDOR (D3), D11 input validation, D4
  Redis idempotency (claim→enqueue→confirm, key = `(message_id, action_id,
  operator_uid)`), typed `card_action` bot event (D5).
- **`modules/robot`**: `EnqueueBotTypedEvent` on the same GenSeq/ZAdd/Expire
  chokepoint as `EnqueueBotEvent`.
- **`modules/bot_api/send.go`**: retired the P1 blanket type-17 edit reject →
  D6 branch (cardmsg validation + cross-type-mutation guard) + D9 `card_seq`
  CAS stored in `message_extra`.

## Structural learnings

- **The merged P1 `pkg/cardmsg` already pre-cut the P2 seams** (`interactiveByProfile`
  returning `(interactive, ok)`, `walker.interactive`, empty `Input.*`/`Action.Submit`
  switch arms). P2 was a fill-the-seam edit, not a rewrite. When a phased contract
  is designed up front, later phases should extend the P1 authority in place —
  do **not** graft a second whitelist (the POC branch, forked pre-P1, carried a
  parallel `cardmsg` that would have collided).
- **`plain.go` was left untouched**: the P1 version (goldmark AST markdown strip +
  `RecheckPayloadSize`) is strictly better than the POC's regex version. When
  harvesting from a POC that forked before review hardening landed, diff each file
  against the merged version rather than porting wholesale.
- **D2 side effect is intended**: widening `cardmsg.Validate` to octo/v2 makes the
  existing bot/robot/webhook send ingresses accept interactive cards automatically
  (a bot must be able to *send* one before anyone can act on it). Two P1 tests
  asserted octo/v2-rejected and had to flip to accept.

## Gotchas worth remembering

- **D9 CAS deadlock (caught in verify, would have shipped without a concurrency
  test)**: `SELECT ... FOR UPDATE` + `INSERT ... ON DUPLICATE KEY UPDATE` on
  concurrent *first* `card_seq` edits of the same message deadlocks on InnoDB
  insert-intention gap locks (ER_LOCK_DEADLOCK 1213), dropping higher-seq frames
  and stranding the card on a stale frame — a direct violation of D9's
  "latest-frame-wins". Fixed with a bounded retry (`isRetriableMySQLLockErr`
  1213/1205, matching `modules/conversation_ext/service.go` precedent). **A
  sequential CAS test cannot surface this; any read-then-write CAS needs a
  concurrent `-race` test.** See the promoted learning.
- **Local test infra**: `OCTO_MASTER_KEY` must be exported (CI value
  `0123456789abcdef...`) or `common.Route` panics; and the shared `test` DB drifts
  its migration ledger across module test binaries (each embeds only its own
  imports' migrations) — drop & recreate the DB between running different module
  packages.
- **D4 accepts a documented race**: a duplicate submit landing in the
  claim→(failed enqueue)→Release window gets a `replay:true` ack with no event
  enqueued; recovery is the client-side D8 timeout + re-tap (out of scope for this
  repo). This is the brief's explicit D4 caveat, not a defect.
- **Revoke/delete gate (caught in PR #548 review, blocking)**: the action
  endpoint read `message_extra` only for `content_edit` and did **not** check
  `revoke` / `is_deleted` (nor `message_user_extra.message_is_deleted`), so a
  stale client could tap a recalled/deleted card and trigger bot side effects.
  Fixed to mirror the single-message read gate (`api_message_get.go:241-248`).
  **Lesson: any new read/write path over a message must apply the same
  revoke/delete/user-local-delete visibility gate the existing read paths do —
  actionability must not outlive visibility.**

## Out of scope (sibling PRs)

D10 card revision history → PR-C; D12 capability manifest → PR-D.
