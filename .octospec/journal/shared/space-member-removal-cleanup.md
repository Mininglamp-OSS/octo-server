---
type: Journal
title: "Journal: space-member-removal-cleanup"
description: Removing a member from a Space now takes them out of its groups and sub-threads — a transactional outbox drives a leased, retried cascade that exits every group in the Space, while the membership caches are invalidated inside the request. Five removal paths, not the four the plan assumed. The DM half was split out to space-member-dm-isolation after review found it inert in every current deployment.
tags: ["space", "isolation", "auth", "acl", "thread", "concurrency", "data-integrity", "wire-contract", "error-response", "testing"]
timestamp: 2026-08-21T05:30:00Z
# --- octospec extension fields ---
task: space-member-removal-cleanup
upstream: none
source: self
---

# Journal: space-member-removal-cleanup

## What was done

Removing a member from a Space used to perform exactly one durable action — a
soft delete of the `space_member` row — plus, on two of the paths, an in-process
notify cache invalidation. Everything downstream of membership was left behind:
the removed member kept their `group_member` rows and WuKongIM group
subscriptions, kept passing `SpaceMiddleware` for up to 60s, and kept a usable
DM channel with every former co-member.

Removal now:

- writes a `space_member_removal_cleanup` row in the **same transaction** that
  zeroes `space_member` (transactional outbox), so a crash between the
  membership commit and the cascade cannot lose it;
- invalidates the Redis `SpaceMiddleware` key synchronously per member and the
  notify cache once per batch, inside the request — that is the isolation
  boundary and must land before the response;
- cascades the member out of every group in that Space, reusing the existing
  group primitives (`RemoveGroupMembers`: IM unsubscribe, invited-bot cascade,
  sub-threads, per-Space pinned/conversation extras, member-update CMD, tip),
  handing the group creator over first because that primitive silently skips a
  creator;

Wiring uses init-time reverse registration (`RegisterMemberRemovalCleanupStep`)
because `modules/group` already imports `modules/space`. That registry is also the
extension point the DM step plugs back into — see
`.octospec/tasks/space-member-dm-isolation/brief.md`.

**Scope note.** Person-channel (DM) cutoff and restore were implemented and reviewed
as part of this task, then split out. Measured reasons: WuKongIM's
`options.WhitelistOffOfPerson` defaults to `true`, so that half changes no delivery
behaviour in any current deployment; and it produced a new escape in six consecutive
review rounds from structural causes (peer scope from a mutable conversation view,
authorization-read freshness, channel form) that need a membership epoch and a
bilateral pair index. The findings are preserved in the follow-up brief.

## Structural learnings

**The event bus is not a retry queue.** `modules/base/event` marks a failed
listener's event `Fail` and `QueryAllWait` selects only `Wait`, so a failed
event is never re-dispatched; multiple listeners also share one status row whose
`version_lock` is never incremented, making it last-writer-wins. Anything that
needs at-least-once delivery must carry its own durable record — the
`user_session_revocation_intent` pattern (status / attempts / next_attempt_at /
lease) is the in-repo reference. `SpaceMemberRemove` is emitted as an observer
event only, and is not even persisted when no listener is registered.

**A cleanup predicate must not be written in the tense of the thing it is
cleaning up after.** An implementation scoped its targets with an *is-currently*
predicate (`space_member.status=1 AND space.status=1`). Cleanup always runs *after*
the member row is zeroed, and disband zeroes the Space row too, so the set came back
empty and the job silently did nothing — after a disband, and whenever two peers were
removed in the same batch. The fix is a deliberately status-agnostic scope. Observed
in the DM half (now split out), but the lesson is the scope rule, not the DM code.

**A half that changes nothing in production should not gate a half that does.**
This was discovered late, and only by measurement: probing the broker showed the DM
cutoff was inert under its default configuration. By then it had absorbed six review
rounds. The signal to act on is "this code path is verifiably not reached in any
deployment" — that is a scoping decision, not a footnote for the PR body.

## Gotchas worth remembering

- **`NewMinimalChannelResp` rebuilds `Extra` from an allowlist.** A new
  Person-channel extra is invisible to clients unless it is added to
  `callerOwnedExtraKeys` — and the minimal path triggers on *no friendship and no
  common group*, which is exactly when a Space-removal signal matters.
- **The lease must outlast the job, not the request.** A 60s lease against a
  cascade that genuinely runs for minutes means the row is legitimately
  re-claimed and the steps execute twice. A per-claim owner token only makes the
  loser's write land on zero rows; it does not prevent the second execution.
  Long lease + idempotent steps + in-transaction re-validation of anything
  order-sensitive (here: the creator handover).
- **`lease_owner` is `VARCHAR(64)`.** A claim token of two concatenated UUIDs is
  78 chars and MySQL rejects the whole claim with "Data too long", so the worker
  silently processes nothing. Assert the token length in a test.
- **`SET GLOBAL max_connections` does not survive a mysqld restart.** At the
  default 151 a package that constructs many test servers panics inside
  `testutil.NewTestServer`, which kills the test process — every test declared
  after it never runs, and the package looks like it has one flaky failure
  instead of a truncated suite. CI sets 1000 for this reason.
- **Space IDs are `util.GenerUUID()` — 32 hex chars.** `ParseChannelID` only
  strips the `s{spaceID}_` prefix for a registered ID or a 32-hex match, so a
  fixture using a readable fake ID exercises the wrong path.

## Deliberately not done

Message history, `dm_space_presence` rows, session/token revocation, category
data, eager `space_join_apply` convergence, and email-invite invalidation are all
out of scope and unchanged. Promoting another user's bot to group creator stays
allowed — `TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft` pins that as
deliberate, and changing it would alter `groupExit`.

One wire-visible change: `POST /v1/space/:space_id/members/remove` now enforces
the same 200-uid batch cap the manager endpoint always had.
