---
type: Task
title: "Task: card-message-p2-action-loop"
description: PR-B of the card message P2 protocol — the interaction closed loop (D3–D9/D11). New POST /v1/message/card/action dispatch endpoint (authz + anti-IDOR + D11 input trust boundary + D4 Redis idempotency), a typed card_action bot event on the existing robot event queue, the type-17 botMessageEdit branch (cardmsg validation + D9 card_seq CAS stored in message_extra), and the pkg/cardmsg octo/v2 whitelist extension (Action.Submit + Input.*). Builds on the merged P1 (#543). Excludes D10 revision history (PR-C) and D12 capability manifest (PR-D). Zero octo-im changes.
tags: ["message", "card", "wire-contract", "trust-boundary", "bot-api", "rate-limit", "i18n", "error-response", "space", "isolation", "testing"]
timestamp: 2026-07-08T02:00:00Z
# --- octospec extension fields ---
slug: card-message-p2-action-loop
upstream: self
source: self
---

# Task: card-message-p2-action-loop

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

> This PR-B brief is authoritative for the external/third-party Bot pull
> implementation. First-party callback routing and terminal rendering are
> defined only by
> [`../card-action-callback-dispatch/brief.md`](../card-action-callback-dispatch/brief.md).

Ship the **first PR of card message P2** (parent contract:
`.octospec/tasks/card-message-interaction/brief.md`, decisions D1–D12): the
**interaction closed loop** so a user can tap a button (or fill inputs and
submit) on a bot-sent card, the owning bot receives a `card_action` event,
rewrites the card, and all clients converge on the new state via the existing
`/extra/sync` CMD channel.

Scope of **this PR (PR-B)** = **D3, D4, D5, D6, D7, D8, D9, D11** plus the
**`pkg/cardmsg` octo/v2 whitelist** (D1/D2) that all of them depend on:

```
client taps → POST /v1/message/card/action   (NEW: authz + anti-IDOR + D11 input check + D4 idempotency)
           → robot event queue, event_type="card_action"   (NEW typed-event sibling on the existing chokepoint)
           → bot polls /v1/bot/events → botMessageEdit content_edit   (existing endpoint, NEW type-17 branch: cardmsg validate + D9 CAS)
           → message_extra + SendCMD /v1/message/extra/sync  (existing fanout, reused as-is)
           → three clients re-render                          (existing extra-sync client logic — octo-im/client repos, unchanged)
```

**D10 card revision history** and **D12 capability manifest** are split into
sibling PRs (PR-C / PR-D) and are explicitly out of scope here — see Out of
scope. This PR must be mergeable and useful on its own: bots can send octo/v2
interactive cards, receive actions, and rewrite state.

## Background

- **P1 foundation (#543, `feat/card-message-p1-display`, APPROVED/CLEAN)** ships
  `pkg/cardmsg` (single whitelist authority), the InteractiveCard=17 constant,
  `Validate`/`Finalize`/`BuildPlain`, `IsCardPayload`/`IsCardRawPayload`/
  `IsCardContentEdit`, `Enabled()` (the `OCTO_CARD_MESSAGE_ENABLED` gate),
  `modules/cardtrust`, the display-text helper, the P1 errcodes, and
  `docs/card-protocol.md` (which **already contains the full P2 action
  contract**). PR-B branches from **main after #543 merges** (locked decision
  2026-07-08) and extends `pkg/cardmsg` in place — it does **not** fork a second
  whitelist.
- **POC (`origin/poc/card-message`)** prototyped and WuKongIM-verified most of
  this loop, but forked from main **before** #543, so it carries its own
  P1+P2 `pkg/cardmsg` that conflicts with the merged P1. PR-B **ports the
  POC's P2 logic onto the merged P1 cardmsg** (merge, not overwrite) and
  **fixes three POC defects** (see Load-bearing list):
  1. `event_data.data` is missing from the POC event map — the frozen wire
     contract requires the server-extracted `Action.Submit` static `data`;
  2. POC member check uses `groupService.GetMembers` (O(n) full scan) — repo
     already has `group.IService.ExistMemberActive(groupNo, uid)`
     (`modules/group/service.go:590`), a single-point query;
  3. POC stores `card_seq` in a standalone Redis key `cardseq:{id}` — locked
     decision is to store it in `message_extra` alongside `content_edit` in the
     same write (D9), no extra Redis dependency, consistent with the revision
     side table PR-C will add.

## Load-bearing list
<!-- touches tags: wire-contract, trust-boundary, bot-api, rate-limit, space,
     isolation, error-response, i18n, testing -->

- **New authenticated write endpoint `POST /v1/message/card/action`**
  (`rate-limit`, `space`, `isolation`, `trust-boundary`, `wire-contract`): the
  first client-initiated card write path. Mount on the existing `/v1/message`
  group (`modules/message/api.go:291`) which already carries
  `AuthMiddleware → SharedUIDRateLimiter → SpaceMiddleware` in the D3-required
  order; add a route-local `http.MaxBytesReader` **64 KiB** pre-decode cap. The
  D3 trust chain MUST NOT be weakened and MUST run in this order:
  1. **stored-message lookup + channel binding (anti-IDOR)** — resolve the
     message by the *request-declared* channel (person channels via
     `common.GetFakeChannelIDWith(loginUID, peer)`; group/topic by the declared
     id) so the sharded lookup itself proves "declared channel == stored row";
     not-found and mismatch collapse to one 400 (anti-enumeration);
  2. **membership of the *stored* channel** — person: stored fake-channel id
     contains `loginUID`; group/topic: `ExistMemberActive` (single-point, NOT
     `GetMembers`); topic resolves to parent group first;
  3. **`type==17` and sender is a bot identity** — `robotService.ExistRobot`
     on the stored `from_uid`; `iwh_` webhook senders and gate-bypassed human
     senders fail closed (D7);
  4. **`action_id` exists in the effective frame** — `content_edit` (latest
     frame) if present, else original payload; a tap on a button removed by a
     rewrite → 400 (D3 stale-tap fail-closed);
  5. **D11 input trust boundary** — `cardmsg.ValidateInputs(effective, inputs)`;
  6. **D4 idempotent enqueue**.
  Anti-enumeration: everything except membership (403-class) collapses to a
  single 400 `invalid`; the specific reason goes to logs only.
  **P1-4 ordering revision (PR#548):** the D4 idempotency *claim* (step 6) is
  checked **before** steps 4–5 for the replay path — an existing claim returns the
  replay ack without re-running the stale-frame/input gates, so a lost-ack retry of
  an already-accepted action replays even after a rewrite removed its button; a
  **fresh** claim runs steps 4–5 and is **released** on failure so a corrected
  retry can re-claim.
- **`event_data` wire contract is FROZEN by the parent brief** (`wire-contract`,
  `bot-api`): the enqueued `card_action` `event_data` MUST carry exactly the
  frozen keys — `message_id, channel_id, channel_type, space_id, action_id,
  data, inputs, operator_uid, client_token, acted_at` — additive-only, never
  renamed/removed. **`data` is server-extracted** from the matched
  `Action.Submit`'s static author object in the effective frame (D11
  anti-forgery — never from the request; present only when the action declared
  one). `inputs` is D11-shape-checked (declared ids only, typed, size-capped);
  content stays untrusted user text. `client_token` is a D4 correlation id only.
  **`space_id` (P1-3, PR#548)** is the card's authoritative origin Space
  (server-resolved from the stored row: GROUP/COMMUNITY_TOPIC → group `SpaceID`;
  PERSONAL → the `space_id` injected into the stored payload at send), **never**
  the operator's request-context Space; omitted fail-closed when unresolvable.
  A contract test pins the shape.
- **D4 idempotency store** (`trust-boundary`, `rate-limit`-exception): Redis
  dedup key `(message_id, action_id, operator_uid)` — **not** `client_token` —
  with the spec-fixed ordering: `claim SET NX EX 60s "pending"` → enqueue →
  `confirm SET XX EX 24h <event_id>`; enqueue failure → compensating `DEL` + 5xx
  internal envelope (retryable); a crash between claim and confirm leaves only
  the 60 s pending claim (no 24 h lockout). Any existing claim → 200 replay ack,
  no second event. This is **business-identity idempotency** (rate-limit rule's
  explicit exception clause) — request-frequency limiting stays
  `SharedUIDRateLimiter`'s job. Built on the `pkg/redis` instrumented client
  (SetNX/SetXX/Del) per the OIDC-lock / token-bucket precedent, not the octo-lib
  Conn wrapper.
- **Typed bot event on the existing queue** (`bot-api`, `wire-contract`): add
  `EnqueueBotTypedEvent(robotID, eventType, eventData) (int64, error)` to
  `robot.IService` (`modules/robot/api.go:61`) and to the `Message` struct's
  narrowed `robotService` handle (`modules/message/api.go:223`). It hangs off
  the **same GenSeq/ZAdd/Expire chokepoint** as `EnqueueBotEvent`
  (`enqueueBotEventGeneric`, `modules/robot/api.go:179,198,208,211`) as a
  typed-event sibling — not an overload of the message-shaped path. The public
  `eventResp` already carries `EventType`/`EventData`
  (`modules/bot_api/events.go:27-28`); delivery stays cursor-polling
  at-least-once (`getEventsResult` ZRangeByScore, `events.go:103`) — no push, no
  long-poll (D5). Bot SDKs must tolerate unknown `event_type` (doc'd).
- **`botMessageEdit` type-17 branch** (`bot-api`, `wire-contract`,
  `trust-boundary`): `modules/bot_api/send.go:647`. Today the RichText branch
  routes `content_edit` through `richtext.NormalizeContentEdit` (`:785`); P1
  (#543) **blanket-rejects** type-17 edits via `err.server.bot_api.card_edit_forbidden`.
  PR-B **retires that reject on the bot path** (the parent brief: "retired, not
  repurposed") and adds a type-17 branch that runs the `cardmsg` analog of
  `NormalizeContentEdit` (whitelist + size + URL scheme + `plain` recompute,
  symmetric to send). Frame invariants: (a) replacement envelope MUST be
  type 17 — **cross-type mutation rejected** (card→text or text→card); (b) each
  frame independently validated (heterogeneous consecutive frames, incl. moving
  between octo/v1 and octo/v2, are legal); (c) sender/ownership/render-gate are
  message-bound — the existing YUJ-60-lineage own-message guard applies (a bot
  edits only its own card). The **user** `/v1/message/edit` path stays
  type-17-rejected **permanently** (users don't own bot cards). Reuses the
  existing `content_edit_hash` dedup (`send.go:797`), the `message_extra`
  upsert (`:821`), and the `SendCMD(CMDSyncMessageExtra)` fanout (`:830-835`)
  as-is — **octo-im zero changes**.
  **PR#548 review 补强:** (i) the own-message binding is now **enforced** —
  a `message_id`/`message_seq` mismatch is **hard-rejected** (was warn-only),
  symmetric with the user path's `ErrMessageIDSeqMismatch`. `message_extra` is a
  single `UNIQUE(message_id)` table, so ownership-checked-on-`(channel, seq)` but
  written-by-`message_id` was a confused-deputy (a bot could overwrite another
  bot's card `content_edit` — the frame the action endpoint trusts — and forge its
  tap-time action surface). The canonical flow omits `message_seq` (server
  resolves it), so legitimate callers are unaffected. (ii) the card-edit branch
  now **rejects editing a revoked/globally-deleted card** (`extra.Revoke`/
  `is_deleted`), symmetric with the action endpoint's revoke gate — no re-populating
  `content_edit` on a withdrawn card.
- **D9 `card_seq` CAS stored in `message_extra`** (`wire-contract`): the
  type-17 envelope gains an **optional** monotonic integer `card_seq`. Locked
  decision (2026-07-08): the stored value lives in **`message_extra`** (a new
  column via a `modules/message/sql/` migration — altering the existing
  legacy `message_extra` table, no new-table prefix rule), written in the same
  upsert as `content_edit`, **not** a standalone Redis key. When an edit
  carries `card_seq`, the write is a **conditional CAS** (reject `card_seq ≤`
  stored → dedicated i18n conflict code, nothing stored); absent `card_seq` →
  last-write-wins unchanged (single-writer bots and all existing edits
  untouched, zero behavior change for them). The CAS must be atomic against
  concurrent edits (conditional `UPDATE ... WHERE card_seq < ? OR card_seq IS
  NULL`, or a `SELECT ... FOR UPDATE` tx) — LWW-with-a-read-then-write race is
  not acceptable.
  **PR#548 review 补强:** `card_seq` is decoded as an **exact int64**
  (`json.Decoder` + `UseNumber()` in a shared `decodeEnvelope`, used by both
  `NormalizeContentEdit` and `CardSeqFromContentEdit`), never via `float64`. A
  plain `json.Unmarshal` coerces a JSON integer to `float64`, quantizing values
  `> 2^53` to the 53-bit mantissa **before** the CAS compare — adjacent frames
  collapse to equal and the no-lost-update guarantee silently fails for realistic
  producers (ns-epoch ~1.75e18, snowflake). Quantization is closed at **both**
  decode sites (send reads the incoming `card_seq` from the *normalized* blob, so
  the round-trip in `NormalizeContentEdit` matters too). Non-integral / overflow
  `card_seq` degrades to absent = last-write-wins, never a truncated value.
- **`pkg/cardmsg` octo/v2 whitelist extension** (`wire-contract`,
  `trust-boundary`, `url-destination`): extend the merged P1 `Validate`
  (`pkg/cardmsg/validate.go:22`) to accept `profile: "octo/v2"` in addition to
  `octo/v1` (D2 — accepted set becomes `{octo/v1, octo/v2}`), adding
  `Action.Submit` (element `id` + optional static `data`), `Input.Text`,
  `Input.Toggle`, `Input.ChoiceSet`, and `selectAction` carrying
  `Action.Submit`. `Action.Execute` stays rejected (P3). **D1 frame-unique
  ids**: `Validate` rejects a frame whose `Action.Submit` ids or `Input.*` ids
  collide (D3 addressing + D4 dedup key require frame-unique ids). New pure
  helpers: `SubmitActionIDs(effectiveFrame) → set` and the matched action's
  `data`, `ValidateInputs(effectiveFrame, inputs)`, `NormalizeContentEdit` /
  `IsCardContentEdit` extension for type-17, `CardSeqFromContentEdit`, and an
  `EventTypeCardAction = "card_action"` constant. **D2 side effect (called out
  deliberately)**: because the existing P1 send ingresses (`modules/bot_api`,
  `modules/robot`, `modules/incomingwebhook`) all call the shared
  `cardmsg.Validate`, they will **begin accepting octo/v2 cards** on send once
  the whitelist widens — this is intended (a bot must be able to *send* an
  interactive card before anyone can act on it); no per-ingress send-side
  restriction is added, and webhook-sent v2 cards remain display-only by D7
  (their taps are rejected at the action endpoint, not at send).
  **PR#548 review 补强:** an accepted `Input.*` element's action-bearing
  sub-properties (`inlineAction` — a standard AC 1.2+ face — and `selectAction`)
  are routed through the same positive action allowlist (`w.action`: `checkURL` +
  action-type whitelist + `Action.Submit` id/`registerID`/data-object discipline)
  as container `selectAction`. The walker is tolerant of unknown *properties*, so
  widening the whitelist to accept `Input.*` element **types** otherwise left
  `inlineAction` an unvalidated render+dispatch face (a `javascript:`
  `Action.OpenUrl`, or a P3 `Action.Execute`, or an unregistered `Action.Submit`
  smuggled past the gate) — 校验面必须 ≥ 渲染面 (Decision 3d/6).
  **PR#548 review 补强 (round 2 — 校验面 ≥ 派发面):** the three tree-walks are now
  aligned so the **dispatch/resolution surface ≤ the validation surface**. (a)
  `Validate` calls `w.selectAction` **unconditionally** at the top of `element()`,
  so a `selectAction` on any element (incl. `TextBlock`/`FactSet`, previously
  skipped) is validated; (b) the dispatch-side walkers `SubmitAction`
  (`findSubmitInElements`) and `collectInputSpecs` (`collectInputSpecsFromElements`)
  recurse `items`/`columns` **only for the container types `Validate` recurses**
  (`Container`/`ColumnSet`→`Column`), not unconditionally — so a Submit/Input under
  a leaf's `items[]` (e.g. `TextBlock.items[]`) is neither validated nor resolvable;
  (c) `SubmitAction` also resolves an Input's `inlineAction` Submit (validated at
  send), closing the send-accepted-but-dispatch-dead face. Prevents an
  `Action.Submit` escaping D1 frame-unique-id / data-object discipline while still
  being dispatch-resolvable.
- **Error responses** (`error-response`, `i18n`): every new rejection via
  `httperr.ResponseErrorL` + registered `pkg/errcode` codes (new codes:
  card-action invalid / denied, card_seq conflict; reuse P1 codes where they
  fit) + zh-CN entries in `pkg/i18n/locales/active.zh-CN.toml`. Anti-enumeration
  per D3. `make i18n-extract && make i18n-extract-check && make i18n-lint` green.
  New handler-bearing files (`modules/message/api_card_action.go` and any bot_api
  additions) join the module `Test<Module>NoLegacyResponseError` guard lists.
- **Protocol doc**: `docs/card-protocol.md` (shipped by #543) already carries
  the full P2 action contract — PR-B implementation must **not drift** from it;
  the doc and the parent brief are amended together or not at all. No new doc
  text is expected beyond confirming alignment.

## Out of scope

- **D10 card revision history** — side table `message_card_revision`, the
  `GET /v1/message/card/revisions` API, the `transient` frame flag, tombstone
  on clear, cap-20 eviction. **Split into PR-C.** (The `transient` field is
  therefore *not* honored in PR-B's `botMessageEdit` branch beyond being
  tolerated as an unknown envelope field — PR-C adds the behavior.)
- **D12 producer capability manifest** — `GET /v1/bot/card/profile`. **Split
  into PR-D.**
- **P3**: `Action.Execute` / auto-refresh, templating/data-binding,
  `Action.ShowCard` / `ToggleVisibility`, ephemeral responses, multi-step forms,
  per-element `fallback`, bot-side real-time event delivery (long-poll / push
  channel — D5 stays cursor-polling), cross-ecosystem card mapping.
- **OBO × card** — stays rejected (P1 Decision 2b, unchanged).
- **Synchronous action responses** — the endpoint returns an async ack only; no
  card-in-response (D5).
- **Client renderers** (input controls, loading/disabled transient state, the
  10 s client timeout) — client repos, per the P1 responsibility split.
- **octo-im** — zero code changes (refresh rides the existing CMD channel).
- **wukongim-message-indexer** — card `searchText` materialization is a tracked
  cross-repo follow-up, unchanged by P2.

## Acceptance
<!-- Machine-checkable where possible. -->

- `go test ./pkg/cardmsg/... ./modules/message/... ./modules/bot_api/... ./modules/robot/...` pass;
  `make i18n-extract && make i18n-extract-check && make i18n-lint` green with
  zh-CN entries for every new code; `golangci-lint run ./...` clean; guard
  tests updated.
- **Whitelist (D1/D2)**: `pkg/cardmsg` accepts `Action.Submit` / `Input.Text` /
  `Input.Toggle` / `Input.ChoiceSet` (incl. `selectAction` carrying
  `Action.Submit`) **only** under `profile: "octo/v2"`; `Action.Execute` still
  rejected; an octo/v2 card where the caller pinned octo/v1 → rejected; a frame
  with duplicate `Action.Submit` or `Input.*` ids → rejected (frame-uniqueness).
  An `Input.Text` whose `inlineAction`/`selectAction` carries a `javascript:`
  `Action.OpenUrl` → rejected (bad URL scheme), an `Action.Execute` → rejected
  (P3), an id-less `Action.Submit` → rejected, and a `Submit` id colliding with a
  top-level action id → rejected (proves it goes through `registerID`); a valid
  `https` `inlineAction` still accepted (no false positive).
- **Happy-path e2e**: operator in channel taps a bot card action → endpoint
  acks `{accepted:true, replay:false}` → `getEvents` returns exactly one event
  with `event_type="card_action"` and the frozen `event_data` shape (incl.
  server-extracted `data` and shape-checked `inputs`) → bot calls
  `botMessageEdit` with a type-17 `content_edit` → `message_extra` updated,
  CMD emitted on the `/extra/sync` channel.
- **Idempotency (D4)**: same `(message_id, action_id, operator_uid)` submitted
  twice **with different `client_token`s** → second acks `replay:true`, queue
  contains **exactly one** event; **enqueue-failure recovery** (injected
  failure) → 5xx internal envelope + dedup key released → an immediate retry
  succeeds (no 24 h lockout). (UID rate-limit bucket reset in test setup per the
  testing rule.)
- **Trust model / anti-IDOR (D3/D7)**: non-member → 403-class envelope; stored
  sender is a human (non-bot) → 400; `iwh_`-sent card → 400; `action_id` absent
  from the **effective** frame → 400; a tap on an `action_id` present in the
  original payload but **removed by a `content_edit` rewrite** → 400
  (stale-tap fail-closed); **cross-channel IDOR**: operator is a member of
  channel A but not B, submits B's `message_id` with A's `channel_id` →
  rejected (binding derives from the stored row) — asserted for both person and
  group channels; request body > 64 KiB → rejected pre-decode.
  **PR#548 review 补强 (round-3 — canonical visibility parity):** the endpoint
  must mirror the single-read (`respondSingleMessage`) visibility gates, not just
  a subset — a group member **excluded by the card's `visibles` allowlist**, or
  whose per-user/channel offset has cleared it, or an **expired** message, is
  rejected (collapsed `invalid`, no event enqueued) exactly as the read path 404s
  it; otherwise an excluded member could enumerate the `message_id` and fire the
  bot side-effect on a card they cannot see. Person DMs (2-party, `visibles`
  n/a) keep membership+lifecycle gating. Test: `TestCardActionVisibilityParity`
  (visibles-excluded member → rejected + no event; visibles-included → allowed).
  **PR#548 review 补强 (round-4 — group/thread status parity, H1/P1-a + P2-a):** round-3
  still missed two read-path gates the action path also owes. (i) A group in
  `GroupStatusDisabled` (admin moderation): `requireGroupMember` 404s it, but
  `ExistMemberActive` still returns true (disabling leaves member rows `status=Normal`),
  so a disabled-group member could still fire the bot side-effect. Now gated by
  `groupStatusVisibleForAction` (mirrors `requireGroupMember`: only `Normal`/`Disband`
  visible; uses `groupDB.QueryWithGroupNo` so a missing group is `nil`→collapsed-invalid,
  not a query error). (ii) A CommunityTopic whose thread is `ThreadStatusDeleted`
  (`getThreadMessage` 404s it) — now gated in the topic branch. Both collapse to
  `invalid`, no event. `TestCardActionVisibilityParity` extended: disabled group →
  rejected + no new event; deleted thread → rejected + no new event.
- **Input validation (D11)**: `inputs` key not declared in the effective frame
  → 400; `Input.ChoiceSet` value outside declared choices → 400; `Input.Text`
  value > 4 KiB → 400; valid declared inputs arrive in `event_data.inputs`
  verbatim; the matched `Action.Submit` static `data` from the effective frame
  arrives in `event_data.data`, and a `data` field supplied in the **request**
  is **ignored** (server uses the stored frame's copy).
- **Update path (D6/D9)**: non-owner bot editing another bot's card → rejected;
  type-17 `content_edit` failing whitelist/size/scheme → 400, no `message_extra`
  write; **cross-type mutation** (type-17 ↔ non-17 body) → 400; two consecutive
  rewrites with entirely different structures both accepted
  (heterogeneous-frame test); `content_edit_hash` dedup asserted; user
  `/v1/message/edit` with a type-17 body → rejected; **D9** edit carrying
  `card_seq ≤` stored → 409 conflict i18n code, nothing stored (stored value
  read back from `message_extra` unchanged); edit without `card_seq` →
  last-write-wins unchanged; concurrent CAS test asserts no lost-update /
  stale-overwrite; two adjacent `card_seq` values `> 2^53` parse to **distinct**
  int64s (no float64 collapse) and survive a `NormalizeContentEdit` round-trip,
  and a non-integral `card_seq` degrades to last-write-wins.
  **PR#548 review 补强 (round-3 — non-CAS version monotonicity):** the
  `!hasCardSeq` last-write-wins branch also allocates `version` **inside** a
  `FOR UPDATE` row lock (`cardVersionInLockWrite`, same lock as the CAS branch),
  not outside — otherwise a no-`card_seq` frame's lower version, allocated before
  the lock and committed after a concurrent CAS (or another non-CAS) frame's
  higher version, overwrites it and delta-sync (`version>?`) permanently misses
  the final frame. Test: `TestBotCardEditMixedFrameVersionMonotonicIM` (concurrent
  interleaved CAS + non-CAS frames → row `version` never regresses, final ≥ peak).
  **PR#548 review 补强 (round-4 — version monotonicity scope, H2/P1-b):** the "version
  never regresses" guarantee holds **only intra-process**, and the claim was scoped down
  to say so. `version` comes from `ctx.GenSeq`, a per-process HiLo allocator
  (process-global `seqMap`+mutex, per-process `seqStep=1000` block); the `FOR UPDATE` row
  lock serializes the *write* but not the *allocation value* — across replicas a
  lower-block instance can commit a lower version after a higher one, and delta-sync
  (`version>?`) then permanently skips the terminal frame until a full resync. This is
  **pre-existing** (richtext edit shares `GenSeq`; the PR doesn't touch `config/seq.go`),
  not #548-introduced; the fix is honesty — the `cardVersionInLockWrite` comment, the
  (single-process) test, and this brief now state the intra-process scope. A true
  cross-replica fix needs a channel-level totally-ordered version source (DB/Redis
  counter) touching **all** version carriers (richtext included) → out of scope for #548,
  tracked as follow-up. (Reviewer contradiction resolved from octo-lib `config/seq.go`
  source: yujiawei/Jerry-Xin correct that `GenSeq` is process-local; OctoBoooot's initial
  "row lock ⇒ cross-replica monotonic" retracted after byte-reading the allocator.)
- **Rate limiting**: a route-mount test asserts `SharedUIDRateLimiter` is
  mounted after `AuthMiddleware` on the `/v1/message` group that carries
  `/card/action`.
- **Event lifecycle (D8)**: the D4 idempotency TTL and the `card_action`
  actionable window are **one shared constant** (asserted by test); a bot
  re-polling from a stale `event_id` cursor re-receives in-window events in
  order (at-least-once — documents the bot-side `event_id` idempotency
  requirement).
  **PR#548 review 补强:** the two were **de-coupled** (dedup TTL hardcoded 24h vs
  event window `Robot.MessageExpire` = 7d), leaving a 24h–7d gap where a re-tap
  escaped dedup and produced a **second** event. Fixed by sourcing the dedup
  key TTL from `Robot.MessageExpire` (the same value `EnqueueBotTypedEvent`
  stamps on the event), so dedup and event lifetime are literally one constant;
  `TestCardActionD8SharedWindowConstant` asserts `idemTTL == Robot.MessageExpire`.
- **Wire freeze**: a contract test pins the `card_action` `event_data` field
  set to the frozen example (additive-only).
  **PR#548 review 补强 (round-4 — P2-b):** `TestCardActionEndToEndAndIdempotency` now
  asserts the **full** `event_data` key set (count + whitelist), not just individual
  keys, so an accidental added / renamed / dropped key is caught, not silently shipped.
- **PR#548 review 补强 (round-2 P2 + CI):** (a) **replay only for a *confirmed*
  claim** — a bare-`pending` claim (concurrent first request still in flight)
  returns a retryable `409 ErrMessageCardActionInProgress`, not a false
  `replay:true`, so a valid concurrent action isn't lost behind a success ack
  (`TestCardActionClaimConfirmedState`); (b) **CAS false-409 fix** — a concurrent
  byte-identical retry (same `card_seq`, same `content_edit_hash`) returns OK
  instead of a stale-`card_seq` 409 (`send.go cardSeqCASWrite` compares the hash
  when `stored == cardSeq`); (c) **test isolation** — `api_card_action_test.go`
  (the only active `testutil.NewTestServer` file in `modules/message`'s default
  build) is moved behind `//go:build integration`, matching the package
  convention (13+ e2e files; `api_card_p1_test.go` avoids `NewTestServer`
  entirely), so it no longer collides with the package's bare-create unit tests
  under `-race -shuffle` (`Error 1050 Table already exists`). bot-side D6/D9 CAS
  stays CI-covered via `modules/bot_api`.
- **PR#548 review 补强 (round-4 — P2 cleanups):** (a) **P2-c** — `Release` is now a
  release-only-if-pending Lua CAS-del (`releaseIfPendingScript`): a >60s-stalled
  request's compensating release no longer deletes a *confirmed* claim written by a
  concurrent retry (which would reopen the dedup window and double-enqueue).
  `TestCardActionReleaseOnlyIfPending` asserts a confirmed key survives a stale release
  while a pending key is still deleted. (b) **P2-d** — the dead `case float64` in
  `cardmsg.CardSeq` is removed: all live callers decode via `UseNumber`→`json.Number`, so
  a stray `json.Unmarshal`-fed `float64` would only silently truncate `>2^53`; the
  remaining `int/int64/json.Number` cases are all exact, and an unrecognised type now
  fail-safes to LWW `(0,false)` instead of accepting a truncated seq. (c) group/thread
  status test fixtures seed proper `group` rows (the canonical read path requires them
  too, so this is fixture-completion, not a behaviour change).
