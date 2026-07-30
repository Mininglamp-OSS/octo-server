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
`make i18n-extract-check`, `make i18n-lint` all pass. The test binary compiles.

**The tests were not executed**: this environment has no MySQL/Redis/WuKongIM and
no Docker, so `go test ./modules/space/...` cannot run here (`TestMain` panics on
`dial tcp 127.0.0.1:3306`). The 8 new/updated tests are unrun and CI is the first
real execution.
