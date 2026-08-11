---
type: Learning
title: Scope revocation cleanup to the revoked generation
description: Monotonic credential invalidation does not make a shared session index safe from stale-event cleanup; cleanup must carry generation ownership.
tags: ["security", "redis", "concurrency", "revocation", "idempotency"]
timestamp: 2026-08-10T01:17:49+08:00
# --- octospec extension fields ---
source: self
origin_task: token-lifecycle-hardening-pr2
status: pending
candidate_rule: distributed-state
---

# Scope revocation cleanup to the revoked generation

## Context

Rotating a per-user generation immediately invalidates old bearers and makes an
event replay idempotent for authentication. A separate per-user session index
was still deleted unconditionally after rotation. An exact replay after a new
login therefore left the new bearer valid but unindexed, bypassing the session
cap; the first event also raced a new-generation issuer in the same way.

## Rule of thumb

When a monotonic event updates one security authority and later cleans an
auxiliary shared collection:

1. Record the exact old epoch/generation replaced by the event in the atomic
   authority update.
2. Partition or tag auxiliary state by that epoch.
3. On exact replay, retry cleanup only for the recorded old epoch.
4. On an older out-of-order event, do not clean current auxiliary state.
5. Test both validity and secondary invariants such as caps, enumeration, and
   device targeting after a post-event login.

## Why worth a rule

Checking only that the post-event bearer still authenticates misses the defect:
the damage is to capacity and future revocation behavior. The pattern applies to
distributed indexes, leases, caches, and outboxes whenever a primary monotonic
state change is followed by non-transactional cleanup.
