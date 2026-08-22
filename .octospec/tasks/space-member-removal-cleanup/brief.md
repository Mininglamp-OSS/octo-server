---
type: Task
title: "Task: space-member-removal-cleanup"
description: Make Space member removal actually take the member out of the Space's groups and sub-threads — a transactional outbox drives a leased, retried cascade, membership caches are invalidated in-request, and a removal event is emitted. The DM half was split out to space-member-dm-isolation.
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

Removing a member from a Space must actually take them out of that Space's
**groups and sub-threads**, not just flip a database flag.

Today every removal path performs exactly one durable action — a soft delete
(`UPDATE space_member SET status=0`) — plus, on two of the five paths, an
in-process notify cache invalidation. The downstream consequences of membership
are left behind: the removed member keeps their `group_member` rows and WuKongIM
group subscriptions, keeps receiving and sending group messages, and keeps passing
`SpaceMiddleware` for up to 60s.

After this change, a removal must:

1. **Durably record the cleanup as work to be done** — a row written in the *same
   transaction* as the membership flip, so a crash between the two cannot lose it,
   driven by a leased worker with bounded retries.
2. **Invalidate membership caches immediately** — both the Redis
   `SpaceMiddleware` cache and the notify member cache, on every path, inside the
   request. That is the authorization boundary and it must land before the response.
3. **Cascade the member out of every group in that Space** — full group-exit
   semantics (IM unsubscribe, membership row, sub-threads, pinned/conversation
   extras), with a system tip and a member-update CMD pushed to each group so
   remaining members' clients refresh. The group creator is handed over first,
   because the underlying primitive silently skips a creator.
4. **Emit a removal event** — one event fired identically by every removal path,
   so downstream modules can react without `modules/space` importing them.

### Explicitly not in this task

**Direct messages.** Cutting and restoring Person-channel whitelists was originally
part of this task and was split out after review — see
`.octospec/tasks/space-member-dm-isolation/brief.md`. Two reasons, both established
by measurement rather than judgement:

- The DM half is **inert in every current deployment**: WuKongIM's
  `options.WhitelistOffOfPerson` defaults to `true`, and while it is true the send
  path never consults the Person-channel whitelist. Verified end to end against the
  CI-pinned broker with a real client.
- It did not converge by point fixes — a new escape was found in six consecutive
  review rounds, with structural root causes (peer scope from a mutable conversation
  view, authorization-read freshness, channel form) that need a membership epoch and
  a bilateral pair index.

Keeping it here would have blocked the group cascade — which is live behaviour today
and has been settled since round 5 — behind a half that changes nothing in production.

The cleanup-step registry (`RegisterMemberRemovalCleanupStep`) is the extension point
the DM step plugs back into; that is why the dispatcher's fail-soft and per-step panic
containment are kept even though only one step is registered today.

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
  mechanism. The Redis invalidation plus the group cascade are.

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
  the group cascade eventually consistent. Alternatively a
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
  access inside a Space they no longer belong to; the fix must not
  overshoot and cut access that a second shared Space or a friendship still
  authorizes. (rule: `space-isolation`)
- **thread** — the group cascade reaches sub-thread membership, sub-thread IM
  subscriptions and per-Space pinned rows via
  `removeUserFromGroupThreadsCleanup` (`modules/group/thread_cleanup.go`); Issue
  #27 and YUJ-52 established that skipping it leaves a bot or member subscribed
  to a sub-channel after losing the parent. (rule: `space-isolation`)
- **concurrency** — removal already runs under `SELECT ... FOR UPDATE` to
  prevent an ownerless Space (PR #339). The new side effects run after commit and
  must not reintroduce a check-then-act window: membership is re-evaluated
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
- **error-response** — removal handlers keep the existing localized envelope
  (`httperr.ResponseErrorL` + `pkg/errcode`); cascade failures are logged, not
  surfaced as new user-facing error codes. No raw `c.ResponseError` / `c.JSON`
  is added. (rule: `error-handling`)
- **rate-limit** — no new HTTP route and no request-frequency policy change. The
  cascade's IM fan-out is bounded by the peer/group set, not by request rate;
  it must not hand-roll a Redis counter. (rule: `rate-limit`)
- **testing** — behavior-level tests against the real removal handlers and the
  real cascade boundary, with synthetic UIDs/Spaces, covering the multi-Space
  and friendship carve-outs, batch partial failure, and idempotent replay. New guard test entries for any new handler file in the
  module's `Test<Module>NoLegacyResponseError` list. (rule: `testing`)
- **commit** — English Conventional Commits; Implement records RED/GREEN
  evidence without production identifiers. (rule: `commit-style`)

## Out of scope

- **Message history and archives.** Messages the removed member sent or received
  stay readable to remaining members; nothing is erased or retracted.
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
- **Closing the rejoin window completely.** The group step now re-checks live
  membership itself (`CheckMembership`, immediately before it computes the group
  set), so the exposure shrank from "claim to acting" to the
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

- **Group-side IM unsubscribe stays best-effort.** A review round asked for
  propagating the error so the work record retries. It was deliberately not applied:
  the authoritative subscriber source for a group channel is the
  `IMDatasource.Subscribers` callback, which re-reads `group_member`. Moving
  `IMRemoveSubscriber` before the delete would be undone by the next reload
  (the documented YUJ-4185 root cause); propagating after the delete buys
  nothing because the retry scope (`queryGroupsWithMemberUIDAndSpaceID`) no
  longer contains that group — the member row is already deleted, so a retry
  cannot re-enumerate it. Propagating would burn all 20 attempts on a no-op.
  Reasoning is recorded at the call site.
- **Per-package CI budget for `modules/user`.** The package sits at roughly 90%
  of its 5-minute budget before this change (248s locally with `-race
  -shuffle=on`, more on a CI runner) across 186 `testutil.NewTestServer` calls at
  ~0.42s each. This task shrinks its own footprint (one process-wide cleanup
  scheduler instead of one per `Route()`) but
  cannot move the package off the ceiling. Splitting `modules/user` across shards
  or raising its timeout is a CI-config follow-up.
- **Remaining P2 review findings.** `LockRemovableMemberTx` conflating "became
  creator" with "already left"; no supporting index on the purge query; the
  locking successor query locking more rows than it returns; `attempts` counted
  but with nothing acting on the ceiling; a transient DB error in the bot check
  skipping the prefixed-channel cleanup. All advisory, none new in this change's
  behavior. Follow-up.

- **The datasource callbacks are dead code in the shipped broker.** `s.datasource`
  is assigned once in `internal/server/server.go` and never read;
  `datasource.GetSubscribers` has zero callers. So the
  `IMDatasource` callbacks registered in `modules/group/1module.go` and
  `modules/user/1module.go` are never consulted, and there is no "next reload
  self-heals" backstop anywhere. An earlier round of this task argued from that
  backstop; the argument was wrong and has been retracted in the code comments.

## Deviations from the original plan

Recorded so the diff can be read against the spec rather than against memory.

- **`modules/botfather`'s delete path is weaker than its command path.** Both zero
  `space_member` outside this task's outbox (already recorded as a follow-up), but
  the asymmetry is worth writing down for whoever picks that up: `command.go` calls
  `IMRemoveSubscriber` and soft-deletes `group_member` before zeroing the membership,
  while `deleteUserBot` (`api_user.go:513`) goes straight from the friend rows to
  `space_member` and `deleteRobot` with neither step — so the API path leaves the bot
  in the Space's groups with live IM subscriptions. Untouched here; raised in review
  against this branch and recorded so the follow-up covers both paths.
- **The removal cascade's per-member lock loop now sorts by uid.** `RemoveGroupMembers`
  takes `FOR UPDATE` on each member row inside one transaction, in the order the caller's
  list produced — the ABBA precondition against `handOverGroupCreator`, which locks the
  leaver and then its successor. Same group, different orders, and MySQL aborts one:
  the cleanup job retries and converges, but the manager batch-kick path surfaces a 500.
  Sorting `removableMembers` (the iteration order, which comes from
  `QueryMembersWithUids`, not from the request slice) makes `RemoveGroupMembers`
  deterministic **with respect to itself**. It does **not** close the class: the
  motivating pair — a batch removal against a concurrent `handOverGroupCreator` — is
  still reachable, because the handover locks leaver-then-scan-order and its successor
  scan (`ORDER BY created_at LIMIT 1 FOR UPDATE`) has no serving index, so it locks in
  storage order rather than uid order, and locks more rows than it returns. Bounded
  (MySQL aborts one side; the job retries to convergence, the manager path returns a
  retryable 500). Closing it needs the same uid ordering in `handOverGroupCreator` plus
  a `(group_no, created_at)` index — both follow-ups. Not reproduced as a deadlock:
  derived from the two lock orders.
- **A fifth removal path.** The owner-initiated disband was not in the original
  survey (see the path table). It now runs the same cascade.
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
