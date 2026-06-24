---
type: Task
title: "Task: incoming-webhook-mention-broadcast"
description: Auto-compose the visible broadcast pill (@所有人 / @所有AI) into native incoming-webhook text content when the capability-gated broadcast flag is set, so all three clients render a pill instead of only firing the red-dot.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary", "render-layer"]
timestamp: 2026-06-24T06:30:00Z
# --- octospec extension fields ---
slug: incoming-webhook-mention-broadcast
upstream: Mininglamp-OSS/octo-server#448
source: self
---

# Task: incoming-webhook-mention-broadcast

> Follow-up to incoming-webhook-mention (#445 functional layer, #449 directed
> entities). This is the deferred **broadcast** half of #448 item ②.

## Goal

When a native incoming-webhook push sets `mention.all` (@所有人 / humans) or
`mention.bots` (@所有AI / ais) **and** the per-webhook capability bit is permitted
(`broadcastPermitted`), the server **prepends the canonical broadcast literal**
(`@所有人` for humans, `@所有AI` for ais) + a trailing space to the **text**
content, so every client renders a visible broadcast pill.

Today (#445) those flags fire the red-dot / notify / bot-summon but render **no
pill** unless the caller also writes the literal into `content` (the #448 item ②
gap). This task closes that gap at the render layer only — routing semantics
(`humans`/`ais` flags, red-dot, bot expansion) are untouched.

## Background

- **#448 item ②** ("Broadcast ergonomics"): `mention.all` / `mention.bots`
  "fires the notify/bot but renders no pill unless the caller also writes
  `@所有人` / `@所有AI` into `content`. Option: backend auto-prepends the fixed
  broadcast strings when the capability-gated flag is set." This task takes that
  option.
- **Cross-client analysis** (posted on #448; re-verified against the three
  client repos): **all three clients require the broadcast literal to be present
  in the content text to render a pill — none fabricate a pill from the flag
  alone.**
  - iOS `WKMentionService.parseMention` regex-scans `content` for a
    locale-independent broadcast-label set; absent literal → no token.
  - Android `WKUIChatMsgItemEntity` uses the flags (`mentionAll==1` /
    `mentionHumans==1` / `mentionAis==1`) only to **gate** an `indexOf`
    highlight of a literal **already in content**; absent literal → no pill.
  - Web `buildMessageMentions` synthesizes a `{name:"@所有人",uid:"all"}` render
    entry from the flag, but `MarkdownContent.segmentText` only styles it where
    the **name matches the content text** (regex over `content`); absent literal
    → inert entry, no pill.
- **Canonical literals**: `@所有人` (humans), `@所有AI` (ais) — confirmed by
  Android `strings.xml` (`所有人` / `所有AI`) and the iOS/Android hardcoded
  locale-independent token sets. Clients also recognize English aliases
  (`@All People` / `@All AIs` / `@all`) on **read**, but the canonical token the
  pickers **insert** is the Chinese one; the server emits the Chinese canonical.
- **Capability gate** is already computed in `handlePush`
  (`broadcastPermitted = settings.IncomingWebhookMemberCanBroadcast() ||
  creatorIsAdmin`) and applied in `assembleMention`
  (`AllowMentionAll==1 && broadcastPermitted`, same for bots). Auto-compose must
  fire **only** for a flag that survives that gate — a flag reported in
  `mention_ignored` must NOT compose a pill.
- **Trailing space is mandatory**: Android's highlight skips a hit whose next
  char is a letter/digit/`_` (CJK counts as a letter, so `@所有人执行` would be
  skipped); iOS `@\S+`/`\b` and web regex also need the delimiter. A single ASCII
  space delimits the token from the following text.
- **Coexistence with directed entities (#449)**: web and Android bind directed
  pills by **UTF-16 offset**. Prepending text shifts the content, so every
  surviving directed-entity offset must be increased by the prepended prefix's
  **UTF-16 code-unit** length. iOS binds directed mentions **positionally** and
  skips broadcast tokens for the `uids` index, so iOS is unaffected either way.

## Load-bearing list

- **wire-contract** — only the text `content` and the existing
  `mention.{humans,ais,entities}` keys are touched. The prepended token is the
  exact canonical literal the clients parse; no new wire field. With no broadcast
  flag set (or none permitted), `content` and the payload are **byte-identical**
  to today.
- **trust-boundary / external-content / webhook** — the push endpoint is an
  attacker-controlled, token-in-URL ingress. The prepend is **server-controlled
  fixed text** (a compile-time constant; the caller only flips a boolean), gated
  by the per-webhook capability bit. The literal contains no markdown/regex
  metacharacters, so it adds no injection/escape surface (trust-boundary escape
  rule still satisfied — structured token, not caller-rendered markdown).
- **broadcast capability gate** — compose fires only when the flag survives
  `AllowMention* == 1 && broadcastPermitted`; an un-permitted (ignored) flag must
  not compose a pill (no pill for a broadcast that did not route).
- **render-layer / entities offset consistency (#449)** — prepending shifts
  `content`; directed-entity offsets emitted by `finalizeEntities` must shift by
  the prefix's UTF-16 length so web/Android keep binding the right member; the
  `'@'` anchor still holds at `offset+prefixLen` because a prepend only moves
  text rightward.
- **adapter parity (native-only)** — compose only on the native path
  (`ad.allowMention`); sibling adapters (wecom/feishu/github/gitlab/multica)
  neither set the flags nor compose.
- existing push pipeline (auth → rate-limit → group-active → creator-membership
  → length cap → audit) and the `richtext` path must be unaffected except the
  new text-path content prepend.

## Out of scope

- **Richtext broadcast pills** — compose is **text-path only** (mirrors entities,
  which are text-only); the richtext path keeps today's flags-only behavior
  (humans/ais set, no literal). A future task can add block-level broadcast pills.
- **i18n / English-alias canonicalization of the inserted literal** — the server
  always emits the Chinese canonical (`@所有人` / `@所有AI`), which every client
  recognizes locale-independently. No per-locale token.
- **De-dup against caller-written English aliases** — idempotency checks only the
  canonical literal. A caller that both writes an English alias (`@all`) AND sets
  the flag may see a minor double-pill; documented, not handled (keeps the server
  decoupled from the clients' alias list).
- **Routing semantics** — red-dot / notify / bot-summon / `ExpandAisToBotUIDs`
  are unchanged; this is purely the visible render layer.
- Name resolution; directed-uid / entities validation behavior (#445/#449).

## Acceptance

- `all=true` + `allow_mention_all` permitted + literal absent → delivered
  `content == "@所有人 " + original`; `mention.humans == 1`.
- `bots=true` + `allow_mention_bots` permitted + literal absent → `content`
  prepended `"@所有AI "`; `mention.ais == 1`.
- both permitted + both absent → `content` prepended `"@所有人 @所有AI "`
  (humans first).
- flag set but **not** permitted (capability off) → `content` unchanged, no
  prepend, flag still surfaced in `mention_ignored` (existing behavior), no pill.
- idempotency: canonical literal already present in `content` → that token is
  **not** prepended again (exactly one occurrence remains).
- directed entities (#449) coexisting with a prepend → every surviving entity
  `offset` is increased by the prefix's UTF-16 length, and the entity still
  anchors a `'@'` in the final content (parity test with an emoji/CJK prefix).
- `richtext` path with broadcast flags → no content prepend (no top-level
  `content` key mutated); `humans`/`ais` still set.
- no broadcast flag / flags unset → `content` byte-identical to today (backward
  compatibility with existing native callers, incl. directed-only and plain).
- non-native adapter with broadcast fields → no compose.
- `go test ./modules/incomingwebhook/...` passes incl. new broadcast tests +
  `TestIncomingWebhookNoLegacyResponseError`; `golangci-lint run
  ./modules/incomingwebhook/...` clean (govet, the CI gate).
