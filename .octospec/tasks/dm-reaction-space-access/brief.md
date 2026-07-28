---
type: Task
title: "Task: dm-reaction-space-access"
description: DM (Person) reaction endpoints deny same-Space colleagues because the access gate only consults the friend table; align them with the canonical DM predicate.
tags: ["space", "isolation", "auth", "acl", "error-response", "testing"]
timestamp: 2026-07-28T00:00:00Z
# --- octospec extension fields ---
slug: dm-reaction-space-access
upstream: production report — POST /v1/reactions on a human-to-human DM returns err.server.message.channel_access_denied (403)
source: self
---

# Task: dm-reaction-space-access

## Goal

Reacting to a message in a **human-to-human DM** fails in production:

```
POST /api/v1/reactions
{"message_id":"2081994021533552640","channel_id":"17630a8d72b24adc96db007d73380696","channel_type":1,"emoji":"[尚方宝剑]"}
→ {"error":{"code":"err.server.message.channel_access_denied","http_status":403}, "status":400}
```

Replace the friend-only DM gate on the two reaction endpoints with the exact
predicate `modules/user/api_pinned.go::validateChannelAccess` already uses
(evaluated in order, first hit wins):

1. peer == caller (notes-to-self);
2. friend (authoritative in personal-space deployments);
3. peer is a bot — own bot allowed; someone else's bot still requires friendship;
4. peer is a real user and `AreSpaceMembers(requestSpaceID, caller, peer)`.

The Space used in step 4 is **only** the request's middleware-verified
`space_id` (`spacepkg.GetSpaceID(c)`). With no Space on the request the gate
degrades to friend-only — i.e. today's behavior, 403 for a non-friend — by
decision (D1 below).

Denials keep the existing code and envelope; every lookup stays fail-closed.

## Background

**Localisation of the fault (evidence chain).**

- Route: `modules/message/api.go:384` `POST /v1/reactions` → `addOrCancelReaction`
  (`api.go:1978`); `modules/message/api.go:388` `POST /v1/reaction/sync` →
  `syncReaction` (`api.go:1726`). Both mount `AuthMiddleware` + `uidLimit` +
  `spacepkg.SpaceMiddleware`.
- For `channel_type=1` (Person), `addOrCancelReaction` has **exactly one** path to
  `errcode.ErrMessageChannelAccessDenied`: the `!isFriend` branch at
  `api.go:2036-2044`. `syncReaction` mirrors it at `api.go:1755-1763`. So the
  reported 403 is unambiguously the friend lookup returning false.
- `channel_id` in the report is a bare 32-hex UID, `channel_type=1`, peer is a
  human — the bot branches never ran; the friend check is the only gate involved.
- `IsFriend` (`modules/user/db_friend.go:71`) is a plain `friend` table lookup.
  Nothing populates `friend` on Space join: the only writers are the
  system-bot / fileHelper / botfather seeds (`modules/user/api.go:3100,3150,3168,3251`,
  `api_manager.go:1224,1266`) and the explicit add-friend flow.
- The rest of the codebase already treats Space membership as the DM
  reachability signal, and says so:
  - `modules/user/api_pinned.go:264 validateChannelAccess` — friend ∪ own bot ∪
    `AreSpaceMembers`, "允许同 Space 成员（即便未互加好友）之间互相置顶";
  - `modules/messages_search/authz.go:92 checkP2PAccess` — same predicate, with
    the reason spelled out: "in Space (enterprise contact-book) mode the friend
    table is near-empty (mostly system bots); in non-Space deployments friend is
    authoritative. Either-or covers both";
  - `modules/message/api.go:471 sendMsg` — Space channel → `CheckBothMembers`,
    friend only as the personal-space fallback.
- Conclusion: the reaction endpoints are the outlier. In the enterprise
  contact-book deployment every colleague DM reaction is a guaranteed 403, for
  every user, on both the write and the read/sync side (so the reaction UI cannot
  even load its state).

**Why read and write must move together.** `syncReaction`'s existing comment
(`api.go:1739-1741`) pins read/write parity as an invariant — "同步鉴权必须与写路径
（addOrCancelReaction）对齐 … 否则读/同步路径会成为绕过写路径加固的短板". Fixing one
side only would both leave the feature broken and break that invariant.

**Space context available to the gate.** `spacepkg.GetSpaceID(c)`
(`pkg/space/middleware.go:158`) is populated from the `space_id` query param or
the `X-Space-ID` header; octo-web injects it on every request
(`packages/dmworkbase/src/Service/APIClient.ts`, `X-Space-Id`). `AreSpaceMembers`
→ `spacepkg.CheckBothMembers` (`pkg/space/membership.go`) requires both
`space_member.status=1` rows plus `space.status=1`.

**Prefixed DM channel ids are not a live case** (investigated for D2). The
current system does not produce `s{spaceID}_{uid}` Person channel ids: this
repo's own design record states "DM is a single physical WuKongIM Person channel;
conversation-level `SpaceID` is intentionally empty"
(`.octospec/tasks/dm-space-isolation-484/brief.md`, `api_conversation.go:1584`) —
which is precisely why the `payload.space_id` soft tag + `dm_space_presence`
mechanism exists. The only non-test constructor of a prefixed id is
`modules/webhook/common.go:29 resolvePushChannelID`, and it merely preserves a
prefix that arrived from WuKongIM. Everything else is read-side tolerance for
legacy data (`webhook/api.go:926` "legacy fallback for prefixed channel_ids",
`channel/api.go:166`, and octo-web's `stripSpacePrefix` in
`ChannelSettingService.ts:22` / `datasource.ts:202`). Note octo-web's reaction
call passes `requestChannel.channelID` unstripped
(`features/messageReaction/controller.ts:179`), but with bare-UID DM channels
that is inert.

## Load-bearing list

- **DM authorization on `POST /v1/reactions`** (`api.go:2033-2046`) — widened
  from friend-only to friend ∪ own-bot ∪ same-Space. `touches: space, isolation, auth, acl`
- **DM authorization on `POST /v1/reaction/sync`** (`api.go:1753-1765`) — same
  widening; read/write parity invariant (`api.go:1739-1741`) must still hold.
- **Bot DM semantics** — own bot exempt from friendship *and* Space (a bot has no
  `space_member` row, so a Space-first ordering would deny own-bot DMs — see
  `messages_search/authz.go:99-115`); someone else's bot still needs friendship
  (`modules/robot/event.go` "用户与Bot非好友关系，拒绝转发消息"). Disabled bots
  (`user.robot=1`, `robot.status!=1`) yield `creatorUID=""` and therefore stay
  friendship-gated (`modules/user/db_pinned.go:61`).
- **Per-message DM Space isolation** — `personSpaceAllows`
  (`modules/message/space_filter.go:582`) + `payloadSpaceIDFromRaw`, used by both
  `addOrCancelReaction` (`api.go:2134-2147`) and `filterReactionsByMessageVisibility`.
  Runs *after* the access gate and must keep running unchanged, including the
  untagged-message compat branch (`msgSpaceID=="" && spaceID==defaultSpaceID`).
- **Anti-enumeration collapse** — target-not-visible / cross-Space / deleted-thread
  all collapse to `ErrMessageNotFound`; access denial stays
  `ErrMessageChannelAccessDenied`; lookup failures collapse to
  `ErrMessageQueryFailed`. No new error code, no new wire status (`ResponseErrorL`
  stays pinned 400 with `error.http_status=403`). `touches: error-response`
- **Fail-closed on DB error** — every relationship lookup must deny on error and
  log the cause via `zap.Error`; a Redis/MySQL blip must never open the gate.
- **Group / CommunityTopic gates** — `ExistMemberActive`, thread-not-deleted,
  group-disbanded guards: untouched.
- **Guard test** `TestMessageNoLegacyResponseError` (`api_i18n_test.go:24`) — its
  file list must cover any new handler-side file. `touches: error-response`
- **Reaction integration suite** (`api_reaction_integration_test.go`) — the
  friend-seeded DM cases and the cross-Space DM isolation cases must keep
  passing unchanged. `touches: testing`

## Decisions (confirmed)

- **D1 — no default-Space fallback.** When the request carries no `space_id` /
  `X-Space-ID`, the gate does **not** resolve the caller's default Space
  (`space.GetUserDefaultSpaceIDE`); it falls through to friend-only and a
  non-friend gets 403. Keeps the predicate byte-identical to
  `validateChannelAccess` and adds no query on the no-Space path.
- **D2 — prefixed DM channel ids not handled.** `req.ChannelID` is used as the
  peer UID as-is; a legacy `s{spaceID}_{uid}` id keeps failing the gate exactly
  as it does today (see Background for why this is not a live case).
- **D3 — scope is the two reaction endpoints only.**

## Out of scope

- Everything ruled out by D1 / D2 / D3 above.
- The identical friend-only DM gate on other endpoints — same latent production
  bug, deliberately left for a separate task:
  `modules/message/api_pinned.go:59,357,551` (pin/unpin/clear pinned,
  `ErrMessageConversationForbidden`), `modules/message/api_channel_files.go:197`
  (`ErrMessageNotFriend`), `modules/channel/api.go:99` (channel setting, still on
  legacy `c.ResponseError`).
- Bidirectional blacklist on the reaction DM path. `modules/message` consults
  blacklist nowhere today and the canonical `validateChannelAccess` does not
  either; adding it is a tightening, not this fix. (`messages_search` does check
  it — worth its own task.)
- `fakeChannelID` computation (`common.GetFakeChannelIDWith`) and the
  `reaction_users` storage layout.
- The two reasons `modules/message` integration tests run in no CI lane, both
  pre-existing and each needing its own task: (a) `ci.yml:236-253` deliberately
  runs the **default** build with no `-tags integration`, because
  `modules/conversation_ext/*` expects a separate pre-baked `conv_ext_test`
  database that the job never provisions — the comment flags flipping the flag
  as a maintainer decision; (b) even with the flag flipped this package's
  integration build does not compile — `api_card_action_test.go` → `app_bot` →
  `bot_api` → `messages_search` → `message` is an import cycle in test
  (reproduced on clean `origin/main`).
- The unreachable pre-built-fake-channel branch in `syncReaction`
  (`api.go:1799`, `strings.Contains(req.ChannelID, "@")`): no such string can
  pass the DM gate before or after this change, and the write path never
  supported that shape. Cleanup belongs in its own task.
- Any octo-web change (none required).

## Acceptance

Unit (no DB — must run in the default, untagged build):

- Decision table over the DM predicate covering: self; friend; own bot (allowed
  without friendship *and* without Space); other's bot without friendship
  (denied even when same-Space); other's bot with friendship; real user
  same-Space; real user different-Space; real user with **no** Space on the
  request → denied (D1); and fail-closed on each individual lookup error.
- Friend hit short-circuits: no bot / Space lookup afterwards.

Integration (`-tags integration`, MySQL + Redis):

- Space colleague, **no** `friend` row, request carries `space_id` →
  `POST /v1/reactions` returns 200 and persists exactly one `reaction_users`
  row; `POST /v1/reaction/sync` returns 200 and includes that reaction.
  (This is the production repro — red before, green after.)
- Neither friend nor Space colleague → both endpoints denied with 4xx (not 5xx,
  so a DB error cannot masquerade as a pass) and zero `reaction_users` rows.
- `TestReactionRejectsCrossSpaceDMWrite` / `TestSyncReactionHidesCrossSpaceDMReaction`
  and the group/thread gate cases pass unchanged.

Gates:

- `go build ./...`; `go vet ./modules/message/`;
  `golangci-lint run ./modules/message/...` → 0 issues.
- `make i18n-extract-check` and `make i18n-lint` pass (expected no-op: no new codes).
- `TestMessageNoLegacyResponseError` passes with any new file registered in its list.
