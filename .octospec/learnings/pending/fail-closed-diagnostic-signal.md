---
type: Learning
title: "Preserve diagnostics when fail-closed policy collapses errors"
description: A safe authorization denial must not erase the bounded signal that distinguishes an infrastructure failure from a clean negative result.
tags: ["authorization", "observability", "privacy", "error-handling"]
timestamp: 2026-07-24T09:13:44+08:00
# --- octospec extension fields ---
source: self
origin_task: bot-send-permission-error-classification
status: pending
candidate_rule: error-handling
---

# Preserve diagnostics when fail-closed policy collapses errors

## Context

The Bot OBO friend gate correctly failed closed when its grant lookup or
grantor-access re-check failed, but it converted the error to the same
`not_friend` result as a clean authorization denial. The underlying helper also
logged raw Bot, channel, and grantor identifiers. The client stayed safe, yet
operators lost the bounded failure classification and the log violated the
task's privacy boundary.

## Rule of thumb

When an authorization helper deliberately collapses an infrastructure failure
to a business denial:

1. Keep the outward denial and fail-closed behavior unless the public contract
   explicitly requires a retryable internal response.
2. Preserve a bounded internal diagnostic signal up to the request boundary.
3. Emit the terminal log and metric exactly once at that boundary, where the
   request trace ID is available.
4. Never use user, Bot, channel, group, Space, token, SQL argument, or raw error
   text as a metric label or log correlation key.
5. If the raw cause may contain identifiers or query arguments, replace it with
   a safe sentinel and rely on stage/reason telemetry for classification.

## Why worth a rule

Fail-closed authorization can be correct for security while still being
operationally opaque or privacy-unsafe. Separating the public denial from the
internal diagnostic signal preserves both properties and prevents future
incident triage from depending on short-lived container logs.

