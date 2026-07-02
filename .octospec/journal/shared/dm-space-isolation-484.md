---
type: Journal
title: "Journal: dm-space-isolation-484 (octo-server #484)"
description: Authoritative per-Space DM presence index — fixes cross-Space DM history leak (symptom 1) and mutual conversation hiding (symptom 2).
tags: ["space", "isolation", "dm", "webhook", "p1"]
timestamp: 2026-06-27T00:00:00Z
# --- octospec extension fields ---
task: dm-space-isolation-484
upstream: Mininglamp-OSS/octo-server#484
source: self
---
# Journal: dm-space-isolation-484 (octo-server #484)

## What was done

DMs are a single physical WuKongIM Person channel per user-pair; multi-Space
isolation was emulated by a soft `payload.space_id` tag plus filters that
**scanned messages** to infer Space. That produced two P1 defects (reproduced
end-to-end first): cross-Space history leak (symptom 1) and DMs mutually hiding
between Spaces (symptom 2). Product decision: DMs are per-Space isolated. Fixed
server-side, authoritatively, without any client change.

1. **Authoritative presence index** — new table `dm_space_presence(fake_channel_id,
   space_id, last_timestamp)` (`modules/space/sql/20260627000001_dm_space_presence.sql`)
   + accessor `pkg/space/dm_presence.go` (`UpsertDMSpacePresence`, batch
   `DMSpacePresenceSet`). Keyed by the **symmetric** `common.GetFakeChannelIDWith`
   pair id, so the webhook write side `(sender, peer)` and the conversation read
   side `(viewer, peer)` compute the same key.

2. **Write at ingest** — `modules/webhook/api.go handleMessageNotify` collects
   `(fakeChannelID, space_id)` for non-signal Person messages carrying
   `payload.space_id` and upserts them **after `tx.Commit()`, best-effort**
   (log-and-continue). It never rolls back message persistence; a missed write
   self-heals on the next message because readers OR it with the Recents scan.

3. **Symptom 2 read** — `resolveDMPresence` batch-resolves presence for all bare
   DM pair-ids vs `filterSpaceID` (one query, mirroring `resolveBotFilter`) and
   **ORs** it with the existing Recents-window scan inside `decideConvKeepInSpace`
   (both v1 `FilterConversationsBySpace` and v2 `FilterRawConversationsBySpace`).
   Visibility becomes a strict superset of before → no regression, and a DM is
   visible in every Space it has messages in, simultaneously.

4. **Symptom 1 read** — `filterPersonMessagesBySpace` gained `defaultSpaceID`;
   an untagged non-systembot DM message is kept only when
   `spaceID == defaultSpaceID`. Caller `syncChannelMessage` passes
   `space.GetUserDefaultSpaceID(ctx, loginUID)`.

## How it was verified

- Reproduced both symptoms first (`TestRepro484`, integration-tagged, real
  handlers + MySQL/Redis + mocked WuKongIM), then **flipped to post-fix
  assertions**: a DM with presence in spaceB AND spaceC is visible in both at
  once (presence written via the REAL `POST /v1/webhook/message/notify`, HMAC
  signed); a DM with presence only in spaceB is absent from spaceC; untagged
  history shows only in the default Space.
- `go test -tags=integration ./modules/message/ -run TestRepro484 -v` → PASS (3).
- Unit: `go test ./modules/message/...`, `./pkg/space/...`, `./modules/webhook/...`,
  `./modules/space/...` → PASS (fresh-DB per package to avoid the shared
  gorp_migrations cross-binary state issue).
- Gates: `make i18n-lint`, `make i18n-extract-check`, `lint-personal-msgsendreq` → OK.

## Learnings

- octo-server **does** persist every message server-side via the WuKongIM
  webhook (`handleMessageNotify`), which is the authoritative, sender-path-
  independent hook for per-message derived state — better than the send
  chokepoint (it also captures the peer's messages, forwards, card messages).
- `common.GetFakeChannelIDWith` is symmetric (CRC32-sorted pair), which is what
  makes a single presence key reproducible from either participant's viewpoint.
- OR-ing a new authoritative signal with the legacy heuristic (rather than
  replacing it) makes visibility a strict superset — fixes the defect with zero
  regression risk and no backfill.

## Out of scope (follow-ups)

- Per-message authoritative history via `message_id` cross-ref (kept the
  payload-tag history filter + default-Space policy here).
- Existing-DM presence backfill (relying on OR-with-Recents fallback).
- Physical per-Space DM channel split; any client change; `space_unread.go`.
