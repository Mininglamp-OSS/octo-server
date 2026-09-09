---
type: Task
title: "Task: ai-container-default-no-mention"
description: AI session containers report 免@ to their bot so an AI session answers without an @mention.
tags: ["bot-api", "ai-team", "wire-contract"]
timestamp: 2026-09-08T00:00:00Z
# --- octospec extension fields ---
slug: ai-container-default-no-mention
upstream: Mininglamp-OSS/octo-server#849
source: self
---

# Task: ai-container-default-no-mention

## Goal

Make an AI session answer a message that does not @mention the bot.

Server-side routing inside an AI session container is already unconditional:
`aiteam.LookupReadySessionTarget` resolves exactly one bot from the persisted
`ai_team_agent` association, and `modules/robot/event.go` never consults the
payload's `mention` for that path. The adapter, however, applies its own mention
gate — `openclaw-channel-octo`'s `isGroup` covers `CommunityTopic`, i.e. session
threads — and relaxes it only when `GET /v1/bot/groups/:group_no/mention_pref`
reports `effective: true`. For a container that endpoint reported `false`, so
every non-@ message was filed as history context and the AI stayed silent.

This change makes the endpoint report the container's 免@ decision.

## Background

- `bot_mention_pref` is the two-axis (bot owner intent × group switch) preference
  from octo-server#237 / YUJ-2996.
- A container can never carry a preference row: the owner endpoints reject
  mention_pref writes for `purpose = ai_session_container`
  (`modules/robot/mention_pref.go` `rejectAIContainerMentionPref`). So "let the
  owner switch 免@ on" is not an available answer — the rule has to be
  server-side.
- Containers are already provisioned with `allow_no_mention = 1`
  (`modules/ai_team/service.go`), so only the bot axis was missing.
- The alternative — teaching the adapter to recognise AI sessions — was rejected:
  it needs changes in two repos plus a plugin upgrade on every user's machine,
  and `/v1/bot/events` does not even forward the AI session metadata
  (`space_id` / `session_key` / `input_id` are dropped by the local struct in
  `modules/bot_api/events.go`). Deciding server-side needs no adapter change and
  takes effect within the adapter's existing 30s cache TTL.

## Load-bearing list

- `GET /v1/bot/groups/:group_no/mention_pref` wire contract: `no_mention` on this
  adapter-facing endpoint is the AND-combined final decision (option A of
  YUJ-2996 Blocking 1), NOT the owner's raw intent. Legacy adapters read only
  `no_mention`, so it must keep agreeing with `effective`.
- The two-axis AND for ordinary groups must be unchanged — including groups whose
  `purpose` is some other non-empty value.
- Membership gate ordering: the bot must still be rejected before any decision is
  made, so the endpoint cannot be used to probe arbitrary group ids.
- `aiteam.Enabled()` is deliberately NOT consulted, mirroring
  `aiteam.IsProtectedGroup`: turning rollout off must not change how an
  already-created container behaves.
- Owner-side protection (`rejectAIContainerMentionPref`) is a premise of this
  design, not an independent fact — the tests pin it.

## Out of scope

- The adapter (`openclaw-channel-octo`): unchanged, by design.
- `/v1/bot/events` not forwarding `space_id` / `session_key` / `input_id`. Real
  gap, separate concern — messages reach the adapter over the WuKongIM WebSocket,
  not through the event queue, so it does not block this fix.
- Ordinary groups' 免@ UX, the owner endpoints, and the group-level switch.

## Acceptance

- `resolveEffectiveNoMention` truth table: ordinary groups keep the 4-row AND;
  containers are true in all 4 combinations; a non-container purpose does not
  take the container branch.
- `GET /v1/bot/groups/:group_no/mention_pref` on a container returns
  `effective: true` and `no_mention: 1` with NO `bot_mention_pref` row present.
- The same bot in an ordinary group still returns `effective: false`.
- Cross-module contract test: a container provisioned through the REAL
  `/v1/ai-team` API (not a hand-written `group` row) carries
  `purpose = ai_session_container`, has the bot as a member, has no preference
  row, and reports 免@.
- The owner endpoints still reject read/write/delete of a container's mention
  preference.
- Disabling the container branch must make the above fail (the tests are pinned
  against a real regression, not vacuously true).
