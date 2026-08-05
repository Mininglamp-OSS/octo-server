---
type: Journal
title: "Journal: bot-api-per-bot-ratelimit (issue #696)"
description: Moves bot rate limiting off the client-IP axis onto bot identity, and gives the two self-heal channels — heartbeat and register — quotas of their own. The production incident was not a bot starving itself but a bot starved by a co-located neighbour sharing one per-IP bucket; the follow-on incident showed that protecting heartbeat alone still leaves the bot unable to get back up, because the reconnect path itself was rate limited. Buckets are self-built so quotas are hot-tunable and can run in shadow mode; every layer ships disabled by default.
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

**Buckets are self-built rather than octo-lib's.** lib's middlewares fix
rps/burst at construction, so changing a quota requires a rolling restart — and
the incident's own mitigation (500 → 1500) demonstrated the cost: during the
93-second rollout, old and new replicas shared one Redis bucket while passing
different parameters to the Lua script, limiting behaviour oscillated, and a bot
was kicked inside that window. Any new bucket that could only be retuned by
restart would clone that failure mode. `modules/incomingwebhook`'s per-webhook
bucket had already walked this path (three-tier settings, read per request,
read-side sanitisation), so this reuses a proven shape instead of a cross-repo
change to lib.

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
- **Rollout windows are not atomic for limiter parameters.** Old and new replicas
  share the Redis bucket and each passes its own rps/burst to the script. Never
  sample verification data inside a rollout window; the transitional state is not
  the new configuration.
- **`CleanAllTables` does not clear Redis.** Two keyspaces need resetting in test
  setup (`ratelimit:bot:*` and `ratelimit:strict:bot_register:*`). Missing the
  second one produces cross-test pollution whose failures land on assertions that
  have nothing to do with rate limiting.
