---
type: Journal
title: "Journal: featuregate-user-scoped-flags"
description: Revive the generic feature-gate framework, extend it to a user dimension, and expose per-user flags through a new authenticated endpoint.
tags: [featuregate, rollout, auth, wire-contract, rate-limit, testing]
timestamp: 2026-08-25T00:00:00+08:00
task: featuregate-user-scoped-flags
source: self
---

# Journal: featuregate-user-scoped-flags

## What was done

- Revived `pkg/featuregate` (pure `Evaluate`, zero IO) and `modules/featuregate`
  (MySQL truth + Redis read cache + env kill switch + superadmin management
  endpoints) from the never-merged PR #280. Only the framework was revived; the
  PR's second commit, which gated incoming webhooks, was deliberately left out.
- Extended evaluation to a **user** dimension: `scope_type=user` whitelist
  entries, and a per-rule `bucket_by` (`group`/`space`/`user`) so one framework
  can serve both "roll out by group" write gates and "roll out by user" display
  flags.
- Added `GET /v1/featuregate/flags` — an authenticated, read-only endpoint that
  returns the current user's flags. `GET /v1/common/appconfig` was not touched.
- Added a client-visible flag registry that decouples the ops-facing
  `feature_key` from the wire-facing `client_key`.

## Structural decisions worth knowing

- **The whitelist now spans modes.** The original `ModePercent` branch never read
  `scopes`, so the standard rollout path `whitelist (dogfood) -> percent` dropped
  the whole dogfood cohort at the moment of the switch, and `addScope` during the
  percent phase was a silent no-op — the write returned 200 and the read path
  simply did not look. The whitelist is now an exemption list that applies under
  `whitelist` and `percent`, and is **never** honoured under `off` (an emergency
  stop must stay unconditional).
- **Three fail policies, fixed per call site.** `AllowCreate` fail-closed,
  `AllowPush` fail-open, and a new `AllowDisplay` that returns `(allow, ok)`.
  `ok=false` means a storage failure and the caller must **omit** that key so the
  client keeps its previous value; a bare bool cannot express that, because the
  response carries only booleans and the client otherwise cannot tell "really
  off" from "the server blipped".
- **The kill switch must be a definite `false`, never an omission.** Omitting it
  would make clients keep the previous value, so pressing the emergency stop on
  an already-rolled-out feature would never converge in the UI.
- **`bucket_by` defaults to `group`, which collides with the flags endpoint.**
  That endpoint only has a UID. Bucketing on an empty group_no puts every user in
  one bucket, so a configured 50% silently becomes 0% or 100% while the console
  still shows 50%. Blocked on the write path for client-visible keys and
  fail-closed on the read path — the write check alone is bypassable by editing
  the DB, the read check alone lets ops believe a broken config took effect.

## Gotchas worth remembering

- **A wrong `Vary` header is worse than none.** `Vary: Authorization` was copied
  from `modules/bot_api/card_profile.go`, but octo-lib's `AuthMiddleware` reads
  the **`token`** header. Naming a header that is empty on every request gives a
  shared cache no discriminator at all.
- **`CleanAllTables` does not clear Redis, and that includes this module's own
  cache.** Dropping the test DB while `ft:rule:*` was still warm made the suite
  fail non-deterministically — and pass again once the 60s TTL expired. Every
  DB/Redis test now goes through one fixture that flushes both the gate cache and
  the UID rate-limit buckets.
- **`X-RateLimit-Scope: uid` is the cheapest proof that the limiter is mounted
  correctly.** octo-lib's `UIDRateLimitMiddleware` silently skips when it cannot
  read a uid, so the header's presence proves both "mounted" and "mounted after
  `AuthMiddleware`" without hammering the endpoint to force a 429.
- **MySQL 8 enforces `REGEXP_LIKE` inside `CHECK`.** Used to pin the
  `feature_key` charset at the DB layer, closing the direct-DB-edit bypass.
- The framework ships with an **empty** client-flag registry: no product feature
  is gated yet. That is deliberate — this change delivers the mechanism.

## Review round 1 (2026-08-25)

One blocking finding, and it was real: `update` validated the incoming `bucket_by`
but never the **already-persisted** scope rows. The registry is a compile-time list
while gates are DB rows, so "an internal gate accumulates group scopes, then a later
deploy makes it client-visible" is the *normal* lifecycle — and after that, an
`update` to `whitelist`/`bucket_by=user` was accepted with a whitelist that could
never match. Silent on both sides: `Evaluate` returned `whitelist_miss`, which is
indistinguishable from a genuine miss, so no log fired either.

Fixed on both sides, matching the write+read pattern used everywhere else here:

- write: `update` now loads the persisted scopes and rejects when none uses a
  dimension the flags endpoint can supply (an *empty* whitelist stays legal — that
  is a real state, not a misconfiguration);
- read: `Evaluate` distinguishes "didn't match" from "couldn't possibly match" via
  a new `whitelist_dim_unavailable` reason, and `Decision.DimensionUnusable` reports
  a dimension mismatch **independently of the decision** — which is what lets the
  `percent` 0%/100% short-circuits keep their (correct) answer while still surfacing
  a misconfigured `bucket_by`. Two reviewers disagreed on that one: the decision is
  right at 100% (console and reality agree, and failing closed there would break a
  rollout at full ramp), but the misconfiguration must not stay invisible, or the
  operator discovers it the moment they dial 100 down to 50 and everyone drops out.

Also in this round: `updated_by` audit columns on both tables; the
`unavailable` wire field replacing the omission semantics; a whitelist size cap
enforced on the **write** side (a `LIMIT` on the evaluation query would have traded
"slow but correct" for "fast but wrong"); `scope_id` charset validation closing an
insert-but-cannot-delete asymmetry; `description` counted in runes to match a
character-counted column; `context.Context` parameters dropped from the cache
loaders because octo-lib's `redis.Conn` has no ctx-aware methods at all, so honoring
it was never possible and the signature was lying; a cache schema version in the
Redis key prefix; and `docs/cutover-framework.md`, which described this module as
"fail-open" back when it did not exist — two of its three call sites are fail-closed.

Date discipline: the migration filename and three octospec timestamps were all five
days early. Run `date` before naming anything after today.

## Review round 2 (2026-08-25)

Both reviewers independently landed on the same blocker, and it was a regression
introduced by round 1's own fix: the new persisted-scope check ran on **every**
`update`, including a de-escalation. `modeNeedsScopes` excludes `off`/`on` — those
modes never consult the whitelist or `bucket_by` — so the check was validating a
precondition the requested state does not use, and a client-visible gate carrying a
legacy group-only whitelist could not be turned off through its own API. The obvious
rollback body `{"mode":"off"}` failed twice over: once on the defaulted
`bucket_by=group`, once on the new scope check. What remained was deleting up to
1000 scope rows one call at a time, or the env kill switch — which this module's own
docs position as the last resort for when DB/Redis are down, not as the primary
rollback.

The rule that came out of it: **turning something off must never be harder than
turning it on.** Both checks are now gated on `modeNeedsScopes`, and the reasoning is
that every transition *into* a scope-consuming mode re-validates the request anyway,
so permitting a stale `bucket_by` to sit in a row that nothing reads costs nothing.

Also this round: the omission→`unavailable` change had landed in code but only in the
brief's summary — Acceptance and Load-bearing still specified the opposite, as did
five code comments and an errcode note. Since there is no OpenAPI spec, the brief is
the only durable description of this contract, so a client team implementing from
Acceptance would have built exactly the failure the change was made to prevent. All
of it now says one thing. Worth remembering that changing a contract means changing
every place that states it, and a dated revision note at the top does not cover the
document below it.

Smaller: `addScope`'s documented idempotency broke exactly at the quota boundary
(re-adding an existing entry was rejected before reaching the `ON DUPLICATE` no-op —
precisely when an operator is most likely retrying), so existence is now checked
before the count. `delScope` deliberately does *not* validate the prospective
post-delete state: blocking on it creates an ordering trap where neither entry of a
`{user, group}` pair can be removed first. That path is left to the read-side
backstop, which round 1 added and which is now explicitly noted in the code.

## Review round 3 (2026-08-25)

The blocker was a collation mismatch, and it is the sharpest finding of the three
rounds because the defence I had written did not work and I had asserted in a comment
that it did.

Both new tables inherited the database default `utf8mb4_general_ci`, while every Go
comparison of those same values is byte-exact (`s.ID == id`, `rule.Mode != ModeOff`,
the `switch` in `dimValue`). Reproduced against MySQL 8.0.46:

- `scope_id` `"UserA"` and `"usera"` collide under the unique key. The second `add`
  hits `ON DUPLICATE KEY UPDATE updated_by=...`, so only `updated_by` changes and the
  row keeps the first spelling. The operator gets 200; that user is never admitted.
  And the read side stays quiet too — a usable `user`-dimension entry does exist, so
  `Evaluate` returns an ordinary `whitelist_miss` with `DimensionUnusable=false` and
  `AllowDisplay` logs nothing. Write succeeds, read silently misses, both sides mute:
  the round-1 blocker's exact shape in a different dimension.
- **The CHECK constraints did not enforce what their own comments claimed.**
  `REGEXP_LIKE` and `IN` both inherit the column's collation, so under `_ci`,
  `feature_key='ZZ_UPPER'` passed `'^[a-z][a-z0-9_]*$'` and `mode='OFF'` passed
  `IN ('off',...)`. I had called the `feature_key` charset check "尤其重要" in the
  migration header while it was inert.
- A hand-written `mode='OFF'` splits policy across entry points: `AllowPush` reads it
  as not-off and allows, `Evaluate` reads it as an unknown mode and denies.

All five identity columns are now explicitly `utf8mb4_bin`, verified: the uppercase
key and `mode='OFF'` inserts are rejected by the existing CHECKs, and `UserA`/`usera`
become two distinct rows. The repo had already been bitten by this class — `modules/
message`'s `message_reaction_emoji_binary` is a forward-only repair ALTER, and
`card_template_catalog` builds at `_bin` from the start.

**A CHECK constraint written against a `_ci` column is decorative.** Worth checking
the collation before trusting any string constraint, and worth testing the constraint
by trying to violate it rather than by reading it.

Also this round: orphan whitelist rows are rejected (`addScope` requires the rule to
exist — otherwise the row is invisible to `list`, which enumerates rules and joins
scopes onto them, yet goes live the moment someone creates a matching rule); an empty
uid now lists every key as `unavailable` instead of returning a definite `false` for
all of them, which was the one response shape the whole design exists to avoid;
`AllowCreate` now consumes the same misconfiguration signal `AllowDisplay` does; the
dimension warning is deduplicated per (key, reason) because the display endpoint
evaluates once per user per fetch and a single misconfigured rule would otherwise
flood the log with a message of constant information content.

One of my own edits from round 2 had reversed a negation in the brief's most-read
section — a blind `省略` → `unavailable 数组` substitution turned "not omitted from
flags" into nonsense. Mechanical find-and-replace across prose needs the result read
back.
