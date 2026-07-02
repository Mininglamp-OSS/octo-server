---
type: Task
title: "Task: conv-space-catchall-484"
description: Fix the two deterministically reproducible cross-Space paths in the recent-conversation list (default-Space DM catch-all; spaceless group shown everywhere)
tags: [space, isolation, message, conversation]
timestamp: 2026-07-02T00:00:00Z
# --- octospec extension fields ---
slug: conv-space-catchall-484
upstream: octo-server#484 (follow-up; production trace + client SpaceFilter diagnostic)
source: self
---

# Task: conv-space-catchall-484

## Goal

Close the two cross-Space paths in `POST /v1/conversation/sync` / `POST /v1/sidebar/sync`
that reproduce **with a fully well-formed request** (space_id present and valid):

1. **Default-Space DM catch-all** (`decideConvKeepInSpace`, `space_filter.go:305`):
   when `filterSpaceID == defaultSpaceID`, every bare non-bot DM is kept
   unconditionally — including DMs whose messages are known (via the #484
   authoritative `dm_space_presence` index) to belong **exclusively to other
   Spaces**. Fix: in the default Space, hide a bare DM iff it has ≥1 presence row,
   none of them for the default Space, and its Recents window carries neither a
   default-Space-tagged nor an untagged message. DMs with **no presence rows at
   all** (pre-#484 / legacy / untagged-only) keep today's catch-all — the fix
   only acts on positive, durable evidence, and self-heals (one message in the
   default Space re-adds visibility).

2. **Spaceless group / topic fail-open** (`decideConvKeepInSpace` group branch
   `if spaceID == "" { return true }` and `filterThreadConvCore:369`
   `return parentSpaceID == ""`): a group whose `group.space_id` is empty (and any
   topic under it) is listed in **every** Space. Fix: attribute spaceless
   groups/topics to the user's **default Space only** — consistent with the
   existing policy for legacy DMs (#337) and untagged DM history (#484 symptom 1).

Both fixes follow the established product decision (per-Space isolation; legacy /
unattributable content lives in the default Space, not everywhere).

## Background

- Issue #484 fixed DM mutual-hide + untagged history leak (branch
  `fix/dm-space-isolation-484`, not yet merged).
- A production incident + an Android SpaceFilter diagnostic log identified three
  deterministic cross-Space paths; two reproduce with a well-formed request and
  are locked by integration tests `TestRepro484_DefaultSpaceListsOtherSpaceOnlyDM`,
  `TestRepro484_ProdTrace_DefaultSpaceCatchAll_ShowsAllDMs`,
  `TestRepro484_ProdTrace_SpacelessGroupShownInEverySpace` (currently asserting
  the buggy behavior; this task flips them).
- The client renders DMs with zero local filtering (`person-pass`) because DM
  conversations carry no space_id — the server list is the only line of defense.
- **Dependency**: fix 1 requires `dm_space_presence` (+ webhook write + test
  harness), which exists only on `fix/dm-space-isolation-484`. This branch is
  therefore stacked: `origin/main (236b78b)` + rebased #484 commits + this work.

## Load-bearing list

- `space` / `isolation`: conversation-list Space visibility for Person, Group,
  CommunityTopic (`decideConvKeepInSpace`, `filterThreadConvCore`,
  `filterConversationsCore`, v1 `FilterConversationsBySpace`, v2
  `FilterRawConversationsBySpace` → both `/v1/conversation/sync` and
  `/v1/sidebar/sync`).
- System bots (`SystemBots` + `EnsureSystemBotsPresent`): must remain visible in
  every Space even when their presence rows point elsewhere (e.g. botfather with
  activity only in one non-default Space).
- Regular bots: default-Space visibility is still gated by the existing bot
  membership sub-check; the new hide rule must not apply when `skipBotFilter`
  (bot resolution DB error) since bot identity is unknown.
- Fail-open ethos on infra errors: presence lookup failure ⇒ no new hiding;
  default-Space lookup failure ⇒ behave as before (`GetUserDefaultSpaceIDE` +
  fallback), never hide more than today on a DB hiccup.
- `test`: existing `TestRepro484_*` integration suite semantics.

## Out of scope

- The **missing-space_id** path (request without `?space_id`/`X-Space-ID` skips
  filtering entirely). That skip is documented intentional legacy-client compat
  (YUJ-226 / lml P1-1); changing it is a client-contract decision, not a bug fix.
- Backfilling `group.space_id` data or `dm_space_presence` history.
- Sending a conversation-level `space_id` for DMs to the client (structural
  follow-up, separate task).
- Message-history filtering (`/v1/message/channel/sync`) — unchanged by this task.
- Client (Android/iOS/Web) changes.

## Acceptance

- Flipped integration tests pass (real handlers, real MySQL/Redis, mocked IM):
  - Default Space **hides** a bare DM whose presence rows exist only for other
    Spaces (and whose Recents carry no default/untagged message).
  - Default Space **keeps**: DMs with no presence rows (legacy), DMs with a
    default-Space presence row, DMs whose Recents contain an untagged or
    default-tagged message, and system bots regardless of presence.
  - A spaceless group (and its topic) is listed **only** in the user's default
    Space; groups with a real space_id stay isolated to their own Space.
- All pre-existing `TestRepro484_*` tests still pass (symptom 1/2 fixes, bot
  scenarios, missing-space_id path unchanged).
- Unit suites `go test ./modules/message/ ./pkg/space/` green; `go vet`/gofmt
  clean; `make i18n-lint` unaffected (no new error responses).
