---
type: Journal
title: "Journal: bot-setting-store"
description: Add a generic per-bot config store (bot_setting) with a bot → system_setting → code-default resolution chain, and put the four bot-level card switches on it.
tags: ["bot-api", "robot", "config", "wire-contract", "auth", "rate-limit", "testing"]
timestamp: 2026-08-06T16:20:00+08:00
# --- octospec extension fields ---
task: bot-setting-store
upstream: "openclaw-channel-octo 推理进度卡策略请求"
source: user
---

# Journal: bot-setting-store

## What was done

- Added `bot_setting`, a sparse `(robot_id, key_name)` override table, plus a
  registry in `modules/robot/bot_setting.go` that is simultaneously the write
  whitelist, the owner-facing catalog, and the resolution chain. A new config
  key is a registry entry, not a migration.
- Resolution: bot override → `system_setting` deployment default → code default.
  Deleting an override falls back to the layer underneath rather than latching
  false; the read endpoint returns `value` / `effective_value` / `source`
  separately so a restore-default control can tell "I chose off" from "I never
  set it".
- Owner endpoints on `/v1/robot/:robot_id/settings` (GET catalog + echo, PUT
  batch write, DELETE one override), guarded by `assertRobotOwner` and mounting
  `SharedUIDRateLimiter` per route.
- First consumer: four card switches. `bot.card_enabled` is derived from
  `cardmsg.BotEnabled()` and rejects writes; `display` / `interaction` /
  `reasoning` are owner-editable and default true.
- `GET /v1/bot/card/profile` gained one additive `config` object carrying the
  already-AND-ed values, with the ref invariants asserted in tests.
  `sendMessage` enforces the same config independently.
- Writes enqueue a valueless typed bot event so an adapter drops its cached
  profile immediately instead of waiting out a TTL.

## Decisions worth keeping

**The precedent that looked right was the wrong one.** `bot_mention_pref` is a
sparse table, so it reads as "this repo prefers sparse tables for bot config".
It is not: its dimension is `(robot_id, group_no)`, and a `robot` column cannot
express two dimensions — the table was forced, not chosen. The actual precedent
for *one-dimensional* bot config is a column on `robot` (`auto_approve`,
`inline_on`, `placeholder`, `bot_commands`). So this task replaces
"keep adding columns", and borrows only mention_pref's *surrounding* shape:
owner guard, delete-means-fall-back, post-write cache invalidation, and the
owner-writes / adapter-reads split across two modules.

**A derived key must not be storable.** `card_enabled` reflects the deployment
env chain. Had it been a normal stored key, the DB could hold `true` while the
env holds it off — the manifest would advertise a capability the send path
refuses, breaking the "manifest agrees with the send gate" invariant
`pkg/cardmsg` already established. Making it read-only and rejecting writes
removes the state rather than documenting it.

**Sub-switches default true because the master switch is already fail-closed.**
The upstream asked for a conservative default. `OCTO_CARD_MESSAGE_ENABLED`
defaults to false, so with sub-switches at true the effective outcome is still
"no cards" until an operator opts in. Defaulting them false as well would have
made operators opt in twice for no extra safety.

**Three switches, not a profile filter.** The upstream proposed clipping
`profiles` per bot (octo/v1 = display, octo/v2 = interaction). Two reasons not
to: `pkg/cardmsg/profiles.go` documents `acceptedProfiles` as the single
authority shared by the validator and the D12 manifest, and clipping breaks that
by design; and the reasoning card spans both profiles (active/error are
`octo/v2`, result is `octo/v1`), so a profile-shaped switch would leave it with
only its terminal state or only its progress states. The three switches are
orthogonal: display/interaction gate the raw-card path, reasoning gates the
template path.

**No per-bot cache, on purpose.** Reads go to MySQL every time, so an owner's
change on one replica is visible on every other immediately. A process-local
cache would trade that for TTL drift, and the invalidation event reaches only
the bot, not sibling replicas — Redis pub/sub would be required to close it.
Cost of the no-cache choice: one indexed single-row read per card *creation*
(non-card sends never enter the gate; streaming updates go through
`message/edit`, which is deliberately ungated so an in-flight card can still
reach its terminal state).

## What went wrong on the way

1. **A hot-path global mutex, found only because performance was questioned.**
   The resolver called `common.EnsureSystemSettings` per request, and that
   function takes a process-wide `sync.Mutex` on *every* call — on the card send
   path. `SystemSettings` is explicitly built so readers never take a lock
   (atomic.Pointer snapshot); calling the constructor-shaped accessor per request
   silently undid that. Fixed by resolving the singleton once at construction and
   holding it on `Robot` / `Service`.

2. **Test isolation broke under `-shuffle`, and the cause is a production
   property.** `EnsureSystemSettings` is a process-wide singleton whose in-memory
   snapshot `CleanAllTables` does not touch. A case that wrote a deployment
   default leaked it into every later case in the binary, surfacing as
   `source="global"` where `"default"` was expected. Fixed by forcing `Reload()`
   in setup. Same family as the already-documented "CleanAllTables does not clear
   Redis rate-limit buckets" — staged as a learning.

3. **The contract guard did its job.** `TestBotCardProfile_AdditiveContractFieldSet`
   freezes the profile's top-level field set, so adding `config` failed it. The
   fix is to extend the frozen set — and `config`'s own five sub-fields were
   frozen the same way `limits` already is.

## Verification

`go build ./...`, `go vet`, `make i18n-extract-check`, `make i18n-lint`, both
modules' `NoLegacyResponseError` source guards, and `-race -shuffle=on` on
`modules/robot` and `modules/bot_api` against MySQL 8 + Redis + WuKongIM
v2.2.4, run per-package with the CI drop/create + FLUSHALL discipline.
