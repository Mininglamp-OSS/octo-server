---
type: Journal
title: "Journal: message-reaction-hardening (channel-scoped, text-only, multi-reaction, authz parity)"
description: Record of the reaction rework — channel-scoping + write-path visibility hardening + text-only restriction, then multi-reaction (atomic upsert + unique index) and write/sync/read authorization parity from an external review (F1–F4).
tags: ["message", "reaction", "acl", "wire-contract", "auth", "thread", "space", "isolation", "i18n", "error-response"]
timestamp: 2026-07-16T00:00:00Z
# --- octospec extension fields ---
task: message-reaction-hardening
upstream: self
source: self
---

# Journal: message-reaction-hardening

## What was done

Reworked the reaction API (`POST /v1/reactions`, `POST /v1/reaction/sync`, inline
`reactions[]`) into one commit (`modules/message`), brief
`.octospec/tasks/message-reaction-hardening/brief.md`:

- **Channel scoping**: every reaction read/write is keyed by `(channel_id,
  channel_type, message_id)`. Dropped the global `message_id`-only DB helpers
  (`queryWithMessageIDs`, `queryReactionWithUIDAndMessageID`) that let a reaction
  row from another channel leak into a message's list / be toggled cross-channel.
- **Write-path visibility gate** (`addOrCancelReaction`): target message must
  exist in the requested channel and pass `messageVisibleToViewer` — a shared pure
  predicate (visibles / revoke / global-delete / user-delete / expire / user-offset
  / channel-offset). Group uses `ExistMemberActive` (excludes blacklist); topic
  resolves the parent group + rejects deleted threads (`threadNotDeleted`).
- **Text-only**: reactions restricted to `payload.type == 1` (`payloadIsPlainText`);
  everything else → new i18n code `err.server.message.reaction_unsupported_type`.
- **Multi-reaction model**: `(uid, message_id, channel_id, channel_type, emoji)` is
  UNIQUE; toggling is a single atomic `INSERT ... ON DUPLICATE KEY UPDATE
  is_deleted = 1 - is_deleted, seq = VALUES(seq), ...` (`toggleReaction`). Different
  emojis are independent rows (append); same emoji toggles in place. Migration
  de-dups (keep max `id`) before creating the unique index.
- **Sync authz parity** (`syncReaction`): same membership posture as write
  (`ExistMemberActive`, topic parent-group + non-deleted-thread, `else` reject),
  plus a read-time visibility filter (`filterReactionsByMessageVisibility`) that
  drops reactions whose target message is no longer visible to the caller.
- **Wire additions**: write endpoint returns `{emoji, seq, is_deleted}` (optimistic-
  update reconciliation); `syncMessageReaction` CMD param gains `message_id/emoji/seq`.
- **DM Space isolation** (external review, blocking): reaction routes now mount
  `spacepkg.SpaceMiddleware` (opt-in, same posture as `/v1/message`). Extracted the
  per-message rule from `filterPersonMessagesBySpace` into a shared
  `personSpaceAllows(msgSpaceID, isSysBot, spaceID, defaultSpaceID)` (issue #484 /
  YUJ-226 rules unchanged), reused by message sync, reaction sync, and reaction
  write — sync drops reactions whose target `payload.space_id` fails the predicate;
  write returns 404 for out-of-Space DM targets (merged with not-found, placed
  before the type gate so no "exists-but-unsupported" signal leaks). Client contract
  (opt-in / fail-open when no `X-Space-ID` / `space_id`) is unchanged and matches
  the message-sync side exactly.
- **Small hardenings alongside**: emoji bounded to `varchar(20)` runes via
  `utf8.RuneCountInString`; `reactionTargetVisibleToViewer` swapped off
  `fetchMessageExtras` (which also queried reactions and discarded them) to a
  minimal `extra + user_extra` fetch; migration gained a `+migrate Down`
  (`DROP INDEX`; the dedup `DELETE` is inherently irreversible).

## Structural learnings

- **One visibility predicate, two call sites.** The write path (single message) and
  the sync path (batch) must apply the *identical* visibility rule set, so it was
  extracted into a pure `messageVisibleToViewer(msg, extra, userExtra,
  userOffsetSeq, channelOffsetSeq, loginUID)`. Write fetches single-row and calls
  it; sync batch-fetches (messages + extras + user-extras + one channel-offset + one
  channel-setting = 5 queries, independent of reaction count) and calls it. Any
  future reaction reader must reuse this predicate rather than re-deriving the rules.
- **Per-viewer invisibility can only be filtered at read time.** Revoke / global
  delete are channel-global and *could* clean reaction rows on write, but
  user-delete / channel-offset / `visibles` / expire are per-viewer — the reaction
  row is shared, so there is nothing to "clean". The only complete fix is filtering
  when reading for a specific caller. Do not attempt write-time reaction cleanup for
  these states; it is structurally impossible to get right.
- **Atomic upsert removes a check-then-insert race for free.** The original
  query→branch→insert/update had no unique key and no transaction, so concurrent
  double-taps could create duplicate `is_deleted=0` rows (and a later toggle would
  only flip one). `INSERT ... ON DUPLICATE KEY UPDATE` on the new unique key is
  race-safe in one statement — no `SELECT ... FOR UPDATE`, no retry loop. dbr does
  this via `InsertBySql` (precedent: `modules/bot_api/send.go`, `modules/webhook`).
- **Text-only is a product decision, not in the web spec.** The web reaction spec
  never restricted reactions to text messages; the server does. This is a real
  three-way (server/web/product) alignment item, not a server bug — flagged in the
  handoff contract so the web picker only surfaces on text messages.
- **DM is one physical channel shared across Spaces — any per-message endpoint
  needs the same Space filter `/v1/message` uses.** Person(DM) is routed by bare
  uid in WuKongIM, so a single reaction/message row can belong to different Spaces;
  the Space label lives only in `payload.space_id`. Reaction was a fresh surface
  that shipped without `SpaceMiddleware` and without the `payload.space_id`
  predicate, and the review caught the exact same leak class YUJ-226 already fixed
  on message sync. The rule this reinforces: **any new per-DM-message endpoint must
  (a) mount `SpaceMiddleware` and (b) apply the shared `personSpaceAllows`
  predicate on `payload.space_id`, driven off the middleware's validated `spaceID`
  and the caller's default Space.** Extract the per-message decision into one
  helper the moment there's a second caller, so the DM isolation posture cannot
  drift between entry points.

## Gotchas worth remembering

- **`db.Time.String()` is `"YYYY-MM-DD HH:mm:ss"` (local, no timezone), not
  RFC3339.** This is the project-wide wire format for every `created_at` (via
  `db.Time.MarshalJSON`), so reaction `created_at` was *not* special-cased to
  RFC3339 (that would diverge from the whole API). Clients must sort reactions by
  the monotonic `seq`, not `created_at`.
- **Local `test` DB drifts after any rebase that pulls new migrations.**
  Rebasing onto a newer `origin/main` brought a new upstream migration
  (`20260716000001_add_octo_space_welcome_delivery.sql`) the pre-existing `test`
  DB had never seen → `NewTestServer` panics `Unable to create migration plan ...
  unknown migration in database`. Fix is drop & recreate the DB
  (`docker exec octo-test-mysql mysql -uroot -pdemo -e "DROP DATABASE test; CREATE
  DATABASE test ..."`), never per-migration reconciliation.
- **Squash base is the merge-base, not `origin/main`.** When squashing a
  behind-main branch, `git reset --soft origin/main` folds *main's* newer commits
  into the diff (reverting unrelated work). Reset to `git merge-base origin/main
  HEAD` instead; verify the staged set contains only your files.
- **`json.Number` accepts quoted integers.** `payloadIsPlainText` treats
  `{"type":"1"}` as text (`json.Number("1").Int64()==1`) — harmless (semantics
  preserved; `"2"` still rejected), documented in the unit test.
- **Fork-PR CI noise (dmwork-org → upstream)**: `secret-scan` / `dependency-review`
  are expected FAILURE and non-blocking; the real gate is `check-sprint` (set the
  Sprint field on the Octo Board) + Build/Test.
- **`SpaceMiddleware` is opt-in — fail-open when the request carries no `space_id`.**
  `pkg/space/middleware.go` returns `c.Next()` (no filter, no reject) if neither
  `?space_id=` nor `X-Space-ID` is present. This is deliberate for backward
  compatibility with pre-Space clients, but it means DM isolation on reaction
  reads/writes is *only enforced when the client opts in*. The client-side rule
  documented in the web contract is: **reaction requests must carry the same
  Space parameter the client already sends to `/v1/message`**; otherwise the
  reaction view can diverge from the message view. Non-member `space_id` is a
  hard 403 from the middleware with a raw JSON body (`{"msg":"无权访问该 Space"}`),
  not the i18n envelope — clients need to handle both response shapes on reaction
  endpoints.

## Out of scope

Sticker reactions (server stores only `emoji`), `reaction_type`/`reaction_key` wire
fields (web derives from `emoji`), inline summary/count for large channels, a
by-message-id reaction endpoint, and a server-side reaction feature flag.
