---
type: Journal
title: "Journal: space-removal-creator-handover-notice"
description: A Space removal that hands a group to a new owner now says so, in one group-visible message naming both the departure and the successor. The per-member departure broadcast an earlier revision shipped was removed — it was the source of eight review findings, all of which dissolved with it. Also fixes a real defect in the first attempt, where the announcement was permanently lost whenever the cleanup job retried.
tags: ["space", "isolation", "wire-contract", "external-content", "escape", "concurrency", "testing"]
timestamp: 2026-08-23T11:00:00Z
# --- octospec extension fields ---
task: space-removal-creator-handover-notice
upstream: follow-up to Mininglamp-OSS/octo-server#795
source: self
---

# Journal: space-removal-creator-handover-notice

## What was done

`#795` made Space removal cascade into the Space's groups. The reported symptom
afterwards was that the cleanup happened but nothing was said:

> 相比没有 795 之前会清理群聊了, 但是没有系统消息

The diagnosis was not "no message was sent". A message *was* sent —
`SendGroupMemberBeRemove` — but octo-lib pins **both** of its recipient layers
(top-level `subscribers` and `payload.visibles`) to the removed member, and its
text is second-person. It is by design a private notice to the person being
removed. Measured by recording `/message/send` for a kick cascade in a
three-member group, the two remaining members received exactly one thing: a
`type=99` CMD with no visible content.

Shipped: one group-visible message, emitted only when the removal caused a
creator handover.

> `“{0}”已离开当前空间，“{1}”已成为新群主`

on content type `GroupTransferGrouper` (1008) — the same type the manual transfer
path emits, so one event does not render two ways — with both names in `extra`
behind placeholders rather than concatenated into the string.

An ordinary member's removal stays silent, deliberately. That is the second half
of the design and the more consequential decision; see below.

## What changed between the first attempt and the shipped version

The first revision of this branch broadcast a departure notice for **every**
removed member. Adversarial review produced fourteen findings against it. Eight
of them turned out to be consequences of that one decision rather than
independent defects, and all eight disappeared when it was dropped:

| finding | fate |
|---|---|
| N×M volume (200-uid batch × M groups = 10k permanent messages) | gone as a *per-member* flood; the residual handover chain surfaced in round 2 below |
| `sanitizeTipName` missed U+2028/U+2029/U+202E/U+200B | gone — no interpolation left to sanitize |
| a load-bearing comment claiming generic `Tip` has no `{0}`+`extra` precedent | gone — refuted, and the hand-rolled payload it justified is gone |
| `RedDot: 0` + `NoUpdateConversation` left the "broadcast" with no UI signal | gone — 1008 carries `RedDot: 1` like every sibling |
| name resolved remark-first while the adjacent bot-cascade tip used the global name | gone — no adjacent pair any more |
| "当前空间" disclosed to external members from another Space | gone — wording is group-relative |
| a false claim pinned in history when the member rejoined | gone |
| content type split from the manual transfer path (2000 vs 1008) | gone — same type |

The lesson is in `learnings/pending/eight-findings-one-decision.md`: when review
findings cluster, check whether they share a parent decision before fixing them
one by one.

## The defect the first attempt introduced

Worth recording because it was subtle and six independent review angles found it.

`handOverGroupCreator` commits its own transaction. The first version announced
the handover from the **caller**, after `RemoveGroupMembers`. That call can fail
several ways (DB error, invited-bot cascade, the `Removed == 0` concurrency
guard), and on failure the cleanup job retries. On the retry the leaver is
already `MemberRoleCommon`, so the handover branch is not re-entered, the
successor variable stays empty, and the announcement is **never sent** — exactly
the silent owner change the change existed to remove.

Moving the announcement to the commit point inside `handOverGroupCreator` fixes
it and is idempotent for free: a retry re-reads the role under the row lock, sees
it is no longer the creator, and returns without re-announcing.
`TestGroupCascadeHandoverAnnouncedOncePerRetry` pins this — though not in its
first form, which review showed was vacuous; see round 2.

## Two things I got wrong, recorded so the next reader does not trust them

1. **"Generic `Tip` (2000) has no `{0}` + `extra` precedent, and the substitution
   cannot be confirmed from this repo."** Stated confidently, used to justify
   hand-rolling a payload and writing a sanitizer, and **false**. Three in-repo
   call sites use it, one of them `modules/group/event.go` — the group-disband
   notice, in the same package. The claim came from grepping a single sibling
   (`sendBotCascadeRemovedTip`) and generalising.

2. **`sanitizeTipName` did not do what its own doc comment claimed.** Executed
   against the real function: `unicode.IsControl` covers only category Cc, so
   `\n` was stripped while U+2028, U+2029, U+202E and U+200B all survived — it
   blocked the character the web client would have collapsed anyway and passed
   the ones that actually force a line break. A name of only zero-width
   characters also bypassed the empty-name fallback. Deleted rather than widened.

## Testing

Full stack locally (MySQL 8.0.46 + Redis 7 + WuKongIM v2.2.4-20260313), with the
per-package database reset CI uses.

- `pkg/space`, `modules/space`, `modules/group`, `modules/bot_api` — all pass
  under `-race -shuffle=on`.
- `gofmt`, `go vet`, `make i18n-extract-check`, `make i18n-lint` — clean.
- Every guard mutation-checked individually: reverting one turns its own test red
  and only that one.

Two verification notes worth keeping:

- **A mutation that should have failed, passed.** The "no departure broadcast"
  guard asserted on a content fragment; the mutation re-added a broadcast with
  the name in `extra`, so the fragment never matched and the test stayed green.
  Fixed by counting group-visible messages that carry any visible content
  (excluding the `type=99` CMD) instead of matching text. Recorded as
  `learnings/pending/mutation-check-the-assertion-not-the-guard.md`.
- **An intermittent `modules/group` failure under `-shuffle=on` is not ours.**
  `TestGroupCreate_WithCategoryID` + `TestGroupSettingUpdate_AllowNoMention_*`
  fail roughly 1 run in 10. Merge base `fe9ddeb` reproduces the same pair at the
  same rate. Root cause is the connection-pool exhaustion already documented in
  `ci.yml` — local MySQL defaults to `max_connections=151`; raising it to 1000 as
  CI does makes both the merge base and this branch green 5/5.

## Follow-ups

- **The ordinary group-kick and bot-API paths have the same defect.**
  `modules/group/api.go` `memberRemove` and `modules/bot_api/groups.go` reach the
  same `SendGroupMemberBeRemove`, so remaining members see nothing there either.
  **Not tracked in #797** — checked the whole issue.
- **A third party's bot can be promoted to creator and is now announced.**
  `querySecondOldestNonBotMemberTx` excludes only the leaver's own active bots; a
  bot owned by someone else is an eligible successor, pinned as an existing
  contract by `bot_cascade_test.go`. This change makes a previously silent
  outcome publicly visible without altering the selection.

## Round 2 — what PR #804 review found

Two blocking findings, both **measured by the reviewers rather than argued**, and
both reproduced here before being fixed.

### The batch chain (P1-1)

The brief and PR claimed the notice is "at most one per group regardless of batch
size", reasoning that firing only on a handover bounds it. False. A batch removes
uids independently (one cleanup job each), so when the removed members are
consecutive in a group's seniority list the handover chains and each link
announces. Measured with a 3-uid batch in one group:

```
group-visible 「已成为新群主」 in ONE group: 3   (claimed ≤1)
   extra[0]=u-c  -> extra[1]=u-s2
   extra[0]=u-s2 -> extra[1]=u-s3
   extra[0]=u-s3 -> extra[1]=u-s4
```

The first two were already false when written. This is the same chaining the
disband path is suppressed for — the reasoning transferred verbatim and I did not
notice, despite having written that reasoning into the code comment myself.

Fixed by checking whether the elected successor is itself `pending` in
`space_member_removal_cleanup` before announcing. Mid-chain links stay silent; the
final link announces the settled owner.

**"Order-independent" is how that read here, and it was wrong — see round 4.**

### The vacuous guard (P1-2)

`TestGroupCascadeHandoverAnnouncedOncePerRetry` did not test what it claimed.
Running the step twice does not produce a retry: the second run returns early at
`queryGroupsWithMemberUIDAndSpaceID` because the member row is already deleted, so
the handover path is never re-entered and `count == 1` holds trivially under
either placement. Two reviewers independently applied the placement mutation and
watched all five tests stay green.

I had claimed "every guard mutation-checked individually". That was true of the
mutations I ran, and the mutation that mattered was not among them: I had mutated
"disable the notice" (red) but never "move where it is sent from" — which is the
decision the test exists to protect. **A guard is only as good as the mutation you
aim at it, and the mutation to aim is the one that reintroduces the defect, not
the one that deletes the feature.**

Rewritten with `RemoveGroupMembers` fault-injected to fail once after the handover
commits. It now goes red (`expected 1, actual 0`) under the placement mutation.

One detour worth recording: my *first* attempt at that mutation also stayed green,
and that was a flaw in the mutation, not the test — I passed the successor through
a package-level variable that survived across calls, where the real "announce in
the caller" shape uses a local reset per call. A mutation has to be faithful to
the code it is impersonating, or a green result means nothing.

### Also from review

- Three comment blocks contradicted the shipped code — leftovers from the
  abandoned broadcast revision. Removed or rewritten.
- "A later rename does not leave a stale name" was false: all three clients render
  `extra[i].name` and none re-resolves by uid. Corrected in both code and brief.
- `sendGroupExitTip`'s display name silently changed to remark-first as a side
  effect of a parameter refactor — outside the stated scope and unguarded. Kept
  (it matches the repo's rule) and now pinned by a test.
- Nothing pinned the `GroupTransferGrouper` content type, nor the best-effort
  delivery decision. Both now have guards.
- Android's `MessageFormat` treats ASCII `'` as an escape — a future English
  translation would silently lose the placeholders there. Recorded in the code.

A third, automated reviewer approved the same head, having noticed the identical
test weakness but classified it as a comment-accuracy issue. It stated plainly
that it ran no code. The two humans ran the mutation. Same observation, opposite
verdict, and the difference was execution.

## Round 3 — the fix's own premise was false (PR #804 round-4 review)

Three reviewers independently found it, two of them by revising an APPROVE they had
already given on the same head. I confirmed every claim against the code before
accepting it.

The chain suppression above was justified in code and brief by one sentence: a
batch's jobs are enqueued atomically in one transaction
(`enqueueMemberRemovalCleanupBatchTx`), so every sibling's `pending` row exists
before any worker starts. That function has exactly two callers, both disband —
and disband suppresses the announcement anyway, so **the primitive I cited never
protected a path that announces**. Of the paths that do announce, the superadmin
`removeMembersForce` is atomic, but the user-side `members/remove` runs
`removeMemberLocked` once per uid, each with its own `Begin`/`Commit`, under
`reason=kicked` — which is not suppressed. The repository's own comments say
`一人一事务` and reason explicitly about earlier uids having already committed. The
evidence was in the file the whole time; I cited a function name without checking
who calls it.

A 10 s worker tick landing mid-loop claims the committed prefix, the later uids
have no rows yet, and the check reads a successor who is about to be removed as
"not pending". Measured: seeded up front → 1 notice; per-iteration enqueue fully
interleaved → 3; per-iteration plus a single tick after the first commit → 2. The
last needs no unusual timing at all.

Two things worth keeping from this round:

- **A test can encode a premise instead of testing it.**
  `TestGroupCascadeBatchHandoverAnnouncesOnce` seeds the batch's pending rows
  before the loop, which models exactly the atomic entrypoints. It is green, it is
  not vacuous, and it is still blind to this — because its fixture *is* the
  assumption under test. One reviewer said they had trusted that test in an earlier
  round and that this is what caused them to miss the defect.
- **Round 2's lesson repeated itself one level up.** There I recorded that I had
  transferred the disband reasoning verbatim without noticing. The fix I wrote for
  it then inherited a different unchecked claim, and I wrote *that* one into the
  code comment too. Checking who calls a function is cheaper than either round.

Also corrected this round: the earlier claim that moving the pending check inside
the transaction "needs a test hook in production code" — it does not.
`HasPendingRemovalCleanup` only calls `SelectBySql`, which is on
`dbr.SessionRunner`, which `*dbr.Tx` satisfies. That fix is cheap; it is simply
independent of this defect and cannot fix it, because there the sibling row does
not exist yet.

Per the maintainer's decision this round corrects the record only — the code
comment, the brief's Chain suppression section, and this journal. The `kicked`-path
defect itself is not fixed here: the per-uid transaction is load-bearing
(transactional outbox plus the in-lock role-hierarchy re-read), so the fix is a
design change, not an edit.

## Round 4 — the composition, and the fix

Round 3 corrected the record but changed no behaviour, on the maintainer's call.
Review rejected that as a resolution — "documented is not resolved" — and while
re-reviewing, one reviewer found something worse than the defect they had filed.
They had approved the corrected head hours earlier on this reasoning:

> Read as a sequence, the three messages are a truthful log of three transitions
> that genuinely occurred. **The last one is always correct, and it is last.**

That sentence is false, and it was mine too — I had shipped the same claim in the PR
description as "the final notice stays correct, so this is wrong content emitted,
not correct content lost". Neither of us ran the case that disproves it.

Two gaps, each correctly judged tolerable **alone**:

- **A** — the `kicked` batch enqueued per uid, so a worker tick mid-loop announces a
  mid-chain handover. Alone: noise. Every line is true when written and the last one
  settles the picture.
- **B** — the suppression is decided once and never revisited. If the elected
  successor's own job later reaches `abandoned`, or they rejoin and it closes as
  `skipped_rejoined`, that handover is announced by nothing. Alone: silence, which
  is exactly what `main` does always.

Compose them and the last ownership message in group history names someone who has
left, while the real owner is never announced. Reproduced here before accepting it:
group `C→S2→S3→S4`, `S4` holding an ordinary retry-backoff `pending` job, batch kick
`{C, S2, S3}` — two notices emitted, last one claiming `S3`, and `S3` is not in the
group. `S4` is the owner, `role=1`, announced by nothing.

The quiet route is the bad one: `skipped_rejoined` needs only an admin removing
someone and re-adding them before the cleanup runs. No error logs, no metric moves.

### What was fixed

**A**, at its source: the user-side batch now enqueues in one transaction
(`removeMembersLocked`), mirroring `removeMembersForce`. My round-3 counter-argument
— that batching the inserts would trade this bug for a torn-write bug — was aimed at
a straw man. All three reviewers had asked for the *other* shape, one transaction
around the whole loop, which keeps both properties the per-uid transaction existed
for. A reviewer had to point that out; I had argued against a design nobody proposed.

**The multi-replica window** as well, since it can compose with B the same way.
`HasPendingRemovalCleanup` now takes a `dbr.SessionRunner` so the check runs inside
the handover transaction.

**B is not fixed** and is still tracked. With A closed it degrades to silence, which
is the pre-PR behaviour — never a false name.

### Two things about the tests

The first guard I wrote for the composition **passed vacuously**: it asserted "if a
notice was emitted, the last one must name the real owner", and with suppression
working the scenario emits nothing, so the assertion block never ran. That is the
third time on this branch that a fixture quietly became the conclusion — and I had
already written the learning about it. Rewritten with two scenarios and hard counts.

The atomicity guard is the one that carries the fix, and it is deterministic rather
than timing-based: a competing row lock stalls the batch on its second uid, and the
test asserts no cleanup row is visible at that instant. Mutating the implementation
back to per-uid commits turns it red with exactly the right message — one job
visible while the batch is still blocked.

One fix here has **no** red test and the code says so: moving the pending check back
outside the transaction leaves everything green, because the window needs a stall
between a `COMMIT` and an indexed `COUNT`. It rests on the row-lock argument, not on
a test.

### Also from this round

An intermittent `modules/group` failure recurred during verification and I chased it
rather than re-running until green: `TestGroupCreate_WithCategoryID` +
`TestGroupSettingUpdate_AllowNoMention_SilentToggleSucceeds`, 2 failures in 7 runs,
both the same pair, both in `api_test.go` which this change does not touch — the
documented pre-existing pair that the merge base reproduces at the same rate.

`seedGroupMember` uses `NOW()`, and `group_member.created_at` is second-resolution,
so members seeded in the same second **tie** under `ORDER BY created_at ASC`. Any
test that depends on seniority was really depending on InnoDB's return order. New
seniority-dependent tests set explicit timestamps; the production ambiguity (two
members joining in the same second have unspecified relative seniority) is recorded
as a follow-up.

## Round 5 — the atomic-batch fix had a deadlock in its lock acquisition

The round-4 fix (batch enqueues in one transaction) acquired its row locks with a
per-uid `FOR UPDATE` loop in caller order, holding up to 200 locks for the
transaction. The old per-uid-transaction loop held one at a time and could not
deadlock; the batch could. Reviewer measured it; I reproduced the mechanism and
switched to a single `SELECT ... uid IN ? ... FOR UPDATE`.

That was necessary but **not sufficient**, and the next round showed why.

## Round 6→7 — the single statement did not use the index I claimed

I shipped the single-statement fix with a comment asserting it "走唯一索引
(space_id, uid)，锁按索引序". A reviewer took that to a live MySQL and it was false:
`space_member` has two candidate indexes, and at this endpoint's real sizes the
optimizer picks `spacemember_spaceid_status`, which locks **every active member of
the space** in `(space_id, status, id)` order — not the 200 targets, and not the
order `transferOwnerAdminLocked` uses (`(space_id, uid)`). Two orders over one row
set is the cross-path deadlock, and it was still reachable from the ordinary admin
endpoint. Batch-vs-batch was fixed (both sides pick the same plan); batch-vs-transfer
was not.

I verified all of it myself before acting, on a scratch `space_member` with the real
index set:

- `EXPLAIN` at 250-member/200-uid → `spacemember_spaceid_status` (ref, 250 rows);
  at 2000-member → `spacemember_spaceid_uid` (range). Plan is size-dependent.
- `data_locks` during the batch: 252 locks on `spacemember_spaceid_status` + 250
  PRIMARY — the whole space. With `FORCE INDEX (spacemember_spaceid_uid)`: 201 + 200
  — only the targets.
- The transfer demote (`UPDATE … WHERE role=2`) uses `(space_id, uid)`.

I could **not** get my own harness to reproduce the actual 1213 (the race is
timing-fragile and needs the batch to grab a low-id prefix before blocking on a
mid-list target held by the transfer). The reviewer did. So I fixed what I could
prove — the plan and the lock scope — with `FORCE INDEX`, and guarded it the way
that is actually deterministic:

**`TestRemoveMembersLockedForcesUniqueIndexPlan`** `EXPLAIN`s the shared SQL const
and asserts the plan is `(space_id, uid)`. Removing `FORCE` turns it red. This is
the primary guard; the concurrent reversed-overlap test is a smoke test whose
docstring now says so — it cannot catch "optimizer picked the wrong index" because
two batches with the *same* wrong plan still don't cross-cycle.

Two lessons worth keeping:

- **A lock-order claim is a claim about the query plan, and the plan is not stated
  in the SQL — the optimizer chooses it.** "It uses the unique index" is unverified
  until you `EXPLAIN` it at the size it runs at. My comment asserted an index the
  optimizer did not pick; a deadlock guard whose fixture was too small to trigger
  the real plan passed for the wrong reason. Same shape as the fixture-encodes-the-
  premise learnings, now at the storage-engine layer.
- **When you cannot reproduce the failure, fix and guard the mechanism you can
  measure.** I couldn't win the deadlock race, but the plan and lock-scope are
  deterministic and are the actual mechanism. A plan assertion protects the next
  editor better than a flaky deadlock test would.

The remaining review items were record accuracy (a godoc attached to the wrong
symbol by a missing blank line, `require` in a worker goroutine, a comment claiming
a COUNT hiccup can never roll back the handover when a connection-level one can —
safely), plus a genuinely missing guard: swapping the two `Name` values in `extra`
shipped an inverted message and nothing caught it, because the tests asserted the
uid positions but not the names. A positional `payloadExtraNameAt` closes it; the
mutation now goes red.
