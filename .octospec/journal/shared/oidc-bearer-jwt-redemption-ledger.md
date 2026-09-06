---
type: Journal
title: "Journal: oidc-bearer-jwt-redemption-ledger"
description: /exchange-jwt stops judging a bearer JWT's freshness by its iat and judges the redemption itself instead — a Redis ledger keyed by the token's digest, bounding how late a first redemption may arrive (F) and how long a token may sit unused between redemptions (T).
tags: ["oidc", "auth", "bearer-jwt", "replay", "redis", "observability", "error-response", "testing"]
timestamp: 2026-09-06T00:00:00Z
# --- octospec extension fields ---
task: oidc-bearer-jwt-redemption-ledger
upstream: Mininglamp-OSS/octo-server#829
source: self
---

# Journal: oidc-bearer-jwt-redemption-ledger

## What was done

`/exchange-jwt` refused any bearer JWT whose `iat` was more than ten minutes old
(`bearerJWTMaxAge`, added by #829). A client that logged in and exchanged 36
minutes later got a 401 indistinguishable from "your credential is invalid" —
observed in production. The ceiling was not wrong to exist: before it, `exp` was
the only freshness control, so a captured assertion could mint sessions for its
whole ~15-day life, including after the user logged out upstream, and we cannot
query the IdP's revocation state.

The anchor was the defect. `iat` is when the upstream signed the token; it says
nothing about when the user actually came to redeem it. So the ceiling missed in
both directions — it refused legitimate late arrivals and still admitted an
attacker who captured the token inside the window.

Freshness is now judged from **the redemption**, by a Redis ledger keyed by
`sha256(token)`:

| ledger state | condition | result |
|---|---|---|
| no record | `now - iat ≤ F` | admit, write `last_at` |
| no record | `now - iat > F` | refuse |
| record | `now - last_at ≤ T` | admit, refresh `last_at` |
| record | `now - last_at > T` | refuse |

- **F** (`OCTO_OIDC_BEARER_JWT_FIRST_REDEEM_MAX_AGE`, default 24h) bounds a token
  captured **before its first use** — without it a never-redeemed token could open
  an account at any point inside `exp`, which is the hole #829 closed.
- **T** (`OCTO_OIDC_BEARER_JWT_REDEEM_IDLE_WINDOW`, default 7d) bounds a token
  captured **after its owner stopped using it** — the logout case named in #829's
  comment. A client that keeps redeeming refreshes the window and is never
  refused, whatever its `iat`; the design therefore does not need to know whether
  the client redeems once or on every launch.

Both bounds are configurable, and both are far tighter than `exp`. Setting them
does not require knowing the client's habits; tightening to true one-shot
semantics later is a config change plus a decision, not a redesign.

Supporting changes:

- `VerifyForRedemption` no longer judges freshness and now returns
  `RedeemedBearerJWT{Claims, IssuedAt, ExpiresAt}`. The ledger needs `iat`/`exp`,
  and letting the handler re-parse the payload would be a second JWT parse — two
  readings of one token that can disagree.
- `bearerJWTMaxAge` is deleted. Both production callers now pass `maxAge=0`; a
  policy constant that production does not use is an invitation to wire it back
  onto some path and re-install the `iat` anchor. The pure function keeps the
  parameter and `ErrJWTTooOld`, and the value moved into the tests that exercise
  them.
- `oidc_exchange_jwt_redemption_total{outcome}` carries the six decisions; the
  terminal curve gains `redeem_refused`, separate from `token_rejected`.
- Refusals return the same `ErrOIDCExchangeTokenRejected` 401 as a bad signature.
  A distinct code would tell the caller "this token was valid once", which is
  what this endpoint's anti-enumeration rule exists to withhold.

## Learnings

- **A ceiling's anchor matters more than its value.** Every proposal that only
  argued about the number (10 minutes → 30 → an hour) kept the same defect,
  because `iat` cannot express "this redemption is suspicious". Moving the anchor
  to the redemption ended the false refusals outright and narrowed two of the
  three capture windows — before first use (`exp` → `F`) and after abandonment
  (`exp` → `T`). It did not narrow all three: a token captured while its owner
  is still redeeming stays usable for the rest of its `exp`, where the old
  ceiling would have refused it after ten minutes. That widening is the trade
  this design makes knowingly, and the honest claim is "the goals stopped
  competing on two of the three", not on all of them.

- **A sliding window cannot be expressed by the record's TTL.** The first design
  set `TTL = T` and let expiry mean "abandoned". It is backwards: an expired key
  is indistinguishable from a never-seen token, so the record's death would
  *readmit* the token as a first redemption. The record has to outlive the window
  — TTL is the token's own remaining life — and idleness is computed from the
  stored `last_at`. The bug was invisible in the decision table and obvious the
  moment the absent-record branch was written out.

- **A count cap looked like defence and was not.** An earlier draft also capped
  redemptions per token at N. An attacker needs one redemption, not twenty, so N
  stopped nothing, while a client restarting more than N times a day would hit
  the same indistinguishable 401 this task exists to remove. It survives as a
  metric (`admit_repeat`) instead of a gate — though only as a negative signal:
  the ledger stores `last_at` alone, so the curve cannot separate a client
  reusing its token from someone replaying one.

- **Verification and redemption had to stay separate.** `api_exchange.go` calls
  the same verify method to *classify* a credential ("is this a business JWT
  posted to the wrong endpoint"). Had the ledger lived inside the verifier, a
  token delivered to the wrong endpoint would have written or refreshed a ledger
  record — granting it an idle window it never earned. Verification stays
  side-effect-free; the side effect belongs to the one caller that redeems.

- **The degraded path chooses which promise to keep.** With Redis down we cannot
  tell a first redemption from a repeat, so the fallback applies F alone: still
  bounded, never "anything within `exp`", and not a self-inflicted login outage
  either. `degraded_admit`/`degraded_reject` are pre-warmed so the alert can be
  written before the first outage.

## Review round

A review pass found four defects in the first cut and three stale comments:

- **Sub-second bounds truncated to zero.** `normalized()` accepted any positive
  duration, but the Lua script compares whole seconds, so `F=500ms` reached it as
  `0` and the script evaluated `now - iat > 0` — every redemption refused, which
  is the `F=0` outage `normalized()` exists to prevent, entering by another door.
  Bounds are now truncated to whole seconds with a 1s floor, which also removes a
  real divergence: the degraded path compares Durations in Go, the ledger compares
  seconds in Lua, and they now compare the same value.
- **`T` could exceed the record's lifetime.** A record cannot outlive
  `redemptionRecordMaxTTL`, so a longer idle window was unenforceable *and*
  misreported: the late redemption came back as `reject_stale_first`, pointing an
  operator at `F`. `T` is now capped where it is read, so the startup log prints
  the value that actually applies.
- **A defensive branch that could never run.** `admitRedemption` refuses a nil
  credential, but the handler logged `rj.IssuedAt` unconditionally in the refusal
  path — the nil would have panicked one line earlier, on an unauthenticated
  endpoint. The log is now conditional.
- **`Close()` wrote a field the request path reads.** Niling `redeemLedger`
  during graceful shutdown races in-flight handlers on an interface value. The
  closable client is now held in a separate field; `Close()` only touches that,
  and a closed client simply makes `Admit` fail into the degraded path.
- Stale comments: `bearerJWTClaims.Iat` still said "not consumed, kept for
  debugging" while being the input to `F`; `modules/integration`'s test still
  documented the deleted ten-minute ceiling as current; and the `admit_repeat`
  comment overclaimed — the ledger stores only `last_at`, so that curve cannot
  separate a client reusing its token from someone replaying one, and it is
  honest only as a negative signal.
- The Redis integration test salted its digests with a nanosecond timestamp,
  which made its own `del()` calls dead code and left ~4 keys per CI run in the
  shared test Redis for 15 days. Keys used are now registered and deleted in
  `t.Cleanup`.

Two findings were not acted on, deliberately:

- **"Losing a ledger record is indistinguishable from never having redeemed."**
  True, and the chosen direction: the ledger *is* this path's security state, and
  requiring re-authentication after losing it is the safe side. The finding's
  stated contrast — that the Redis *error* path admits the same token — does not
  hold: `admitWithoutLedger` refuses anything past `F` too, so both paths agree.
  The behaviour is now documented at the top of the file, together with the
  operational signal (a sudden batch of `reject_stale_first` means Redis lost
  data, not that clients changed).
- **"A repeat redemption is never re-checked against `F`."** Correct, and it is
  the trade this design makes: an actively-used token stays usable until it idles
  out or its `exp` passes. Re-applying `F` to repeats would refuse exactly the
  long-lived reuse `T` exists to permit. Closing it means one-shot redemption
  (`N=1`), which needs a fact about the client that the repo does not record.

## Learnings staged for promotion

- `learnings/pending/a-sliding-window-is-not-a-record-ttl.md` — when absence of a
  record already means "never seen", letting the record expire with the window
  makes expiry readmit exactly what the window excluded.
- `learnings/pending/validate-in-the-representation-the-consumer-uses.md` — a
  bound checked as a `time.Duration` and consumed as whole seconds passed
  validation and arrived as the value validation existed to forbid.

## Second review round (PR #843)

Approved, with five P2s. Four are folded in; the fifth is answered rather than changed.

- **`F` was uncapped while `T` was capped, and the asymmetry defeats `T`.** A
  record cannot outlive `redemptionRecordMaxTTL`, so with `F` longer than that,
  every post-eviction redemption looks like a first one and passes `F` — a 60-day
  `F` silently waves through a 7-day idle window. Both bounds are now capped by
  the record's lifetime. The cap on `T` was argued for in the file already; the
  same argument applied to `F` and nobody had turned it around.
- **The degraded bound is now `min(F, T)`.** `T < F` is a legitimate
  configuration ("first use may be late, reuse must be frequent"), and applying
  `F` alone made a Redis outage *looser* than normal operation — the one thing
  the degraded path is not allowed to be. On the defaults (`F=24h`, `T=7d`)
  nothing changes.
- **"Ledger not configured" is no longer reported as "Redis is flapping".** Both
  states fell into the same `degraded_*` labels, so a wiring regression — a
  permanent, non-self-healing downgrade where `T` never applies — would read on a
  dashboard as transient Redis trouble. New `unconfigured_*` labels plus a log
  line separate them, and a `New()`-level test now asserts the ledger is wired
  whenever `/exchange-jwt` is mounted. Every handler test injects a double, so
  without that test the constructor could be dropped with the suite still green —
  the exact failure `new_wiring_integration_test.go` was written for.
- **The headline regression was not pinned numerically.** The 36-minute test
  stubs the ledger, so it proves the handler applies no `iat` ceiling — not that
  the shipped default admits a 36-minute-old token. Reverting `defaultFirstRedeemMaxAge`
  to ten minutes would have reinstated the production bug with a green suite.
  There is now a floor assertion on the default.
- **`Admit` normalises its own policy.** It trusted its constructor; the
  sub-second defect fixed one round earlier was exactly a value reaching the
  script un-normalised.

Two claims were softened rather than defended: the two paths use the same
*bound* but not the same comparison resolution (whole seconds in Lua,
`time.Duration` in Go), and the anti-enumeration test pins the response, not the
timing — a refusal by the ledger costs a Redis round trip that a bad signature
does not. Neither is worth code; both were worth saying plainly where the test
name would otherwise overstate.
