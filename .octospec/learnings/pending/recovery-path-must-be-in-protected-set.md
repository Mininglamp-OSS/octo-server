---
type: Learning
title: "Every endpoint that exists to recover from failure must be in the protected set"
description: Protecting a liveness signal without protecting the recovery path it feeds ends in the same outage — the client can notice it is down but never get back up.
tags: ["rate-limit", "throttle", "availability", "resilience", "review"]
timestamp: 2026-08-05T08:10:00Z
# --- octospec extension fields ---
source: self
origin_task: bot-api-per-bot-ratelimit
origin_pr: Mininglamp-OSS/octo-server#696
status: pending
candidate_rule: rate-limit
---

# Every endpoint that exists to recover from failure must be in the protected set

## Context

Issue #696: a bot went offline for ~40 minutes because the global per-IP rate
limit bucket had been exhausted by a *different* bot on the same egress IP.
The fix — reserve quota for `/v1/bot/heartbeat` so liveness cannot be starved by
business traffic — was designed, specified, and reviewed.

It was not enough. A second incident the same day traced the full chain:

```
quota exhausted → heartbeat 429 → heartbeat key (TTL 60s) expires
  → connection considered dead → client reconnects
  → reconnect calls POST /v1/bot/register to refresh the IM token
  → register sits behind the same bucket → 429
  → the bot never gets back up
```

Heartbeat was protected. Register was not. **Preserving the ability to notice you
are down, without preserving the ability to get up, ends in the same outage** —
the observable symptom (bot offline, cannot self-recover) is identical.

The original brief listed heartbeat as the protected channel and never asked what
the client does *after* heartbeat fails. That question is the whole rule.

## Rule of thumb

When reserving quota / carving an exemption / adding a circuit breaker, do not
enumerate "the endpoints that must not be throttled". Instead **walk the client's
recovery path** and protect all of it:

1. **Follow the failure to its conclusion.** After this call fails, what does the
   client do? Retry? Re-authenticate? Reconnect? Re-register? Each of those is an
   endpoint, and each one being throttled turns a transient failure into a
   permanent one.
2. **Watch for the auth boundary.** Recovery endpoints frequently sit *before*
   authentication (they exist to obtain or refresh credentials), so the identity
   dimension used for the rest of the surface is unavailable there. Plan a
   different dimension rather than discovering the gap in production — but note
   that a dimension derived from an unauthenticated, client-supplied credential is
   rotatable, so it needs an IP-level bucket in front of it.
3. **Exemption is not protection.** Removing an endpoint from a bucket also
   removes its only ceiling. Every exemption must be paired with a quota of its
   own; the two halves are not independently useful.
4. **State the recovery path in the spec.** If a brief names a protected endpoint
   without naming what the client calls after that endpoint fails, the protected
   set is probably incomplete.

## Why worth a rule

Both incidents had the same root shape and the second was entirely predictable
from the first — the gap survived a written brief and a review pass because
everyone was looking at "which endpoint got 429" rather than "can the client
recover". It generalises well beyond rate limiting: the same reasoning applies to
circuit breakers, maintenance-mode allowlists, degraded-mode read paths, and any
kill switch that might disable the path used to turn itself back on.
