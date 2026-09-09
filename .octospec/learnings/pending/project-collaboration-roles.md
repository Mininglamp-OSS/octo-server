---
type: Learning
title: "Page the base relation before filtering integrity violations"
description: A LIMIT applied after anomaly predicates bounds returned violations, not the number of healthy rows examined by a periodic integrity scan.
tags: ["database", "maintenance", "pagination", "performance", "review"]
timestamp: 2026-09-09T10:55:00+08:00
# --- octospec extension fields ---
source: self
origin_task: project-collaboration-roles
status: pending
candidate_rule: database
---

# Page the base relation before filtering integrity violations

## Context

A periodic collaboration-role integrity query initially joined the complete binding
table, filtered down to invalid rows, and then applied `LIMIT`. That bounded the number
of violations returned, but it did not bound work: on a healthy table, or whenever a
page contained fewer than the requested number of violations, MySQL still examined the
entire remaining keyspace on every maintenance tick and on every pod.

## Rule of thumb

For a recurring integrity scan whose anomalies are expected to be sparse:

1. Keyset-page the base table by a stable unique key in a derived table or CTE.
2. Join and evaluate integrity predicates only for those base rows.
3. Persist the last examined base key, including healthy rows, across ticks.
4. Accumulate the violation count privately and publish the gauge only after a full
   rotation, so a page-local count is not presented as a table-wide count.
5. Add a regression containing a valid row before an invalid row with `limit=1`; the
   first page must return the valid row as examined rather than scanning ahead to the
   violation.

`LIMIT` is a work bound only when it is applied before the selective anomaly filter.

## Why worth a rule

Healthy production data is exactly the state in which a post-filter `LIMIT` provides
the least protection. Recurring scans amplify that mistake by pod count and interval,
so the query can become an avoidable full-table tax without changing functional output.
