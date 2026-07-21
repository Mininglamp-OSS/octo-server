---
type: Learning
title: "A durable queue + a per-process in-memory config table can dead-letter valid work across a restart"
description: When work outlives the process but routing/config is built once from env at startup, a config-divergent restart makes the consumer reject valid queued work; treat 'config absent' as a bounded retry, not a first-attempt DLQ.
tags: ["reliability", "dispatch", "queue", "dlq", "config", "review"]
timestamp: 2026-07-20T16:30:00+08:00
# --- octospec extension fields ---
source: self
origin_task: route-missing-retry
origin_pr: Mininglamp-OSS/octo-server (branch claude/docs-access-request-allow-no-response-69whvw)
status: pending
candidate_rule: dispatch-reliability
---

# A durable queue + a per-process in-memory config table can dead-letter valid work across a restart

## Context
`internal/cardactiondispatch` persists card-action events in a durable Redis queue but
resolves the destination route from a per-process in-memory registry built once at startup
from `OCTO_CARD_ACTION_ROUTES`. An event is only ever enqueued when the route is present
(enqueue-time `Resolve` gate), yet the dispatcher rejected it with `route_missing` — because
between enqueue and dispatch the process restarted into a run whose env had not (yet) loaded
the route. The queue outlived the process; the config did not. The miss was then treated as a
**permanent** error (`retry=false` → immediate DLQ), so a decision whose route existed moments
later was lost with no self-heal — surfacing as "the button does nothing."

This looked, in turn, like a grant failure, then a 401 secret mismatch, then a cross-pod
divergence — all wrong. The DLQ reason field (`route_missing`) plus the "single replica" fact
were what finally pinned it: same process, so enqueue-with-route and dispatch-without-route can
only happen across a restart, carried by the durable queue.

## Rule of thumb
When a durable/shared work queue is consumed against config that is loaded **per process at
startup** (env, in-memory registry, feature flags):

1. **A "config/route not found" at consume time is usually TRANSIENT, not permanent** — the
   item was accepted when the config *was* present, and a rollout / restart / not-yet-loaded
   window is the common cause. Let it ride out the window (defer / re-check) so it processes
   once the config returns; do not dead-letter on the first miss.
2. **Wait WITHOUT spending the delivery-attempt budget.** Model "config not ready yet" as a
   *defer* (no attempt consumed), not a *failed attempt* (a nack/retry). If waiting shares the
   same attempt counter as real delivery, then once the config returns the item can be at (or
   past) its max-attempts and get dead-lettered as `attempts_exhausted` the moment it became
   processable — the opposite of self-healing. (This repo's first cut nacked with a bounded
   backoff and an xhigh code review caught exactly this; the fix was to defer instead.)
3. **Keep the DLQ as a real signal**: still dead-letter after a bounded window so a genuinely
   removed/misconfigured route stays visible and alertable — just later, not on the first miss.
4. **Delivery outcomes are diagnosable only if the failure reason is recorded** on the item
   (here: the DLQ `reason` field). Preserve it verbatim.
5. **A per-process config table + a cross-process durable queue is a divergence source by
   construction** — enqueue and dispatch can run under different config even on one replica
   (across a restart) and trivially across replicas/rollouts. Design the consumer to tolerate it.
6. **Anchor a bounded "wait it out" on when the WAIT started, recorded per item — not on a
   business timestamp like acted-at.** The window here bounds "how long we wait on a missing
   route." Measuring it from `Event.ActedAt` (when the user acted) is wrong: an item can be far
   older than the window for reasons unrelated to the wait — queue backlog, an outage, redelivery
   — so on its FIRST miss `now - ActedAt` already exceeds the window and it dead-letters with zero
   self-heal, exactly in the long-outage/rollout case the wait was meant to cover. And a missing or
   zero acted-at has nothing to measure against at all (a permanent-defer wedge). The robust design
   is a durable per-item marker written when the wait first begins (first observed miss) and read on
   every re-check; the window is `now - firstSeen`. That needs no business timestamp and no
   special-case for legacy/zero values.
7. **A per-item marker in a shared structure needs explicit lifecycle cleanup — a whole-key TTL is
   not per-field GC.** The first-miss marker lived as a field in one Redis hash with a hash-level
   `PEXPIRE`. Redis cannot expire individual fields, and every new miss refreshed the whole-hash
   TTL, so under sustained traffic the key never expired and a field-per-item leaked for every
   completed event. Delete the marker on EVERY exit transition (success, terminal dead-letter,
   replay); keep the TTL only as a backstop that reaps the key once activity stops. (PR #621 walked
   this exact path across review rounds: acted-at anchor → zero-acted-at special-case → first-miss
   marker → marker-lifecycle cleanup, each step caught by a reviewer.)
