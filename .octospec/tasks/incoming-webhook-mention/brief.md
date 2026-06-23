---
type: Task
title: "Task: incoming-webhook-mention"
description: Add caller-controlled @mention (users + bots, one or many) to the native incoming-webhook push endpoint, with per-webhook capability switches for the two broadcast forms.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary"]
timestamp: 2026-06-23T13:15:14Z
# --- octospec extension fields ---
slug: incoming-webhook-mention
upstream: self
source: self
---

# Task: incoming-webhook-mention

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work.

## Goal

Let an incoming-webhook caller @mention **specific group members (users and/or
bots, one or many)** and optionally broadcast **@所有人 (humans)** and
**@所有 AI (bots)** from the **native** push endpoint. Mentioned humans get the
existing `[有人@我]` red-dot; mentioned bots get delivery (no red-dot), all via
the **existing** message-module reminder listener — this task only puts a
well-formed `mention` map on the wire.

The two broadcast forms are abuse-sensitive (a leaked webhook token spamming the
whole group), so each is gated by a **per-webhook capability switch**, default
**off**, toggled through the management API by any legitimate member who manages
the webhook (its creator, or a group admin) — the per-webhook, default-off switch
is the control, not a separate admin grant. Targeted `@uid` needs no switch but is
constrained to current group members + a hard cap. (Revoking members' ability to
flip these later is an additive group-level or system-setting policy ANDed at the
push read path — see the journal/notes; no schema change.)

## Background

- Today `buildPayload` (`modules/incomingwebhook/api.go`) emits only
  `type/content/from/space_id` and **discards `extra`**; there is no mention
  support (`@all/@here` is left as literal text). The code comment there is the
  sanctioned extension point: add an **explicitly whitelisted** field, never a
  passthrough.
- The wire mention contract is the `mention` sub-map (`uids/humans/ais/entities`)
  owned by `pkg/mentionrewrite` and read by `modules/message/api_reminders.go`
  (`getMention`). Reminder generation is **sender-agnostic** (a WuKongIM message
  listener), so a webhook message carrying `mention.uids/humans` fires reminders
  with no message-module change.
- `mention.ais=1` is expanded to bot-member UIDs by
  `pkg/mentionrewrite.ExpandAisToBotUIDs`, fed by a per-ingress composer
  (`GetMembers` + `robot.ExistRobot`) — mirror `modules/bot_api/mention_expand.go`.
- Decisions locked with maintainer: native endpoint only; caller supplies UIDs
  (no name resolution); both broadcast switches default off; UID cap = 50.
- Industry parity: structured-field opt-in (caller must set `all/bots:true`) ≈
  Discord `allowed_mentions`; the per-webhook capability ≈ Slack "who can
  @channel" but at webhook-resource granularity.

## Load-bearing list

- **wire-contract** — `mention.{uids,humans,ais}` shape must match what
  `pkg/mentionrewrite` + `message/api_reminders.go getMention` already consume
  (UID strings; truthy-one flags). Do not invent a new shape.
- **external-content / trust-boundary / webhook** — the push endpoint is an
  attacker-controlled, unauthenticated (token-in-URL) ingress. New field must be
  validated at the boundary: membership gate, dedup, count cap; mention is a
  **whitelisted structured field**, never `extra` passthrough.
- **space / isolation** — mention targets MUST be filtered to current members of
  the webhook's `group_no`; a token must not ping arbitrary platform users or
  cross-Space identities.
- **error-response / i18n** — any new management-side validation reuses
  `httperr.ResponseErrorL` + a registered `pkg/errcode` code; push-side
  "ignored/unresolved" feedback is non-fatal data in the 200 body, not an error.
- **adapter parity (native-only)** — mention is gated to the native adapter via a
  `pushAdapter` capability flag; sibling adapters (wecom/feishu/github/gitlab/
  multica) neither parse nor emit mention. Adding a feature only to native is
  safe (no new attack surface for siblings); the parity rule about *escaping*
  still holds because mention carries structured UIDs, not rendered markdown.
- **bot-api / robot wiring** — `IncomingWebhook` gains a robot dependency
  (`ExistRobot` / bot-member enumeration) it does not currently hold.
- existing push pipeline (auth → rate-limit → group-active → creator-membership
  → audit) and `buildRichTextPayload` must be unaffected except for the new
  mention insertion point.

## Out of scope

- Name/username/email → UID resolution (`mention.names`).
- Mention via platform adapters (WeCom/Feishu/GitHub/GitLab/Multica).
- `mention.entities` offset/pill generation (v1: `uids` drive the ping; the
  literal `@name` in `content` is plain text). Pills are a later phase.
- `mention.all` legacy field semantics and any change to the message-module
  reminder/read path.
- Group-level mention policy (`allow_no_mention` / `bot_mention_pref`).

## Acceptance

- Native push with `mention.uids=[<member uids>]` → delivered payload carries
  `mention.uids` containing **only** current group members, **deduped**, **≤50**;
  non-members and blanks dropped.
- `mention.all=true` + webhook `allow_mention_all=1` → payload `mention.humans=1`;
  with `allow_mention_all=0` → no `humans`, and 200 body reports it ignored.
- `mention.bots=true` + webhook `allow_mention_bots=1` → `ExpandAisToBotUIDs`
  appends bot-member UIDs; with `allow_mention_bots=0` → not expanded + reported.
- A non-native adapter (e.g. wecom) with a `mention` field in its body → no
  `mention` in the delivered payload.
- `create`/`update` accept + persist `allow_mention_all` / `allow_mention_bots`
  (default 0); `webhookResp` surfaces them.
- Empty/absent `mention`, malformed `mention.uids` → no panic, no `mention` key
  (full backward compatibility with existing native callers).
- `go test ./modules/incomingwebhook/...` passes incl. a new mention test +
  `TestIncomingWebhookNoLegacyResponseError`; `make i18n-extract-check` +
  `make i18n-lint` pass; `golangci-lint run ./modules/incomingwebhook/...` clean.
