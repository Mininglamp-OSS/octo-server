---
type: Task
title: "Task: dm-card-action-peer-channel-id"
description: DM card_action events must carry the peer user UID as channel_id, not the card sender bot's own UID.
tags: [card, wire-contract, bot-api]
timestamp: 2026-08-19T00:00:00Z
# --- octospec extension fields ---
slug: dm-card-action-peer-channel-id
upstream: openclaw-channel-octo DM card_action mismatch report
source: self
---

# Task: dm-card-action-peer-channel-id

## Goal

`card_action` events for a DM card must report `channel_id` in the **event
consumer's** frame of reference — the peer user UID — so it matches the
`channel_id` the bot passed to `/v1/bot/sendMessage` when it sent the card.
Today the ingress echoes the click request's `channel_id`, which for a DM is
the *bot's own* UID (the clicking user's peer), and every consumer that
compares the two drops the click.

Group and CommunityTopic semantics do not change: those ids name the channel
itself, so both sides already agree.

## Background

`channel_id` on the wire always means "the other end of this conversation, as
the identifier's holder sees it":

- Bot sends a DM card: `POST /v1/bot/sendMessage {"channel_id": "<user uid>"}`.
- User clicks: `POST /v1/message/card/action {"channel_id": "<bot uid>"}`.

`modules/message/api_card_action.go` copied the request value straight into
`event_data.channel_id` (bot queue) and `cardactiondispatch.Event.ChannelID`
(internal callback route). The card was registered under the user UID and the
callback arrived naming the bot UID, so the OpenClaw channel plugin's
card-session identity check (`pendingSession.channelId === action.channelId`,
`src/card-action-handler.ts`) rejected it as mismatched and the button appeared
dead. Every other field matched (event_id, accountId, channel_type, action_id).

The in-tree internal consumers already worked around this: both
`modules/notify/action_finalizer.go` and `modules/notify/standard_action_finalizer.go`
overwrite `channelID` with `event.OperatorUID` for `ChannelTypePerson` before
calling the card mutator, because the mutator needs the sender-side channel id.
Fixing the ingress makes the event self-consistent; those overrides stay as the
compatibility path for events enqueued before this change.

## Load-bearing list

- `event_data` wire shape for `card_action` on `/v1/bot/events` (frozen key set,
  additive-only). This changes the **value** of an existing key for DM only.
- `cardactiondispatch.Event.ChannelID` → legacy `DecisionRequest.channel_id` and
  the `octo-card@1.0` envelope's `card.channel_id` delivered to internal HMAC
  callback routes.
- `modules/notify` finalizers' DM channel substitution (must keep working for
  events already sitting in the Redis queue when this deploys).
- Anti-IDOR channel binding in `authorizeCardChannelMember` — the new projection
  reads only facts that gate already established; it must not become a second,
  weaker source of channel identity.

## Out of scope

- Group / CommunityTopic `channel_id` (unchanged).
- The client request shape for `POST /v1/message/card/action` (unchanged — the
  operator still sends their own peer, i.e. the bot).
- `event_data.space_id`, `operator_uid`, and every other event key.
- Draining or rewriting `card_action` events already enqueued before the deploy.

## Acceptance

- Contract test: bot DMs a card to a user → user clicks →
  `event_data.channel_id == event_data.operator_uid == peer user UID`, and it is
  **not** the bot UID.
- Group and CommunityTopic clicks still echo the request `channel_id`.
- The internal callback route (`DecisionRequest.channel_id`) carries the same
  peer UID for a DM click.
- Unit coverage for the projection helper, including the DM row whose sender is
  not a participant (fail closed, no invented peer).
- `go test ./modules/message/... ./internal/cardactiondispatch/... ./modules/notify/...`
  and `make i18n-extract-check i18n-lint` pass.
