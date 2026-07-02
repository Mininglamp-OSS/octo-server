---
type: Task
title: "Task: dm-space-isolation-484"
description: Make DM (Person) Space isolation authoritative — stop cross-Space history leak and mutual conversation hiding.
tags: ["space", "isolation", "webhook", "dm", "p1"]
timestamp: 2026-06-27T00:00:00Z
# --- octospec extension fields ---
slug: dm-space-isolation-484
upstream: octo-server#484
source: self
---

# Task: dm-space-isolation-484

## Goal

Fix issue #484: a DM with the same contact (1) leaks history across Spaces and
(2) mutually hides between Spaces. Product decision: **DMs are per-Space
isolated** (a DM is an independent conversation per `(contact, Space)`).

Replace the two soft-tag-scanning filters with a server-authoritative signal so
DM Space visibility is **window-independent** and untagged messages stop leaking
into every Space.

- **Symptom 2 (mutual hide):** conversation visibility in a non-default Space is
  currently decided by scanning the single shared `Recents` window
  (`personConvHasSpaceMessages` → `decideConvKeepInSpace`,
  `modules/message/space_filter.go:283/347`). Both Spaces share one window, so
  the most-recently-active Space wins and the other hides. → Make visibility
  query a durable per-`(dm-pair, space)` presence record instead.
- **Symptom 1 (history leak):** untagged DM messages are kept in **every** Space
  by rule 2 of `filterPersonMessagesBySpace` (`space_filter.go:457`). → Keep
  untagged DM messages only in the user's **default** Space.

## Background

- DM is a single physical WuKongIM Person channel; conversation-level `SpaceID`
  is intentionally empty (`api_conversation.go:1584`). Isolation is emulated by
  the per-message `payload.space_id` soft tag + filtering.
- Send-side authoritative `space_id` injection is already hardened (#33/#37 via
  `NewPersonalMsgSendReq` + CI lint); #153 backfilled conversation-level
  `space_id` for **groups only**. Neither touches the DM single-channel + scan
  mechanism. Symptom 2 (per-Space *loss* of a DM) is new — no prior issue.
- octo-server already persists every message server-side via the WuKongIM
  webhook `POST /v1/webhook/message/notify` → `handleMessageNotify`
  (`modules/webhook/api.go:234`), which is the authoritative, sender-path-
  independent write point. For a Person message it persists under
  `common.GetFakeChannelIDWith(FromUID, ChannelID)` — a **symmetric** canonical
  pair id (CRC32-sorted), so the same key is reproducible on the read side from
  `(loginUID, peerUID)`.
- Reproduced end-to-end in `modules/message/dm_cross_space_repro_test.go`
  (`TestRepro484`, integration-tagged).

## Design (server-only)

1. **Presence table** `dm_space_presence(fake_channel_id, space_id,
   last_timestamp)`, PK `(fake_channel_id, space_id)`. Migration in
   `modules/space/sql/`. Accessor in `pkg/space` (both `webhook` and `message`
   already import `pkg/space`; leaf package, no import cycle):
   - `UpsertDMSpacePresenceTx(tx, fakeChannelID, spaceID, ts)`
   - `DMSpacePresence(session, fakeChannelIDs, spaceID) -> set` (batch read).
2. **Write (authoritative):** in `handleMessageNotify`, for `ChannelTypePerson`
   messages whose `payload.space_id != ""`, upsert presence inside the existing
   tx, keyed by the `fakeChannelID` already computed there.
3. **Read — symptom 2:** in `FilterConversationsBySpace` /
   `FilterRawConversationsBySpace`, batch-resolve presence for all bare Person
   conv pair-ids against `filterSpaceID` (one query, mirroring the existing
   bot/group batch lookups), and feed the result into `decideConvKeepInSpace`'s
   `hasSpaceMsg` callback — replacing the Recents-window scan. Fail-open vs
   fail-closed on DB error: follow the existing per-entrypoint convention
   (v1 fail-open, v2 sidebar fail-closed) used for group lookups.
4. **Read — symptom 1:** `filterPersonMessagesBySpace` gains a `defaultSpaceID`
   param; rule 2 keeps an untagged non-systembot message only when
   `spaceID == defaultSpaceID`. Caller `syncChannelMessage` (`api.go:1277`)
   passes `space.GetUserDefaultSpaceID(ctx, loginUID)`.

## Load-bearing list

- `space` / `isolation` — core multi-tenant boundary; the whole change is a
  Space-isolation correctness fix (space-isolation rule).
- `webhook` / `trust-boundary` / `wire-contract` — adds a write inside the
  HMAC-authenticated message webhook ingest (trust-boundary rule).
- DM conversation visibility filter (`decideConvKeepInSpace`, both v1 and v2
  sidebar entrypoints) — must stay consistent across v1/v2.
- DM history message filter (`filterPersonMessagesBySpace`) and its callers.
- DB migration (new table; idempotent, additive).
- `test` — flip the integration repro to post-fix assertions.

## Out of scope

- Physical per-Space DM channel split (the "A1" long-term direction).
- Changing send-side `space_id` injection (`enrichPayloadWithSpaceID` /
  `NewPersonalMsgSendReq`) — already hardened.
- Full per-message history authority via `message_id` cross-reference to the
  persisted table (follow-up; this change keeps the payload-tag history filter
  but fixes the untagged-everywhere leak via the default-Space policy).
- Any client (Web/Android/iOS) change — server remains the authoritative source.
- Per-Space unread rework in `space_unread.go` (separate concern).
- The pre-existing legacy `c.ResponseError` in `webhook.messageNotify` (not our
  lines; leaving the protocol endpoint as-is).

## Acceptance

`go test -tags=integration ./modules/message/ -run TestRepro484 -v` passes with
post-fix assertions:

- **Symptom 2 fixed:** a DM with persisted presence in BOTH spaceB and spaceC is
  visible in spaceB AND spaceC **simultaneously** (no mutual hide), independent
  of which Space last filled the Recents window. A DM with presence only in
  spaceB is visible in spaceB and absent from spaceC (correct isolation — it
  genuinely has no spaceC messages).
- **Symptom 1 fixed:** with history `[tagged-B, UNTAGGED, tagged-C]` and default
  Space = spaceDefault: sync under spaceB returns `tagged-B` only (NOT
  `UNTAGGED`, NOT `tagged-C`); sync under spaceDefault returns `UNTAGGED`.
- Existing message-package unit tests still pass
  (`go test ./modules/message/...`).
- New presence write is exercised through the real webhook endpoint (or a direct
  presence insert) in the integration test.
