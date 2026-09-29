---
type: Learning
title: "Check the caller can present a credential before making it mandatory"
description: A service-to-service auth check is only enforceable if the calling peer actually sends the credential. Read the peer's client code at the deployed version before turning a check fail-closed; otherwise the "secure default" silently stops the integration.
tags: ["auth", "webhook", "trust-boundary", "config", "wire-contract"]
timestamp: 2026-09-29T00:00:00Z
# --- octospec extension fields ---
source: self
origin_task: webhook-grpc-hardening
status: pending
candidate_rule: trust-boundary
---

# Check the caller can present a credential before making it mandatory

## Context

The webhook gRPC listener already had a token interceptor, enabled when
`TS_GRPC_AUTH_TOKEN` is set. The obvious hardening was "require it". Reading
the IM's webhook client at the pinned version (`v2.2.4-20260313`) showed it
sends no metadata at all and has no config field for a token — so any
configured token, mandatory or not, rejects every callback. A local run with
the IM pointed at the listener confirmed it.

## Rule of thumb

- Before making a service-to-service check fail closed, find the **caller's**
  client code at the version you actually deploy and confirm it can send the
  credential (and has somewhere to configure it).
- If it cannot, ship enforcement as an explicit opt-in that fails closed only
  when chosen, document the incompatibility next to the setting, and track the
  caller-side change separately.
- Size structural caps from the caller's real batching behavior, not from a
  guess of "normal" traffic: a cap that rejects a legitimate batch is an outage
  that looks like a validation error.
