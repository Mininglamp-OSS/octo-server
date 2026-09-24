---
type: Task
title: Durable reconciliation of business group and child-channel membership
description: Preserve business-to-IM intent through lost requests, lost replies and process failures without replaying stale removals after rejoin.
slug: im-business-reconcile
source: user
---

# Goal

Close the business boundary that an IM-internal durable queue cannot cover. Group and
child creation/membership transactions must leave durable reconciliation evidence;
recovery must converge to current authoritative membership without reviving old deltas.
The user authorized an end-to-end solution alongside the replacement IM throughput PR.
The implementation is accompanied by SQL, SDK/business and injected-failure tests.
See docs/im-business-reconciliation.md for exact evidence and rollout limits.

# Evidence

Against the isolated backend image 4442db4 and PR54-derived IM: replacing a successful
IM group-create reply with 503 causes the backend to delete its group/member rows while
IM retains three subscribers. Dropping a child-create request before forwarding causes
business HTTP400 while a committed child row remains without IM subscribers after15s.
Current main d723844 has the corresponding commit-before-call/compensating-delete paths.
The image and inspected main are different revisions; main must be rebuilt for acceptance.

# Load-bearing scope

- Group admission/removal/blacklist/status and child creation/deletion SQL transactions.
- Bounded durable group reconciliation intent and channel ownership/tombstones.
- Current membership snapshots: active non-blacklisted group members; children inherit
  parent membership, independently of thread_member notification preferences.
- Stable request identity plus receiver-enforced monotonically increasing generations.
- Honest completion: no success from leader admission alone; no lost error as done.
- Preserve auth, Space isolation, blacklist, disband, bot and child lifecycle behavior.

# Design constraints

Read the withdrawn im-pending-outbox brief before implementation. Failure-only enqueue
misses the crash window. A lease or check-before-send cannot fence a delayed old request.
Do not mix unfenced legacy membership deltas with a managed generation stream. Activation
requires all IM replicas to support the protocol; an old node must never silently apply
an unsupported command. Coalesce at group/channel level rather than per user×child.
Do not hold a MySQL transaction/row lock across network calls. Claim/checkpoint updates
must be indexed and CAS-protected. New intent must revive retry after an older failure.

# Acceptance

1. Lost successful create reply leaves no permanent IM orphan after compensation/restart.
2. Never-delivered child create and crash after SQL commit converge without user retry.
3. Failed kick, rejoin and late old kick preserve latest membership at parent and children.
4. Actual SDK ACK, delivery, denied send and no delivery after confirmed removal.
5. Multiple workers/lease expiry cannot publish stale state or clear newer work.
6. Repeated create and membership churn preserve bounded queue cardinality and resources.
7. Main-source runtime tests, focused/race tests, and appropriate cross-module checks.

# Excluded

Unrelated business features, message payload/protocol changes, rate-limit relaxation,
and reporting pending reconciliation as completed to improve a test's success count.
