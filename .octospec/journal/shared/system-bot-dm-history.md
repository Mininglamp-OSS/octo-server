---
type: Journal
title: "system-bot-dm-history: system-bot DM history must not be Space-filtered"
description: Exempt system bots from filterPersonMessagesBySpace so botfather/fileHelper/u_10000 history is not returned empty under an X-Space-ID
tags: [space, isolation, message, bot, systembot]
timestamp: 2026-07-03T00:00:00Z
---

# system-bot-dm-history

Branch `fix/system-bot-dm-history` (based on latest `origin/main`, which already
carries #484 / PR #519).

## What was done

`filterPersonMessagesBySpace` (`modules/message/space_filter.go`) now **early-returns
the full message list for system bots** (`spacepkg.IsSystemBot(channelID)`), before
the per-message Space switch. System bots (`botfather` / `u_10000` / `fileHelper` /
`notification`) are Space-independent and must always be visible — the conversation
list already forces them into every Space via `EnsureSystemBotsPresent`, so their DM
history must not be Space-filtered either. The old rule 4
(`payload.space_id == "" && isSysBot → drop`) was silently emptying botfather history
whenever the sync request carried an `X-Space-ID`. With the early return, the
`isSysBot` variable and its switch cases are removed; the remaining switch handles
only regular DMs (rules 1–4 renumbered, #484 symptom-1 semantics unchanged).

## Root cause / attribution

- Bug origin: YUJ-219-A / #1283 (`e39b69f`, 2026-05-04) added the system-bot
  message-drop branch. **Not** introduced by #484 (PR #519) — verified by diff:
  #484 only changed the `!isSysBot` regular-DM branch; the system-bot path is
  byte-identical before/after #484, and the filter call + its `X-Space-ID` gate in
  `syncChannelMessage` (`api.go`) pre-date #484.
- Read-time filter only: the endpoint filters the sync **response**, never writes
  or deletes. WuKongIM storage is intact; the fix restores history immediately
  (no backfill). Worst case was a client that wipe-replaced its local cache on the
  empty response — self-heals on the next sync.

## Verification

- Reproduced first (pure function): botfather untagged history 0/2 under a Space,
  2/2 without X-Space-ID; a regular peer 2/2 in default (isolates rule 4 as cause).
- Flipped 3 existing YUJ-219-A tests that locked the drop behavior
  (`_SystemBot`, `_PayloadSpaceIDWrongType` fileHelper case, `_SystemBotListCoverage`)
  to assert full-visibility; `_SystemBotListCoverage` now also covers the
  **non-default** Space (the exact production repro).
- #484 guard kept: `_UntaggedDroppedInNonDefaultSpace` / `_OrdinaryDMLegacyCompat`
  still green (regular untagged DM stays default-Space-only).
- `go test -race -shuffle=on ./modules/message/ ./pkg/space/` green; `go build`,
  `go vet` green.

## Learnings

- Two Space contracts must stay in lockstep: conversation-list visibility
  (`EnsureSystemBotsPresent` / `decideConvKeepInSpace`) and message-history
  visibility (`filterPersonMessagesBySpace`). A system bot shown in the list but
  empty on open is the smell of the two diverging.
- Follow-up: confirm whether the client `filterSystemBotMessages` (Android/iOS)
  also drops system-bot history; if so the server fix alone won't surface it.
