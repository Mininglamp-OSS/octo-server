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
  to the redemption made both the false refusals and the real replay window
  smaller at once — the two goals stopped competing.

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
  metric (`admit_repeat`), where it answers a question — is the client reusing
  one token? — instead of enforcing one.

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
