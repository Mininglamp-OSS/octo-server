---
type: Learning
title: Validate the value that lands, not the first one you find
description: When a batch endpoint applies every item in order, a validator that stops at the first match for a key approves one value and persists another — returning success for a configuration nobody validated.
tags: ["validation", "system-setting", "batch", "review", "config"]
timestamp: 2026-09-03T00:00:00Z
status: pending
source: oidc-auto-join-initial-space
---

# Validate the value that lands, not the first one you find

## What happened

`POST /v1/manager/common/system_setting` takes a list of `{category, key, value}`
items and upserts each one in order. Nothing rejects a list naming the same key
twice, so for duplicates the **last** item is what ends up in the database.

A new validator for `space.oidc_initial_space_id` looped over the prepared plans,
checked the first entry matching that key, and `break`ed. Sending a valid
space_id followed by a bogus one returned `200` and stored the bogus one.

The same gap applied to normalisation: only the first duplicate was trimmed, so a
padded value could win the write and then miss on every lookup while reading as
correctly configured in the GET response.

Two neighbouring guards in the same handler — the onboarding space-welcome
combination and the thread-archive window ordering — were already correct,
because both collect incoming items into a map before validating, which
naturally keeps the last value. The new code introduced the inconsistency.

## Why it was easy to miss

The single-item case, which is all a well-behaved admin console ever sends, works
perfectly. Every hand-written test used one item per key. The bug needs a caller
that duplicates a key, which no UI does — so it would have surfaced as an
unreproducible "the setting didn't take" report, long after the write.

It was also invisible downstream **by design**: this particular setting is
consumed asynchronously on a later account creation, where failure must not break
a login, so the only symptom of a bad value is a warn log and a metric label.
A validator is the last place the operator can be told anything at all.

## The rule

When validating a batch write, first reduce the batch to the state it will
actually produce — the merged, last-wins value per key — and validate *that*.
Never validate a single item in isolation and stop.

Two follow-on habits from the same reasoning:

- **Normalise every occurrence, not just the one you validated**, or the winning
  duplicate can be the un-normalised one.
- **A rejected write must persist nothing.** Assert it: a partial write that
  keeps the "good" item is worse than a clean failure, because it looks like it
  worked.

## Test that would have caught it

Two items naming the same key in one request, the second one invalid; assert the
request is refused **and** that nothing was stored — not even the valid first
item.
