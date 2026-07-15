---
type: Task
title: "Task: webhook-cardmsg-adapter"
description: GitHub/GitLab incoming-webhook adapters render their event subset as InteractiveCard(17) octo/v1 cards, degrading to the existing markdown text when OCTO_CARD_MESSAGE_ENABLED is off. Server-only; structured card layout; no new events, no changed ping/skip/no_event semantics.
tags: ["incomingwebhook", "adapter", "webhook", "card", "wire-contract", "trust-boundary", "external-content", "markdown", "url-destination", "i18n", "testing"]
timestamp: 2026-07-15T00:00:00Z
# --- octospec extension fields ---
slug: webhook-cardmsg-adapter
upstream: self
source: self
---

# Task: webhook-cardmsg-adapter

> Related specs (context, not restated here): the InteractiveCard wire contract
> lives in `.octospec/tasks/card-message-protocol/brief.md` and `pkg/cardmsg`
> (the single octo/v1 whitelist authority); `docs/card-protocol.md` mirrors it.
> This brief captures only the incomingwebhook GitHub/GitLab adapter change.

## Goal

Upgrade the **GitHub** and **GitLab** incoming-webhook adapters so their
rendered event subset is delivered as an **InteractiveCard (=17) octo/v1 card**
(structured header + body + a "View" `Action.OpenUrl`) instead of a single
markdown text line — while keeping the whole event pipeline (subset, ping,
skip, no_event, audit) byte-for-byte unchanged.

Because cards sit behind the deployment gate `OCTO_CARD_MESSAGE_ENABLED`
(default off, protocol Decision 2) and depend on the client render-gate, the
adapters **degrade to the existing markdown text path when the flag is off**.
Merge never breaks GitHub/GitLab delivery; enabling cards is an ops decision
gated on the client renderer (already shipped in octo-web — see Background).

## Background

- The push pipeline (`modules/incomingwebhook/api.go handlePush`) dispatches by
  `req.MsgType`: `""`/`text` → `buildPayload`, `richtext` → `buildRichTextPayload`,
  `card` → `buildCardPayload` (already exists, `card.go`). Adapters only translate
  the platform body into a `pushPayloadReq`; today github/gitlab return
  `{Content: <markdown>}` and ride the text path.
- `pkg/cardmsg` owns the octo/v1 whitelist authority: `Validate` (write-strict:
  element/action whitelist, 512 KiB, positive http(s) URL allowlist incl.
  TextBlock markdown links, node/depth caps) + `Finalize` (re-derives the
  server-authoritative `plain`, re-checks size). `buildCardPayload` already runs
  both on a caller-supplied `req.Card map[string]interface{}`.
- We are the **producer** here (we author the card server-side, unlike the
  native `msg_type:"card"` path which accepts caller AC JSON). `pkg/cardtmpl` is
  the reviewed server-side template precedent (`modules/notify` summary/docs
  cards): header + optional excerpt + FactSet + `Action.OpenUrl`, with
  `escapeMarkdown` on every leaf and `metadata.octo.{variant,source}`.
- **octo-web renderer is ready**: `packages/dmworkbase/src/Messages/InteractiveCard/`
  is a full octo/v1 pipeline; `senderTrust.classifyCardSender` trusts `iwh_*`
  senders (`renderDecision` renders their structured card, display-only —
  `interactive:false`). GitHub/GitLab cards (FromUID `iwh_*`, only whitelisted
  elements + `Action.OpenUrl`) render with **no web change**.
- Governing rule: `.octospec/rules/trust-boundary.md` (load-bearing) — escape at
  the boundary the caller can't cross, keep parity across sibling adapters,
  no URL-destination breakout, bound the payload.

## Load-bearing list
<!-- touches tags: adapter, webhook, external-content, trust-boundary, markdown,
     url-destination, wire-contract, i18n, card, testing -->

- **GitHub adapter render subset** (`adapter_github.go`): push / pull_request
  (opened/closed·merged/reopened/ready_for_review) / issues / issue_comment /
  release; ping → 200 no-msg; subset-outside → 200 skip `event`; missing
  `X-GitHub-Event` → 400 `no_event`. Subset and these outcomes MUST NOT change.
- **GitLab adapter render subset** (`adapter_gitlab.go`): Push / Tag Push /
  Merge Request / Issue / Note / Pipeline (terminal states only); `X-Gitlab-Token`
  second gate; skip / `no_event` semantics. MUST NOT change. `verifyGitLabToken`
  and the URL-token auth chain are untouched.
- **Trust boundary / external content**: VCS-controlled text (actor login/name,
  repo path, PR/issue/release titles, commit messages, comment snippets) is
  attacker-influenced. It must be escaped **at the card leaf** (TextBlock text,
  FactSet title/value) before delivery; all URLs (`Action.OpenUrl.url`, any
  Image url, any TextBlock markdown link) go through the positive http(s)
  allowlist. Prefer structured `Action.OpenUrl` over markdown link
  destinations inside TextBlock to avoid the destination-breakout surface.
- **Adapter parity**: whatever escaping/URL discipline the card path applies to
  GitHub must hold identically for GitLab; the two card producers share one
  escaper + one card-anatomy helper (no divergent leaf handling).
- **Card wire contract** (`pkg/cardmsg`): the produced `req.Card` envelope is
  validated by `cardmsg.Validate`/`Finalize` exactly like every other type-17
  producer — no bypass, server pins `card_version`/`profile`, `from.kind=webhook`,
  server-derived `space_id`; `RecheckPayloadSize` after mention expansion still
  applies (handlePush already calls it).
- **Server-authoritative `plain`**: `Finalize` derives `plain` from the card
  body (TextBlock/FactSet); it must be non-empty and readable (drives search /
  quote / offline push / conversation summary). The old markdown line's
  information is preserved on the card so derived plain stays informative.
- **Degrade path** (`OCTO_CARD_MESSAGE_ENABLED`): flag off → existing markdown
  text path, unchanged bytes. Flag on → card. Decision is per-push via
  `cardmsg.Enabled()`; no request context, so outbound language is the
  deployment default (`i18n.OutboundLanguage(context.Background())`, same
  discipline as notify/botfather).
- **Deliveries / audit**: adapter names stay `github`/`gitlab`; `status`/`reason`
  columns and body caps (1 MiB github/gitlab input; card output < 512 KiB via
  cardmsg) unchanged; card build failure must not silently drop — it degrades to
  text (never a new 400 on the happy path).
- **i18n**: card action label ("View on GitHub"/"在 GitHub 查看",
  "View on GitLab"/"在 GitLab 查看") localized via outbound-language negotiation;
  no hardcoded user-facing strings.

## Out of scope

- WeCom / Feishu / Multica adapters and the native `msg_type` paths
  (text/richtext/card) — untouched.
- Any new event type or action, or any change to the ping/skip/no_event/subset
  decisions — this is a rendering-format change only.
- octo-web / any client repo — renderer already ships octo/v1 + `iwh_` trust.
- `pkg/cardmsg` whitelist / validation semantics, `OCTO_CARD_MESSAGE_ENABLED`
  default, and the P2 interactive contract (Input.*/Action.Submit) — not touched.
- Enabling cards in any environment (ops decision, client-gate precondition).
- HMAC signature verification (`X-Hub-Signature-256` etc.) — remains a separate
  optional follow-up, out of this change.

## Acceptance

- **Flag off** (`OCTO_CARD_MESSAGE_ENABLED` unset/false): a GitHub `push` and a
  GitLab `Merge Request` event each deliver the **same markdown text payload as
  today** (type 1, `content` byte-identical to the pre-change renderer). Existing
  `adapter_github_test.go` / `adapter_gitlab_test.go` text assertions pass
  unchanged (flag-off is the default test env).
- **Flag on**: the same events deliver a type-17 card whose envelope passes
  `cardmsg.Validate` + `Finalize`; `payload["plain"]` is non-empty and contains
  the actor + action + repo (and, for push, the commit summary). A "View" action
  (`Action.OpenUrl`) points at the PR/issue/commit/pipeline/repo URL and passes
  the http(s) allowlist.
- **Trust boundary**: a table test proves a PR title / branch / actor name
  containing `]`, `)`, `*`, `[`, `<`, `javascript:`/`data:` URL does not (a)
  break out of a TextBlock/FactSet leaf into forged markup, nor (b) inject a
  non-http(s) actionable URL — parity-asserted for **both** github and gitlab.
- **Subset unchanged**: `ping` → 200 no message; subset-outside event → 200
  `skipped:"event"`; missing event header → 400 `no_event`; a
  `pull_request:synchronize` / GitLab MR `update` still skips — with the flag in
  either state.
- **Degrade on card-build failure**: if the card builder returns an error while
  the flag is on, the push still delivers the text payload (200), not a 400 —
  covered by a test forcing a build error.
- Gates green: `golangci-lint run ./...`; `go test ./modules/incomingwebhook/...`
  (MySQL+Redis); if any errcode/response touched, `make i18n-extract-check` +
  `make i18n-lint` and zh-CN entries; guard test
  `TestIncomingWebhookNoLegacyResponseError` file list updated if a new
  handler-bearing `.go` file lands.
- Scope hygiene: `git diff --stat` touches only `modules/incomingwebhook/**`
  (+ `docs/`/`.octospec/` + a shared card escaper if promoted to `pkg/cardmsg`);
  no octo-web change; no WeCom/Feishu/Multica/native-path diff.
