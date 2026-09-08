---
type: Learning
title: Lifecycle cleanup must synchronize and validate its authority
description: Completion evidence and target validation are both required around destructive asynchronous cleanup
tags: [testing, concurrency, async, cleanup, authorization, lifecycle]
timestamp: 2026-09-08T09:34:00+08:00
task: my-ai-team-sessions
source: self
---

# Synchronize asynchronous cleanup tests on completion evidence

For asynchronous job tests, do not treat a claim-time attempt counter as proof
that callbacks or downstream effects completed. Wait on evidence written after
the complete execution phase, and use atomics or channels for any callback state
observed across goroutines. Run the regression with both `-race` and shuffled
test order so the synchronization contract is executable.

Before a privileged endpoint invokes a cleanup helper that accepts a raw
identifier, first resolve the identifier as the expected entity type. Authorization
of the operator does not prove the target is a Bot rather than a human user. Cleanup
wrappers must also preserve intentional no-op outcomes from their shared primitive
(for example, creator membership cannot be removed) instead of turning them into a
misleading hard failure after earlier destructive steps have committed.

When one handler set is mounted on multiple authentication faces or resource
scopes, enforce protected-resource policy inside the shared authorization
funnel, after the caller's membership is established. This covers the whole
route family and avoids leaking the protected purpose to unaffiliated callers.

# Keep paired contracts aligned

When a product predicate hides rows from an enumeration, trace every aggregate
used beside or above that enumeration. A filtered list with an unfiltered count
creates impossible pages and misleading dashboards even though each query is
locally valid.

Durable idempotency keys also need an explicit terminal-state replay contract.
If deletion keeps the ledger row, detect its tombstoned resource and return a
stable conflict; do not let replay fall through to a lookup that necessarily
returns not found.

For JSON paging contracts, initialize empty result slices before ORM loading.
Many ORMs leave a nil destination unchanged on zero rows, turning `[]` into
`null` at the wire.
