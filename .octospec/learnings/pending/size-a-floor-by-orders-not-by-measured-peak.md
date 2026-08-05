---
type: Learning
title: "Size a limiter floor by orders of magnitude, not by a multiple of the measured peak"
description: A quota and an abuse floor can be the same token bucket but must be sized by different methods. A quota hugs measured traffic and converges from shadow data; a floor only has to turn unbounded into bounded, so it needs an order of magnitude of headroom. Sizing a floor at 2x the measured peak looks rigorous and produces an availability bug, because what a floor blocks does not live near the measured volume.
tags: ["rate-limit", "throttle", "availability", "capacity", "review"]
timestamp: 2026-08-05T10:00:00Z
# --- octospec extension fields ---
source: self
origin_task: bot-api-per-bot-ratelimit
origin_pr: Mininglamp-OSS/octo-server#696
status: pending
candidate_rule: rate-limit
---

# Size a limiter floor by orders of magnitude, not by a multiple of the measured peak

## Context

In #696 two pre-auth per-IP buckets were added as the *price* of a per-bot
limiting change (see [[when-to-self-build-a-limiter]] for that goal-versus-price
split). Both were first sized from production measurements with 2x headroom —
which reads like the responsible thing to do, and cites real data:

| bucket | measured single-IP peak | first sizing | prior ceiling |
|---|---|---|---|
| `bot_register` | ~4.4 rps | 10 rps / burst 50 | global bucket, 1500 rps |
| `bot_heartbeat` | ~45 rps | 100 rps / burst 300 | global bucket, 1500 rps |

Both were wrong, and the review question that exposed them was simply *"does this
have breaking changes?"* — because unlike the per-bot buckets (which ship
`enabled=false`), these two have no kill switch by design and take effect the
moment the process starts.

**`heartbeat`**: reverse-engineering the fleet from the limiter's own constants —
2903 active bots, heartbeat key TTL 60s, so a client must beat at least every 60s
and realistically every ~30s — puts *fleet-wide* heartbeat at ~97 rps. The
measured 45 rps on one IP therefore means roughly half the fleet shares an egress
IP. That is 2.2x headroom, one NAT consolidation or one fleet doubling from
breaching, **on the endpoint whose 429 was the original incident**.

**`register`**: at the time this number was chosen it was still in the global
bucket, so its real prior
ceiling was 1500 rps. 10 rps is a 150x tightening — applied to the *self-heal
path*. During a fleet-wide reconnect (the only moment the path matters) burst 50
admits 50 bots and the rest queue at 10/s: ~140 seconds for 1450 bots, against
clients already known to conflate 429 with 400.

(A later round excluded `register` from the global bucket as well. That does not
change the sizing conclusion — 100 was already the binding constraint of the two
— but it does mean the floor is now the path's *only* ceiling, which raises the
cost of getting it wrong rather than lowering it.)

## Rule of thumb

**Decide which of the two things your bucket is, then size it accordingly:**

| | quota | floor |
|---|---|---|
| answers | "what is this identity's fair share?" | "what stops unbounded from reaching the expensive thing?" |
| sizing | hug measured traffic; converge from shadow data | order-of-magnitude headroom over legitimate peak |
| when it trips | routinely, by design | only during abuse or a bug |
| kill switch | yes — needs staged rollout | often no, so it must not be tight |

**The load-bearing asymmetry: what a floor blocks does not live near the measured
volume.** The abuse it exists to stop is 10-100x normal, so moving the limit from
2x to 20x normal costs *nothing in protection* and buys all the headroom for
growth, traffic concentration, and thundering herds. Conversely a quota set 20x
above normal protects nobody. So "2x the measured peak" is the worst of both: too
loose to be a meaningful quota, too tight to be a safe floor.

**Two corollaries worth applying directly:**

1. **State the prior ceiling before choosing the new number.** The `register`
   error was invisible until someone wrote down "before: 1500, after: 10". A
   number sized against *measured traffic* rather than against *the ceiling it
   replaces* hides its own blast radius.
2. **A floor with no kill switch must be loose.** Not having a switch is often
   right — a toggleable outer layer leaves a window with no protection when the
   inner layer defaults to off. But it means the only rollback is an env change
   plus a rolling restart, and for limiter parameters a rollout window is itself
   a period of oscillating behaviour (old and new replicas share the Redis bucket
   and each passes its own parameters). Tightness and un-rollback-ability are a
   bad combination.

**Recovery paths get the loosest floors of all.** Any endpoint that exists to get
a client back up (`register`, token refresh, reconnect, resubscribe) sees its
*entire population arrive at once*, precisely when the system is already
degraded. Its burst must be sized for the herd, not the steady state — see
[[recovery-path-must-be-in-protected-set]], which establishes that these
endpoints belong in the protected set at all. This learning adds the sizing half:
being in the protected set does not help if the protection is sized for a quiet
Tuesday.

## Consequence for tests: re-sizing should not turn tests red

Correcting the two numbers broke two tests, and neither break was a real
regression — both were tests coupled to the old values:

1. **A test that overruns the bucket by volume encodes the burst.** "Fire 200
   requests, expect a 429" silently means "burst < 200". Assert the *property*
   (the floor is in this chain and cannot be bypassed) by pre-draining the bucket
   and checking attribution via `X-RateLimit-Scope`, so the test is independent of
   the configured quota.
2. **A helper that pre-drains a bucket must stop the clock, not just zero the
   tokens.** Writing `tokens=0, ts=now` leaves the bucket refilling during the
   interval before the request: with the token-bucket Lua's
   `delta = max(0, now - ts)`, that is 10 ms of grace at 100 rps and **2 ms at
   500 rps** — less than a Redis plus DB round trip. The race pre-existed; raising
   the rate merely widened the window enough to fire it, intermittently. Write
   `ts` into the *future* so `delta` is pinned at 0 at any rate. This one is worth
   remembering beyond re-sizing, because its symptom is "the limiter occasionally
   doesn't limit" — the most misleading way a rate-limit test can fail.

## Why worth a rule

The `rate-limit` rule tells you *which* middleware to mount but says nothing about
picking numbers, so every author reaches for "measured peak x2" — which is the
correct instinct for a quota and a latent outage for a floor. Both mistakes in
this task were made by someone who had just written a brief arguing that the
recovery path must never be throttled. Two lines in the rule (the quota/floor
distinction, and "state the prior ceiling in the PR") would have caught them at
authoring time rather than at review.
