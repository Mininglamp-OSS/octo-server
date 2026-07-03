---
type: Task
title: "Task: system-bot-dm-history"
description: System-bot DM history must not be Space-filtered — botfather/fileHelper/u_10000 returned empty from /message/channel/sync under an X-Space-ID.
tags: [space, isolation, message, bot, systembot]
timestamp: 2026-07-03T00:00:00Z
# --- octospec extension fields ---
slug: system-bot-dm-history
upstream: octo-server (regression of YUJ-219-A / #1283)
source: self
---

# Task: system-bot-dm-history

## Goal

Stop `filterPersonMessagesBySpace` from dropping a **system bot**'s DM history.
`POST /v1/message/channel/sync` for a system bot (`botfather` / `u_10000` /
`fileHelper` / `notification`) returns `messages: []` whenever the request
carries a validated `X-Space-ID`, because the bot's messages are Space-agnostic
(`payload.space_id == ""`) and the filter's system-bot branch drops all untagged
system-bot messages. System bots are Space-**independent** and must always be
visible, so their history must never be Space-filtered.

## Background

`filterPersonMessagesBySpace` (YUJ-219-A / #1283, commit `e39b69f`, 2026-05-04)
added message-level Space filtering to close a cross-Space DM-history leak. Its
rule 4 (`payload.space_id == "" && isSystemBot → drop`) hides untagged system-bot
history — but this directly contradicts the invariant in `pkg/space/query.go`
("系统 Bot … 必须始终对客户端可见") and the conversation-list contract
(`EnsureSystemBotsPresent` forces system bots into every Space's list). Net effect:
the conversation list shows botfather, but opening it returns empty history.

This is **not** introduced by #484 (PR #519): that PR only changed the
`!isSysBot` (regular-DM) branch (untagged history → default-Space-only); the
system-bot branch is byte-identical before and after #484. Read-time filter only
— no stored data is deleted; the fix restores history immediately, no backfill.

## Load-bearing list

- `modules/message/space_filter.go` `filterPersonMessagesBySpace` — message-level
  Space filter for the DM history endpoint (`/v1/message/channel/sync`).
- Invariant `pkg/space/query.go` `SystemBots` / `IsSystemBot` — system bots are
  Space-independent and always visible.
- Conversation-list contract `EnsureSystemBotsPresent` / `decideConvKeepInSpace`
  (`SystemBots[channelID] → keep`) — must stay consistent with history visibility.
- #484 symptom-1 rule (regular untagged DM → default-Space-only) — must NOT
  regress.

## Out of scope

- Client-side `filterSystemBotMessages` (Android/iOS) — if the clients also
  Space-filter system-bot history, they need a matching change; tracked separately.
- The regular-DM (`!isSystemBot`) rules — unchanged.
- Any storage / migration / backfill (read-time filter only).

## Acceptance

- `filterPersonMessagesBySpace(msgs, "<systembot>", spaceID, defaultSpaceID)`
  returns the **full** list unchanged for every system bot, in both default and
  non-default Spaces, incl. untagged messages.
- #484 guard: a regular peer's untagged DM is still kept only in the default
  Space and dropped in a non-default Space.
- `go test -race ./modules/message/ ./pkg/space/` green; CI green.
