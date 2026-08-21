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

1. **Emit a removal event** — one event fired identically by every removal path,
   so downstream modules can react without `modules/space` importing them.
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
| `DELETE /v1/space/:space_id` (owner disband) | `modules/space/api.go:583` | `disbandSpace` (`modules/space/db.go:62`) |

The last row was missed in the original survey: `DB.disbandSpace` was a single
`UPDATE space SET status=0` that left every `space_member` row active, so it did
not even register as a removal path. It is the route an ordinary Space owner
takes, and therefore the common one.

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

- One event, fired by every removal path, carrying `(space_id, uid,
  operator_uid, reason)` where reason distinguishes kicked / left / force-removed
  / space-disbanded. There are **five** such paths, not four: the owner-initiated
  `DELETE /v1/space/:space_id` was originally overlooked (it only flipped
  `space.status`) and is the most common disband route, not the super-admin one.
- The event is only persisted when a listener is registered. Writing a row costs
  a transaction plus two more round-trips, and `handleEvent` marks it Success
  even with no listeners; disbanding a large Space would spend tens of thousands
  of DB operations on nobody. The extension point stays: registering a listener
  starts delivery with no further change.
- The event is persisted (`wkevent` table + `EventCommit`, following
  `fireSpaceMemberJoinEvent` at `modules/space/api.go:2207`) so a crash between
  the membership commit and the dispatch does not lose the cascade. **The event
  table is crash recovery, not a retry queue** — see *Failure semantics*.
- The event must be fired off the request goroutine (`go s.fire...`, as the join
  path does). In the listener branch, `handleEvent`
  (`modules/base/event/handler.go:36`) runs every listener **synchronously on the
  caller's goroutine** with no `EventPool` hand-off, so a slow cascade would
  otherwise block the HTTP handler.
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
  (`ctx.IMSyncUserConversation`) ∩ everyone who has **ever** had a
  `space_member` row for this Space (`MembersEverInSpace`, deliberately
  status-agnostic). The conversation list is itself the evidence that a DM
  exists; the membership term only scopes it to this Space.
  - Membership must **not** be filtered on `status=1`: cleanup always runs after
    the member row is zeroed, and disband zeroes the Space row too, so an
    "active member" predicate returns the empty set after a disband and whenever
    two peers are removed in the same batch — the cutoff silently does nothing.
  - `dm_space_presence` is **not** a gate. It is a best-effort, never-backfilled
    index, written only for non-encrypted Person messages carrying a space id and
    dropped on write failure; its own documentation tells readers to OR it with a
    fallback so a missing row never hides a DM. As the sole gate it would let any
    pair it missed escape the cleanup entirely.
- **Condition**: for each peer, re-evaluate authorization *after* the removal
  commits, **per direction**. X's Person-channel whitelist is
  `friends(X, is_alone=0) ∪ coMembers(X)` — who may send *to* X — so the two
  directions can differ under a one-sided friendship and must be judged
  separately. A direction is cut only when the pair shares no remaining active
  Space (`SharesActiveSpace`, same predicate as `queryCoMemberUIDs`) **and** that
  side's whitelist no longer authorizes the sender. ORing the two directions
  leaves a stale entry that nothing removes.
- **Action**: `IMWhitelistRemove` on each Person channel whose direction was
  revoked, mirroring `handleDeleteFriend`.
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
- `event.SpaceMemberCacheInvalidator(spaceID)` on **every** path, closing the
  current manager-path gap. It is per-Space, so a batch invalidates once, not
  once per member.
- `event.SpaceMemberCacheInvalidator` clears only the calling process's notify
  cache; other replicas keep a stale entry until their own 60s TTL expires. That
  is accepted (notify only gates card/notification targeting, and the DB is
  re-read on expiry) — but it means the in-process hook is not the isolation
  mechanism. The Redis invalidation plus the group/DM cascade are.

### Failure semantics

The event bus does **not** provide retry. Verified behavior:

- `updateEventStatus` (`modules/base/event/api.go:128`) marks a failed listener's
  event `Fail`, and `QueryAllWait` (`modules/base/event/db.go:49`) selects only
  `status = Wait`. A `Fail` row is therefore never re-dispatched. The timer
  (`main.go:488`, every 59s, with a `created_at < now-60s` guard) recovers only
  events that were never dispatched at all — i.e. a process crash between insert
  and `EventCommit`.
- `UpdateStatus` (`modules/base/event/db.go:34`) matches `version_lock` in its
  `WHERE` but never increments it, so with multiple listeners on one event every
  write succeeds and **last writer wins**: one listener's success can overwrite
  another's failure on the shared row. The repo's existing convention for
  multi-listener events is therefore `commit(nil)` plus module-owned
  compensation (`modules/notify/api.go:252` and the comment at `:257`).

Consequently:

- Listeners for this event call `commit(nil)` and **own their own retry**. Do not
  rely on the event timer, and do not report cascade failure through
  `updateEventStatus` — with two listeners it is not a reliable signal.
- The cascade needs a durable per-`(space_id, uid)` work record with `status`,
  `attempts`, `next_attempt_at` and a lease, modeled on
  `user_session_revocation_intent`
  (`modules/user/sql/20260809000001_user_session_revocation_intent.sql`,
  driver in `modules/user/session_revocation.go`, scheduled at
  `modules/user/api.go:445`). This is the repo's established pattern for
  "external call that must survive a crash and be retried", and it is what makes
  the group cascade and DM cutoff eventually consistent. Alternatively a
  reconciler-scan may substitute for the ledger where a cheap authoritative
  re-derivation exists (notify's `spaceWelcome` reconciler is the precedent), but
  "the event will be retried" is not an option.
- The HTTP removal response must not depend on IM or group cascade success. The
  membership write and both cache invalidations are synchronous and inside the
  request; everything else is driven by the record above.
- Every step is idempotent under retry: re-running a completed cascade produces
  no duplicate tips, no duplicate CMDs that change state, and no error.
- The work record must be reconciled against live state at execution time, not
  trusted blindly: a member who was removed and has since rejoined must not have
  their groups and DMs torn down by a late-arriving retry.

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
- **event-bus semantics** — `modules/base/event` is a fire-once dispatcher, not
  a retry queue: a failed listener is marked `Fail` and never re-selected
  (`modules/base/event/api.go:128`, `db.go:49`), and multiple listeners share one
  status row whose `version_lock` is never incremented (`db.go:34`), so
  last-writer-wins. Any design that assumes at-least-once delivery to a listener
  is wrong. This change adds a second listener to a fan-out event and must not
  make the existing `SpaceMemberJoin`/`GroupMemberAdd` listeners' behavior worse.
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
- **Observability for the cascade queue.** The worker is single-flight per
  process and drains at most 20 jobs per 10s tick, each walking every group in
  the Space, so a large disband takes a long time to converge and the removed
  members hold group access for that whole window. The design converges, but
  there is no gauge on pending rows or oldest pending `created_at` — nothing
  shows how far behind it is. Follow-up.
- **A peer whose conversation the removed member deleted.** Peer scope comes
  from `IMSyncUserConversation` on the removed member. Verified against the
  pinned broker: `/conversation/sync` appends a conversation only when its
  recent-message window is non-empty, and after a delete it fetches only
  messages past `DeletedAtMsgSeq` — so a conversation the member deleted with
  nothing sent since is simply absent, that peer never enters the candidate set,
  and **both** whitelist entries survive. `GetLastConversations` is also capped
  at `conversation.userMaxCount` (default 1000). This is the one remaining way a
  pair escapes the cutoff entirely.

  The restore step **widened** this rather than merely inheriting it: before this
  task nothing granted whitelist entries to non-friend co-members at all, so the
  chain unfriend → rejoin (restore re-grants on co-membership) → delete the
  conversation → get removed is newly constructible.

  **Part of it is now closed in this task** (round 10) — the **bare** Person channel
  only; see the scope note below. `dm_cutoff` starts by
  overwriting the removed member's own bare Person channel with the derived set
  `friends(uid, is_deleted=0, is_alone=0) ∪ coMembers(uid)`
  (`/channel/whitelist_set`, `reconcileRemovedMemberChannel`). The broker's
  `allowSend(from, to)` consults the allowlist of **`to`'s** channel
  (`internal/service/permission.go:252`), so overwriting the removed member's own
  channel closes "who may send **to** him" by construction — with no peer
  enumeration, and therefore no dependence on his conversation view.
  `dm_restore` applies the identical call on the join side, because the cutoff
  now removes strictly more than per-peer enumeration ever could and an
  asymmetric restore would leave the rejoiner unable to receive.

  Measured end-to-end against the pinned broker with `whitelistOffOfPerson=false`
  (probe in the task journal): removal-with-deleted-conversation leaves both
  directions ALLOWED (the escape reproduced); after the overwrite peer→removed is
  BLOCKED and removed→peer is still ALLOWED; after the rejoin overwrite both are
  ALLOWED again.

  **Scope of what the overwrite closes.** It writes exactly one channel — the bare
  `uid` form. Space-prefixed `s{spaceID}_{uid}` channels are **not** touched; entries
  there are removed only by `removeSpaceScopedWhitelist`, reachable only from
  `cutOffDM`'s `if bot` branch, i.e. still enumeration-bound. Reviewed and corrected
  after review pointed out the original claim read as covering the whole inbound
  direction.

  How big that prefixed residual is, checked rather than assumed: every site that
  writes a prefixed Person-channel whitelist entry (`app_bot` ×1, `botfather` ×3)
  writes bidirectional friend rows first, so an entry surviving a Space removal
  usually belongs to a pair the friendship still authorizes — nothing that should
  have been revoked. The exception is real, though, and **pre-dates this task**:
  `handleDeleteFriend` clears only the two bare channels, never the prefixed ones,
  and the `IMBlacklistAdd` it adds for bots lands on the **bot's** channel
  (blocking user→bot), so it does not stop bot→user. Deleting a Space-scoped bot
  friend therefore leaves `s{spaceID}_{user}` authorizing that bot inbound. A defect
  in the friend-delete path rather than the removal path; the overwrite does not
  reach it either. #797, together with the channel-form decision below.

  **The other direction is also open** — "who may the removed member send to".
  Those entries live on each *peer's* channel, which a single-sided overwrite
  cannot reach, and enumerating the peers is exactly what the conversation view
  fails to do. That still needs an authoritative or bilateral pair index this repo
  does not have (`dm_space_presence` is best-effort, which is why it was demoted
  from a gate). Follow-up, #797.

  Safety of the overwrite (this is what makes it usable at all): all seven call
  sites in the repo that write a bare Person-channel whitelist entry —
  `modules/user` friend-approve ×2 and botfather-on-register, `modules/app_bot`,
  `modules/botfather` ×3 — write a **bidirectional friend row** before granting
  (bots included, via `AddFriend`), and the only other grant basis is
  co-membership. So the derived set is a superset of everything this codebase has
  ever put there, and the overwrite cannot drop a grant the model does not know
  about.
- **Closing the rejoin window completely.** The group step now re-checks live
  membership itself (`CheckMembership`, immediately before it computes the group
  set), so the exposure shrank from "the whole duration of the DM step" to the
  gap between two adjacent queries. It is not zero: a rejoin committing inside
  that gap still gets its fresh `group_member` rows torn down. Closing it fully
  needs a membership epoch on `space_member` re-checked inside each step, which
  the join path must also stamp. Follow-up.
- **Lock-order documentation across `space` / `space_member`.** Both disband
  paths now take `space_member FOR UPDATE` before updating `space`, inverting the
  parent-then-child order the old `forceDisbandSpace` used. Every transaction in
  `modules/space` that touches both was checked and no ABBA deadlock is reachable
  today, but the inversion is real and deserves a written ordering note next to
  the other lock-order comments. Follow-up.
- **Bot deletion paths.** `modules/botfather` flips `space_member` rows to
  `status=0` without enqueueing cleanup; they predate this change and handle
  group exit inline. They are the remaining "membership ends, surface stays"
  paths after this lands. Follow-up.
- **Promoting a non-owner's bot to group creator.** A review round flagged
  `QuerySecondOldestMemberExcludingBotsOf` for allowing it, but
  `TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft` pins that as
  deliberate ("他人的 bot 不在排除范围内"). Changing it would alter `groupExit`;
  out of scope here.
- **Fixing `modules/base/event`'s retry and multi-listener status semantics
  globally.** The `Fail`-is-terminal behavior and the non-incrementing
  `version_lock` affect every event in the repo; this task works within them
  (own retry record, `commit(nil)`) and does not change the shared bus. Worth a
  separate task.

- **Group-side IM unsubscribe stays best-effort.** A review round asked for the
  same error propagation the DM step now has. It was deliberately not applied:
  the authoritative subscriber source for a group channel is the
  `IMDatasource.Subscribers` callback, which re-reads `group_member`. Moving
  `IMRemoveSubscriber` before the delete would be undone by the next reload
  (the documented YUJ-4185 root cause); propagating after the delete buys
  nothing because the retry scope (`queryGroupsWithMemberUIDAndSpaceID`) no
  longer contains that group. The DM step differs only because its scope
  (`MembersEverInSpace`) is status-agnostic, so a retry really does re-run.
  Reasoning is recorded at the call site.
- **Per-package CI budget for `modules/user`.** The package sits at roughly 90%
  of its 5-minute budget before this change (248s locally with `-race
  -shuffle=on`, more on a CI runner) across 186 `testutil.NewTestServer` calls at
  ~0.42s each. This task shrinks its own footprint (one shared server for the DM
  test file, one process-wide cleanup scheduler instead of one per `Route()`) but
  cannot move the package off the ceiling. Splitting `modules/user` across shards
  or raising its timeout is a CI-config follow-up.
- **Remaining P2 review findings.** `LockRemovableMemberTx` conflating "became
  creator" with "already left"; no supporting index on the purge query; the
  locking successor query locking more rows than it returns; `attempts` counted
  but with nothing acting on the ceiling; a transient DB error in the bot check
  skipping the prefixed-channel cleanup. All advisory, none new in this change's
  behavior. Follow-up.

- **Person-channel whitelist enforcement is off by default in WuKongIM.** Verified
  end to end against `v2.2.4-20260313` (the CI-pinned tag) with a real client:
  `options.WhitelistOffOfPerson` defaults to **true**, and when it is true
  `PermissionService.allowSend` never consults the whitelist — a send with no
  whitelist entry is ACCEPTED. With `whitelistOffOfPerson=false` the same probe
  gets `ReasonNotInWhitelist` with no entry, ACCEPTED after `whitelist_add`, and
  rejected again after `whitelist_remove`. This repo's CI sets only `WK_MODE` /
  `WK_TOKENAUTHON` / `WK_EXTERNAL_*`, so **in the CI configuration the DM cutoff
  is a no-op**. The cutoff and the restore are both correct and harmless either
  way, but whether removal actually blocks DMs depends entirely on the deployed
  broker's `whitelistOffOfPerson`. That value lives in octo-deployment and must be
  confirmed before this task's DM guarantee can be claimed in production.
- **The datasource callbacks are dead code in the shipped broker.** `s.datasource`
  is assigned once in `internal/server/server.go` and never read;
  `datasource.GetSubscribers` / `GetWhitelist` have zero callers. So the
  `IMDatasource` callbacks registered in `modules/group/1module.go` and
  `modules/user/1module.go` are never consulted, and there is no "next reload
  self-heals" backstop anywhere. An earlier round of this task argued from that
  backstop; the argument was wrong and has been retracted in the code comments.

## Deviations from the original plan

Recorded so the diff can be read against the spec rather than against memory.

- **A join-side restore was added.** The plan only covered revocation. Removing a
  member drops Person-channel whitelist entries, and nothing granted them back —
  the broker's whitelist store is mutated only by the proactive
  `whitelist_add` / `whitelist_remove` APIs, so `kick → re-add` left the pair
  permanently unable to DM wherever whitelist enforcement is on. A `dm_restore`
  step now mirrors `dm_cutoff` (same scope, same per-direction predicate, same
  bot prefixed channels), registered next to it and wired into all four join
  paths. It is deliberately best-effort rather than outbox-backed: failing to
  revoke is an authorization leak and must converge, failing to re-grant is a
  visible, user-recoverable loss of function.
- **The `isRobot(self)` hoist changed error semantics.** Moving the "is this member
  a bot" lookup out of the per-peer loop removes a query that was invariant across
  peers, but it is not behaviour-preserving: the lookup used to run inside
  `cutOffDM` *after* the bare-channel writes, so an error cost one peer its
  prefixed channel and later peers still ran. It now runs before any peer is
  touched, so a persistent `robot`-table error fails the whole step with nothing
  cut at all. The returned value is unchanged and retries are idempotent, so this
  is slower convergence rather than a leak — but it is a change in the losing
  direction, made by a refactor no review asked for.
- **Four join paths, not three.** `modules/space/api.go` `addMembers` and the
  manager endpoint both bypass `afterJoinSpace`; only join-by-code and approved
  applications go through it.
- **The Person-whitelist derivation was de-duplicated, and its error handling
  tightened.** The rule lived inline in `modules/user/1module.go`'s
  `IMDatasource.Whitelist`; it now lives once in
  `modules/user/person_whitelist.go` (`derivePersonWhitelist`) and both callers
  share it. This is required, not cosmetic: `reconcileRemovedMemberChannel`
  applies the same rule with **overwrite** semantics, so two implementations
  drifting apart would turn into wrongly-revoked authorization on whichever side
  drifted. One behaviour change came with the move — the old inline version
  swallowed a `GetCoMemberUIDs` error and returned a friends-only set; the shared
  function propagates it. For the overwrite caller that is mandatory (a truncated
  set *is* a mis-revocation); for the broker callback, which the pinned broker
  never invokes, returning a partial whitelist was never better than erroring.
- **`restoreDM`'s guard moved to the write it actually guards.** Round 9 moved the
  three authorization reads inside `restoreDM`, after `CheckMembership`. That fixed
  the peer-direction window but shifted rather than closed the problem: bare
  Person-channel grants are authorized by `friends ∪ coMembers(any active Space)` —
  `CheckMembership` was never their guard — while the `s{spaceID}_` bot-channel
  grants are Space-scoped and `CheckMembership` is the *only* thing tying them to
  this Space. After round 9 those sat four queries and two broker round trips
  downstream of it. The order is now: peer reads → bare writes → `CheckMembership`
  → `isRobot` → Space-scoped writes. Peer-side window 0, joiner-side window 0 for
  the only membership-dependent write, and both extra queries are skipped entirely
  when neither direction grants. Consequence worth stating: `restoreDM` no longer
  refuses a bare-channel grant merely because the joiner left *this* Space — if the
  pair is still friends or still shares another Space the grant is correct, and the
  cutoff would not have cut them either. Not zero-window; membership epochs (#797)
  remain the real close.
- **`dmPeerCandidates` uses the job's Space id as a fallback, not a fast path.**
  Round 9 stripped the job's own prefix first. Space ids may contain `_`, so one id
  can be a `_`-delimited prefix of another — which is exactly why `knownSpaceIDs` is
  sorted length-descending. Stripping unconditionally makes the shorter id win: with
  `minglue` and `minglue_default` both live and the job on `minglue`,
  `sminglue_default_botfather` strips to `default_botfather` instead of `botfather`,
  the peer fails `MembersEverInSpace`, and the bot DM is silently skipped with the
  job marked `done` — the same failure the round-9 change set out to remove, and this
  variant hits **active** Spaces. `ParseChannelID` now runs first and the job's id
  applies only when it resolves nothing, which still covers the disbanded non-hex
  case it was added for. Latent rather than live in this tree (no colliding pair
  exists today), fixed anyway because the cost is one line.
- **`modules/botfather`'s delete path is weaker than its command path.** Both zero
  `space_member` outside this task's outbox (already recorded as a follow-up), but
  the asymmetry is worth writing down for whoever picks that up: `command.go` calls
  `IMRemoveSubscriber` and soft-deletes `group_member` before zeroing the membership,
  while `deleteUserBot` (`api_user.go:513`) goes straight from the friend rows to
  `space_member` and `deleteRobot` with neither step — so the API path leaves the bot
  in the Space's groups with live IM subscriptions. Untouched here; raised in review
  against this branch and recorded so the follow-up covers both paths.
- **The overwrite resolves an inconsistency the repo already had, in the widening
  direction — recorded as a decision, not left silent.** `derivePersonWhitelistOfUID`
  is channel-form-agnostic (the broker callback strips any prefix and applies the same
  rule), but bot onboarding writes the whitelist entry **only** to the prefixed channel
  when a common Space exists. So a Space-scoped bot friend has no bare-channel entry
  today, and the overwrite adds one: the bot gains an unscoped DM route it never had,
  and keeps it after the user leaves that Space, because removal does not touch friend
  rows. This task treats the declared rule as authoritative — the overwrite makes the
  broker agree with the rule the repo states — but that is a judgement call between two
  inconsistent things, and the alternative reading (the prefixed-only write was
  deliberate Space scoping, and the rule should become channel-form-aware) is equally
  available. Inert while `whitelistOffOfPerson` holds its default `true`. **This must be
  decided before whitelist enforcement is enabled**, alongside join-time provisioning.
  #797.
- **A failed `dm_restore` overwrite now costs more than it used to.** The restore is
  deliberately best-effort with no retry, calibrated when a failure cost the enumerated
  peers. The overwrite replaces the joiner's whole own-channel set, so a failure now
  leaves them unable to receive from **every** co-member of the Space they just joined,
  not a handful. Still loss-of-function rather than over-permission, still the safe side
  of the asymmetry, and it self-heals on the next removal or join — but the enlargement
  is real and is recorded here rather than discovered later. Whether this one call
  deserves a bounded retry, unlike the per-peer half, is open.
- **The removal cascade's per-member lock loop now sorts by uid.** `RemoveGroupMembers`
  takes `FOR UPDATE` on each member row inside one transaction, in the order the caller's
  list produced — the ABBA precondition against `handOverGroupCreator`, which locks the
  leaver and then its successor. Same group, different orders, and MySQL aborts one:
  the cleanup job retries and converges, but the manager batch-kick path surfaces a 500.
  Sorting `removableMembers` (the iteration order, which comes from
  `QueryMembersWithUids`, not from the request slice) fixes the ordering for one sort's
  cost. Not reproduced as a deadlock — derived from the two lock orders; the successor
  scan additionally has no usable index for its `ORDER BY created_at`, so it locks more
  rows than it returns. That index is a follow-up.
- **The overwrite can lose a concurrent grant, and that is accepted.** Between the
  derive (a DB read) and the `whitelist_set` POST, another module may
  `whitelist_add` to the same channel — a friend approval or a bot approval. That
  grant is then overwritten away, and nothing re-adds it, because the granting side
  does not re-run. Per-uid `whitelist_remove` does not have this failure mode: it
  only names the people who should not be there. It is accepted on the same
  asymmetry the rest of this task uses — a missed revocation is **over-permission**
  and must converge, whereas a wrongly-dropped grant is **loss of function**: visible,
  affecting one direction of one pair, and recoverable by the user (re-friend,
  rejoin) or by operations. Removing the window entirely requires read-then-diff,
  and octo-lib exposes no whitelist read. Follow-up, #797.
- **The derivation now has two entry points, deliberately.** The prefix strip it
  inherited (`HasPrefix("s")` + `LastIndex("_")` ⇒ treat as `s{spaceId}_{uid}`) is a
  heuristic, and it had only ever been fed channel IDs coming from the broker. The
  overwrite feeds it a **uid** taken from the work record, where the same heuristic
  turns a real uid shaped like `s..._...` into a different user — deriving someone
  else's whitelist and writing it onto this member's channel, i.e. a silent
  mis-revocation. `derivePersonWhitelistOfUID` (no guessing) is therefore the entry
  point every write path uses; `derivePersonWhitelist` (strip, then delegate) stays
  for the channel-ID caller. Pinned by
  `TestDMCutoffOverwriteDoesNotGuessSpacePrefixOnUID`, which asserts both entry
  points' differing results so they cannot be merged back together by accident.
- **A cost the overwrite carries.** The derived set contains `coMembers(uid)`,
  bounded by the total membership of every Space the member still belongs to, so
  a removal from a very large Space produces one POST carrying that many uids.
  It is once per removal job (not once per peer) and removals are low-frequency,
  which is the trade accepted here — and it is precisely why the per-peer loop was
  **not** switched to this set.

- **A fifth removal path.** The owner-initiated disband was not in the original
  survey (see the path table). It now runs the same cascade.
- **`dm_space_presence` demoted from gate to nothing.** The plan used it to
  narrow the peer set; two review rounds showed that as the sole gate it lets any
  pair the index missed escape the cleanup. Scoping is now conversation list ∩
  ever-a-member.
- **A batch cap on the user-facing endpoint (wire-visible).**
  `POST /v1/space/:space_id/members/remove` had no limit while the manager
  endpoint enforced `managerMaxBatchUIDs=200`; each uid costs a transaction, a
  Redis DEL and an outbox row, so an unbounded list is a denial-of-service lever.
  The same 200 cap now applies. **A client that previously sent more than 200
  uids in one call will start receiving a batch-too-large error.**
- **Retry envelope widened.** The plan said "attempts / next_attempt_at / lease"
  without numbers. Shipped: 5-minute backoff cap, 20 attempts (~70 minutes),
  10-minute lease. The lease is long because the group cascade genuinely runs for
  minutes and an expired lease means concurrent re-execution, not just a lost
  write.
- **Terminal rows are purged.** Not in the plan; an hourly bounded delete keeps
  the outbox and its pending index from growing without limit. Only `done` rows
  are removed — `abandoned` is the only durable record that a cleanup gave up.
- **The per-member group tip is suppressed on disband.** The acceptance list says
  each affected group receives one system tip. For `space_disbanded` that would be
  N members × M groups of "removed by <admin>" messages in a Space that no longer
  exists, all landing on whoever is removed last. Suppressed for that reason and
  for `left` (where the operator is the member themselves); ordinary kicks still
  notify, pinned by `TestGroupCascadeKickStillNotifies`.

## Acceptance

**Event and cache**

- Every removal path — including the owner-initiated
  `DELETE /v1/space/:space_id` — zeroes the member rows, enqueues exactly one
  cleanup job per removed uid inside the same transaction, and emits one removal
  event per uid after commit, with the reason distinguishing kicked / left /
  force-removed / space-disbanded.
- With no listener registered, no event row is written at all.
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
- A peer with **no** `dm_space_presence` row is still cut when the relationship
  no longer authorizes the DM: the presence index is best-effort and must not
  gate the isolation cleanup. Peer scoping uses "ever a member of this Space",
  and the implementation issues no `LIKE` query against `fake_channel_id`.
- Cutting is per direction: under a one-sided friendship only the unauthorized
  direction's whitelist entry is removed, and the authorized one is left intact.
- After a force-disband, and when two peers are removed in the same batch, the
  cutoff still runs — an "active member" predicate would return the empty set in
  both cases and silently do nothing.
- The Person `ChannelResp` reports the not-sendable state for a cut peer and the
  sendable state for a retained peer; the field is additive and `be_deleted` /
  `be_blacklist` keep their current meanings.

**Batch and robustness**

- In a batch removal where one uid's cascade fails, the other uids are fully
  processed and the failed one is retried by this task's own durable work record
  — asserted by driving the retry, not by asserting on `event.status`.
- A listener failure does not leave the cascade permanently dropped: a test that
  fails the first attempt and then runs the retry driver reaches the same final
  state as a clean run.
- A member removed and then rejoined before a pending retry executes keeps their
  groups and DMs: the retry re-checks live membership and becomes a no-op.
- The HTTP response of every removal path is unchanged in shape and is not
  affected by IM or cascade failure.

**C1 — the deleted-conversation escape (round 10)**

- Given a removed member whose conversation with a peer is absent from
  `/conversation/sync`: the per-peer loop issues nothing (that is the escape), and
  the member's own Person channel is still overwritten with the derived set, with
  the peer absent from it.
- The overwrite does not over-revoke: a double-sided friend and a peer sharing
  another active Space both remain in the written set.
- An empty derived set is still written, rather than skipped — skipping would
  leave every stale grant in place for exactly the member who lost all of them.
- A failing `whitelist_set` is propagated (so the work record retries) and does
  **not** cause the per-peer half to be skipped.
- The join side performs the same overwrite, so a rejoiner regains what the
  cutoff's wider revocation removed.
- The derivation resolves `s{spaceID}_{uid}` to the same set as the bare `uid`.
- **The prefixed-channel gap is pinned as a gap.** A bot peer combined with an empty
  conversation list — the one combination no earlier test covered, since the existing
  bot cases feed the conversation in and therefore exercise the enumerated path —
  asserts that the bare channel is overwritten *and* that the `s{spaceID}_` channels
  are not touched at all. It is a characterization test, named `...LeavesBot...` on
  purpose: extending the overwrite to prefixed channels turns it red, which forces
  whoever does that to come back and correct the "closed" wording rather than letting
  the docs and the code drift apart. This PR has already made that mistake once.

**The `dm_forbidden` delivery chain (round 10c)**

- Driving the real `GET /v1/channels/:id/:type` for a no-relation pair returns
  `dm_forbidden` **through the minimal-response strip**, and a co-member pair does
  not. Both wiring links are covered: deleting the `annotateDMSendability` call in
  `modules/user/1module.go`'s `BussDataSource.ChannelGet`, or removing the two keys
  from `callerOwnedExtraKeys`, each turns the test red. Before this test both could
  be deleted with the whole suite staying green — the derivation was well covered
  and the delivery was not covered at all.

**Repo gates**

- `go test ./modules/space/... ./modules/group/... ./modules/user/...` passes,
  including the new regression tests.
- `gofmt`, `go vet` on touched packages, `make i18n-extract-check`,
  `make i18n-lint`, and `golangci-lint run` on touched packages pass — or an
  infrastructure-only blocker is recorded with its exact command and output.
- Any new handler file is added to the module's
  `Test<Module>NoLegacyResponseError` guard list.
- The cascade work-record migration follows the in-repo file convention
  `modules/<name>/sql/<yyyyMMdd><seq>_<name>.sql` (as in
  `modules/space/sql/20260627000001_dm_space_presence.sql`) and is embedded via
  the module's existing `//go:embed sql`. It applies cleanly on a fresh database
  and is a no-op on re-run.
- No production UID, Space ID, group number, or domain appears in the diff,
  tests, fixtures, or commit text.
