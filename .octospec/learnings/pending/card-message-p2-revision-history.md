---
type: Learning
title: "Every new read/write path over a message must reapply the visibility gate — not rely on cleanup"
description: New endpoints touching a message (actions, history, projections) must reapply the revoke/is_deleted/user-local-delete visibility gate the single-message read applies; deleting derived data on revoke is a best-effort second layer, never the sole guarantee.
tags: ["trust-boundary", "space", "isolation", "visibility", "message", "review"]
timestamp: 2026-07-08T11:30:00Z
# --- octospec extension fields ---
source: self
origin_task: card-message-p2-revision-history
origin_pr: self
status: pending
candidate_rule: space-isolation
---

# Every new read/write path over a message must reapply the visibility gate — not rely on cleanup

## Context

This gap surfaced **three times** across the card message P2 PRs, on two
different endpoints and across two *dimensions* of the canonical read gate:

- **PR-B** (`POST /v1/message/card/action`): first shipped reading
  `message_extra` only for `content_edit`, so a **revoked / deleted** card was
  still actionable (a stale tap drove a bot event after the card was recalled).
- **PR-C** (`GET /v1/message/card/revisions`), lifecycle dimension: first
  shipped with only the channel-membership gate, so a **revoked / globally-deleted /
  operator-locally-deleted** card's revision *content* was still queryable as
  long as the rows existed.
- **PR-C review B1** (`GET /v1/message/card/revisions`), canonical-visibility
  dimension: even after the lifecycle gate was added, the query still enforced a
  strict **subset** of the canonical read — it omitted the `visibles` allowlist,
  the per-user read offset, the channel offset, and message expiry. A group
  member excluded by a non-empty `visibles`, or whose read/channel offset was
  past the message (cleared history), could still read the full revision history
  (`full=1` → complete historical envelopes) though the canonical single read
  would hide it. Fixed by extracting `cardCanonicalVisibleToViewer` (visibles /
  expiry / offset) and calling it from **both** `card/action` and the query, so
  the two endpoints share one visibility口径.

Each time the established single-message read path (`respondSingleMessage` /
`api_message_get.go`) already had the correct, complete gate — and each new
endpoint reapplied only *part* of it. The canonical read is the reference; a
derived surface must match it in **every** layer (membership + group/thread
status + lifecycle revoke/delete + visibles + offset + expiry), not just the
layer that was top-of-mind.

A tempting-but-wrong mitigation on PR-C was "delete the derived rows on revoke."
That does not cover `is_deleted` (global delete) or user-local delete (neither
triggers the revoke path), and the delete was best-effort and mis-ordered
(after the notify step, which could return early). Deletion is a second layer;
it is never the guarantee.

## The rule

- **Any new endpoint that reads or acts on a stored message** (actions, edit
  history, revision history, projections, previews, exports) MUST reapply the
  **complete** visibility gate the canonical single-message read applies, in
  every layer: channel membership + group/thread status +
  `message_extra.revoke` / `message_extra.is_deleted` (global) +
  `message_user_extra.message_is_deleted` (operator) + the `visibles` allowlist +
  per-user read offset + channel offset + message expiry. Extract each dimension
  into a shared helper (`authorizeCardChannelMember`, `isCardMessageWithdrawn`,
  `cardCanonicalVisibleToViewer`) so the endpoints cannot drift and cannot ship a
  *subset*.
- **A derived surface must never be MORE permissive than the canonical read.**
  Reviewers should diff the new gate against `respondSingleMessage` layer by
  layer; "membership + one lifecycle check" is a subset, not the gate. Revision
  history in particular discloses *more* than a single action trigger, so its
  gate must be at least as strict as the action path's.
- **Actionability and queryability must not outlive visibility.** If a message
  is not viewable via the normal read, no derived surface may act on it or expose
  its content.
- **Cleaning up derived data on revoke is a best-effort second layer, not the
  primary guarantee.** It must run immediately after the state-change commit and
  before any step that can fail-and-return (e.g. an IM notify). The read/act path
  must still gate independently, so a missed/failed cleanup cannot leak.
