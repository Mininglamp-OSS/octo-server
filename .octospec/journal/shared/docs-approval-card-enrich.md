---
type: Journal
title: "docs-approval-card-enrich: richer docs access-request card + reviewer deny-reason flow"
description: Enrich the docs access-request approval card (header/status, big title, requester row, reason box) across pending + terminal states, and add a reviewer deny-reason dialog whose value rides the existing declared-input channel to the docs backend.
tags: [cardtmpl, notify, docs-approval, card-message, i18n, trust-boundary, wire-contract, testing]
timestamp: 2026-07-17T00:00:00Z
slug: docs-approval-card-enrich
---

# docs-approval-card-enrich

Branch `claude/docs-permission-card-template-bccd8h` (octo-server + octo-web).
Brings the 通知助手 「文档申请权限」 card closer to the approved prototype and adds a
reviewer deny-reason capture flow.

## What was done

### octo-server
1. **Enriched pending card** — rewrote `pkg/cardtmpl/docs_action.go`
   `BuildDocsAccessRequestCard` to emit a structured AC 1.5 body via
   `DocsApprovalContent`: header row (source label + colored status), big title
   (`separator`), a bold-actor banner (`RichTextBlock`), a requester
   `ColumnSet` (optional Person avatar + name/role + timestamp), an
   `emphasis` reason box, and a **hidden `Input.Text id="deny_reason"`**. All
   caller strings run through `escapeMarkdown`; the avatar goes through
   `requireHTTPS`. Primary sentences sized `Medium` (14px), labels/meta `Small`
   (12px), title `ExtraLarge` → a 20/14/12 hierarchy.
2. **Terminal card** — new `BuildDocsApprovalOutcomeCard` (header + title +
   good/attention result box; denied box surfaces the reviewer reason).
   `modules/notify/action_finalizer.go` `buildTerminalDocument` uses it for
   approved/denied and threads `event.Inputs["deny_reason"]`; cancelled/
   unavailable keep the plain rebuild. view-details is a **body ActionSet**, not
   a root action, so the terminal card carries no decision buttons (preserves the
   existing "terminal removes approval actions" invariant).
3. **Contract + labels** — additive optional `DocsCardFields.ActorAvatarURL`
   (https, docs-backend owned); new zh-CN/en-US `docsLabels`
   (header/status/banner/role/reason/result copy). deny action `data` carries
   `doc_title`/`actor` as local display context for the web dialog.
4. `scripts/cardpreview` emits the three new states for offline SDK preview.

### octo-web
5. **Deny dialog** — `denyReasonDialog.tsx`: `InteractiveCardCell.handleSubmit`
   intercepts the docs deny action (matched by `data.owner/action_type/decision`),
   opens a `wkConfirm` danger modal with a required reason (≤200), then
   `performSubmit` merges `inputs[deny_reason]`. Approve/other paths unchanged.
   i18n keys + dialog CSS added.

## Why it works end-to-end

The deny reason must ride under a **declared** input id — the card/action
endpoint fail-closes on undeclared inputs (`pkg/cardmsg.ValidateInputs`). The
hidden `deny_reason` input declares it; the web dialog fills `inputs.deny_reason`;
it flows `Event.Inputs → DecisionRequest.Inputs` (signed POST) to the docs
backend **for free**, and the finalizer reads the same value onto the denied
terminal card.

Both the server (`pkg/cardmsg/validate.go`) and client
(`validateCardForOcto.ts`) card whitelists are type/structure gates that are
lenient on presentation props, so all enrichment (`Container.style`, `color`,
`size`, nested `ColumnSet`, `Image.style=Person`) passes; **images must be
absolute https** on both sides (data:/http: stripped), which is why the avatar is
a validated https field, not a data URI.

## Verification
- `go test ./pkg/cardtmpl/... ./modules/notify/{card,finalizer}` (new tests:
  enriched layout + declared deny_reason input + https-only avatar + markdown
  escaping + terminal approved/denied + reason threading); golangci-lint 0
  issues; gofmt/vet clean; `make i18n-extract-check` + `make i18n-lint` pass;
  full `go build ./...` + card-dispatch lint pass.
- octo-web: 221 InteractiveCard vitest pass incl. new `denyReasonDialog` suite.
- Faithful render preview via real `adaptivecards@3.0.6` SDK + octo HostConfig
  (cardpreview JSON → headless Chromium).

## Out of scope / follow-ups
- docs-backend owns `actor_avatar_url` population and `DecisionRequest`
  reason consumption.
- Terminal card shows header + title + result box only (finalizer lacks
  actor/timestamp); a fuller terminal requires docs-backend echoing them.
- Pill-shaped status badge + right-aligned footer/meta line would need a
  per-variant client renderer / HostConfig change (status is colored text this
  round).

## Learning
When a card needs a free-text value on submit, the value MUST be declared as an
`Input.*` id in the server-authored frame even if the UI collects it in a custom
modal — `ValidateInputs` drops undeclared keys fail-closed. A hidden
`Input.Text` is the minimal declaration; the frontend can then own the capture UX
and just populate `inputs[<id>]`.
