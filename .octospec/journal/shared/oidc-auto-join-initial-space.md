---
type: Journal
title: "Journal: oidc-auto-join-initial-space"
description: One admin setting (space.oidc_initial_space_id, empty = off) makes every account created through OIDC — browser callback, /bind/create, and both token-exchange endpoints — an ordinary member of that Space right after its identity row lands, unblocking POST /v1/integrations/oidc/exchange for SSO users who previously belonged to no Space at all.
tags: ["oidc", "space", "isolation", "system-setting", "idempotency", "observability", "error-response", "testing"]
timestamp: 2026-09-03T00:00:00Z
# --- octospec extension fields ---
task: oidc-auto-join-initial-space
upstream: self
source: self
---

# Journal: oidc-auto-join-initial-space

## What was done

Accounts created through SSO belonged to no Space, while
`POST /v1/integrations/oidc/exchange` requires an active Space **and**
membership (`modules/integration/api.go:324`). Those users logged in
successfully and then failed exchange forever, with no reachable remedy: they
carry no email or phone and a domain-account name, so the email/phone invite
flows cannot reach them, and a manual admin add requires knowing the uid of a
person who just appeared. The defect predates #829 and is independent of it.

One admin-tunable setting now closes it: **`space.oidc_initial_space_id`**
(string, empty = off, no separate enable flag). When set, an account created by
`modules/oidc` becomes an ordinary member (`role=0`) of that Space immediately
after its `user_oidc_identity` row is persisted.

- `pkg/space.IsSpaceActive` — an existence gate that asks about `status`
  directly. `GetSpaceName` answers the same question by proxy through a
  non-empty name, which is fine for a display string and wrong for a gate:
  `space`.`name` carries no non-empty guarantee, so a blank-named Space would
  read as absent and could never be configured.
- `modules/common` — schema row, a getter that trims on read, and write-path
  validation refusing a missing or inactive target. The value is trimmed **into
  the plan**, so the stored value, `value` and `effective_value` all agree.
- `modules/space.AutoJoinInitialSpace` — refuses to reactivate a member an admin
  removed, bypasses approval mode, honours `max_users`, and runs the identical
  `afterJoinSpace` side effects. Returns a typed outcome the caller uses directly
  as a metric label. The judgement and the write happen in **one transaction**
  (`atomicJoinInitialSpace`), with the authoritative Space-status recheck under a
  current read before commit — see the lock-order note below.
- `modules/oidc` — hooks on **every** account-creating path. `CreateUser` is
  set in exactly three places in the module, and all three are hooked:
  `api.go` (browser callback), `bind_service.go` (`/bind/create`), and
  `exchange_complete.go`, which serves both `POST …/exchange` and
  `POST …/exchange-jwt`. The exchange endpoints arrived on `main` with #829,
  after this brief was written; leaving them unhooked would have reproduced
  the exact stranding this task removes. `bind_service.go`'s confirm path is
  `CreateUser: false` and is correctly not hooked.
  Never returns an error, never writes a response; the outcome lands in
  `oidc_initial_space_join_total`.

## Load-bearing decisions

- **The trigger is account creation, never login.** This single distinction
  carries two other required behaviours for free: a returning user does not
  re-enter the join path, and a member an administrator removed is not silently
  added back on their next SSO login. Anything that later "ensures membership on
  every login" breaks both at once.
- **The identity-race loser is excluded.** Two concurrent first logins for one
  `(issuer, sub)` each create a user; the unique key admits one identity row and
  the loser's session is re-signed onto the winner, leaving a ghost account
  nobody can log into. Joining it consumes a seat under `max_users`, so a Space
  sized for the workforce could refuse a real hire over an account that exists
  only in the audit trail. The hook therefore sits **after** the identity insert
  and clears its uid on the recovery branch.
- **Refusing to reactivate is a deliberate divergence from `executeJoinSpace`.**
  That function reactivates a `status=0` row, which is right for "user re-applies
  with an invite code" and wrong here. Under the current trigger the branch is
  unreachable (a brand-new uid has no member row); it is written as a refusal so
  the function stays safe if a future caller puts it on a login path.
- **The join is synchronous.** Clients call
  `GET /v1/integrations/oidc/spaces` as soon as they hold the session, and that
  endpoint filters on an active member row, so a deferred join shows an empty
  first screen.
- **Failure is invisible to the user by design**, which makes the counter the
  only line of sight. Alerts belong on `space_full` / `space_inactive` / `error`,
  not on waiting for someone to report "I logged in but exchange fails".
- **The Space-status recheck goes *after* the INSERT, not before it.** The
  intuitive shape — `SELECT space FOR UPDATE`, then insert — deadlocks: both
  disband paths take the `space_member` range lock first and update `space`
  second, so locking `space` first inverts the order (Error 1213, possibly
  rolling back the *administrator's* disband). The trap is that `FOR UPDATE` on
  a row that does not exist yields a **gap lock, and gap locks are mutually
  compatible** — so "lock space_member first" does not serialize anything; the
  only real rendezvous with disband is the INSERT's insert-intention lock. The
  fix is to let the INSERT be the synchronisation point and recheck status under
  a current read before committing: a disband that already committed is seen,
  and the transaction rolls the row away. Written the wrong way round first and
  caught by the deterministic race test, which is why the reasoning is recorded
  in the function.
- **The panic guard stops at the `go` boundary, and the comment now says so.**
  The hook recovers what it runs synchronously; `afterJoinSpace` dispatches two
  goroutines that escape it. `joinPresetGroups` already had its own recover,
  `fireSpaceMemberJoinEvent` did not — and `EventCommit` runs the registered
  listeners synchronously inside it, so a listener panic took the process down.
  Pre-existing, but auto-join lets an ordinary SSO login reach it, and logins
  are far more frequent than invite redemptions. A recover was added there —
  together with `defer tx.RollbackUnlessCommitted()`, because a recovered panic
  skips the commit and would otherwise leave the transaction and its locks
  hanging on the connection: containing a crash by leaking a transaction just
  trades one failure for another.

## Gotchas worth remembering

- **`querySpaceByID` swallows query errors.** It returns `(nil, nil)` whenever
  `m.SpaceId == ""`, checked *before* `err`, so a real failure reads as "Space
  does not exist". Harmless for a handler that maps both to 404; wrong for any
  caller that must distinguish a misconfigured id from a database blip.
  `queryActiveSpaceForAutoJoin` exists for that reason — do not "simplify" it
  back.
- **A batch settings write can name one key twice.** The write loop upserts
  every plan in order, so the *last* item wins. A validator that stops at the
  first match approves one value and stores another. Review caught this in the
  first revision; the onboarding and archive-ordering guards already judged their
  merged last-wins value, and this one now agrees.
- **`SystemSettings` is a process-wide singleton.** A test that writes a setting
  must clear it in cleanup, or every later test in the package inherits it and
  fails somewhere unrelated.
- **`fakeIdentityStore` could not express the real race.** Pre-seeding a binding
  makes `ResolveOrLink` match on the first lookup, so the insert never runs. The
  interleaving that matters — the winner commits *between* our lookup and our
  insert — needs a call-ordering knob. This branch added one; #829 landed an
  equivalent `winnerAppearsAfterFirstGet` first, so the merge dropped ours and
  the ghost test drives theirs. Theirs is the better model: it keys on which
  call this is, which is what a race *is*, rather than on "an insert was
  attempted", which infers the timing from a result.
- **A test asserting only an error-free path proves less than it looks.** The
  first version of the query-failure test asserted only the missing-row half, so
  an implementation that reported "inactive" unconditionally would have passed
  under a name promising the opposite. It now renames the `space` table away to
  induce a real failure.
- **A test asserting only absences proves less than it looks.** The first
  version of the ghost test asserted the loser had no member row *and* the
  winner had none either (the winner's callback was never driven), so an
  implementation that suppressed the join for everyone would have passed.
  Review caught it. It now drives both callbacks and asserts the winner *did*
  join, which is what makes "only the winner" discriminating — verified by
  disabling the hook entirely and watching it go red.
- **Each package needs its own freshly created test database.** Sharing one
  panics with `unknown migration` inside `testutil.NewTestServer`, before any
  test body runs.

## Follow-ups deliberately not done

- **Deprovisioning has no reverse flow.** Nothing anywhere removes Space
  membership when an IdP account is disabled — `sync_worker` only syncs realname
  claims. The hole predates this change, but it was empty while SSO users
  belonged to no Space; auto-join fills it. Recorded as a known limitation.
- **The Space-status race is closed on this path only.** `executeJoinSpace` and
  the manager add path still read Space status outside the membership
  transaction — the window `forceDisbandSpace` documents as a repo-wide
  follow-up. Closing it there means moving the recheck into
  `atomicAddMemberIfNotFull` for every join path with a consistent lock order,
  which has its own blast radius and deserves its own change. The lock-order
  reasoning and the deterministic test in this task are the template for it.
- **A single global Space is a blunt instrument.** Same-Space membership implies
  `HasAuthzRelation` (profile visible, DM allowed), so the feature converts
  "can authenticate at the IdP" into "can see the company Space". Right for a
  single-org deployment where the IdP is the employee directory; wrong for
  multi-tenant or multi-IdP. Per-issuer / per-claim mapping stays out of scope,
  and today's `IDTokenClaims` carries no department or group claim to map on.
- Backfilling existing SSO accounts; admins use the manager members endpoint.
