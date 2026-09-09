---
type: Learning
title: "External readiness needs an idempotent reconciliation path"
description: A local ready bit records a completed attempt, not the continued existence of externally managed state; an explicit retry endpoint must replay idempotent external upserts.
tags: [idempotency, reconciliation, distributed-state, recovery]
timestamp: 2026-09-08T00:00:00+08:00
# --- octospec extension fields ---
source: self
origin_task: assistant-eager-ai-group
status: pending
candidate_rule: testing
---

# External readiness needs an idempotent reconciliation path

## Context

AI Team stores `container_state=ready` after creating its WuKongIM parent and
topic channels. That state proves the last provisioning attempt succeeded, but
cannot prove that the external channels still exist later. A retry that skips
external work solely because the database says `ready` cannot repair external
state loss.

## Learning

When an API is explicitly documented as create-or-repair across a database and
an external system, a local ready bit may optimize background work but must not
short-circuit the explicit repair operation. Replay the external system's
idempotent upsert, return its failure to the caller, and preserve enough durable
local identity for the next request to retry the same object rather than create
a duplicate.

Tests should cover both halves: stable local identity across retries, and an
observed external upsert even when the local row was already present.
