---
type: Learning
title: "When to self-build a limiter instead of mounting the shared middleware"
description: The shared middleware fixes its quota at construction and is a process-wide singleton, so it cannot express per-identity quotas or hot tuning; self-building is legitimate there. The stop condition is not whether an unexported helper is in the way but whether the layer is the task's goal - paying new infrastructure for a layer that is merely the price widens the rule deviation for nothing.
tags: ["rate-limit", "throttle", "config", "review"]
timestamp: 2026-08-05T09:00:00Z
# --- octospec extension fields ---
source: self
origin_task: bot-api-per-bot-ratelimit
origin_pr: Mininglamp-OSS/octo-server#696
status: pending
candidate_rule: rate-limit
---

# When to self-build a limiter instead of mounting the shared middleware

## Context

The `rate-limit` rule reads:

> - Authenticated routes: mount `SharedUIDRateLimiter`.
> - Unauthenticated routes: mount `StrictIPRateLimitMiddleware`.
> - Never hand-roll a Redis counter for generic request-frequency limiting.
>
> **Exception (intentional)** — per-resource cooldowns keyed by a business
> identity (phone / email / bind-session) that the IP/UID buckets cannot express
> may use a hand-written Redis counter.

In #696 the bot API needed a per-bot quota. `SharedUIDRateLimiter` *can* express
the dimension (`botActorUID` writes `uid`), but:

1. It is a **process-wide singleton** (`uidRateLimitReady`), so bots and logged-in
   users share one `rps/burst`. Bot traffic is an order of magnitude above human
   traffic; retuning the singleton would loosen every user route at once.
2. Its quota is **captured at construction**, so changing it needs a rolling
   restart. That is exactly what made the incident's own mitigation expensive:
   during the 93-second rollout, old and new replicas shared one Redis bucket
   while passing different parameters, and a bot was kicked inside that window.

The Exception's examples are all "per-resource cooldown" shaped (phone, email,
bind-session), so it took a paragraph of argument to establish that a per-bot
quota lands inside it. The next person will have to write that paragraph again.

## Rule of thumb

**Self-building is legitimate when the shared middleware cannot express what you
need** — and there are exactly two such cases so far:

1. **A quota dimension it does not key on**, or a dimension it keys on but whose
   value it cannot separate (process-wide singleton ⇒ one quota for unrelated
   populations).
2. **Runtime tunability**, when the quota will need to be retuned from data
   after launch. If the answer to "what should this number be" is "we will find
   out from production", a restart-only knob clones the rollout-oscillation
   failure mode.

**When you do self-build, reuse the same token-bucket semantics** (the Lua in
octo-lib `pkg/wkhttp/ratelimit.go`, already duplicated verbatim in
`modules/incomingwebhook`). Two different meanings of "1 second" inside one
system is worse than the duplication.

**Then ask whether the layer you are building is the task's goal or its price.**
This task's goal was per-bot quotas. The two pre-auth IP floors were not the goal —
they are the price of excluding heartbeat from the global bucket, and of `register`
having no identity to key on. They went through two reversals:

1. Library middleware + env, on the grounds that self-building would need a copy of
   the unexported `getClientIP`, and two drifting copies would "compute different
   keys for the same request".
2. Self-built + settings-backed, once that reasoning was found wrong: the buckets
   use separate keyspaces and share no tokens, so drift only makes the new bucket
   shard imprecisely — bounded. Meanwhile env needs a rolling restart, which is the
   very oscillation that made the incident worse.
3. **Back to library middleware + env.** Not because (2) was technically wrong, but
   because of scope: self-building bought hot tuning plus config/observability
   uniformity, and cost a copy of unexported logic, **a deviation from this rule's
   second clause** (unauthenticated routes mount `StrictIPRateLimitMiddleware`), and
   ~180 lines of new infrastructure. **Introducing new infrastructure — and widening
   a rule deviation — for something that is the price rather than the goal does not
   pay.** An IP floor is an abuse threshold; it does not need the data-driven
   convergence a per-identity quota does.

So the stop condition is not "an unexported helper exists" (that is a cost to price)
but "is this layer the goal". Deviate where the goal requires it; take the shared
middleware everywhere else, so review has exactly one deviation to weigh.

Two details worth carrying even when you do self-build: reuse the same token-bucket
Lua, and watch for **inverted defaults at the seam** — `Check` treats an empty key as
fail-open (right for authenticated paths, where a missing identity means a mis-ordered
mount), but an IP floor must fail *closed*, so reusing it there needs a wrapper.

**Also**: `ParseRPSFromEnv` is `strconv.ParseFloat` underneath and accepts
`"NaN"`/`"+Inf"`, while `newKeyedLimiter` only rejects `rps <= 0` — and
`NaN <= 0` is false. So an env-backed quota **must** be sanitised by the caller,
or `NaN` reaches the Lua and every comparison inside it silently fails.

## Why worth a rule

The Exception clause is real but its examples do not cover the case that actually
came up, so each occurrence costs a fresh argument in review. Two sentences in the
rule — when self-building is allowed, and that the test is goal-versus-price
rather than helper visibility — would make it a decision instead of a debate. The `NaN` gap is worth one line on
its own: it is a live hole in the library's startup validation that every
env-backed caller inherits.
