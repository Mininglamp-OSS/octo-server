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

## Review round 2 — the guard created a lockout of its own

@yujiawei blocked on a P1 that the round-1 fix introduced. Reproduced end to end
over HTTP before touching anything: apply → approve → `/leave` → re-apply gives
`400 already_member` and no pending application, permanently.

`space_join_apply` has four writers and none of them is in a member-removal path
(`removeMemberLocked` only sets `space_member.status=0`). So `status=1` means
"was approved once", not "is currently a member" — and the round-1 readback read
it as the latter. On `main` this worked only because the unconditional upsert
reset the stale row; the `IF(status=1, …)` guard is what closed that door.

Same failure class the task exists to fix — an application that can be neither
approved nor repaired — except reachable by an ordinary user action rather than a
race.

### The obvious fix does not work, and that is provable

The first instinct was to discriminate at the readback: `existing.Status == 0`
(a removed-member row) means the approved row is stale, so reset it. Implemented
it and ran two cases against it:

| case | candidate fix |
|---|---|
| ex-member re-applies | PASS |
| ex-member's *in-flight re-approval* not reverted | **FAIL** — `status=0 reviewer="" code="cB2"` |

Both cases present **identical stored state** at the moment of the request —
`space_member.status=0` and `space_join_apply.status=1` — because
`executeJoinSpace` reactivates the member *after* the status flip. The correct
answer differs between them, so no discriminator over that state can exist. The
reviewer's own suggestion used a time heuristic for exactly this reason, and said
so plainly.

### What was done instead

Made the invariant true rather than inferring it. All three paths that end
membership now delete the approved application in the same transaction —
`removeMemberLocked` (leave + remove), `removeMembersForce` (admin batch), and
`forceDisbandSpace`. With the stale state gone, a `status=1` row can only mean an
approval in flight, and the round-1 readback becomes correct as written.

Delete rather than re-status: `status` has only 0/1/2 and octo-admin renders
those three, so a new value would need a frontend change; `0` would inject a
phantom row into the pending queue; `2` would claim a rejection that never
happened.

A data migration handles rows that already exist — removal-time cleanup only ever
helps future removals, and every approval-mode Space today may hold rows that
become lockouts the moment this merges. Its predicate was verified against all
five states (approved+active kept, approved+removed deleted, approved+no-member
deleted, pending kept, rejected kept).

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
