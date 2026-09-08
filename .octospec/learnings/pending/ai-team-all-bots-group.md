---
type: Learning
title: "Retry the complete idempotent projection after transactional lock conflicts"
description: A unique key guarantees one durable projection identity but concurrent multi-transaction reconciliation can still deadlock; retry the complete idempotent orchestration on MySQL 1213/1205 only.
tags: ["database", "mysql", "concurrency", "idempotency", "testing"]
timestamp: 2026-09-08T19:20:00+08:00
# --- octospec extension fields ---
source: self
origin_task: ai-team-all-bots-group
status: pending
candidate_rule: testing
---

# Retry the complete idempotent projection after transactional lock conflicts

## Context

Concurrent first activation of one Agent correctly converged to one Agent row,
one private group, and one team group through unique constraints and row locks,
but one caller could still receive MySQL 1213 while transactions crossed the
Agent, group, and member tables.

## Rule of thumb

When an operation is composed of multiple independently committed, idempotent
reconciliation steps, retry the complete orchestration from its first authority
check after MySQL 1213 or 1205. Do not retry arbitrary errors, do not retry only
the statement that lost its transaction, and keep the attempt budget small.

Pair this with a real-MySQL concurrent test that asserts both caller success and
the cardinality of every durable identity. A unique-row assertion alone can pass
while several callers still fail.
