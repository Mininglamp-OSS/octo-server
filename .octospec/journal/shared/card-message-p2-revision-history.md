---
type: Journal
title: "Journal: card-message-p2-revision-history (PR-C, card message P2 D10)"
description: Record of PR-C — the D10 card revision history side table, query API, transient flag, bot clear+tombstone, and revoke cleanup, stacked on PR-B; includes two P1 fixes from verify (query-side withdraw gate + revoke-cleanup ordering).
tags: ["message", "card", "wire-contract", "bot-api", "rate-limit", "space", "isolation", "error-response", "i18n"]
timestamp: 2026-07-08T11:30:00Z
# --- octospec extension fields ---
task: card-message-p2-revision-history
upstream: self
source: self
---

# Journal: card-message-p2-revision-history (PR-C, card message P2 D10)

## What was done

Shipped D10 of the `card-message-interaction` contract — a queryable card
revision history that the D6 latest-frame-only `content_edit` model cannot
answer on its own. Stacked on PR-B (#548).

- **`pkg/cardrevision`**: shared store over the new `octo_message_card_revision`
  table (written by `modules/bot_api` on card edits/clear, read by
  `modules/message`) — `AppendFrame` (+ cap-20 prune, rune-safe `plain`
  truncate), `Query` (newest-first, incl. tombstones), `Clear` (tx: delete
  frames + write auditable tombstone), `DeleteByMessageID`.
- **`modules/bot_api`**: `botMessageEdit` appends a non-transient frame after
  the content write (best-effort); new `POST /v1/bot/message/card/revisions/clear`
  (owner-checked, writes a tombstone).
- **`modules/message`**: `GET /v1/message/card/revisions` (summary / `full=1`);
  extracted `authorizeCardChannelMember` shared with `card/action`; revoke
  deletes a card's revisions.
- **`pkg/cardmsg`**: `Transient` / `PlainFromContentEdit`.

## Structural learnings

- **Shared-table access via a `pkg/` store, not per-module raw SQL.** The
  revision table is written by `bot_api` and read by `message` (same split as
  `message_extra`). Instead of duplicating column-level SQL across two modules,
  a `pkg/cardrevision.Store` wrapping `ctx.DB()`'s `*dbr.Session` centralizes
  the schema. Both modules construct it. This is the cleaner pattern when a new
  shared table has more than one accessing module.
- **Extract the auth gate the moment a second endpoint needs it.** PR-C's
  `GET /card/revisions` needs the exact anti-IDOR channel-binding + membership
  gate `card/action` already had. Factoring `authorizeCardChannelMember`
  (parameterized by the invalid/denied errcodes) made both endpoints share one
  implementation — the brief explicitly called for this to prevent drift.
- **New table → `octo_` prefix.** The parent brief's illustrative name
  `message_card_revision` was overridden to `octo_message_card_revision` per the
  repo's new-table naming rule; the prefix-less legacy tables are untouched.

## Gotchas worth remembering (verify + code-review)

- **Actionability/queryability must not outlive visibility.** The GET endpoint
  first shipped with only the channel-membership gate and would have served the
  revision content of a **revoked / globally-deleted / operator-locally-deleted**
  card as long as the rows existed — the same class of gap PR-B hit on the action
  path. Fixed with a shared `isCardMessageWithdrawn` (mirrors
  `api_message_get.go:241`) applied on the query path, returning an empty list.
  **Any new read/write path over a message must apply the same
  revoke/is_deleted/user-local-delete visibility gate the existing single-message
  read applies — and must NOT rely on a best-effort cleanup elsewhere.** See the
  promoted learning.
- **The lifecycle gate is only ONE layer of the canonical read (PR#549 review
  B1).** Even after `isCardMessageWithdrawn`, the query still enforced a strict
  *subset* of `respondSingleMessage`: it omitted the `visibles` allowlist, the
  per-user read offset, the channel offset, and message expiry — so a
  visibles-excluded or offset-truncated group member could still read the full
  revision history (and with `full=1`, complete historical envelopes) though the
  canonical single read hides it. Revision history discloses *more* than a single
  action trigger, so its gate must be ≥ the action path's, not a subset. Fixed by
  extracting `cardCanonicalVisibleToViewer` (visibles / expiry / user-offset /
  channel-offset) out of `card/action` and calling it from **both** endpoints;
  the query returns an empty list on a visibility miss (mirrors the withdrawn
  path, no existence leak). New test `TestCardRevisionsCanonicalVisibility` pins
  the visibles-excluded and offset-truncated cases. The lesson generalized: a
  derived surface must match the canonical read in **every** layer, not just the
  one that was top-of-mind — see the promoted learning.
- **Best-effort cleanup must run right after the state commit, before any
  fail-and-return step.** The revoke revision-cleanup was first placed after
  `SendRevoke`; a `SendRevoke` failure returns early, so the DB would be marked
  revoked while the revision rows survived. Moved to immediately after the revoke
  transaction commits (before the notify loop). The query-side withdraw gate is
  the second layer so a delete failure still can't leak.

## Out of scope (sibling PR)

D12 capability manifest (`GET /v1/bot/card/profile`) → PR-D.
