---
type: Learning
title: "Fail-closed runtime systems must preserve a bounded diagnostic read path"
description: Readiness may block unsafe data and state transitions, but authenticated detail and audit reads must remain available to explain and recover the failure.
tags: [fail-close, readiness, diagnostics, observability, runtime-catalog, error-handling]
timestamp: 2026-07-29T07:20:03+08:00
# --- octospec extension fields ---
source: self
origin_task: cardtmpl-runtime-catalog-overlay
origin_issue: 672
status: pending
candidate_rule: error-handling
---

# Fail-closed runtime systems must preserve a bounded diagnostic read path

## Context

The runtime catalog correctly poisons its own serving state when startup proves
a persistent integrity violation. The first implementation neither propagated
that state into `/v1/ready` nor separated it from manager diagnostics. As a
result, a poisoned process stayed in normal traffic rotation while the only safe
APIs that could explain the invalid active pointer or audit history were
unavailable precisely during the incident they served.

## Rule of thumb

Separate endpoints into two classes when introducing readiness:

1. Runtime data access and state mutation remain fail-closed until readiness is
   established. Do not fall back to stale local state. Propagate that closure to
   the process readiness probe so normal traffic is removed from the replica;
   keep liveness dependency-free so the process remains observable.
2. Authenticated, read-only diagnostics bypass the runtime readiness gate, but
   keep authorization, pagination/size bounds, response redaction, and a
   server-owned dependency deadline.
3. A diagnostic DB failure is still unavailable; bypassing readiness is not a
   bypass of database errors or integrity checks in the read itself.
4. Test pending and integrity-poisoned states explicitly. A normal-ready test
   cannot prove that incident diagnostics survive the failure transition.

## Why worth a rule

"Keep the process up" is not operational recovery by itself. Without a safe
read path, operators are pushed toward direct SQL, guesswork, or blind rollback.
Preserving bounded diagnostics improves recovery without weakening fail-closed
runtime behavior.
