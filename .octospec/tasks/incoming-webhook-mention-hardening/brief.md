---
type: Task
title: "Task: incoming-webhook-mention-hardening"
description: Post-merge follow-up to #450 — fold invisible/confusable display names into the forged-broadcast guard, and add a handler→wire seam test, both flagged non-blocking on #450.
tags: ["incomingwebhook", "mention", "trust-boundary", "render-layer", "follow-up"]
timestamp: 2026-06-24T10:00:00Z
slug: incoming-webhook-mention-hardening
upstream: Mininglamp-OSS/octo-server#448
source: self
---

# Task: incoming-webhook-mention-hardening

> Follow-up PR after #450 (which closed #448). Picks up the two non-blocking P2s
> reviewers (yujiawei, Jerry-Xin, Octo-Q) recommended landing as a fast-follow.

## Goal

1. **Invisible/confusable guard.** `isBroadcastLikeName` (the forged-broadcast guard)
   matched on `strings.ToLower(strings.TrimSpace(name))` only. A display name that
   embeds a zero-width / bidi / full-width confusable — e.g. `所有<U+200B>人` (ZWSP
   *inside* the label) — slipped the prefix match yet renders as the visually-identical
   `@所有人`. Harden: NFKC-fold + strip Unicode format runes (category `Cf`) before the
   compare, and strip invisibles from the rendered `@name` too, so neither the
   comparison nor the emitted pill carries an invisible confusable.
2. **Wire-seam test.** Reviewers flagged that the composed content + generated entities
   were asserted at `buildMention`'s return, one layer below the wire. Extract
   `handlePush`'s payload assembly into a behavior-preserving `assemblePushPayload`, and
   add a MySQL test asserting the composed `payload["content"]` + entities reach the map
   handed to `SendMessageWithResult` — without a live WuKongIM send.

## Background

- Both items are **cosmetic / completeness**, not security-critical: the invisible-name
  spoof affects only the *visible* pill — broadcast routing (`humans`/`ais`) is set
  exclusively by the capability-gated `assembleMention`, never derived from pill text —
  so it cannot escalate to a real all-member ping. Closed as defense-in-depth on the
  token-in-URL push ingress. (Jerry-Xin's #450 comment pinned the exact `所有<U+200B>人`
  inside-label vector.)

## Load-bearing list

- **trust-boundary** — `isBroadcastLikeName` is the forged-broadcast guard; the fold must
  catch zero-width/bidi (`Cf`) + full-width (NFKC) confusables without over-blocking real
  names that merely continue a label into a longer word (`所有人事部`, `allen`).
- **wire-contract** — `assemblePushPayload` is a pure extraction; the outbound payload
  shape (content + `mention.{uids,entities}`) and the `content != req.Content` richtext
  write-back guard are byte-for-byte unchanged.
- **render-layer** — the rendered `@name` is stripped of invisibles; entity offsets/length
  remain UTF-16 over the (sanitized) composed content.

## Out of scope

- The other #450 P2 trust-model notes (raw `@所有人` literal in content without the
  capability bit; markdown inert-escaping of `@name`) — documented-intentional design,
  human-confirmed; no code change.
- The broadcast-prefix-vs-`maxContentRunes` ≤11 overshoot — accepted documented tradeoff.

## Acceptance

- `所有<U+200B>人` / `<U+202E>所有AI` / full-width `ａｌｌ` → skipped for render (folded to a
  broadcast label); a real name with an embedded ZWSP (`Bob<U+200B>`) → renders as `@Bob`
  (invisible stripped), entity length over the sanitized text.
- `所有人事部`, `allen` still render (boundary check preserved, no over-block).
- `assemblePushPayload`: composed `content` + generated entities land in the returned
  payload; richtext blocks array preserved (guard unchanged); behavior identical to the
  prior inline block.
- No literal invisible runes in module source; `go test ./modules/incomingwebhook/...`
  (DB-backed) + pure tests pass; `golangci-lint` clean.
