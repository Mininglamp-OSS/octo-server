---
type: Task
title: "Task: card-message-p2-revision-history"
description: PR-C of the card message P2 protocol — D10 card revision history. A side table octo_message_card_revision appended by the botMessageEdit card branch (non-transient frames only, cap 20), a GET /v1/message/card/revisions query API (Auth → Space → SharedUIDRateLimiter + channel-membership, same gate as card/action), an optional envelope `transient` flag that keeps progress frames out of history, bot-driven revision clearing that writes an auditable tombstone row, and revision cleanup on message revoke. Stacks on PR-B. Zero octo-im changes.
tags: ["message", "card", "wire-contract", "bot-api", "rate-limit", "space", "isolation", "error-response", "i18n", "testing"]
timestamp: 2026-07-08T10:00:00Z
# --- octospec extension fields ---
slug: card-message-p2-revision-history
upstream: self
source: self
---

# Task: card-message-p2-revision-history

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Ship **D10 of the card message P2 contract** (`card-message-interaction`): a
queryable **card revision history**. The full-frame-replacement model of D6
(each `botMessageEdit` overwrites `message_extra.content_edit` with a complete
new envelope, latest-frame-only) means the message table cannot answer "这张卡
以前是什么" — which approval-class cards owe their audience. PR-C adds a side
table that records each non-transient frame as a complete renderable envelope,
plus a member-visible query API.

```
bot rewrites card → botMessageEdit content_edit (PR-B D6/D9)
                  → IF NOT transient: append octo_message_card_revision row (frame + plain + editor + time)
                  → cap 20 non-transient frames/message (oldest evicted)
member opens history → GET /v1/message/card/revisions (Auth → Space → UID-limit + membership)
                     → summary list by default; ?full=1 returns full renderable frames
bot clears history → tombstone row (editor + time + cleared count), itself visible in the listing
message revoked    → revisions deleted (a revoked message leaves no queryable content history)
```

**Stacking**: PR-C builds on **PR-B** (`feat/card-message-p2-action-loop`,
#548) — it hooks the revision append into PR-B's D6 `botMessageEdit` card branch
and reuses PR-B's `pkg/cardmsg` type-17 helpers. PR-C's PR is opened with
**base = PR-B's branch** (stacked diff), retargeted to `main` after PR-B merges.

## Decisions (inherited from D10, with implementation-level bindings)

| # | Decision | Binding |
|---|---|---|
| D10.1 | **Side table = `octo_message_card_revision`** (NOT the brief's illustrative `message_card_revision` — repo rule: new tables MUST carry the `octo_` prefix, `dm_`/prefix-less reserved for legacy). Columns: `message_id`, `channel_id`, `channel_type`, `card_seq` (nullable — the frame's D9 seq if any), `content` TEXT (full renderable frame envelope; NULL for tombstone), `plain` (list summary; NULL for tombstone), `is_tombstone`, `cleared_count`, `editor_uid`, `edited_at`, + `db.BaseModel`. Index `(message_id, id)`. Shared-table pattern like `message_extra`: **written by `modules/bot_api` raw SQL, read by `modules/message`** — no cross-module service dependency. Migration lives in `modules/message/sql/`. | new migration + db layer |
| D10.2 | **Appended by the D6 `botMessageEdit` card branch** (`modules/bot_api/send.go`, PR-B), alongside the `content_edit` overwrite, **only for non-transient frames**. `content_edit` storage stays latest-frame-only (render path unchanged); the revision table is the history surface. `editor_uid` = the editing bot. Append is best-effort-after-commit relative to the content write: the card update MUST NOT fail if the revision append fails (log + continue), since the durable card state is `content_edit`, not the history. | send.go card branch |
| D10.3 | **Cap 20 non-transient frames per message** (tunable const). On append, evict the oldest frame rows beyond the cap for that `message_id`. Tombstone rows are audit markers, not frames — they do NOT count toward the cap and are not evicted by it. | append + prune |
| D10.4 | **Envelope gains optional `transient: true`** — progress frames (thinking / tool-state) marked transient are applied to `content_edit` normally (D6/D9 unchanged) but **never enter the revision table**, so approval-state changes aren't drowned by progress noise. `cardmsg` gains a `Transient(payload)` reader (mirrors `CardSeq`); `transient` is a tolerated optional envelope field (forward-compat, not whitelisted per-element). | pkg/cardmsg + send.go |
| D10.5 | **Query API `GET /v1/message/card/revisions`** — `AuthMiddleware` → Space → `SharedUIDRateLimiter` + **channel-membership check identical to `card/action`** (D3 anti-IDOR: bind channel from the stored message row, then membership of that channel). Request: `message_id`, `channel_id`, `channel_type` (channel params are the anti-IDOR lookup key, same as card/action — NOT taken as an authorization subject), `limit`, `full`. Default returns a summary list (`card_seq`/`plain`/`editor_uid`/`edited_at` + tombstone rows); `full=1` returns each frame's complete envelope. Visibility = **all channel members** (same permission as message edit history, D10(a)). | new message endpoint |
| D10.6 | **Erasure allowed but explicitly recorded** (D10(b)): a bot may clear its own card's revisions via a new bot endpoint (bot-token, must own the message). Clearing deletes the frame rows and writes a **tombstone row** (`is_tombstone=1`, `cleared_count`, `editor_uid`, `edited_at`) that itself appears in the history — erasure is auditable, not silent. | new bot endpoint |
| D10.7 | **Revoke deletes revisions** (D10(b)): when a message is revoked (`modules/message` revoke path), its `octo_message_card_revision` rows are deleted (best-effort) — a revoked message leaves no queryable content history. | revoke hook |

## API surface (wire definitions)

**`GET /v1/message/card/revisions?message_id=&channel_id=&channel_type=&limit=&full=`**
(new — Auth → Space → SharedUIDRateLimiter + membership):

```json
// summary mode (default); full=1 adds a "card": {…full envelope…} field per non-tombstone row
{ "revisions": [
    { "card_seq": 2, "plain": "审批单 #42:✅ 已通过", "editor_uid": "bot_x", "edited_at": 1751791500 },
    { "tombstone": true, "cleared": 3, "editor_uid": "bot_x", "edited_at": 1751791400 },
    { "card_seq": 1, "plain": "审批单 #42:待审批", "editor_uid": "bot_x", "edited_at": 1751791234 } ] }
// newest-first; errors: non-member 403-class / non-card message / channel mismatch → i18n envelope
```

**`POST /v1/bot/message/card/revisions/clear`** (new — bot-token, same middleware
chain as other `bot_api` routes):

```json
{ "message_id": "8234567890123456789", "channel_id": "g_9f2c...", "channel_type": 2 }
// bot must own the target message (sender == robotID); deletes frame rows, writes a tombstone.
// errors: not-owner / non-card / non-bot-sender → i18n envelope
```

**`transient` on the type-17 edit envelope** (D10.4) — optional, consumed by the
D6 `botMessageEdit` branch:

```json
{ "type": 17, "card": {...}, "card_version": "1.5", "profile": "octo/v2",
  "card_seq": 5, "transient": true }
// transient:true → content_edit updated (D6/D9), NO revision row appended.
```

Authoritative doc `docs/card-protocol.md` already carries the D10 contract (P1
deliverable) — implementation must not drift; amend doc + brief together or not
at all.

## Load-bearing list
<!-- touches tags: wire-contract, bot-api, rate-limit, space, isolation,
     error-response, i18n, testing -->

- **New side table `octo_message_card_revision`** (`wire-contract`): shared-table
  pattern (written by `bot_api`, read by `message`, like `message_extra`).
  Migration in `modules/message/sql/` following the repo's plain-DDL / `octo_`
  prefix / MySQL-8 conventions. The revision-row shape (frame + plain + editor +
  time + tombstone) is a stored contract the query API projects.
- **`botMessageEdit` card branch append** (`bot-api`): `modules/bot_api/send.go`
  (PR-B's D6 branch) gains a post-content-write revision append for non-transient
  frames + cap-20 prune. **The card update MUST NOT fail if the append fails** —
  `content_edit` is the durable state; the history is a secondary surface (log on
  append error, still return OK + fire the CMD).
- **New authenticated read endpoint `GET /v1/message/card/revisions`**
  (`rate-limit`, `space`, `isolation`, `wire-contract`): mounted on the existing
  `/v1/message` group (Auth → SharedUIDRateLimiter → Space, mount order per repo
  rule). **The channel-membership + anti-IDOR binding MUST be identical to
  `card/action`** (bind channel from the stored row, membership of that channel,
  `ExistMemberActive` for group/topic, fake-channel containment for person). To
  avoid drift, factor the card-message member gate into a shared helper reused by
  both `cardAction` and the revisions handler (touches PR-B's
  `api_card_action.go`).
- **New bot clear endpoint `POST /v1/bot/message/card/revisions/clear`**
  (`bot-api`, `trust-boundary`): bot ownership (sender == robotID) validated
  before clearing, mirroring `botMessageEdit`'s YUJ-60-lineage own-message guard.
  Writes a tombstone row (auditable erasure).
- **Revoke hook** (`message`): the `revoke` path deletes the message's revision
  rows (best-effort; must not fail the revoke).
- **`transient` envelope field** (`wire-contract`): `pkg/cardmsg.Transient`
  reader; tolerated optional field, not per-element whitelisted.
- **Error responses** (`error-response`, `i18n`): all new rejections via
  `httperr.ResponseErrorL` + registered `pkg/errcode` codes + zh-CN; i18n
  make-target suite green; new handler files join the module
  `Test<Module>NoLegacyResponseError` guard lists.
- **Protocol doc**: `docs/card-protocol.md` D10 section — no drift.

## Out of scope

- **PR-B surface** (D3–D9/D11 + octo/v2 whitelist): shipped in #548, this PR
  stacks on it and only adds the D10 delta.
- **D12 capability manifest** (`GET /v1/bot/card/profile`) — sibling PR-D.
- **P3**: `Action.Execute`/auto-refresh, per-element `fallback`, templating,
  bot-side real-time delivery.
- **octo-im**: zero code changes (history is a pure octo-server read surface;
  no CMD/refresh change — the revision table is queried on demand, not synced).
- **Client history UI**: client repos render the revisions response; not here.
- **Revision diffing / partial-frame history**: only complete-frame snapshots
  are stored (the D6 full-replacement model makes each frame independently
  renderable); no structural diff between frames.
- **Editing/attributing non-bot revisions**: only bot card edits produce
  revisions (P2 keeps cards bot-sender-only); no human/OBO revision authorship.

## Acceptance

All machine-checkable unless noted:

- `go test ./pkg/cardmsg/... ./modules/message/... ./modules/bot_api/...` pass;
  `make i18n-extract && make i18n-extract-check && make i18n-lint` green with
  zh-CN entries; `golangci-lint run ./...` clean; guard tests updated.
- **Append (D10.2/D10.3)**: a non-transient card edit appends exactly one
  revision row (`content` = the full frame, `plain` = server-recomputed summary,
  `editor_uid` = the bot, `card_seq` carried through) while `content_edit` still
  holds only the latest frame; a **`transient: true`** edit updates `content_edit`
  (D6/D9 intact) but appends **nothing**; the 21st non-transient frame evicts the
  oldest so the table holds ≤ 20 frame rows for that message; an append failure
  does **not** fail the card edit (edit still returns OK, CMD still fires —
  asserted with an injected append error).
- **Query API (D10.5)**: a channel member gets the message's revisions
  newest-first (summary shape by default; `full=1` returns each frame's complete
  envelope, and each returned frame validates through `cardmsg.Validate`); a
  **non-member → 403-class** i18n envelope; a **cross-channel IDOR** (member of A
  submits a `message_id` in B with A's `channel_id`) → rejected (same stored-row
  binding assertions as `card/action`, for both person and group channels); a
  non-card / non-existent message → 400-class.
- **Clear + tombstone (D10.6)**: a bot clearing its own card's revisions deletes
  the frame rows and appends a tombstone row (`tombstone:true`, `cleared:N`,
  `editor_uid`, `edited_at`) that appears in the subsequent listing; a bot
  clearing **another** bot's card → rejected (ownership).
- **Revoke cleanup (D10.7)**: revoking a card message deletes its revision rows
  (a subsequent revisions query returns empty); revoke still succeeds if the
  revision delete errors (best-effort).
- **Rate limiting**: the revisions route inherits `SharedUIDRateLimiter` after
  auth on the `/v1/message` group (bucket reset in test setup per testing rule).
- **Member gate parity**: the extracted shared card-message member gate is used
  by BOTH `cardAction` and the revisions handler (asserted by a test exercising
  the same non-member / IDOR rejections on the revisions route).
- **Migration**: `octo_message_card_revision` created via a
  `modules/message/sql/` migration; naming/engine/charset consistent with
  existing scripts; `octo_` prefix.
- `docs/card-protocol.md` D10 section matches this brief (human-reviewed).
