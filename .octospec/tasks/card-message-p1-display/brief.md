---
type: Task
title: "Task: card-message-p1-display"
description: Implementation task (PR-A) for P1 of the card message protocol. Harvests the verified poc/card-message branch into the formal display-only delivery — octo/v1-only accepted set, every ingress/edit gate, uniform display-surface masking via one display-text helper, incomingwebhook msg_type "card", search/pin projections, docs/card-protocol.md. All interactivity (P2) excluded.
tags: ["message", "card", "wire-contract", "trust-boundary", "external-content", "webhook", "bot-api", "i18n", "error-response", "testing"]
timestamp: 2026-07-07T02:30:00Z
# --- octospec extension fields ---
slug: card-message-p1-display
upstream: octo-server#525
source: self
---

# Task: card-message-p1-display

**Spec of record: `.octospec/tasks/card-message-protocol/brief.md`** (frozen via
PR #525). This brief deliberately does **not** restate the contract — it
captures only the execution decisions for delivering P1 as one reviewable PR
(“PR-A”): what is harvested from the verified POC, what is built net-new, and
the PR-A-specific acceptance gates. **If anything here conflicts with the
protocol brief, the protocol brief wins.**

## Goal

Ship P1 display-only cards exactly as specced: `InteractiveCard` (=17) envelope
with the octo/v1 whitelist, server-authoritative `plain`, rejection at user
ingress / OBO / all edit paths, uniform `[卡片]` residual-risk masking on every
server-authored display surface, `OCTO_CARD_MESSAGE_ENABLED` default off.
Server merges ahead of the client renderers — risk is isolated by the flag
(protocol brief Decision 2), so PR-A does not wait for any client repo.

## Background

The full pipeline was proven on `poc/card-message` (@ `96b691b4`): ~40
`pkg/cardmsg` unit tests, 5 MySQL+Redis integration tests, 2 WuKongIM-backed
integration tests, all green, including the round-3 trust-boundary revisions.
The POC deliberately carries **both** P1 and P2 behavior plus demo artifacts,
so PR-A is a **file-level harvest, not a branch merge/rebase** — the POC
branch stays untouched as review evidence.

## Execution decisions (harvest map)

| # | Decision |
|---|---|
| E1 | **Harvest source**: `poc/card-message` @ `96b691b4`, file-level cherry-pick only. The POC branch is never merged or rebased into PR-A; its history (demo HTML, P2 code, filter-branch rewrites) does not enter `main`. |
| E2 | **Profile set pinned to {octo/v1}**: `interactiveByProfile` in PR-A knows only `octo/v1` — `octo/v2` is an *unknown* profile → 400 profile-mismatch (protocol Decision 10), which is exactly the P1 acceptance shape. The walker keeps its internal `interactive` flag (always false in P1) so PR-B is an additive diff, but **`interactive.go` / `inputs.go` and their tests do not land** in PR-A. |
| E3 | **Errcode subset**: PR-A registers only the P1 codes — `ErrMessageCardSendForbidden` / `ErrMessageCardEditForbidden` / `ErrBotAPICardDisabled` / `ErrBotAPICardInvalid` / `ErrBotAPICardOBOForbidden` / `ErrBotAPICardEditForbidden` / `ErrRobotCardEditForbidden` (+ zh-CN entries). P2-only codes (`ActionInvalid` / `ActionDenied` / `SeqConflict`) move to PR-B with their endpoint. |
| E4 | **No `card_seq`, no `message_extra` migration in PR-A**: P1 edit paths are blanket rejects (protocol Decision 7), so D9 CAS state has nothing to store. The POC's Redis-based `cardSeqStale` is P2 material and is NOT harvested (PR-B lands it properly as a `message_extra` column + conditional UPDATE). |
| E5 | **Display-text helper is the masking chokepoint** (net-new — POC only has a TODO): one local helper (wraps `common.GetDisplayText`, adds the type-17 branch) that takes the **stored sender identity** and returns `plain` for bot/webhook senders, `[卡片]` otherwise. Offline push, search hit projection, pin tips, conversation summaries, and quote previews all call it — no surface implements the check privately. Helper-level tests assert both directions once. |
| E6 | **incomingwebhook `msg_type:"card"`** (net-new): dispatch next to `"text"`/`"richtext"`, 8 KiB body cap unchanged, Decision-8 `text` fallback-seed semantics (used only when derivation is empty, ahead of `[卡片]`). |
| E7 | **Search hit projection** (net-new): type-17 mirrors `buildRichTextDetail` through the E5 helper (masking included). Index-side `searchText` emission is filed as a tracked issue on wukongim-message-indexer — explicitly not closed by PR-A. `MultipleForward` child docs inherit the same gap (documented, not solved). |
| E8 | **Pre-decode caps** (net-new): `http.MaxBytesReader` **2 MiB** (`cardmsg.MaxSendBodyBytes`) on `bot_api`/`robot` send+edit routes (protocol Decision 3b; POC only capped the P2 action route, which is not in PR-A). 2 MiB not 1 MiB: the route also carries RichText (payload limit 1 MiB), so a max-size RichText + JSON envelope exceeds 1 MiB of body — a 1 MiB cap would 413 legitimate RichText pre-decode. |
| E9 | **OBO fan-out**: extend `obo_fanout_content_type_test.go` to assert type-17 behavior explicitly (protocol brief load-bearing item). |
| E10 | **`docs/card-protocol.md`** (net-new deliverable): authored in PR-A, mirrors `pkg/cardmsg` (stated in the doc), and includes the full P2 action contract from the sibling brief so clients architect once. The D12 capability manifest is documented as P2-shipped (endpoint lands in PR-B). |
| E11 | **Merge preconditions ride the PR**: client-repo grep evidence for ContentType 17 + envelope field names attached as a PR comment; i18n make-target suite; guard-test lists; COMPREHENSION answers (this is a load-bearing change). |

## Load-bearing list

<!-- touches tags: wire-contract, trust-boundary, external-content, webhook,
     bot-api, i18n, error-response, testing -->

Everything in the protocol brief's Load-bearing list applies to this PR
verbatim (new ContentType wire contract, user/bot/robot ingress, edit-path
rejects, incomingwebhook push contract, offline push, search projection,
error responses, size/truncation invariants). PR-A additionally touches:

- `pkg/cardmsg/` (new package: envelope constants, `Validate`, `Finalize`,
  `BuildPlain`, display-text helper) — the single whitelist authority.
- `pkg/errcode/` + `pkg/i18n/locales/active.zh-CN.toml` (E3 subset).
- `modules/message` (send reject + user edit reject), `modules/bot_api`
  (send gate, OBO reject, bot edit reject, body caps), `modules/robot`
  (ingress validate + edit reject), `modules/webhook` (push masking),
  `modules/incomingwebhook` (`msg_type:"card"`), `modules/messages_search`
  (hit projection), pin display text (`modules/message`).
- Guard tests: new handler-bearing files join the modules'
  `Test<Module>NoLegacyResponseError` lists.

## Out of scope

- **All of P2** (sibling implementation task, “PR-B”): octo/v2 whitelist,
  `Input.*`/`Action.Submit`, `POST /v1/message/card/action` + D4 claim store,
  `card_action` typed events, `botMessageEdit` type-17 unlock, D9 `card_seq`,
  D10 revision history, D11 input validation, D12 capability manifest.
- octo-lib companion PR (ContentType constant upstream) — tracked separately.
- wukongim-message-indexer `searchText` emission (tracked issue, E7).
- Client renderers (three client repos; they implement against
  `docs/card-protocol.md`).
- octo-im: zero changes (deployment WS-frame-size check only).
- The POC branch itself (stays as evidence; deleted only after PR-B lands).

## Acceptance

- **The protocol brief's entire Acceptance section passes verbatim** — that
  list is the contract and is not duplicated here. Plus PR-A-specific gates:
- Profile pinning (E2): an envelope with `profile:"octo/v2"` (or any unknown
  profile) → 400 profile-mismatch; no P2 **Go symbol** appears in the PR's
  changed `.go` files (`grep -lE 'ProfileV2|EventTypeCardAction|
  cardActionClaim|CardSeqFromContentEdit|ValidateInputs|SubmitActionIDs'`
  is empty — harvest hygiene, machine-checkable). Prose references to the
  P2 contract in `docs/card-protocol.md` and code comments are expected —
  the doc MUST describe the P2 action contract per the protocol brief's
  acceptance.
- Masking chokepoint (E5): helper-level tests assert bot-sender → `plain`
  and user-sender → `[卡片]`; every listed surface routes through the helper
  (source guard: no surface file contains a private type-17 branch).
- `make i18n-extract && make i18n-extract-check && make i18n-lint` green;
  zh-CN entries for every E3 code; guard lists updated;
  `golangci-lint run ./...` clean.
- `go test` green across `pkg/cardmsg`, `modules/message`, `modules/bot_api`,
  `modules/robot`, `modules/webhook`, `modules/incomingwebhook`,
  `modules/messages_search` (MySQL+Redis; WuKongIM-backed edit-reject tests
  behind the existing skip guard).
- PR carries: Linked Spec (protocol brief), COMPREHENSION three questions,
  client-repo grep evidence (E11).
- **Rollout precondition restated**: `OCTO_CARD_MESSAGE_ENABLED` stays
  default-off; enabling any deployment waits for the client render-gate
  release (protocol Decision 2) — merge does NOT imply enable.
