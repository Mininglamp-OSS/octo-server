---
type: Journal
title: "Journal: space-join-apply-resubmit"
description: A pending Space join application can now adopt a freshly submitted invite code instead of staying bound to a spent/disabled/expired one, so an applicant can repair their own stuck application without an admin rejecting it first. Approval-time invite failures are classified (exhausted vs invalid), notify the applicant, and share one implementation across all three approval entry points.
tags: ["space", "isolation", "acl", "error-response", "i18n", "wire-contract", "rate-limit", "testing"]
timestamp: 2026-07-30T11:25:47+00:00
# --- octospec extension fields ---
task: space-join-apply-resubmit
upstream: Mininglamp-OSS/octo-server#683
source: user
---
# Journal: space-join-apply-resubmit

## What was done

`joinSpace` used to return a bare `PENDING` the moment it found a `status=0` row,
*before* writing the invite code, refreshing the time, or notifying admins. Since
`uk_space_uid (space_id, uid)` gives an applicant exactly one row per Space, an
application bound to a code that later went exhausted/disabled/expired became
un-approvable **and** un-repairable: approval failed and rolled back to pending,
re-submission was short-circuited, and only an admin rejection broke the cycle.

- **`modules/space/api.go` — re-submission branch.** A pending row whose
  `invite_code` differs from the submitted one is updated to this submission's
  code with the application time refreshed, then admins are re-notified and the
  response becomes `NEED_APPROVAL`. Same code re-submitted stays a no-op
  `PENDING` — no write, no DM.
- **`modules/space/db.go` — `refreshPendingApplyInvite`.** `UPDATE ... WHERE
  id=? AND status=0`. The guard is load-bearing: approval flips status to 1
  *before* consuming the slot, so an unconditional update would knock an
  in-flight approval back to pending and decouple the consumed slot from the code
  on the record. 0 rows affected ⇒ re-read and re-decide (approved ⇒
  `already_member`; rejected/missing ⇒ fall through to the upsert).
- **`upsertJoinApply` refuses to touch an approved row.** This is the *second*
  path a re-submission can take into the application row, and the first version
  of this change left it unguarded — see "Review round 1" below.
- **`upsertJoinApply` now refreshes `created_at`.** Both list paths order by and
  return `created_at` as the application time, so reject-then-reapply used to
  keep showing the first application's date.
- **One shared `consumeInviteForApproval`.** The consume-fail block existed
  byte-identically in *three* approval entry points (Space-scoped, admin console,
  and the H5 `auth_code` flow reached from the admin DM). It now rolls back,
  classifies, notifies, and returns the caller's error code from one place.
- **Failure classification.** `incrementInviteUsedCountAtomic` merges
  status/expiry/use-cap into one WHERE and returns a bare false. On failure the
  invite row is re-read unfiltered: use-cap ⇒ `invite_code_exhausted`,
  disabled/expired/missing ⇒ `invite_code_invalid`, with the precise reason in
  logs. No new error code, so no `active.zh-CN.toml` edit.
- **Applicant notification.** Approval failing on invite consumption now DMs the
  applicant to re-submit with a valid code. Without it the self-repair path would
  never be used — the applicant had no way to know they were stuck.
- **`SharedUIDRateLimiter` on `POST /v1/space/join`.** Re-submission fans out one
  DM per admin, and the route had no UID limiter. Change-detection is the first
  containment layer; the limiter is the second, and closes a pre-existing gap
  against the rate-limit rule.

## What was NOT done

Invite slots are still consumed at approval, not at submission. Approval can
still fail when the bound code was spent in between — this change makes that
state **recoverable**, not impossible. Moving consumption to submission time
changes invite semantics (pending applications hold slots; rejection must refund)
and deserves its own review. Tracked as a follow-up.

## Review round 1 — the guard was half a guard

Two reviewers (@Jerry-Xin, @yujiawei) independently blocked on the same defect,
and they were right. A re-submission can reach the application row by **two**
paths, and only one was guarded:

1. the pending lookup finds a row → `refreshPendingApplyInvite` (guarded);
2. the pending lookup finds **nothing** → `upsertJoinApply` (was unguarded).

Path 2 is reachable inside the approval window: approval flips `status` to 1
before `executeJoinSpace` inserts the member row. In that window a re-submission
sees no member (so the membership check passes) *and* no pending row (because
`queryPendingApplyBySpaceAndUID` filters `status=0`), so it fell through to an
unconditional `ON DUPLICATE KEY UPDATE status=0, reviewer_uid=''`. The approver
never re-asserts `status=1`, leaving an admitted member whose application reads
pending — and `rejectJoinApply` would then accept that row, mark it rejected, and
DM "application rejected" **without revoking the membership**.

Reproduced at the DB layer before fixing: `status=0 reviewer="" invite_code="code-B"`.

The race pre-dates this change (the merge-base had the same unconditional upsert),
but it was in scope: R6 is a stated requirement, and — worse — the doc comment on
`refreshPendingApplyInvite` and this journal both *asserted the containment was
complete*. **Overclaiming in the documentation was the more serious error**: a
reader checking whether the hazard was closed would have believed it was.

Fixed by making `upsertJoinApply`'s duplicate-key branch conditional (`IF(status=1, …)`
per column, `status` assigned last because MySQL evaluates assignments left to
right and the other columns must read its original value), plus a re-read so the
caller answers `already_member` instead of notifying admins about a phantom
application. Covered by A8 (HTTP-level, constructs the window exactly) and A8b
(DB-level guard, both approved and rejected rows).

Also from review, non-blocking but fixed here: `queryInvitationByCodeUnfiltered`
returned "not found" with a nil error on a read failure, which would have polluted
the one diagnostic this helper exists to produce; and A7 asserted in its message
that admins are not re-notified without actually asserting it — the `auth_code`
Redis keys are observable, so it now counts them.

Deferred to follow-ups: approval consumes the invite code from a struct read
before the status CAS, so a concurrent re-submission can charge the stale code
(accounting drift, no admission impact); `already_member` can be reported during
the window before membership actually exists; `created_at` refresh makes offset
pagination less stable.

## Review round 3 — stop adding guards, make the state unrepresentable

@yujiawei blocked again, and the meta-point was the important part: three rounds
had each closed *one more way* for `status=1` to mean something other than
"member" — a write guard, then deletes at three removal call sites, then a
backfill. The set of writers that had to cooperate kept growing, and one of them
already had a hole (`removeMemberLocked` returns before the cleanup when the
member row is already inactive).

The concrete P1: `status=1` can also mean **an approval that died in flight**.
Two generators, both real — a swallowed rollback write, and process death between
the status CAS and the member insert, which were separate transactions with an
invite-consume UPDATE in between. Reproduced; all four product surfaces refuse
the resulting row:

```
re-apply       -> 400 already_member    (the caller is provably not a member)
admin approve  -> 400 apply_processed
admin reject   -> 400 apply_processed
pending queue  -> 0 rows
```

They also corrected the round-2 reasoning: "a readback discriminator cannot work"
was true *before* the removal cleanups existed, but I kept asserting it as a
general impossibility after those changes had eliminated one of the two ambiguous
states. Third over-claim in three rounds.

And the addendum that settled the approach: **A8 asserted the lockout as expected
behaviour.** It built `status=1` with the member row absent and asserted
`already_member` plus an untouched row — correct for a live approval, and
byte-identical to a dead one. So the suite was not merely missing the defect, it
was holding it in place; and A8's 1100 ms sleep meant any bounded-staleness rule
would have had to retune it.

### What shipped

The status flip, the invite consumption, and the member write now happen in **one
transaction** (`approveJoinApplyAtomic`). `status=1` is therefore only ever
committed together with an active member row, so "approved but not a member" is
unrepresentable rather than merely guarded against.

That collapses the rest:

- **The compensation logic is gone.** No writing status back to 0, no refunding a
  consumed slot — a failure rolls the whole thing back. Those compensating writes
  were themselves a P1 generator when they failed.
- **The round-2 removal cleanups and the data migration were reverted.** With the
  invariant established at the only place that creates `status=1`, a stale row is
  unambiguous — `status=1` with no active member can only mean membership ended
  after approval — so the readback repairs it in place, with no time threshold.
  That also retires the two items round 3 flagged for human sign-off: approval
  history is no longer deleted, and there is no migration.
- **P2-1 folded in.** The invite code is re-read *inside* the transaction after
  the CAS locks the row, so a concurrent re-submission can no longer make approval
  charge the code it replaced.
- **P2-2 fixed.** `queryJoinApplyByID` returns read errors before inferring
  absence from a zero-value field, since its result now drives a guard.

Verified by sweeping every approval failure mode (invite disabled, invite
exhausted, space full, happy path) and asserting no orphan row is observable in
any of them.

## Learnings

- **A guard test can pass for the wrong reason.** The acceptance test for "a
  re-submission must not reset an approved application" was satisfied by
  `joinSpace`'s *pre-existing membership check*, which short-circuits earlier —
  it never reached the `status=0` guard it was written to cover. Both the HTTP
  outcome test and a direct DB-level test of the guard are now present. When a
  test targets a specific defensive branch, verify the branch is actually
  reached, not just that the assertion is green.
- **Count the entry points before designing the fix.** The brief named two
  approval paths; there were three, each with a byte-identical copy of the block
  being changed. Rule resolution (the trust-boundary rule's parity principle,
  pulled in via a tag) is what surfaced the third. Duplicated blocks are a signal
  to grep for siblings before editing any one of them.

## Verification

`go build ./...`, `go vet ./modules/space/`, `golangci-lint run ./...` (0 issues),
`make i18n-extract-check`, `make i18n-lint` all pass.

Integration tests **were** run: MySQL 8.0, Redis, and WuKongIM v2.2.4 were
installed and started in the session container (WuKongIM needs
`WK_TOKENAUTHON=false`, since the tests use octo-lib's empty manager token).
`go test ./modules/space/ -count=1` passes in full, including all 7 new cases.

Full repo sweep: **87/87 packages pass** (65 in one serial run, plus the 22 that
had failed re-run individually — 22 ok / 0 fail). No residual failure to
attribute, so no `main` baseline comparison was needed.

### Reproducing the environment

`go test ./...` does **not** work here, in parallel *or* with `-p 1`. Two
harness properties, both pre-existing and unrelated to this change:

- **One shared `test` database.** Each package applies its own embedded
  migration set to it, so a later package's `sql-migrate` finds rows for
  migrations it does not know and panics with
  `unknown migration in database`. The fix is to recreate the database per
  package, not to serialise. (Serialising alone still fails — the first
  diagnosis of "packages wipe each other's rows" was only half right.)
- **`OCTO_MASTER_KEY` must be exported.** CI exports it; only `modules/space`
  and `modules/file` set a fallback themselves. Without it, packages that store
  private keys panic with `refusing to store unencrypted private key`.

```bash
export OCTO_MASTER_KEY=0123456789abcdef0123456789abcdef
# per package: DROP/CREATE DATABASE test; redis-cli flushall; go test <pkg>
```

WuKongIM needs `WK_TOKENAUTHON=false`; the release asset is
`wukongim-linux-amd64` under `releases/download/<tag>/`.

### The first run failed, and the tests were wrong — not the code

Two of the new timestamp assertions failed on the first run. The cause was the
test helper, not the implementation: `Load(&time.Time)` on a bare scalar returns
`rows=1, err=nil` while leaving the zero time, because dbr treats `time.Time` as
a struct and finds no fields to map. Raw SQL confirmed the production UPSERT
refreshed `created_at` correctly the whole time.

Worth recording *how* it presented: the third assertion — equality between two
reads — **passed**, because zero equals zero. That is the exact assertion written
to catch "the timestamp was not refreshed", and it is permanently green under
this bug. A mixed pass/fail result pointed at the implementation when the
instrument was broken. Helper now selects `UNIX_TIMESTAMP` into an `int64` and
asserts non-zero. Staged as a learning.
