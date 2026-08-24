---
type: Task
title: "Task: space-removal-creator-handover-notice"
description: When a Space removal takes out a group's creator, tell the group who took over and why. An ordinary member's removal stays silent on purpose — the roster already shows it, and broadcasting it would put up to 10k permanent messages into groups per batch.
tags: [space, isolation, wire-contract, external-content, escape, concurrency, testing, commit]
timestamp: 2026-08-23T11:00:00+00:00
# --- octospec extension fields ---
slug: space-removal-creator-handover-notice
upstream: follow-up to Mininglamp-OSS/octo-server#795
source: user
---

# Task: space-removal-creator-handover-notice

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. Examples and tests use synthetic UIDs and Space IDs only.

## Goal

When removing a member from a Space cascades them out of a group, the members
still in that group must be able to tell what happened to the group **when the
change is not otherwise visible to them**.

Exactly one case qualifies: the removed member owned the group. The cascade hands
the group to the second-oldest non-bot member, and before this task that happened
in complete silence — the group acquired a new owner with nothing explaining it.
A departure, by contrast, *is* visible: the roster shows one fewer person.

So this task ships one group-visible message, on one condition:

> `“{0}”已离开当前空间，“{1}”已成为新群主`

sent to the whole group when, and only when, a Space removal caused a creator
handover. Both clauses are needed: the group is seeing the consequence
(ownership moved), and without the cause it reads as an unexplained change.

### Explicitly not in this task

An earlier revision of this branch also broadcast a per-member departure notice
(`X 已被移出当前空间…`) into every group. That is **deliberately not shipped** —
see Out of scope for the reasoning, and do not re-add it without revisiting the
volume argument.

## Background

### What #795 left behind

`#795` (`space-member-removal-cleanup`) made Space removal cascade into the
Space's groups. Reported symptom afterwards, and the origin of this task:

> 相比没有 795 之前会清理群聊了, 但是没有系统消息

Measured against the CI-pinned stack by recording the `/message/send` calls of a
kick cascade in a three-member group. For the two members who remained:

| # | type | content | subscribers | visibles |
|---|---|---|---|---|
| 0 | 1020 | `你被{0}移除群聊` | `[victim]` | `[victim]` |
| 1 | 99 | *(none)* | – | – |

Message 0 is octo-lib's `SendGroupMemberBeRemove`, which pins **both** recipient
layers to the removed member and is written in the second person — by design a
private notice to the person being removed, not a group broadcast. Message 1 is
`CMDGroupMemberUpdate`, a silent roster refresh with no visible content. Net
group-visible messages for the remaining members: **zero**.

octo-lib does have a third-person, group-visible `SendGroupMemberRemove`, but it
fires only from the `GroupMemberRemove` event and nothing in this repository ever
emits that event.

### Relationship to #795's brief

`space-member-removal-cleanup` brief, Acceptance › Group cascade, states:

> Each affected group receives a member-update CMD and a system tip so remaining
> members' clients converge.

**This task supersedes the "and a system tip" half of that line.** The tip that
line refers to is `SendGroupMemberBeRemove`, which is not group-visible, so the
line as written never described the behaviour it appears to promise. After this
task an affected group receives the member-update CMD always, and a group-visible
system message only on a creator handover. That brief is left unedited — it
records what its own PR shipped; this brief is the current statement.

### Existing primitives this task reuses

- **`handOverGroupCreator`** (`modules/group/space_member_removal.go`) — already
  promotes the second-oldest non-bot member in its own transaction, re-reading
  the leaver's role under a row lock. Untouched except for where the notice is
  emitted from.
- **Content type `GroupTransferGrouper` (1008)** — what the manual transfer path
  (`api.go transferGrouper` → `base/event/handler.go` → `SendGroupTransferGrouper`)
  already emits, so the cascade renders through the same client path.
- **`{N}` + `extra` payload shape** — established for multi-name system messages;
  `SendGroupMemberScanJoin` (type 1007) uses `“{0}”通过“{1}”的二维码加入群聊` with
  a two-element `extra`. All three first-party clients substitute generically over
  `extra.length` and none reads 1008 structurally — audited per-client in PR #804
  review, which also flagged that Android's `MessageFormat` treats ASCII `'` as an
  escape, a hazard for any future English translation.

## Behavior contract

### Trigger

Emitted from inside `handOverGroupCreator`, immediately after its transaction
commits, for every reason **except** `space_disbanded`.

Emitting it there rather than from the caller is load-bearing, not stylistic.
The handover commits its own transaction; `RemoveGroupMembers` runs afterwards
and can fail (DB error, invited-bot cascade, the `Removed == 0` concurrency
guard). Announcing after that call loses the message permanently: the job
retries, the leaver is already `MemberRoleCommon`, the handover branch is not
re-entered, and nothing ever announces the owner that did change — reinstating
the exact defect this task exists to remove. At the commit point it is also
idempotent: a retry re-reads the role under the row lock, sees it is no longer
the creator, and returns without re-announcing.

### Payload

| field | value |
|---|---|
| `type` | `common.GroupTransferGrouper` (1008) |
| `content` | `“{0}”已离开当前空间，“{1}”已成为新群主` |
| `extra[0]` | leaver `{uid, name}` |
| `extra[1]` | successor `{uid, name}` |
| `Subscribers` | unset — group-visible |
| `payload.visibles` | unset — group-visible |
| `Header.RedDot` | `1`, matching every group system message in octo-lib |
| `Header.NoPersist` | `0` — stays in group history |

Both names travel in `extra` behind placeholders and are **never concatenated
into `content`**. That is what removes the injection surface: a display name is
user-controlled (`group_member.remark` is settable by the member and by any group
manager, with no content validation), and a name baked into a persisted,
group-visible string could forge extra lines of "system notice".

It does **not** buy "a later rename leaves no stale name": all three first-party
clients render `extra[i].name` directly and none re-resolves by uid, so a
`NoPersist: 0` message keeps the name it was sent with. An earlier revision of
this brief claimed otherwise (PR #804 review). The uid in `extra` makes
re-resolution possible; nothing does it today.

The payload is built in `modules/group` rather than by calling
`SendGroupTransferGrouper`, because that primitive's text is fixed at the
one-clause form and the cascade needs the cause stated. The content type is kept
identical so one event does not render two ways.

### Chain suppression

Before announcing, the elected successor is checked against
`space_member_removal_cleanup` for a `status=pending` row in the same Space
(`spacemod.HasPendingRemovalCleanup`). If one exists, the handover still happens
but the notice is skipped: this link is mid-chain and is about to be superseded.

Only the final link — whose successor is not queued for removal — announces, and
what it announces is the settled owner.

**This collapses to one notice only where a batch's jobs are all visible before
any worker starts, and that is not every entrypoint.** An earlier revision of this
brief claimed it held generally, citing `enqueueMemberRemovalCleanupBatchTx`. That
claim was false and is corrected here (PR #804 round-4 review, found independently
by three reviewers and confirmed against the code):

| Entrypoint | Enqueue | Reason | Premise holds? |
|---|---|---|---|
| Disband (`space/db.go`, `db_manager.go`) | `enqueueMemberRemovalCleanupBatchTx`, one tx | `space_disbanded` | Yes — but this path suppresses the notice anyway, so the cited function never protects an announcing path |
| Superadmin force-remove (`removeMembersForce`) | one tx for the whole batch | `force_removed` | Yes |
| User-side `members/remove` (`removeMembersLocked`) | one tx for the whole batch — **fixed in this PR** | `kicked` | Yes (was **No**) |

Before the fix, on the `kicked` path the cleanup worker's 10 s tick could claim the
already-committed prefix mid-loop while the later uids had no rows yet. The check
then read a successor who was about to be removed as "not pending" and announced an
intermediate handover. Measured: all rows seeded up front → 1 notice; per-iteration
enqueue fully interleaved → 3; per-iteration plus a single tick after the first
commit → 2.

**And it composed with the never-re-evaluated check below into something worse.** An
earlier revision of this brief said the surplus was "wrong content emitted, not
correct content lost, because the final notice stays correct". That was false, and
measured false: give the *final* successor a stale `pending` job — the ordinary
state of anything in retry backoff — and the last link is suppressed instead. The
last ownership message in group history then names someone who has left, while the
actual owner is announced by nothing, ever. Gap A supplies a wrong message; gap B
removes the correction. The no-alarm route is `skipped_rejoined`: the successor is
removed, re-invited before their cleanup runs, and their job closes as `done`.

**Fixed** by making the user-side batch enqueue atomically —
`removeMembersLocked` (`space/db_manager.go`) locks the whole batch, re-reads each
role under that lock, flips the rows, and enqueues every cleanup job in **one**
transaction, mirroring `removeMembersForce`. Both load-bearing properties of the
old per-uid transaction are kept: the flip and its cleanup job still commit
together (transactional outbox), and the role-hierarchy re-read still happens under
the row lock. The one semantic change is partial failure: a DB error mid-batch now
rolls the whole batch back instead of leaving a committed prefix, which is why the
caller no longer needs compensating cleanup.

**Round-7 correction (deadlock).** The round-5 shape acquired those locks with a
*per-uid* `FOR UPDATE` loop in caller order, which reintroduced a deadlock the old
one-lock-at-a-time loop could not: two reversed overlapping batches, or a batch
overlapping `transferOwnerAdminLocked`. Measured on MySQL 8.0, the naive
`uid IN (…) FOR UPDATE` does **not** use the unique index at this endpoint's real
sizes — the optimizer picks `spacemember_spaceid_status`, locking every active
member of the space in `(space_id, status, id)` order, while the transfer demote
uses `(space_id, uid)`; two orders over one row set is the cycle. The lock statement
now carries `FORCE INDEX (spacemember_spaceid_uid)` (extracted to the const
`selectMembersForRemovalForUpdateSQL`), which pins the plan to a `(space_id, uid)`
range: only the target rows, and narrows the lock scope from the whole space back to
the targets. The batch-vs-batch case was always safe (same plan on both sides).

**Round-8 correction.** `FORCE INDEX` alone does **not** close the batch-vs-transfer
case, and an earlier revision here wrongly said it did. `transferOwnerAdminLocked`
acquires non-monotonically — it single-row-locks the transfer target first, then
range-scans the demote — so when the target is inside the batch it is a
deterministic AB-BA regardless of the batch's index (two reviewers measured
40/40 and 42/120 with the real functions; the per-uid `main` shape is 0). The batch
can't unify the *other* path's order from its own call site, so ordering is not a
complete fix. The multi-row `space_member` writers are wrapped in `retryOnDeadlock`,
a bounded retry. A deadlock rolls the victim back with nothing half-committed, so a
retry is safe and usually succeeds on the second try. `FORCE INDEX` is kept: it
shrinks the lock footprint and the blocking window and removes batch-vs-batch, it
just is not the cross-path fix. Guarded by `TestRetryOnDeadlockRecoversFrom1213`
(injects a 1213: retries to success, gives up bounded, passes 1205 and domain errors
through) — the real deadlock race is not reliably reproducible locally, so the retry
logic is pinned by injection rather than a flaky concurrent test.

**Round-9 corrections.** Three, all from the round-9 review; the first two are
defects this PR introduced in round 8.

1. *The wrapped set was stated as complete and was not.* An earlier revision here
   said "all three multi-row `space_member` writers". There are four: `upsertMembers`
   (batch add) locks per-uid in **caller order** while the batch removal locks in
   index order, so `add[C,B,A]` against `remove[A,B,C]` on one space is an AB-BA —
   and round 8's own change widened it, since removal went from holding one row lock
   at a time to holding up to 200 for a whole transaction. `upsertMembers` is now
   wrapped too. Stated precisely, because this document is what the next editor
   trusts: what is **not** wrapped is `db.go`'s `atomicAddMemberIfNotFull`,
   `atomicReactivateMemberIfNotFull` and the invite-approve transaction, which take
   `SELECT COUNT(*) … WHERE space_id=? AND status=1 FOR UPDATE` for capacity and so
   lock **every active row in the space** — a wider lock than anything above. That is
   pre-existing, out of scope here, and wants one capacity-check design rather than a
   fourth retry wrapper. Tracked as a follow-up.

   **Wrapping `upsertMembers` was not enough — measured.** The reviewer who found this
   also measured it, and the measurement overturned the fix: with `upsertMembers` merely
   wrapped in the retry, the *removal* side surfaced **60 of 60** unrecovered 1213s to
   its caller (200-uid reversed overlap). The mechanism is **victim starvation**, not an
   unlucky collision — the batch is fresh with zero undo work at every attempt, so InnoDB
   keeps electing it the victim while the still-running upsert makes progress, and the
   retry re-collides with the same live transaction until the attempts are gone. So:
   *bounded retry closes cycles against **short-lived** counterparties (owner transfer,
   force-remove — measured 0) and starves against **long-lived** ones.* The fix is
   ordering, which the retry cannot substitute for: `upsertMembers` now sorts its uids
   ascending, the same order the `FORCE INDEX`'d batch locks in, so the pair cannot form
   a cycle at all. Measured 0/60 across three runs — and 0/60 again with **both sides
   unwrapped**, which is what proves the closure is structural rather than absorbed by
   the retry. Guarded by `TestUpsertMembersLocksInSameOrderAsBatchRemoval`, which calls
   both `…Once` variants for that reason and goes red 30/30 when the sort is removed.
2. *Round 8 silently defanged the round-7 deadlock guard.*
   `TestRemoveMembersLockedNoDeadlockOnReversedOverlap` exists to catch a
   batch-vs-batch AB-BA, but round 8 routed it through the retry, which swallows the
   very 1213s it asserts on. Measured, with a per-uid caller-order regression
   mutated in: calling `removeMembersLockedOnce` (the fix) reports **47/48/48
   deadlocks per 80 calls — red every run**; the round-8 shape (wrapper, 5 attempts)
   **passed 2 of 3 runs** with that same regression underneath. A detector had become
   a coin flip. This is exactly the failure mode of
   `learnings/pending/mutation-check-the-assertion-not-the-guard.md`, added by this
   same PR — a change elsewhere quietly made an existing guard unable to go red.
3. *`retryOnDeadlock` retried 1205 with no backoff.* A lock-wait timeout only returns
   after `innodb_lock_wait_timeout` (default 50s, not overridden in this repo), so
   5 serial attempts could hold an HTTP handler ~250s while retrying the condition
   least likely to have cleared — and the transfer path used to fail once at 50s.
   Now 1213 only (1205 passes through, i.e. the pre-PR behaviour), 3 attempts with
   5ms/20ms backoff, matching `conversation_ext`'s existing helper. This is a
   deliberate divergence from the repo's other four copies of the predicate, which
   do retry 1205; the reason is written at `isDeadlockErr` — those guard small, fast
   operations, these guard a 200-row batch on a user request.

With A closed, B alone degrades to silence — the pre-PR behaviour, never a false
name. B itself is unchanged and still tracked (see below).

Guarded by a **pair** of tests, and neither is sufficient alone:

- `space/TestRemoveMembersLockedForcesUniqueIndexPlan` is the round-7 primary guard:
  it `EXPLAIN`s the shared SQL const and asserts the plan is `(space_id, uid)`, not
  `(space_id, status)`. Removing `FORCE INDEX` turns it red (verified). A concurrent
  smoke test (`…NoDeadlockOnReversedOverlap`) runs reversed overlapping batches with
  zero deadlocks, but the plan assertion is what actually pins the lock order.
- `space/TestRemoveMembersLockedEnqueuesAtomically` proves the atomic-enqueue
  premise: a competing row lock stalls the batch inside its single locking `SELECT`,
  and the test asserts that *no* cleanup row is visible at that moment. Mutating the
  implementation back to per-uid commits turns it red (verified).
- `group/TestGroupCascadeLastNoticeNamesActualOwner` proves the consequence given
  the premise, with two hard-asserted scenarios: a stale-`pending` successor yields
  **zero** notices, and the ordinary case yields **exactly one** naming the settled
  owner. Removing the chain suppression turns both red (verified).

`TestGroupCascadeBatchHandoverAnnouncesOnce` still seeds the whole batch up front,
and that is now a *linked* premise rather than a hidden one: its doc comment names
the space-side test that establishes it. On its own it would pin "an
atomically-enqueued batch collapses to one notice", not "every entrypoint
collapses" — the trap recorded in
`learnings/pending/a-test-can-encode-the-premise.md`.

**A separate and independent window: across replicas.** The check runs after
`handOverGroupCreator` commits, so the successor's row lock is already released.
If worker A stalls between that commit and the check for long enough (~100 ms)
for worker B to finish the successor's entire job and mark it `done`, A reads
`done`, does not suppress, and emits an obsolete notice. The fix is to move the
*check* before `tx.Commit()` while leaving the *send* after it — inside the
transaction A still holds the successor's row lock, so B cannot get past the first
`FOR UPDATE` of its own handover, and the row is unambiguously `pending`. That
row-lock premise was verified by experiment.

**Also fixed in this PR.** `HasPendingRemovalCleanup` now takes a
`dbr.SessionRunner`, which `*dbr.Tx` satisfies, so the check runs inside the
handover transaction while the send stays after the commit. No test hook was needed
— an earlier revision claimed one would be, and that was wrong. This matters beyond
tidiness: the stale notice it admits can compose with gap B exactly as gap A did.

The query is deliberately read-only with respect to the transaction's fate: if the
`COUNT` errors, the code treats the successor as not pending and announces, rather
than rolling back a handover that is already correct.

**This one has no deterministic red test**, unlike gap A. Moving the check back
after the commit leaves every existing case green, because reproducing it naturally
requires worker A to stall between a `COMMIT` and an indexed `COUNT` for long enough
that worker B finishes an entire job — 30 rounds of two goroutines racing sibling
jobs produced zero surplus notices. Its correctness rests on the row-lock argument
(A still holds the successor's row lock inside the transaction, so B cannot get past
the first `FOR UPDATE` of its own handover), which was verified experimentally. The
code comment says so, so the next editor does not expect a test to catch a
regression here.

`done` and `abandoned` do not suppress — but only when they already hold at check
time. The common ordering is the reverse: the check runs first, and the
successor's job reaches `abandoned` later (20 attempts, ~70 minutes) or is marked
`skipped_rejoined` after a rejoin. In those orderings the check saw `pending`,
suppressed, and nothing re-announces — the group keeps an owner it was never told
about. The general answer is re-evaluating once a batch has fully settled rather
than deciding per link, which is the same durable-replay problem already deferred
to #797.

### Display names

**This also changes `sendGroupExitTip`**, which previously resolved the global
`user.name` only. Keeping the change (consistency with `groupExit` and the roster
is this repository's rule) rather than scoping it to the new path, and pinning it
with a test — it arrived as a side effect of a parameter refactor and was
unguarded until PR #804 review pointed it out.

Group `remark` first, falling back to global `user.name`, falling back to uid —
the repository's existing rule (`groupExit` resolves `loginMember.Remark` first;
the roster renders `Remark`). The successor's name comes from the row the
handover transaction already holds under `FOR UPDATE`: no extra query, and no
window in which a re-read could observe a different row.

### Suppression

| reason | handover performed | notice sent |
|---|---|---|
| `kicked` | yes | yes |
| `force_removed` | yes | yes |
| `left` | yes | yes |
| `space_disbanded` | yes | **no** |

Disband is suppressed because it removes *every* member, so the handover chains
down the seniority list (C→S2, S2→S3, …). An M-member group would emit M-1
notices, the first M-2 already false when written, into a Space that no longer
exists.

## Load-bearing list

- **`space` / `isolation`** — the notice is emitted on the Space-removal cascade
  path and is group-visible, so it must not carry Space-scoped facts to members
  of other Spaces. The shipped wording names no Space; `“{0}”已离开当前空间` is
  read relative to the group's own Space.
- **`wire-contract`** — adds a message on content type 1008 whose `content` and
  `extra` arity differ from `SendGroupTransferGrouper`'s. Clients must tolerate a
  second placeholder and a two-element `extra` on that type.
- **`external-content` / `escape`** — a user-controlled display name reaches a
  persisted, group-visible message. Structural (`extra` + placeholder), not
  string interpolation.
- **`concurrency`** — the notice is emitted from inside a function that runs
  under a leased worker with retries and possible concurrent re-claim; it must be
  neither lost on retry nor duplicated.
- **`testing`** — the guards here are assertions about *visibility*, which the
  pre-existing tests did not distinguish from "was sent".
- **`commit`** — English Conventional Commits.

## Out of scope

- **A per-member departure broadcast.** Both removal endpoints accept
  `managerMaxBatchUIDs` = 200 uids and enqueue one cleanup job per uid, each
  walking every group the member is in: 200 members across 50 groups is 10,000
  permanent group-visible messages — the same order as the disband case #795
  already suppresses for that reason. The removed member still gets the private
  notice; the roster refreshes via CMD.

  **Correction (PR #804 review).** An earlier revision of this brief claimed the
  handover notice is "at most one per group regardless of batch size" because it
  fires only on a handover. That was false, and measured false: a 3-uid batch
  whose members are consecutive in one group's seniority list produced **three**
  notices in that group (C→S2, S2→S3, S3→S4), the first two already obsolete when
  written. It is the same chaining the disband path is suppressed for; only the
  trigger differs. Suppression collapses the chain on entrypoints whose jobs are
  enqueued atomically, which — as of the round-5 fix — is **all** of them: the
  user-side `members/remove` (`kicked`) path now enqueues in one transaction
  (`removeMembersLocked`). See Behavior contract › Chain suppression for the
  per-entrypoint table and the measured counts.
- **`upsertMembers` — RESOLVED in round 9, no longer a follow-up.** It looped
  `INSERT … ON DUPLICATE KEY UPDATE` in caller order inside one transaction, against
  the batch removal's index order. It now sorts its uids ascending (same order the
  `FORCE INDEX` batch locks in) and is wrapped in `retryOnDeadlock`. See the round-9
  section above for the measurement that forced the sort; guarded by
  `TestUpsertMembersLocksInSameOrderAsBatchRemoval`.
- **The ordinary group-kick and bot-API paths.** `modules/group/api.go`
  `memberRemove` and `modules/bot_api/groups.go` reach the same
  `SendGroupMemberBeRemove`, so remaining members see nothing there either. Same
  defect, different entry points, **not tracked in #797** — wants its own task.
- **Coalescing several removals into one message per group.** Would require the
  outbox to carry batch identity. Handover-only firing removes most of the need,
  but not all of it: batch identity is also one of the candidate fixes for the
  `kicked`-path chain above, so this may come back with that work.
- **Everything already deferred to #797** — the IM-unsubscribe leak, the residual
  ABBA and its missing index, the rejoin window.
- **A bot as successor.** `querySecondOldestNonBotMemberTx` excludes only the
  leaver's own active bots, so a third party's bot — or the leaver's own
  *inactive* bot — can be promoted and therefore announced. Pre-existing
  selection behaviour, deliberately pinned by
  `TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft` ("他人的 bot 不在排除
  范围内（与旧 QuerySecondOldestMember 语义一致）"); this task makes a previously
  silent outcome visible but does not change the selection. `group_member.robot`
  is populated (`service.go:1298`, `:1328`) so `gm.robot = 0` would be a one-line
  fix, but reversing a deliberately pinned contract needs its own decision.

- **An external member as successor.** `querySecondOldestNonBotMemberTx` has no
  `is_external = 0` predicate, while `modules/group/db.go` `QueryIsGroupManagerOrCreator`
  requires `is_external=0` — so an external guest can be elected creator and then
  refused the creator powers. Before this task that was a silent bad state; this
  task turns it into a persisted, group-visible claim that someone holding no
  creator powers is the new owner. Reachable when a Space group's only eligible
  successor is external. Pre-existing selection behaviour like the bot case above;
  adding the predicate changes election semantics (the group would go ownerless
  instead), so it wants its own decision. Raised in PR #804 round-5 review.
- **A suppressed notice is never re-evaluated.** The chain check decides once,
  against `status=pending`, and that answer can turn out wrong in three ways: a
  successor carrying an older stuck pending job; a successor whose own job later
  reaches `abandoned`; and a successor who rejoins the Space (their job is marked
  `skipped_rejoined`). All three end with an owner the group was not told about.
  The opposite bias — announcing an owner who is about to vanish — is worse, so
  the check is deliberately one-directional; a DB error on the check is likewise
  treated as "not pending" and the notice is sent. Re-evaluating after a batch
  settles is the general fix and belongs with #797.

- **The multi-replica window on the check itself.** See Behavior contract › Chain
  suppression; measured unreachable in 30 rounds of natural concurrency, and a
  deterministic guard would need a production test hook.

## Acceptance

Acceptance spans two packages, all passing under `-race -shuffle=on`. The premise
and the consequence are guarded separately, and by the branch's own rule neither
half is sufficient alone.

In `modules/space/member_removal_test.go` — the atomic-enqueue premise (round-5):

- `TestRemoveMembersLockedEnqueuesAtomically` — a competing row lock stalls the
  batch on its second uid; the test asserts **no** cleanup row is visible at that
  instant. Mutating back to per-uid commits turns it red.
- `TestRemoveMembersLockedNoDeadlockOnReversedOverlap` — two goroutines run
  reversed overlapping batches; **zero** deadlocks. Mutating back to per-uid
  `FOR UPDATE` turns it red (measured 49 deadlocks/80 calls).
- `TestRemoveMembersLockedSkipsOwnerAndPeers` /
  `TestRemoveMembersLockedRollsBackWholeBatchOnError` — role-hierarchy skips and
  all-or-nothing rollback.

In `modules/group/space_member_removal_test.go`:

- `TestGroupCascadeCreatorHandoverIsAnnounced` — a force-removed creator produces
  a group-visible message (no `subscribers`, no `visibles`) containing both
  clauses, with `extra[0]` the leaver and `extra[1]` the successor, exactly once.
- `TestGroupCascadeHandoverAnnouncedOncePerRetry` — with `RemoveGroupMembers`
  fault-injected to fail once *after* the handover commits, a retry yields exactly
  one notice: not zero (lost) and not two (duplicated). **The first version of
  this test was vacuous** (PR #804 review): it merely ran the step twice, and the
  second run returned early at `queryGroupsWithMemberUIDAndSpaceID` because the
  member row was already deleted, so the handover path was never re-entered and
  `count == 1` held under either placement. The fault injection is what makes the
  test discriminate.
- `TestGroupCascadeHandoverPrefersGroupRemark` /
  `TestGroupCascadeHandoverLeaverPrefersGroupRemark` — both sides of the notice
  use the group `remark` over the global `user.name`. They are separate guards
  because `extra[0]` and `extra[1]` resolve through different paths, and pinning
  one leaves the other free (measured: PR #804 review round 2, mutation 11).
- `TestGroupCascadeLoneCreatorAnnouncesNoHandover` — no successor, no notice.
- `TestGroupCascadeDisbandSuppressesHandoverAnnounce` — disband still performs the
  handover but sends no notice.
- `TestGroupCascadeKickSendsNoGroupBroadcast` /
  `TestGroupCascadeForceRemovedSendsNoGroupBroadcast` — an ordinary member's
  removal produces **zero** group-visible messages carrying visible content
  (excluding the type=99 CMD), while the removed member's own private notice is
  still sent.
- `TestGroupCascadeBatchHandoverAnnouncesOnce` — a 3-uid batch sharing one group
  emits exactly one notice, naming the **final** owner rather than a mid-chain one.
- `TestGroupCascadeLastNoticeNamesActualOwner` — the composed-defect guard
  (round-5): with the final successor holding a stale `pending` job, **zero**
  notices; in the ordinary case, exactly one naming the settled owner. Neither the
  first version's vacuous "if emitted…" shape nor a count alone would catch the
  composition; removing the chain suppression turns both subtests red.
- `TestGroupCascadeSelfExitTipPrefersGroupRemark` — pins the `sendGroupExitTip`
  display-name change this task makes (see below).
- `TestGroupCascadeHandoverNoticeIsBestEffort` — a failing send does not fail the
  job; the handover stays committed and the removal completes.
- Pre-existing suppression guards still hold:
  `TestGroupCascadeSelfExitSuppressesRemovedNotice`,
  `TestGroupCascadeDisbandSuppressesPerMemberNotice`,
  `TestGroupCascadeKickStillSendsBotTip`.

Each guard is mutation-checked individually: reverting it turns its own test red
and only that one.

Gates: `gofmt`, `go vet`, `make i18n-extract-check`, `make i18n-lint`, and
`pkg/space` + `modules/space` + `modules/group` + `modules/bot_api` under
`-race -shuffle=on` with the per-package database reset CI uses.
