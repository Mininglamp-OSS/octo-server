---
type: Journal
title: "Journal: agent-message-reactions"
description: Record of adding an idempotent as-bot message-reaction endpoint (single validation authority shared with the user path) plus the openclaw-channel-octo react action and opt-in Discord-style 👀 ack.
tags: ["bot-api", "message", "reaction", "space", "isolation"]
timestamp: 2026-07-21T03:00:00Z
# --- octospec extension fields ---
task: agent-message-reactions
upstream: mininglamp-oss/octo-server (follow-up to #603 reaction hardening)
source: self
---
# Journal: agent-message-reactions

## What was done

Enabled the Discord-style "agent reacts to a user message before replying" flow,
across octo-server and the openclaw-channel-octo plugin.

**octo-server** — bots could not react at all (`/v1/reactions` was user-token
only). Added `POST /v1/bot/message/reaction` (authBot) with **explicit,
idempotent `add`/`remove`** semantics (not the user path's toggle) so an agent
that retries on timeout never cancels a live reaction. To avoid a second,
drifting copy of the #603-hardened validation (membership / DM friend-or-self /
thread-not-deleted / group-not-disbanded / existence / visibility-to-viewer /
text-only / DM per-Space isolation), the write + validation was extracted into a
single authority, `message.ReactionService.WriteReaction(identity, action)`.
`addOrCancelReaction` (user path) was refactored to delegate to it in toggle
mode — behaviour-preserving, verified by the existing reaction integration
tests. New `setReaction` DB primitive does the explicit upsert. bot_api holds a
standalone `message.NewReactionService(ctx)` (NOT `message.New`, which would
re-register message's group-member event listeners) and delegates as-bot; App
Bots stay DM-only.

**openclaw-channel-octo** — `capabilities.reactions:true` surfaces the standard
`react` message-tool action; `handleReact` resolves the target via the SDK's
`resolveReactionMessageId` (explicit `messageId` OR the current message,
threaded through as `toolContext.currentMessageId`) and parses emoji/remove via
`readReactionParams`, calling a new `sendReaction` api-fetch primitive. The
opt-in 👀 ack (`maybeCreateAckReaction`) wires the SDK ack-reaction subsystem
(`resolveAckReaction`/`shouldAckReaction`/`createAckReactionHandle`/
`removeAckReactionHandleAfterReply`) into the inbound dispatch: fire before
dispatch, remove in the finally, fire-and-forget.

## Learnings

- **bot_api already depends on `message` transitively** (via
  `messages_search`), so the new direct `bot_api → message` import is not a new
  coupling. The `-tags integration` build of `modules/message` was *already*
  un-`go test`-able before this change: an integration-tagged internal test file
  (`api_card_action_test.go`, `package message`) imports `app_bot → bot_api →
  messages_search → message`, a cycle. CI sidesteps it entirely — the test lane
  runs the **default** build (no `-tags integration`, ci.yml), walking
  `go list ./...` per-package. So the integration-tagged reaction tests do not
  run in CI; the CI-runnable coverage is bot_api's **untagged** NewTestServer
  tests + the message-module sqlmock tests.
- **`resolveAckReaction` never returns ""** — it falls back to
  `DEFAULT_ACK_REACTION` ("👀"). An "off by default" ack therefore cannot key on
  an empty emoji; it must gate on `ackReactionScope` being explicitly set. Hence
  the opt-in design (unset/off/none → no reaction).
- **Sharded `message{N}` tables** are only partially provisioned in the default
  test DB (shard 0 = `message`); a channel id hashing to a non-zero shard makes
  `queryMessageByID` fail with `Table 'test.message1' doesn't exist`. Bot
  reaction HTTP tests therefore assert on the pre-existence rejection paths
  (non-member, invalid-action) that return before `queryMessageByID`, and leave
  the not-found path to the message-module integration coverage.

See brief: `.octospec/tasks/agent-message-reactions/brief.md`.
