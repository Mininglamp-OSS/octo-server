---
type: Journal
title: "Journal: webhook-cardmsg-adapter"
description: GitHub/GitLab incoming-webhook adapters render their event subset as InteractiveCard(17) octo/v1 cards when OCTO_CARD_MESSAGE_ENABLED is on, degrading to the untouched markdown text path when off. GitLab pipeline cards carry a Branch/Status/Duration/Jobs FactSet. Server-only; octo-web renderer already ships octo/v1 + iwh_ trust.
tags: ["incomingwebhook", "adapter", "webhook", "card", "wire-contract", "trust-boundary", "external-content", "markdown", "url-destination", "i18n"]
timestamp: 2026-07-16T00:00:00Z
# --- octospec extension fields ---
task: webhook-cardmsg-adapter
upstream: self
source: self
---
# Journal: webhook-cardmsg-adapter

## What was done

The GitHub and GitLab incoming-webhook adapters now render their existing event
subset as an **InteractiveCard (=17) octo/v1 card** (structured header + body +
a "View on {GitHub|GitLab}" `Action.OpenUrl`) when `OCTO_CARD_MESSAGE_ENABLED`
is on. When the flag is off — or when a built card fails self-validation — they
**degrade to the existing markdown text path**, whose renderers are left byte-for-byte
unchanged (so the flag-off wire is identical to before, locked by the pre-existing
`adapter_github_test.go` / `adapter_gitlab_test.go` string assertions).

- `adapter_card.go` (new): the shared card model + assembler (`vcsCardData.card`),
  one escaper (`escapeCardText`, mirrors `cardtmpl.escapeMarkdown`) + `cardCodeSpan`
  used by **both** adapters (parity), `httpURLForCard` (positive http(s) allowlist,
  omits the button rather than failing on a bad URL), `validateVCSCard` (self-check
  via `cardmsg.Validate` → degrade), `vcsPushReq` (card-or-text selector),
  `vcsViewLabel` (localized button title), and the pipeline FactSet helpers.
- `adapter_github.go` / `adapter_gitlab.go`: per-event card builders next to their
  text siblings; the `parse` tail builds a card only when `cardmsg.Enabled()` and
  routes through `vcsPushReq`. The card object becomes `req.Card` and flows through
  the existing `msgTypeCard` branch (`buildCardPayload` → `Validate`/`Finalize` →
  server-derived `plain`, `from.kind=webhook`, server `space_id`). No handlePush change.

## Load-bearing decisions

- **Escape at the leaf, parity across siblings** (trust-boundary): every external
  VCS field (actor / repo / title / commit message / comment) is escaped at the card
  TextBlock/Fact leaf by one shared escaper; bold is `weight:Bolder` (not markdown
  `**`) and refs/SHAs are code spans, so escaped text can never re-open emphasis or a
  link. Primary navigation is a structured `Action.OpenUrl` (allowlisted), never a
  markdown destination — no destination-breakout surface. A parity table test drives a
  `] ) * [ javascript:` payload through **both** github and gitlab and asserts the
  authoritative `cardmsg.Validate` still passes (proof no live bad-URL/element leaked).
- **Degrade, never a new 400**: card-vs-text is chosen per push via `cardmsg.Enabled()`;
  the built card self-validates through the same `cardmsg.Validate` the ingress uses, so
  anything malformed falls back to text. No new errcode / response was added
  (`i18n-lint` + the `NoLegacyResponseError` guard both green; `adapter_card.go` added to
  the guard list).
- **Outbound language** = `i18n.OutboundLanguage(context.Background())` (deployment
  default, no request ctx) — same discipline as `modules/notify` / `pkg/cardtmpl`. The
  "View on {..}" and pipeline FactSet labels are localized in-Go (content labels, not
  errcodes).

## Deviation from brief (accepted mid-review)

The brief scoped this as a rendering-format change with "no new event fields". During
visual review the pipeline card was **enriched** (still the same `Pipeline Hook` event,
no subset change): it now renders a **FactSet — Branch / Status / Duration / Jobs (N)** —
parsing `object_attributes.duration` and the `builds[]` array (both previously unparsed).
This is **card-only**: the text path is untouched, so flag-off bytes stay identical. The
headline (`Pipeline #<id>`) carries the status color (Good/Attention/Warning).

## Verification

- `go test ./modules/incomingwebhook/ -run '<card + adapter + guard>'` green;
  `golangci-lint run ./modules/incomingwebhook/...` = 0 issues; `gofmt` clean;
  `make i18n-lint` green.
- Truthful render: a throwaway test dumped the **actual** card JSON from the shipped
  builders; it was rendered headless with the real `adaptivecards@3.0.6` + octo-web's
  own `--wk-*` theme tokens + a host config mirroring `octoHostConfig` — output matches
  the intended design in light and dark, and the hostile push card shows the injection
  rendered literally with no "View" button (its `javascript:` repo URL was dropped).

## Follow-ups / notes

- octo-web: **no change** — `InteractiveCard` renderer already ships octo/v1 and trusts
  `iwh_` senders (`senderTrust.classifyCardSender` → webhook → display-only render).
- WeCom / Feishu / Multica adapters and the native `msg_type` paths are untouched.
- HMAC signature verification (`X-Hub-Signature-256` etc.) remains a separate optional
  follow-up, out of this change.
- Enabling cards in any deployment stays an ops decision gated on the client
  render-gate (`OCTO_CARD_MESSAGE_ENABLED` default off).
