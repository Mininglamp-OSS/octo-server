---
type: Task
title: "Task: agent-message-reactions"
description: Bot-facing idempotent message reaction API (as-bot) so agents can react to a user message before replying (Discord-style).
tags: [bot-api, message, reaction, space-isolation]
timestamp: 2026-07-21T00:00:00Z
# --- octospec extension fields ---
slug: agent-message-reactions
upstream: Mininglamp-OSS/octo-server (follow-up to #603 reaction hardening)
source: self
---

# Task: agent-message-reactions

## Goal
Let an Octo bot/agent add or remove an emoji reaction on a user message it can
see — the server half of the Discord-style "agent reacts (👀) to the message it
is about to reply to". Ship a new **bot-authenticated, idempotent** reaction
endpoint that reuses the same validation as the hardened user reaction path
(#603), plus the plugin wiring (in `openclaw-channel-octo`) that exposes it as a
`react` agent action and an automatic 👀 ack on inbound.

Scope confirmed with maintainer: `1(b) + 2(a) + 3(a) + 4(a)`
- Identity: **as-bot only** (no OBO / as-user this round).
- Semantics: **explicit idempotent `add` / `remove`** (NOT toggle) — retries safe.
- Plugin: agent-driven `react` action **+ inbound auto-👀 ack**.
- Target selection: agent passes an **explicit `messageId`** (no dependence on
  an SDK `toolContext.currentMessageId`, which does not exist today).

## Background
- User-facing reactions already exist and are hardened: `POST /v1/reactions`
  (toggle) + `POST /v1/reaction/sync`, in `modules/message/api.go`
  (`addOrCancelReaction`). Validation chain: channel membership
  (`ExistMemberActive`) / DM friend-or-self / thread not-deleted / group-not-
  disbanded / message existence / **visibility-to-viewer** / **text-only gate**
  (`payloadIsPlainText`) / DM per-Space isolation / emoji rune cap; then an
  atomic upsert `toggleReaction` + a `CMDSyncMessageReaction` fan-out.
- `/v1/bot/*` (`ba.authBot()`, `modules/bot_api`) has **no** reaction endpoint.
  Bots today cannot react at all.
- The reaction validation + write is bound to `*Message` (holds
  `groupService`/`userService`/`threadDB`/`messageReactionDB`/visibility DBs);
  `message.IService`/`*Service` do NOT hold these, so bot_api cannot reuse it
  as-is.
- Plugin (`openclaw-channel-octo`): `capabilities.reactions:false`
  (`src/channel.ts`), `getAvailableActions` returns only `["send","read",
  "search"]`, `actions.ts` has no `react` case. Action context (`toolContext`)
  carries `currentChannelId` + `threadId` but **not** a current message id.

## openclaw SDK precedent (verified in openclaw@2026.6.11 plugin-sdk)
Reactions are a **first-class** openclaw concept — the plugin must reuse the SDK,
not hand-roll:
- `"react"` ∈ `CHANNEL_MESSAGE_ACTION_NAMES` — agent-driven react is a standard
  message-tool action, gated by `capabilities.reactions` ("Enable/disable
  sending reactions via message tool"). Params: `readReactionParams()` →
  `{emoji, remove, isEmpty}` (`remove` maps 1:1 to our add/remove endpoint).
- `resolveReactionMessageId({args, toolContext})` — resolves the target message
  from explicit args **OR** `toolContext.currentMessageId`. So the agent need
  not always pass `messageId`; explicit-or-current is free (supersedes the
  earlier "explicit messageId only" 4a framing — we take both).
- Auto-👀 ack is the SDK `ack-reactions` subsystem: `resolveAckReaction` +
  `shouldAckReaction` (gate by `AckReactionScope` = all|direct|group-all|
  group-mentions|off) + `createAckReactionHandle` (fire 👀 before dispatch) +
  `removeAckReactionHandleAfterReply` (remove after reply). Config knobs
  `ackReaction`/`ackReactionScope` live in the base channel config. Do NOT
  hand-roll throttle/dedup in inbound.
- Status lifecycle reactions (⏳→✅/❌) are the SDK `StatusReactionAdapter`
  ({setReaction, removeReaction?}) + `createStatusReactionController` — OUT of
  scope for v1, but the **same** server primitive powers it, so build the plugin
  primitive in that `{setReaction, removeReaction}` shape to make Layer-2 status
  reactions zero-rework.

## Architecture decision
Add a **new `message.ReactionService`** (`modules/message/service_reaction.go`)
that owns one `WriteReaction(req)` method performing the full validated write,
parameterised by identity (`uid`,`name`) + `action` (`add`|`remove`|`toggle`).
It reuses the in-package primitives/free-funcs (`payloadIsPlainText`,
`messageVisibleToViewer`, `personSpaceAllows`, membership/thread/disband/seq
helpers) so there is a **single validation authority**. bot_api gets a
`reactionService *message.ReactionService` (constructed via
`message.NewReactionService(ctx)`, mirroring `messages_search.Shared(ctx)`) and
the new handler calls it as-bot. New DB method `setReaction(model, isDeleted)`
for the explicit (non-toggle) upsert.

The existing `addOrCancelReaction` user handler is **refactored to delegate** to
`WriteReaction(action=toggle)` so both paths share one validation body (no
divergence). This is behaviour-preserving; because local test infra
(MySQL/Redis/WuKongIM) is unavailable here, correctness of the refactor is
verified by `go build`/`go vet` + close reading locally and the existing
`modules/message` reaction integration tests in **CI**.

New endpoint: `POST /v1/bot/message/reaction`
```
{ "channel_id","channel_type","message_id","emoji","action":"add|remove" }
→ 200 { message_id, channel_id, channel_type, emoji, action, is_deleted }
```
Auth: `authBot`. App Bot (`BotKindApp`) → **DM-only** (reject group/thread,
matching `groups.go` App-Bot gate). Identity = `robotID` +
`resolveBotDisplayName`.

## Load-bearing list
- `error-response` / `i18n` — new bot endpoint must use `httperr.ResponseErrorL`
  + registered `pkg/errcode` codes; reuse `ErrMessage*` where they fit, add bot
  codes only if needed; run `make i18n-extract-check` + `make i18n-lint`; add the
  guard-test file to `TestBotAPINoLegacyResponseError` (and keep
  `TestMessageNoLegacyResponseError` green after the refactor).
- `space` / `isolation` / `auth` / `bot-api` / `thread` — reaction is an ACL-
  bearing write: bot membership (`ExistMemberActive` with `robotID`), DM
  friend/self gate, thread not-deleted + parent-group disband, message
  visibility-to-viewer (anti-enumeration: not-visible ⇒ 404, same code), DM
  per-Space isolation, App-Bot DM-only capability boundary. Must not weaken the
  #603 posture.
- `wire-contract` — refactoring `addOrCancelReaction` MUST preserve the exact
  `/v1/reactions` response shape (`reactionToggleResp`) and every error code +
  status it returns today (D14 pinned-400 via `ResponseErrorL`).
- Reaction DB write semantics — new `setReaction` must be idempotent for
  add/remove and must not disturb the `toggleReaction` upsert / unique-key
  contract (migration `20260712000001`).

## Out of scope
- OBO / as-user reactions (`on_behalf_of`) — deferred (scope 2a).
- Status/lifecycle reactions (⏳→✅/❌, progress-card hooks) — later phase.
- Reaction on non-text messages — text-only gate stays (`payloadIsPlainText`).
- Any change to `/v1/reaction/sync` read path or the reaction table schema.
- New rate-limit tier for the bot endpoint beyond the existing bot-auth posture
  (no hand-rolled Redis counter).

## Acceptance
- `go build ./...` and `go vet ./...` pass.
- `make i18n-extract-check` and `make i18n-lint` pass; any new code has a zh-CN
  entry in `active.zh-CN.toml`.
- `POST /v1/bot/message/reaction` with `action:add` then a duplicate `action:add`
  both succeed and leave the reaction present (`is_deleted=0`) — idempotent.
  `action:remove` (x2) leaves it absent (`is_deleted=1`).
- A bot NOT a member of the target channel → `ErrMessageChannelAccessDenied`;
  a non-existent/not-visible message → `ErrMessageNotFound` (no existence
  leak); a non-text target → `ErrMessageReactionUnsupportedType`.
- App Bot reacting in a group/thread → capability-denied; App Bot in DM works.
- Existing `modules/message` reaction tests still pass in CI (refactor is
  behaviour-preserving); a new `modules/bot_api` reaction test covers add/remove
  idempotency + membership + App-Bot gate.
- Guard tests (`TestBotAPINoLegacyResponseError`, `TestMessageNoLegacyResponseError`)
  green with the new/edited files listed.
- Plugin (Phase A — foundation + agent react): `api-fetch` gains a
  `sendReaction`/`{setReaction,removeReaction}` primitive → new endpoint;
  `capabilities.reactions:true`; `"react"` in `getAvailableActions`;
  `actions.ts` `react` case parses via `readReactionParams` +
  `resolveReactionMessageId(args, toolContext)` and calls the primitive.
- Plugin (Phase B — auto-👀 ack): inbound dispatch wires the SDK ack subsystem
  (`resolveAckReaction`/`shouldAckReaction`/`createAckReactionHandle`/
  `removeAckReactionHandleAfterReply`) around
  `dispatchReplyWithBufferedBlockDispatcher`; `ackReaction`/`ackReactionScope`
  honored. NO bespoke throttle/dedup.
- Plugin tests: `npm test` (vitest) green; add coverage for the `react` action
  (add/remove + message-id resolution) and the ack gate.
