---
type: Task
title: "Task: space-member-removal-cleanup"
description: Make Space member removal actually close the conversation surface — emit a removal event, invalidate membership caches, cascade the member out of the Space's groups, and cut off DMs with peers who no longer share any Space.
tags: [space, isolation, auth, acl, thread, concurrency, data-integrity, wire-contract, error-response, testing, commit]
timestamp: 2026-08-21T01:56:05+00:00
# --- octospec extension fields ---
slug: space-member-removal-cleanup
upstream: none (internal follow-up to a membership-cleanup audit)
source: user
---

# Task: space-member-removal-cleanup

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. Examples and tests use synthetic UIDs and Space IDs only.

## Goal

Removing a member from a Space must end that member's participation in the
Space's conversations, not just flip a database flag.

Today every removal path performs exactly one durable action — a soft delete
(`UPDATE space_member SET status=0`) — plus, on two of the four paths, an
in-process notify cache invalidation. Every downstream consequence of membership
is left behind: the removed member keeps their `group_member` rows and WuKongIM
group subscriptions, keeps receiving and sending group messages, keeps passing
`SpaceMiddleware` for up to 60s, and keeps a usable DM channel with every former
co-member.

After this change, a removal must:

1. **Emit a durable removal event** — one event fired identically by all four
   removal paths, so downstream modules can react without `modules/space`
   importing them.
2. **Invalidate membership caches immediately** — both the Redis
   `SpaceMiddleware` cache and the notify member cache, on every path.
3. **Cascade the member out of every group in that Space** — full group-exit
   semantics (IM unsubscribe, membership row, sub-threads, pinned/conversation
   extras), with a system tip and a member-update CMD pushed to each group so
   remaining members' clients refresh.
4. **Cut off DMs with peers who no longer share any Space** — symmetric WuKongIM
   whitelist removal plus a channel-update CMD to both sides, so each client
   re-fetches channel info and renders the conversation as unable to send.

The DM rule is conditional, not unconditional: two users may share more than one
Space, and DM permission is derived from *any* shared Space (or friendship).
Removal from one Space must not cut a DM that another Space or a friendship
still authorizes.

## Background

### Current removal paths (all verified)

| Path | Handler | Durable write |
|---|---|---|
| `POST /v1/space/:space_id/members/remove` | `modules/space/api.go:830` | `removeMemberLocked` (`modules/space/db_manager.go:506`) |
| `POST /v1/space/:space_id/leave` | `modules/space/api.go:875` | same primitive, `rejectRoleAtOrAbove=2` |
| `DELETE /v1/manager/spaces/:space_id/members` | `modules/space/api_manager.go:670` | `removeMembersForce` (`modules/space/db_manager.go:412`) |
| `DELETE /v1/manager/spaces/:space_id` (force disband) | `modules/space/api_manager.go:325` | `forceDisbandSpace` (`modules/space/db_manager.go:201`) |

All four write `space_member.status=0` and nothing else. `role` and `created_at`
survive; `created_at` is deliberately never reset on rejoin
(`modules/notify/space_welcome_db.go:269`).

### What is missing today

- **No removal event.** `modules/base/event/api.go:57` defines only
  `SpaceMemberJoin`. The join side has a full side-effect bundle
  (`afterJoinSpace`, `modules/space/api.go:1288`: preset groups, default
  category, event, cache invalidation); the removal side has no counterpart, so
  no module can react.
- **`SpaceMiddleware` cache is never invalidated.**
  `pkg/space/InvalidateMembershipCache` (`pkg/space/middleware.go:59`) has zero
  production callers — only two test call sites. The positive entry
  `space:member:{spaceID}:{uid}` lives 60s (`pkg/space/middleware.go:17`), so a
  removed member keeps Space-scoped API access for up to a minute.
- **Manager paths skip even the notify invalidation.**
  `event.SpaceMemberCacheInvalidator` (`modules/base/event/api.go:154`,
  registered at `modules/notify/api.go:139`) is called only from
  `modules/space/api.go:868` and `:909`. The manager force-remove and force
  disband paths do not call it, so notify keeps treating the removed member as a
  valid target for up to its own 60s TTL (`modules/notify/space_verify.go:12`).
- **Groups are untouched.** `joinPresetGroups` (`modules/space/api.go:1307`)
  writes `group_member` rows on join; nothing reverses them. `modules/space` makes
  no WuKongIM calls at all, so the removed member stays subscribed to every group
  channel in the Space and continues to send and receive there.
- **DMs stay open.** The Person-channel whitelist datasource
  (`modules/user/1module.go:121`) returns `friends(uid) ∪ GetCoMemberUIDs(uid)`,
  and `queryCoMemberUIDs` (`modules/space/db.go:279`) already filters
  `status=1`. The *derivation* is therefore already correct — but WuKongIM caches
  the whitelist, and nothing tells it to drop the stale entry, so DMs keep
  working until an unrelated reload happens.

### Existing primitives this task reuses

- **Group exit, complete**: `groupExit` (`modules/group/api.go:3262`) and
  `Service.RemoveGroupMembers` (`modules/group/service.go:1707`) already do IM
  unsubscribe, `ConversationDelete` event, membership delete with a seq version,
  creator transfer via `QuerySecondOldestMemberExcludingBotsOf`, invited-bot
  cascade (#354 / #1186), sub-thread cleanup (`modules/group/api.go:3514`),
  `SendCMD(CMDGroupMemberUpdate)`, a system tip, and
  `RemovePinnedForUserInSpace` / `RemoveConvExtForUserInSpace`.
- **Group set for a member in a Space**:
  `queryGroupsWithMemberUIDAndSpaceID` (`modules/group/db.go:659`).
- **Symmetric DM whitelist removal**: `handleDeleteFriend`
  (`modules/user/event_friend.go:101`) removes each side from the other's Person
  channel whitelist — the exact shape needed here.
- **Person-channel CMD push**: `modules/user/api_setting.go:110` sends
  `CMDChannelUpdate` to one user's Person channel carrying the *peer* channel in
  `Param`. octo-web handles it by re-fetching channel info
  (`packages/dmworkbase/src/module.tsx`, `cmdContent.cmd === "channelUpdate"`).
- **DM/Space presence**: `pkg/space/dm_presence.go` `DMSpacePresenceSet` answers
  "did this DM pair exchange messages in this Space", keyed by
  `common.GetFakeChannelIDWith(a, b)`. Read shape already in use at
  `modules/message/space_filter.go:153-160`.
- **Import-cycle-free wiring**: `modules/group` imports `modules/space`, so the
  dependency cannot be reversed. Two established escapes exist: an init-time
  reverse-registered hook (`modules/space/hooks.go`) and a fan-out event listener
  (`ctx.AddEventListener`, used for `GroupMemberAdd` by notify/message/group).

## Behavior contract

### Trigger

- One event, fired by all four removal paths, carrying `(space_id, uid,
  operator_uid, reason)` where reason distinguishes kicked / left / force-removed
  / space-disbanded.
- The event is durable (`wkevent` table + `EventCommit`, following
  `fireSpaceMemberJoinEvent` at `modules/space/api.go:2207`), because every
  consequence below is an external call that must survive a crash and be retried.
- Fired **after** the membership transaction commits. A rolled-back removal must
  not emit externally visible side effects.
- Batch removal fires one event per removed uid; one uid's failure must not
  abort the rest of the batch.

### Group cascade

- Scope: every group with `group.space_id = <spaceID>` where the member has a
  live `group_member` row.
- Semantics: full exit, reusing the existing group primitive rather than
  reimplementing it. Creator role transfers first; invited bots cascade per
  #354/#1186; sub-thread membership, subscriptions, pinned rows and conversation
  extras are cleaned per Space.
- Each affected group receives a member-update CMD and a system tip so remaining
  members' clients converge.
- Idempotent: replaying the event for an already-exited group is a no-op, not an
  error.

### DM cutoff

- **Peer set** = peers of the removed member's Person conversations
  (`ctx.IMSyncUserConversation`) ∩ active members of the Space, narrowed by
  `DMSpacePresenceSet(fakeIDs, spaceID)` to pairs that actually exchanged
  messages in this Space. Build fake IDs with `common.GetFakeChannelIDWith`;
  a `LIKE` scan over `dm_space_presence.fake_channel_id` is forbidden — the
  column is a primary-key prefix with no index for substring search and the id
  format is not a stable contract.
- **Condition**: for each peer, re-evaluate authorization *after* the removal
  commits. Drop the whitelist entries only when the pair shares no remaining
  active Space (`queryCoMemberUIDs`) **and** is not a friend pair. Peers who
  still share another Space, or who are friends, keep their DM.
- **Action**: symmetric `IMWhitelistRemove` on both Person channels, mirroring
  `handleDeleteFriend`.
- **Bot DMs** use the Space-prefixed channel form `s{spaceID}_{uid}`
  (`pkg/space/channel.go` `BuildChannelID`; see `modules/app_bot/app_bot.go:1139`)
  rather than the bare-uid channel used by human DMs. Both forms must be handled;
  a human-only implementation silently leaves bot DMs open.
- **Event push**: a `CMDChannelUpdate` to each side's Person channel naming the
  peer channel, so both clients re-fetch channel info.
- **Client-visible signal**: the Person `ChannelResp`
  (`modules/user/1module.go:189`) must expose whether the viewer may still send
  to this peer, so octo-web can render the composer read-only the way
  `packages/dmworkbase/src/Utils/groupDisband.ts` does for a disbanded group.
  Prefer an additive field; do not overload `be_deleted` (friend deletion) or
  `be_blacklist` (blacklist), whose meanings clients already act on.

### Cache invalidation

- `pkg/space.InvalidateMembershipCache(redis, spaceID, uid)` on every removal
  path, before the event is emitted, so the removed member cannot slip through
  `SpaceMiddleware` in the interim.
- `event.SpaceMemberCacheInvalidator(spaceID)` on **all four** paths, closing the
  current manager-path gap.
- Cache invalidation is per-process for notify; the durable event, not the
  in-process hook, is what makes the outcome eventually consistent across
  replicas.

### Failure semantics

- The HTTP removal response must not depend on IM or group cascade success. The
  membership write and cache invalidation are synchronous; the cascade is driven
  by the durable event and retried by the existing event timer.
- Every step is idempotent under retry: re-running a completed cascade produces
  no duplicate tips, no duplicate CMDs that change state, and no error.

## Load-bearing list

- **space**, **isolation**, **auth**, **acl** — this change is the enforcement
  half of Space membership. A removed member currently retains group message
  access and DM access inside a Space they no longer belong to; the fix must not
  overshoot and cut access that a second shared Space or a friendship still
  authorizes. (rule: `space-isolation`)
- **thread** — the group cascade reaches sub-thread membership, sub-thread IM
  subscriptions and per-Space pinned rows via
  `removeUserFromGroupThreadsCleanup` (`modules/group/thread_cleanup.go`); Issue
  #27 and YUJ-52 established that skipping it leaves a bot or member subscribed
  to a sub-channel after losing the parent. (rule: `space-isolation`)
- **concurrency** — removal already runs under `SELECT ... FOR UPDATE` to
  prevent an ownerless Space (PR #339). The new side effects run after commit and
  must not reintroduce a check-then-act window: DM authorization is re-evaluated
  post-commit, and a concurrent rejoin must not be undone by a late-arriving
  cascade.
- **data-integrity** — the cascade spans MySQL (`group_member`, `thread_member`,
  `user_pinned_channel`, `conversation_extra`), the event table, and WuKongIM.
  Partial completion must be retryable and idempotent, never leave a member
  half-removed from a group, and never emit side effects for a rolled-back
  removal.
- **wire-contract** — a new Person-channel `ChannelResp` field and a new CMD
  delivery to Person channels are additive client-facing contract changes. Older
  clients must keep working: no existing field's meaning changes, and the server
  enforcement (whitelist) does not depend on the client honoring the new field.
- **error-response** — removal handlers keep the existing localized envelope
  (`httperr.ResponseErrorL` + `pkg/errcode`); cascade failures are logged, not
  surfaced as new user-facing error codes. No raw `c.ResponseError` / `c.JSON`
  is added. (rule: `error-handling`)
- **rate-limit** — no new HTTP route and no request-frequency policy change. The
  cascade's IM fan-out is bounded by the peer/group set, not by request rate;
  it must not hand-roll a Redis counter. (rule: `rate-limit`)
- **testing** — behavior-level tests against the real removal handlers and the
  real cascade boundary, with synthetic UIDs/Spaces, covering the multi-Space
  and friendship carve-outs, the bot-DM channel form, batch partial failure, and
  idempotent replay. New guard test entries for any new handler file in the
  module's `Test<Module>NoLegacyResponseError` list. (rule: `testing`)
- **commit** — English Conventional Commits; Implement records RED/GREEN
  evidence without production identifiers. (rule: `commit-style`)

## Out of scope

- **Message history and archives.** Messages the removed member sent or received
  stay readable to remaining members; nothing is erased or retracted.
- **`dm_space_presence` rows.** The presence index is read-only input here;
  removal does not delete rows (deleting them would break the Space filter's
  historical visibility for remaining members).
- **Session and token revocation.** `allowedSessionRevocationReasons`
  (`modules/user/session_revocation.go:41`) gains no Space reason. A user may
  belong to several Spaces; removal from one is not an account-level event.
- **Category data.** The removed member's `modules/category` rows in that Space
  are preserved; there is deliberately no inverse of
  `ensureDefaultCategoryProvisioned`.
- **`space_join_apply` convergence at removal time.** The existing lazy
  compensation `resetApprovedApplyForRejoin` (`modules/space/db.go:884`) stays as
  is; eager convergence is a separate P1.
- **Unused `space_email_invite` invalidation.** P1.
- **Deleting the removed member's conversations in that Space** and clearing
  their `user_pinned_channel` / `conversation_extra` for *non-group* channels.
  P1 — the group cascade already covers the group-channel subset.
- **Bot ownership reassignment.** Bots created by the removed member keep their
  own `space_member` rows and are unaffected except where the existing group
  invited-bot cascade already applies.
- **octo-web `SpaceService.removeMembers` URL mismatch.** It issues
  `DELETE space/{id}/members`, but the user-side route is
  `POST /v1/space/{id}/members/remove` (the DELETE form exists only under
  `/v1/manager/spaces/...`). Its only caller, `Components/SpaceMembers`, is
  exported but not mounted, so this is latent. Tracked separately.
- **Reworking `SpaceMiddleware`'s TTL model** or making it a global mount. Only
  the missing invalidation call is added.

## Acceptance

**Event and cache**

- All four removal paths emit exactly one removal event per removed uid, after
  the membership transaction commits, with the reason distinguishing kicked /
  left / force-removed / space-disbanded.
- A removal transaction that rolls back emits no event and performs no IM call.
- After any removal path returns, a request from the removed member carrying
  that `space_id` is rejected by `SpaceMiddleware` on the next call — no 60s
  window. Asserted against the Redis cache key, not only the DB.
- `event.SpaceMemberCacheInvalidator` is invoked on the manager force-remove and
  force-disband paths, closing the current gap; asserted with a test double.

**Group cascade**

- After removal, the member has no live `group_member` row in any group with
  `space_id = <spaceID>`, and no WuKongIM subscription to those group channels.
- Groups in *other* Spaces that the member belongs to are untouched.
- A group whose creator is the removed member gets a new creator via the existing
  second-oldest-member rule; no group is left creatorless, and no bot is chosen.
- Bots the removed member invited into those groups are cascaded out, with the
  existing tip, matching `groupExit` behavior.
- Each affected group receives one member-update CMD and one system tip.
- The member's sub-thread rows, sub-thread subscriptions, per-Space pinned rows
  and conversation extras for those groups are cleared.
- Replaying the event for the same `(space_id, uid)` produces no second tip, no
  error, and no state change.

**DM cutoff**

- Given a peer who shared **only** this Space with the removed member and is not
  a friend: both Person-channel whitelist entries are removed, both sides receive
  a channel-update CMD, and a send attempt in either direction is rejected by
  WuKongIM.
- Given a peer who shares **another active Space**: no whitelist entry is
  removed and DMs keep working. This case must have an explicit test — it is the
  primary regression risk.
- Given a peer who is a **friend** but shares no remaining Space: DMs keep
  working.
- Given a **bot** peer whose DM uses the `s{spaceID}_{uid}` channel form: the
  Space-prefixed channels are handled, not silently skipped.
- Only peers with a `dm_space_presence` row for this Space are touched; the
  implementation issues no `LIKE` query against `fake_channel_id`.
- The Person `ChannelResp` reports the not-sendable state for a cut peer and the
  sendable state for a retained peer; the field is additive and `be_deleted` /
  `be_blacklist` keep their current meanings.

**Batch and robustness**

- In a batch removal where one uid's cascade fails, the other uids are fully
  processed and the failed one is retried by the event timer.
- The HTTP response of every removal path is unchanged in shape and is not
  affected by IM or cascade failure.

**Repo gates**

- `go test ./modules/space/... ./modules/group/... ./modules/user/...` passes,
  including the new regression tests.
- `gofmt`, `go vet` on touched packages, `make i18n-extract-check`,
  `make i18n-lint`, and `golangci-lint run` on touched packages pass — or an
  infrastructure-only blocker is recorded with its exact command and output.
- Any new handler file is added to the module's
  `Test<Module>NoLegacyResponseError` guard list.
- No production UID, Space ID, group number, or domain appears in the diff,
  tests, fixtures, or commit text.
