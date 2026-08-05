---
type: Journal
title: "Journal: bot-api-per-bot-ratelimit (issue #696)"
description: Moves bot rate limiting off the client-IP axis onto bot identity, and gives the two self-heal channels — heartbeat and register — quotas of their own. The production incident was not a bot starving itself but a bot starved by a co-located neighbour sharing one per-IP bucket; the follow-on incident showed that protecting heartbeat alone still leaves the bot unable to get back up, because the reconnect path itself was rate limited. The three per-bot buckets are self-built so quotas are hot-tunable and can run in shadow mode; the two pre-auth IP floors mount octo-lib's strict middleware instead, because they are the price of the change rather than its goal. Every per-bot layer ships disabled by default.
tags: ["bot-api", "rate-limit", "throttle", "wire-contract", "observability", "testing"]
timestamp: 2026-08-05T08:00:00Z
# --- octospec extension fields ---
task: bot-api-per-bot-ratelimit
upstream: Mininglamp-OSS/octo-server#696
source: self
---

# Journal: bot-api-per-bot-ratelimit (issue #696)

## What was done

`/v1/bot` carried no per-endpoint and no per-bot limiter at all — the only
constraint was the global per-IP token bucket mounted via `route.Use` in
`main.go`. Because that bucket is keyed by client IP, **every bot sharing an
egress IP shares one quota**.

- **`pkg/ratelimit` (new leaf package)** — token bucket (Lua byte-for-byte
  identical to octo-lib's and `modules/incomingwebhook`'s), plus a `Limiter`
  whose rps/burst/enabled/dry-run are read through a `ParamsFunc` **on every
  decision** rather than captured at construction. Also `OffenderRecorder`: a
  structurally bounded top-N ZSet, the only place bot identity is retained.
- **`modules/bot_api/ratelimit.go`** — three independent channels:
  `business` (key = robotID), `heartbeat` (key = robotID),
  `register` (key = SHA-256 fingerprint of the bot token).
- **`main.go`** — `/v1/bot/heartbeat` added to `globalRateLimitExcludePaths()`,
  metrics registered, and the settings closure wired in.
- **`modules/common`** — 12 settings (DB → env → code default), all hot-tunable,
  each channel with its own `enabled` (default **false**) and `dry_run`
  (default **true**).
- **`pkg/accesslog`** — access log lines for bot-authenticated requests now carry
  `bot=<robotID>`. Zero-touch: `authBot` already wrote `CtxKeyRobotID`.

## Why the design landed where it did

**The reported cause was wrong, and that changed the fix.** The issue assumed
heartbeat shared a quota with business APIs. Sharing was real; the shared axis
was **IP**, not "business vs heartbeat". Log analysis found a *different* bot on
the same egress IP sustaining ~590 rps against `/v1/bot/typing`, which alone
exhausted the 500 rps per-IP bucket. The reporting bot was very likely not
starving itself — it was starved by a neighbour. A per-endpoint quota would not
have helped; only changing the axis does.

**Adding a per-bot bucket cannot rescue heartbeat on its own.** The global
limiter is mounted with `route.Use`, so it runs *before* `authBot` and **group
level middleware cannot bypass it**. Whatever quota the bot group gets, the
per-IP bucket rejects first. Hence heartbeat had to be added to the exclude
list — and, because excluding it removes its only ceiling, that had to be paired
with a bucket of its own. The two halves are not independently useful.

**Protecting heartbeat alone still leaves the bot unable to recover.** A second
incident the same day closed the loop: quota exhausted → heartbeat 429 →
heartbeat key (TTL 60s) expires → connection considered dead → client
reconnects → reconnect calls `POST /v1/bot/register` to refresh the IM token →
register sits behind the *same* per-IP bucket → 429 → the bot never gets back
up. Preserving the ability to notice you are down, without preserving the
ability to get up, ends in the same place. The general lesson: **every endpoint
that exists to recover from failure belongs in the protected set.**

**The three per-bot buckets are self-built; the two pre-auth IP floors are not.**
lib's middlewares fix rps/burst at construction, so changing a quota requires a
rolling restart — and the incident's own mitigation (500 → 1500) demonstrated the
cost: during the 93-second rollout, old and new replicas shared one Redis bucket
while passing different parameters to the Lua script, limiting behaviour
oscillated, and a bot was kicked inside that window. A per-bot quota is exactly
the kind of number that will be retuned from shadow data, so binding it to a
restart would clone that failure mode. `modules/incomingwebhook`'s per-webhook
bucket had already walked this path (three-tier settings, read per request,
read-side sanitisation), so this reuses a proven shape instead of a cross-repo
change to lib.

The two IP floors took the opposite decision, and the dividing line is worth
stating because it went through two reversals before settling: **is this layer
the task's goal, or the price of it?** Per-bot quotas are the goal. The floors
exist only because `register` has no identity to key on before `authBot`, and
because excluding heartbeat from the global bucket removes its only pre-auth
ceiling — they are the price. Self-building them would have bought hot tuning and
config uniformity at the cost of copying lib's unexported `getClientIP`, ~180
lines of new infrastructure, and **a second deviation from the `rate-limit`
rule**. An abuse threshold does not need data-driven convergence the way a
per-identity quota does, so they mount octo-lib's
`StrictIPRateLimitMiddleware` with env-backed parameters. Net effect: review has
exactly one rule deviation to weigh instead of two.

**Shadow mode instead of "measure first, then guess a threshold".** There was no
data to size the quotas: the reject path wrote *no log at all*, access logs had
no `robot_id`, and the cluster has no log collection. Rather than collect a
distribution and infer a limit, dry-run runs the real decision against a
candidate quota and records what *would* have been rejected — answering "who
does this value hurt" directly. This is also why dry-run must not emit
`X-RateLimit-*`: a well-behaved client would throttle itself and the traffic
under observation would stop being real.

## Gotchas worth remembering

- **`ResponseErrorL` pins the wire status to 400.** Rate limiting must return a
  real 429 — clients key their backoff off the status code. The first
  implementation looked completely correct and *was* limiting; it just answered
  `{"msg":"Too many requests...","status":400}`, which a client is entitled to
  read as "bad request, stop retrying". Only an integration test asserting the
  status code caught it. `ResponseErrorLWithStatus` is the right facade here,
  and per CLAUDE.md that choice needs maintainer sign-off.
- **`botActorUID()` sets `uid`, and `GetLoginUID()` reads `uid`.** Mounting it on
  the main group means any handler there calling `GetLoginUID()` silently
  receives a **robotID instead of a logged-in user**. All 7 call sites happen to
  live under `/v1/obo/*` today, so the current mount is safe — but that is a
  "true today" property, not a structural one, so a source guard now pins it.
- **Deriving a limiter key from an unauthenticated, client-supplied credential
  grows the keyspace.** The token fingerprint dimension is right for the failure
  it targets (an invalid-token bot retrying ~4 rps), but the bucket's Lua creates
  a key on first decision, so rotating tokens creates keys at the request rate.
  With production Redis running **without `maxmemory`** (no LRU eviction; OOM is
  an OS kill) that upgrades from "limit bypassed" to "memory can be exhausted".
  Fixed by a per-IP strict bucket **in front of** the token bucket — order is
  load-bearing, behind it the key already exists.
- **Shadow mode that skips the decision is worthless, and looks identical.**
  Both "limiter disabled" and "limiter enabled but nobody over quota" present the
  same way externally. `degraded` is a separate outcome from `allowed` for the
  same reason: Redis failure means the limiter is *not working*, and that must be
  visible rather than folded into the success counter.
- **An exemption moves the attack surface, and the pairing bucket must sit where
  the unauthenticated traffic actually arrives.** Excluding heartbeat from the
  global per-IP bucket was paired with a per-bot bucket — but that bucket was
  mounted *after* `authBot`, so an invalid token aborted in authentication and
  never reached it (and it ships disabled by default anyway). Net effect: the
  exemption opened the very DDoS surface it was supposed to keep closed, letting
  any invalid token drive unbounded Redis/DB lookups in the auth layer. The brief
  had written this constraint down explicitly; the implementation satisfied its
  letter (a bucket exists) and not its point (it must cover unauthenticated
  traffic). Fixed with a strict per-IP bucket *before* `authBot`, deliberately
  with no enable switch — the per-bot layer defaults to off for rollout, so a
  toggleable outer layer would leave a window with no protection at all.
- **A test whose assertion is weaker than its stated scope is worse than no
  test.** `TestHeartbeatBucketStillEnforcesLimit` claimed to verify "exclusion
  does not mean unlimited", but used a *valid* token — so it exercised only the
  post-authentication half and passed happily while the hole above was wide open.
  This shape appeared twice in this task (the other was a source guard that
  degraded to a vacuously-true assertion over an empty set). Both times the fix
  was the same: make the test prove it can fail — self-witness assertions for the
  guard, and a negative run (move the middleware, watch it go red) for the
  behaviour test.
- **Rollout windows are not atomic for limiter parameters.** Old and new replicas
  share the Redis bucket and each passes its own rps/burst to the script. Never
  sample verification data inside a rollout window; the transitional state is not
  the new configuration.
- **A test helper that pre-drains a token bucket must stop the clock, not just
  zero the tokens.** `drainIPBucket` wrote `tokens=0, ts=now`, and the bucket's Lua
  computes `delta = max(0, now - ts)` then `filled = min(burst, tokens +
  delta*rate)` — so the milliseconds between draining and issuing the request
  refill it. At 100 rps that needs 10 ms and the helper looked fine; at 500 rps it
  needs **2 ms**, less than the Redis and DB round trips in between, and the test
  started failing intermittently. **The race pre-existed the sizing change; raising
  the rate only widened the window enough to fire it** — which is the useful part,
  because a latent millisecond race in a test helper presents as "the limiter
  occasionally doesn't limit", the most misleading possible symptom. Fix: write
  `ts` into the future, so `delta` is pinned at 0 regardless of the configured
  rate.
- **The same token bucket, used as a quota and used as a floor, must be sized by
  different methods — and mixing them up produces an availability bug that looks
  like diligence.** Both IP floors were first sized at twice the measured
  single-IP peak (`register 10/50`, `heartbeat 100/300`), which is how you size a
  *quota*: hug the observed traffic and converge from shadow data. A *floor* only
  has to turn unbounded into bounded, so it wants an order of magnitude of
  headroom. Sized as quotas, both were too tight in ways that contradicted this
  change's own thesis: heartbeat had 2.2x headroom against a fleet whose
  aggregate heartbeat rate is ~97 rps (2903 bots, 60s key TTL) with roughly half
  of it behind one egress IP — one NAT consolidation from breaching, on the
  endpoint whose 429 *is* the incident. And `register` — never excluded from the
  global bucket, so its prior ceiling was the global 1500 rps — was tightened
  150x **on the self-heal path**, which during a fleet-wide reconnect (the only
  moment it matters) would drain 1450 bots at 10/s over ~140 seconds. Settled at
  `100/500` and `500/1500`: still an order of magnitude below the global bucket,
  still closing the keyspace-amplification hole (100 rps × ~40s TTL ≈ 4000 live
  keys vs ~60000), but no longer shaping legitimate traffic. The generalisable
  part: **what a floor blocks does not live near the measured volume, so sizing a
  floor from measured volume buys nothing and costs availability.** Worth noting
  how this surfaced: not from review of the limiter, but from the question "does
  this have breaking changes?" — which forces you to write down the prior ceiling
  next to the new one, and `1500 → 10` does not survive being written down.
- **Tests must not couple themselves to quota values.**
  `TestRegisterIPLimitBoundsKeyspaceGrowth` fired 200 requests to overrun a burst
  of 50; raising the burst to 500 turned it red although the behaviour under test
  had not changed. Rewritten to pre-drain and assert attribution via
  `X-RateLimit-Scope`, it now verifies the claim that actually matters — the floor
  is in `register`'s chain and rotating tokens cannot get around it —
  independently of how the floor is sized.
- **`CleanAllTables` does not clear Redis.** Three keyspaces need resetting in
  test setup (`ratelimit:bot:*` plus `ratelimit:strict:bot_register:*` and
  `ratelimit:strict:bot_heartbeat:*` — the latter two belong to the pre-auth IP
  floors, whose prefix octo-lib fixes as `ratelimit:strict:{tag}:`). Missing one
  produces cross-test pollution whose failures land on assertions that have
  nothing to do with rate limiting. The count grew from two to three when the P1
  fix added the heartbeat floor, which is the point: **the reset list is coupled
  to the middleware list and there is nothing that fails when they drift.**
