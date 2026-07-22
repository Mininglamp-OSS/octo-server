---
type: Learning
title: "Authoritative card updates need a monotonic card_seq source — reusing event IDs is an implicit contract, assert or annotate it"
description: CardMutator orders card updates by a CAS on card_seq — a later write must carry a strictly greater card_seq than the stored frame, or it is rejected as a conflict / collapsed as a replay. The docs finalizer sources that card_seq from event.EventID. This is correct only while event IDs are monotonically increasing; nothing in the type system enforces it. Any CardUpdater caller that derives card_seq from an external ID inherits a silent-corruption risk if that ID generator ever becomes non-monotonic. Make the monotonicity requirement explicit at the call site.
tags: ["cardtmpl", "card-update", "wire-contract", "trust-boundary", "cas", "idempotency"]
timestamp: 2026-07-22T16:45:00+08:00
# --- octospec extension fields ---
source: self
origin_task: cardtmpl-interaction-closure
origin_pr: self
status: pending
candidate_rule: card-update
---

# card_seq must come from a monotonic source

## Context

`carddispatch.CardMutator` serializes competing card updates with a CAS on
`card_seq`: a write whose `card_seq` is not strictly greater than the stored
frame's `card_seq` is either rejected (`conflict`) or, when the frame hashes
identically, collapsed into an idempotent replay. This is the whole
correctness story for concurrent/retried updates — no duplicate revision, no
duplicate CMD.

`CardUpdater.ReplaceView` / `Append` take the `card_seq` from the caller. The
docs finalizer (`modules/notify/action_finalizer.go`) passes
`CardSeq: event.EventID`.

## The trap

`event.EventID` works as a `card_seq` **only because the durable event ID is
monotonically increasing** under the current snowflake-style generator. That
property is load-bearing but invisible:

- Nothing in `UpdateTarget` or `CardMutator` declares "this field must be
  monotonic." It is just an `int64`.
- If the event-ID generator is ever swapped (per-shard counters, UUID→hash,
  clock-rollback under NTP correction, a test double that returns a constant),
  the CAS silently misbehaves: a legitimate later update carrying a *smaller*
  ID looks like a stale conflict and is dropped; two updates with the same ID
  look like a replay and the second is swallowed. No error, no log, no metric
  — the terminal card just fails to appear.

## What to do

1. **At every `CardUpdater` call site, document why the chosen `card_seq`
   source is monotonic.** For the docs finalizer: "event IDs are
   monotonic snowflakes; used directly as card_seq." One comment closes the
   gap for the next reader.
2. **Prefer a source whose monotonicity is a stated invariant** (message
   sequence, a dedicated per-message revision counter) over an ambient ID that
   happens to increase today.
3. **Consider asserting it in `CardMutator`** when cheap — e.g. reject a
   `card_seq <= 0`, and where a prior frame is loaded, treat a *lower* incoming
   `card_seq` as a typed conflict (already done) but log at a level that would
   surface a systemic regression, not just a benign race.
4. **Never derive `card_seq` from a client-supplied or display field.** It is
   part of the authoritative write ordering, not presentation.

## Candidate rule promotion

Fits a `card-update` rule (invariants for authoritative card mutation:
authority re-derived from stored state, monotonic `card_seq`, mutator-composed
writes only). Discuss scope in the promotion PR — it may instead extend the
existing trust-boundary guidance rather than stand alone.
