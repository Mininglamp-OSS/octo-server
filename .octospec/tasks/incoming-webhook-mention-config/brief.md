---
type: Task
title: "Task: incoming-webhook-mention-config"
description: Move incoming-webhook @mention from a caller-supplied push-body param to webhook create/update config; mention then applies to every adapter endpoint, not just native.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary", "webhook", "external-content", "space", "isolation", "error-response"]
timestamp: 2026-06-25T00:00:00Z
# --- octospec extension fields ---
slug: incoming-webhook-mention-config
upstream: self
source: self
---

# Task: incoming-webhook-mention-config

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. PRD agreed with the user before coding.

## Goal

Move the incoming-webhook `@mention` target list from a **caller-supplied push
body param** to **webhook create/update configuration**. The push endpoint no
longer reads `mention` from the request body (silently ignored). Because the @
targets now live on the webhook row (server-side octo UIDs) instead of the body,
mention applies to **every** push shape — native **and** every platform adapter
(github/wecom/gitlab/feishu/multica) — not just native.

## Background

- Today the push body carries `mention {uids, all, bots, render, entities}`
  (`modules/incomingwebhook/mention.go buildMention`, parsed from
  `pushPayloadReq.Mention`). Directed `uids` come from the caller; broadcast
  `all/bots` are gated by per-webhook switches `allow_mention_all` /
  `allow_mention_bots`; `render` resolves uids → `@name` pills; `entities` is
  caller-supplied pill ranges.
- Mention is processed **only by the native adapter** (`pushAdapter.allowMention`,
  `adapter.go:61`) — the comment there says platform adapters can't support @
  because their body is a platform-generated event with no octo UID semantics.
  That blocker disappears once targets come from config.
- The wire `mention.{uids,humans,ais,entities}` contract (owned by
  `pkg/mentionrewrite`, consumed by `modules/message/api_reminders.go`) is
  unchanged; this task only changes where the input comes from.

## Load-bearing list

- **wire-contract** — delivered `mention.{uids,humans,ais,entities}` shape must
  stay exactly what `pkg/mentionrewrite` + `message/api_reminders.go getMention`
  consume; only the *source* (config vs body) changes.
- **trust-boundary / external-content / webhook** — push is attacker-controlled,
  token-in-URL ingress. The body `mention` is now untrusted noise → dropped. The
  config `mention_uids` is validated at the management boundary (member gate +
  dedup + cap) and **re-filtered to current members at push time** (config-time
  members may later leave).
- **adapter parity** — mention now runs for ALL adapters identically (the
  native-only `allowMention` gate is removed). No adapter is left diverging.
- **space / isolation** — `mention_uids` targets MUST be current internal-normal
  members of the webhook's `group_no`; a token can't ping arbitrary/cross-Space
  users. Same gate as the prior push path (`filterGroupMembers`).
- **error-response / i18n** — create/update validation reuses
  `httperr.ResponseErrorL*` + existing `errcode.ErrIncomingWebhookRequestInvalid`
  with `reason=mention_uids`; push-side "ignored" broadcast stays non-fatal 200
  body data. No new error code.
- existing push pipeline (auth → rate-limit → group-active → creator-membership
  → audit) and `buildRichTextPayload` unaffected apart from the mention source.

## Out of scope

- Per-push caller selection of mention (explicitly removed).
- Name/email → UID resolution; cross-group / cross-Space @.
- Caller-supplied `mention.entities` (server generates pills via render).
- Inline pill placement (still prepended at content head).
- Group-level mention policy (`allow_no_mention` / `bot_mention_pref`).

## Acceptance

- `create`/`update` accept + persist `mention_uids` (deduped, ≤ cap, all current
  group members; non-member or over-cap → 400 `reason=mention_uids`);
  `webhookResp` surfaces `mention_uids`.
- A configured webhook's native push (no body mention) delivers payload with
  `mention.uids` = configured members (re-filtered to current members) +
  generated `@name` pills (`mention.entities`).
- `allow_mention_all=1` / `allow_mention_bots=1` → every push carries
  `mention.humans=1` / `mention.ais=1` (subject to `broadcastPermitted`); members'
  broadcast is revoked instantly by `member_can_broadcast=0` (reported `ignored`).
- A push **body** containing `mention` has **no effect** (200, no mention from it).
- The SAME configured webhook pushed via a platform adapter (e.g. wecom) also
  carries the configured mention (adapter parity); github `ping`/skip events
  deliver nothing and @ nobody.
- Config target who left the group is dropped at push (re-filter).
- Empty/absent `mention_uids` + switches off → no `mention` key (backward
  compatible with no-@ webhooks).
- `go test ./modules/incomingwebhook/...` passes incl.
  `TestIncomingWebhookNoLegacyResponseError`; `make i18n-extract-check` +
  `make i18n-lint` pass; `golangci-lint run ./modules/incomingwebhook/...` clean.
