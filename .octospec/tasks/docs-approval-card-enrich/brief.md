---
type: Task
title: "Task: docs-approval-card-enrich"
description: Enrich the docs access-request approval card layout + add a deny-reason capture flow
tags: [cardtmpl, notify, docs-approval, card-message]
timestamp: 2026-07-17T00:00:00Z
# --- octospec extension fields ---
slug: docs-approval-card-enrich
upstream: self
source: self
---

# Task: docs-approval-card-enrich

## Goal
Bring the docs "access request" approval card (通知助手 → 文档申请权限卡) closer to the
approved prototype, and add a reviewer **deny-reason** capture flow:

1. **Pending card (待处理)** — richer AC layout: a header line (`文档申请 · <space/doc
   scope>` + a status label), a large document title, a requester row (optional
   avatar + name + role + timestamp), a boxed "申请原因" section, and the
   existing `查看详情 / 拒绝 / 允许` actions.
2. **Deny reason** — clicking 拒绝 opens a dialog (frontend) to collect a required
   reason; the reason rides to the docs backend via the existing
   `DecisionRequest.Inputs` channel under a **declared** `Input.Text` id.
3. **Terminal card (已允许 / 已拒绝)** — the finalizer rebuild gets the same header
   style + a result box; the denied state surfaces the reviewer's reason.

## Background
- Card is built server-side as an AdaptiveCard 1.5 doc, rendered client-side by
  the official `adaptivecards` SDK under a custom octo HostConfig.
- Pending card: `pkg/cardtmpl/docs_action.go` `BuildDocsAccessRequestCard` (today
  reuses the plain `BuildDocsResourceCard` body). Producer:
  `modules/notify/card.go` `buildDocsAccessRequestCard` (ProfileV2, feature flag
  `OCTO_DOCS_APPROVAL_CARD_ENABLED`).
- Terminal card: `modules/notify/action_finalizer.go`
  `DocsActionFinalizer.buildTerminalDocument` (only has `title + state`; ignores
  `event.Inputs`).
- Deny reason channel: submit `inputs` are validated against **declared** card
  `Input.*` ids (`pkg/cardmsg/inputs.go` `ValidateInputs`), flow into
  `cardactiondispatch.Event.Inputs` → `DecisionRequest.Inputs` (HMAC-signed POST
  to docs backend). No reason field otherwise exists.
- Both server (`pkg/cardmsg/validate.go`) and client
  (`validateCardForOcto.ts`) whitelists are **type/structure gates, lenient on
  style props**; all enrichment props pass. **Images must be absolute https**
  (data:/http: stripped).
- `DocsCardFields` carries only pre-formatted strings; no actor uid/avatar today.

## Load-bearing list
- **wire-contract** — additive optional field `actor_avatar_url` on
  `DocsCardFields` (cross-repo docs-notify contract); a hidden `Input.Text`
  `deny_reason` declared in the card frame (contract for the submit `inputs`
  channel). Additive-only; existing payloads keep working.
- **escape / markdown** — card renders caller-supplied strings (actor name,
  title, reason). Must keep `escapeMarkdown` on every rendered field so a
  crafted value can't inject markdown/links (`pkg/cardtmpl/resource.go`).
- **url-destination / trust-boundary** — `actor_avatar_url` is a rendered image
  URL; must pass the positive https allowlist (`requireHTTPS`) before it lands
  in card bytes, same discipline as `IconURL`.
- **i18n** — new localized labels (header label, status badges, role,
  result-box copy) added to `docsLabels` (zh-CN + en-US), resolved via
  `i18n.OutboundLanguage`. No hardcoded user-facing strings.
- **space** — the docs card delivery path (member verification, space_id in the
  envelope) must be unchanged; enrichment is body-only.
- **card-budget** — enriched card stays within `MaxNodes=200` / `MaxDepth=16`
  and passes `cardmsg.Validate` on the server and `validateCardForOcto` on the
  client.

## Out of scope
- Docs-backend changes (it owns `actor_avatar_url` population and the
  `DecisionRequest.reason` consumption). We only make the channels available.
- Right-aligning the action row / footer meta line and the pill-shaped badge
  background (would need octo HostConfig / a per-variant client CSS change);
  status shown as colored text this round.
- Any change to the generic card/action dispatch, rate-limit, or auth layers.
- Reproducing the full requester row + reason box in the **terminal** card
  (finalizer lacks actor/timestamp; it shows header + title + result box only).
- Server-side "reason required" enforcement as a new errcode (kept client-side +
  docs-backend business rule; `isRequired` is not a server card gate).

## Acceptance
- `go test ./pkg/cardtmpl/... ./modules/notify/...` passes, including new tests:
  - enriched pending card passes `cardmsg.Validate` (ProfileV2), declares
    `Input.Text` id `deny_reason`, and carries the two submit actions.
  - `actor_avatar_url` renders an Image only when https; empty → no image;
    non-https → build error (mirrors `IconURL`).
  - actor name / reason are markdown-escaped in the card bytes.
  - terminal denied card includes the reason from `event.Inputs["deny_reason"]`;
    approved card shows the granted result box.
- `make i18n-extract-check` + `make i18n-lint` pass; zh-CN entries added for any
  new registered codes (none expected — labels are template-local, not errcodes).
- `golangci-lint run ./pkg/cardtmpl/... ./modules/notify/...` clean.
- octo-web: `cd apps/web && pnpm test` (card interaction + new deny-dialog tests)
  and `pnpm lint` pass.
- A re-rendered preview (real `adaptivecards` SDK) of the enriched pending +
  terminal cards matches the approved demo direction.
