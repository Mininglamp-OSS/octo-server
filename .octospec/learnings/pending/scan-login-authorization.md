---
type: Learning
title: "Redis write errors have ambiguous outcomes"
description: A client-observed Redis error does not prove the write was absent, so compensation must preserve safety under both committed and uncommitted outcomes.
tags: [redis, auth, compensation, distributed-systems, client-contract]
timestamp: 2026-08-09T13:25:48+08:00
source: self
origin_task: scan-login-authorization
status: pending
candidate_rule: error-handling
---

# Redis write errors have ambiguous outcomes

## Context

Scan-login confirmation first promotes a pending authorization into a
redeemable record, then publishes the `authed` QR state. If the QR-state Redis
write reports an error, the server rolls the authorization back to pending.

The error does not prove that Redis rejected the write. A connection reset or
timeout can happen after Redis committed it, leaving the QR state as `authed`
while the authorization is pending again. Treating the error as proof of
non-commit would make the compensation model and client state machine incorrect.

## Rule of thumb

For a remote write followed by compensation:

1. Model both outcomes behind an error: not committed and committed but the
   response was lost.
2. Choose compensation that preserves the security invariant in both outcomes.
   Here, non-redeemability wins even if the visible QR state is temporarily stale.
3. Do not let a displayed terminal state be the authority for credential
   issuance; re-check the shared authorization record at redemption.
4. Document the recovery contract. A client receiving a failed or ambiguous
   confirmation/redemption must restart the complete flow rather than replaying
   a possibly consumed credential.
5. Add observability for partial completion without logging credentials or
   turning the safe denial into a fail-open retry path.

## Why worth a rule

The pattern is not Redis-specific: any networked store can commit a request and
lose its response. Authentication, payment, and job-claim flows are especially
likely to add compensation that is safe only under the uncommitted branch unless
this ambiguity is made explicit during design and review.

Promotion into `.octospec/rules/` requires a separate reviewed change.
