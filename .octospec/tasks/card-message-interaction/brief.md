---
type: Task
title: "Task: card-message-interaction"
description: P2 of the card message protocol — make octo profile cards interactive. Whitelist extension (Action.Submit + Input.*, profile "octo/v2"), a card action dispatch endpoint with server-side idempotency, a "card_action" bot event on the existing queue, bot-driven card state rewrite via the existing botMessageEdit content_edit path (latest-frame-only, optional card_seq CAS), CMD-driven refresh, and a producer capability-discovery manifest (GET /v1/bot/card/profile). Zero octo-im changes.
tags: ["message", "card", "wire-contract", "trust-boundary", "bot-api", "rate-limit", "i18n", "error-response", "space", "isolation"]
timestamp: 2026-07-06T07:00:00Z
# --- octospec extension fields ---
slug: card-message-interaction
upstream: self
source: self
---

# Task: card-message-interaction

Sibling of `card-message-protocol` (P1). P1 freezes the envelope + display
profile; this brief freezes the **interaction contract**. Both briefs are
published together so clients can architect against the complete contract in
one pass; the phase split is a **contract-freeze order and review boundary,
not calendar serialization** — P2 implementation may start as soon as both
briefs are confirmed, and P1/P2 may land in adjacent releases.

## Goal

Make cards interactive: a user taps a button (or fills inputs and submits) on
a bot-sent card, the owning bot receives a `card_action` event, processes it,
and rewrites the card so all three clients converge on the new state ("button
already used" etc.). The full loop rides existing rails:

```
client taps → POST /v1/message/card/action        (new endpoint: authz + idempotency)
           → bot event queue, event_type="card_action"   (existing queue + getEvents polling)
           → bot processes → botMessageEdit content_edit  (existing endpoint, extended to type 17)
           → message_extra + SendCMD /v1/message/extra/sync (existing channel)
           → three clients re-render the rewritten card    (existing extra-sync client logic)
```

Because rewrites are **full envelope replacements, each frame independently
validated** (D6), consecutive frames of the same message need not share any
structure — an agent's tool-call state machine (queued → running → per-tool
result → final, each with a different card shape, even a different profile)
maps directly onto frames with no schema affinity between them.

The "clicked-once" semantics (防重复操作) are **three cooperating layers**,
each necessary:

1. **Server idempotency (authoritative)** — duplicate submits of the same
   action never produce a second bot event, even before the card refreshes;
2. **State lives in the card content** — the bot rewrites the card (button
   removed / disabled / relabeled "已通过") via `content_edit`; there is **no
   separate card-state store**;
3. **Client transient state** — tap → local loading/disabled until refresh;
   clients MUST NOT persist local action state (the rewritten card is the
   only durable state).

## Decisions

| # | Decision | Rationale / anchor |
|---|---|---|
| D1 | **Action model: `Action.Submit` only in P2** (with element `id` + optional static `data` — the author-supplied context object standard AC merges into a submit, e.g. `{"action":"approve","record_id":42}`; **how `data` reaches the bot is specced in D11** — server-extracted from the frame, surfaced in `event_data.data`, so D1 no longer promises a field the wire cannot carry); inputs: `Input.Text`, `Input.Toggle`, `Input.ChoiceSet`. `selectAction` may carry `Action.Submit` under octo/v2 (tap-region submit) — per the P1 rule that `selectAction` inherits the phase of the action it carries. `Action.Execute` (universal action model, auto-refresh) is deferred to P3 — its refresh semantics are Teams-host-specific and our transport replaces them. **Frame-unique ids** (round-3 nit): `Validate` rejects a frame whose `Action.Submit` ids or `Input.*` ids collide within that frame — D3 addressing and the D4 dedup key are only unambiguous if ids are frame-unique. Design reference for the submit/state model: Slack Block Kit `action_id`/`block_id` addressing and Rocket.Chat UiKit action lifecycle. | AC schema; blocks-protocol prior art |
| D2 | **Profile bump: cards containing P2 elements declare `profile: "octo/v2"`.** The server-side accepted set becomes {octo/v1, octo/v2} (P1 Decision 10). P1-only clients see an unfamiliar profile → degrade to `plain` via the P1 degradation chain (no new mechanism). P1 cards keep `"octo/v1"` and render everywhere. | P1 brief degradation chain + Decision 10 |
| D3 | **Dispatch endpoint** `POST /v1/message/card/action` — `AuthMiddleware` + Space middleware + `SharedUIDRateLimiter` (mounted AFTER auth, per repo rate-limit rule) + **`http.MaxBytesReader` 64 KiB pre-decode cap** (round-3 P1-3: the route accepts a user-supplied `inputs` map, so it gets the same body-cap discipline as the P1 send routes). Request: `message_id`, `channel_id`, `channel_type`, `action_id`, `inputs` (map), `client_token`. **Channel binding derives from the stored message, never from the request** (round-3 P1-4 anti-IDOR): the server loads the message by `message_id`, takes channel/space from the **stored row**, and requires the client-supplied `channel_id`/`channel_type` to match it (mismatch → 400) — they are a lookup hint, not an authorization subject, so an operator cannot point a foreign `message_id` at a channel they happen to be a member of. Validation order: stored-message lookup + channel binding; operator membership **of the stored channel**; message is `type=17` and its **sender is a bot/webhook identity** (trust-model layer (c) from P1 Decision 2); `action_id` exists in the **effective card** — `content_edit` if present, else the original payload (anti-forgery — actions cannot be invented client-side; and since validation targets the LATEST frame, a **first** tap on a button removed by a rewrite fails closed with 400, stale interactions need no extra mechanism); D11 input validation (which also **server-extracts the matched action's static `data` from that effective frame** into `event_data.data` — never from the request); then the D4 idempotent enqueue. **P1-4 ordering revision (PR#548):** the D4 idempotency *claim* is checked **before** the effective-frame/stale-frame lookup — a request finding an existing claim returns the D4 replay ack **without** re-running the stale-frame gate, so a lost-ack retry of an already-accepted action returns `replay` even after a rewrite removed its button (D8's re-tap recovery assumes the control still exists, which a rewrite can violate). A **fresh** claim then runs the stale-frame + D11 checks and is **released** if either fails, so a corrected retry can re-claim. | `.octospec/rules/rate-limit.md`; P1 Decision 2; round-3 P1-3/P1-4 |
| D4 | **Idempotency — business identity + spec-fixed claim ordering** (rewritten per round-3 P1-1, which found the previous key shape self-contradictory with D8). Dedup key = **`(message_id, action_id, operator_uid)`**: it answers "has this operator already acted on this logical action instance", so a client retry after the D8 transient-state timeout — which necessarily carries a fresh `client_token` — can never double-fire the bot event. **`client_token` is demoted to a correlation id**: echoed in the ack and in `event_data` for bot/log correlation, never part of the dedup identity. **Claim/enqueue ordering is part of this contract**: (1) claim `SET key "pending" NX EX 60`; (2) enqueue the bot event; (3) confirm `SET key <event_id> XX EX 86400` (24h = the D8 window, one shared constant). Enqueue failure → compensating `DEL` + 5xx internal envelope (client may retry — the claim is gone); a process crash between claim and confirm leaves only the 60 s pending claim, which expires and lets a retry succeed — **no 24h lockout from a half-completed request**. Any request finding an existing claim (pending or confirmed) → 200 replay ack, **no second bot event**; the edge where a replay ack raced a pending claim that later failed is recovered by the D8 client timeout + re-tap. **Caveat for bot authors: a 200 (accepted or replay) ack does NOT itself guarantee an event was enqueued** — the racing first request's enqueue may still fail and `DEL` the claim. Treat the ack as "accepted for processing", not "delivered"; the durable delivery signal is the card rewrite (or, on its absence, the D8 client timeout + re-tap). This is business-identity idempotency (explicitly allowed by the rate-limit rule's exception clause), not request-frequency limiting — frequency is `SharedUIDRateLimiter`'s job. **`action_id` names a logical action instance**: a later frame that intentionally re-offers the same logical action MUST use a fresh `action_id` (e.g. `approve#2`) — otherwise the same user's legitimate re-action within the TTL collides with the spent dedup bucket (protocol-doc rule for bot authors, not a server mechanism). | round-3 P1-1; `.octospec` rate-limit rule exception |
| D5 | **Async ack model**: the endpoint returns an OK ack immediately; no synchronous card-in-response. The bot consumes `event_type="card_action"` from the existing queue (`/v1/bot/events` polling, `eventResp.EventType`/`EventData` at `modules/bot_api/events.go:27-28` — the public `eventResp` struct; `:117-118` is the inner Redis-decode struct); cross-module enqueue precedent is `EnqueueBotEvent` (YUJ-1424, `modules/robot/api.go` IService) — note it is message-event-shaped; this task adds a **typed-event sibling** on the same GenSeq/ZAdd/Expire chokepoint rather than overloading it (implementation note, POC-verified). **Bot delivery is cursor-polling, not push** (repo-verified: Redis ZSet read via `ZRangeByScore`, `events.go:103`; `getEvents` returns immediately — no long-poll; no bot callback/push channel exists anywhere). **Interaction latency therefore equals the bot's polling cadence** — stated as such in the protocol doc so product expectations are set; bot-side real-time delivery (long-polling `getEvents` or a push channel) is an explicit **P3 open item**, not silently implied. **Additive event type**: bot SDKs must tolerate unknown `event_type` values — documented in the bot API docs as part of this task. | `events.go:27-28,42,101,103` |
| D6 | **Card update = existing `botMessageEdit`** (`modules/bot_api/send.go:647`) extended to type 17: `content_edit` carries the **full replacement card envelope**, validated by the `cardmsg` analog of `richtext.NormalizeContentEdit` (`send.go:785` precedent) — whitelist + size + URL checks + plain recompute, symmetric to send. **Frame invariants**: (a) the replacement envelope MUST itself be type 17 — cross-type mutation (card→text or text→card) is rejected; (b) each frame is independently validated, so **consecutive frames may differ arbitrarily in structure and may move between octo/v1 and octo/v2** (schema change per message is supported by construction); (c) sender/ownership/render-gate are message-bound, never frame-bound. **Message-table storage stays latest-frame-only** (`content_edit` single overwritten column — the render path never changes); the queryable change history is a **side table, now in scope as D10** (maintainer decision, 2026-07-06 — full-frame replacement owes users a "这张卡以前是什么" trust surface for approval-class cards). Ownership: a bot may edit **only its own messages** (existing YUJ-60-lineage guard applies). `content_edit_hash` dedup and the `message_extra` write + `SendCMD(CMDSyncMessageExtra)` fanout on the bot-edit path (`modules/bot_api/send.go:830-836`; the `/v1/message/extra/sync` route itself is only registered at `modules/message/api.go:304`) are reused as-is — **octo-im zero changes**. Migration note (round-3 nit): the P1→P2 transition on this endpoint is server-side only — P1's blanket-reject errcode is **retired, not repurposed**, when this branch opens; bots that never sent type-17 edits observe nothing. | `send.go:647-800`, `send.go:830-836`, `modules/message/sql/20220414000001_message_legacy01.sql`, `api.go:304` |
| D7 | **Interactive cards are bot-sender only in P2.** Actions on incomingwebhook-sent cards (`iwh_` senders) are rejected — webhooks have no event-consuming runtime to receive `card_action`. Webhook cards stay display-only until a webhook callback story exists (not planned). OBO exclusion (P1 Decision 2b) carries over unchanged. | P1 Decision 2b; incomingwebhook has no poll loop |
| D8 | **Event lifecycle & timeout contract.** The `card_action` **actionable window is 24h, one shared constant carried by the D4 dedup-key TTL — NOT by queue eviction**: the reused `/v1/bot/events` ZSet refreshes the whole key's `Expire` on each enqueue and does **not** evict individual members by age, so a stale event is not dropped by the queue. Past the window the bot MUST treat a re-surfaced event as a UX no-op (no late card rewrite; auditing is fine) — and what actually neutralizes a re-delivered old event is the bot's `event_id`-keyed idempotency **plus** the D3 stale-frame fail-closed (a rewrite that removed the action → a later tap 400s), not queue expiry. Bots resume via the `event_id` cursor after disconnect and MUST NOT lose in-window events. Cursor polling gives **at-least-once delivery** (a bot that crashes between processing and cursor advance re-receives) — bots MUST process `card_action` idempotently keyed on `event_id`; the queue guarantees order, not exactly-once. Client transient loading state clears after a fixed timeout (**default 10 s**, a protocol-doc constant) with the control restored to tappable — safe because D4 idempotency makes duplicate submits harmless; the client MUST NOT persist the loading state. | `events.go:103` (ZSet cursor); D4 TTL |
| D9 | **Out-of-order rewrite protection: optional `card_seq` CAS.** Concurrent/reordered frames (parallel tool workers, network retries) under plain last-write-wins can leave the card on a stale frame. The type-17 envelope gains an **optional** monotonic integer `card_seq`; when a `botMessageEdit` carries it, the server compares against the stored value and **rejects `card_seq` ≤ stored** with a dedicated i18n conflict code (fail-closed — the bot learns it raced). Absent `card_seq`, behavior stays last-write-wins (single-writer bots unaffected, zero migration). The stored value rides `message_extra` alongside `content_edit`. **POC-verified** on real WuKongIM (stale frame → 409, stored frame not overwritten). | new; complements D6 full-replacement model |
| D10 | **Card revision history** (maintainer decisions, 2026-07-06). Side table `message_card_revision (message_id, card_seq, content TEXT /*完整帧信封,可渲染*/, plain VARCHAR /*列表摘要*/, editor_uid, edited_at)`, appended by the `botMessageEdit` card branch alongside the `content_edit` overwrite; **cap 20 non-transient frames per message** (oldest evicted; cap tunable). Query API `GET /v1/message/card/revisions?message_id=&limit=` (Auth → Space → SharedUIDRateLimiter + channel-membership check — same gate as `card/action`); summary list by default, `?full=1` returns full frames. Decisions: (a) **visibility = all channel members**, same permission as message edit history; (b) **erasure allowed but explicitly recorded** — a bot may clear a message's revisions, which writes a tombstone row (`editor_uid` + time + cleared-count) that itself appears in the history; revisions are also deleted when the message is revoked; (c) **envelope gains optional `transient: true`** — progress frames (thinking/tool-state) marked transient are applied normally but never enter the revision table, so approval-state changes aren't drowned by progress noise. Rendering payoff of the full-replacement model: every stored frame is a complete renderable envelope — history view reuses the renderer as-is (read-only). | maintainer decisions #3/#4/#5 |
| D11 | **Input trust boundary** (round-3 P1-3 — previously `inputs` was an unvalidated passthrough into `event_data`). The server validates `inputs` against the **effective frame's declared `Input.*` elements** before enqueueing: every key must name a declared input id (undeclared key → 400, fail-closed); values are strings (AC submit wire semantics); `Input.Text` value ≤ 4 KiB UTF-8; `Input.Toggle` value must equal the element's `valueOn`/`valueOff` (defaults `"true"`/`"false"`); `Input.ChoiceSet` value must be one of the declared choice `value`s (with `isMultiSelect`, a comma-separated subset per AC); serialized `inputs` total ≤ 16 KiB. Consequence: `event_data.inputs` only ever carries **declared, shape-checked** values — bots still treat the content as untrusted user text, but never re-derive the shape whitelist. **`Action.Submit.data` (the action's static author context object)** is handled separately and server-authoritatively: it is **NOT accepted from the request** — the server reads the matched action's `data` straight from the **effective frame** (the bot authored it at send/edit time, where it was already whitelist- and size-validated as part of the card payload) and surfaces it **verbatim in `event_data.data`**. Same anti-forgery posture as `action_id` (D3): the client cannot invent or mutate it, so no separate request field, re-validation, or extra size cap is needed beyond the frame's existing caps. `inputs` (user-entered values) and `data` (author static context) stay **distinct keys** in `event_data`. `isRequired` enforcement stays client-side UX + bot business validation — the server guarantees shape and whitelist, not form completeness. | round-3 P1-3; AC Input.* submit semantics |
| D12 | **Producer capability discovery（能力清单动态下发）** `GET /v1/bot/card/profile` — bot-token auth, same middleware chain and quotas as other `bot_api` routes (no new rate limiter). Returns the deployment's card capability manifest: `enabled` (the `OCTO_CARD_MESSAGE_ENABLED` gate — lets producers feature-detect instead of probing with sends, where a 400 cannot distinguish "cards disabled" from "card invalid"), the accepted `profiles` set (a P1-only deployment advertises `["octo/v1"]`; P2 adds `"octo/v2"` — the set varies per deployment/release by design), `card_version`, and `limits` (payload/nodes/depth/input caps — SDKs and the first-party adapter MUST read them here, not hardcode protocol-doc constants). **The manifest is additive-only** (same evolution rule as `event_data`: fields may be added, never renamed/removed/re-typed), and it answers only the **negotiation** half of dynamic capability delivery — the **rendering** half stays client-release-bound (renderer decision B): out-of-profile content degrades via the P1 chain today, and per-element `fallback` is the named P3 mechanism for finer-grained evolution. `enabled:false` still returns 200 with the full manifest (feature detection is the point; only send paths reject). | maintainer request 2026-07-07; mirrors P1 Decision 10 server-side negotiation |

## API surface (wire definitions)

Authoritative doc: `docs/card-protocol.md` (P1 deliverable) mirrors this
section; the two are amended together or not at all.

**`POST /v1/message/card/action`** (new, AuthMiddleware → Space → SharedUIDRateLimiter):

```json
// request (body cap 64 KiB pre-decode, D3)
{ "message_id": "8234567890123456789", "channel_id": "g_9f2c...", "channel_type": 2,
  "action_id": "approve_btn", "inputs": { "comment": "LGTM" },
  "client_token": "b7a0..." }
// channel_id/channel_type MUST match the stored message — binding derives from
// the stored row (D3 anti-IDOR); client_token is a correlation id only (D4 —
// the dedup key is message_id+action_id+operator_uid). NOTE: Action.Submit.data
// is NOT sent here — the server extracts the matched action's static `data` from
// the stored frame (anti-forgery, D11) and surfaces it in event_data.data.
// response (immediate ack; a spent dedup key returns replay=true)
{ "status": 200, "data": { "accepted": true, "replay": false } }
// errors: i18n envelope — membership 403-class; non-card / non-bot-sender /
// unknown action_id / channel mismatch / undeclared-or-malformed inputs (D11) /
// iwh_-sender → 400-class codes per D3/D7/D11; enqueue failure → 5xx internal (D4)
```

**`POST /v1/bot/message/edit`** (existing endpoint `bot_api.go:245`, type-17 extension):

```json
{ "message_id": "8234567890123456789", "channel_id": "g_9f2c...", "channel_type": 2,
  "content_edit": "{\"type\":17,\"card\":{...},\"plain\":\"ignored — server recomputes\",\"card_version\":\"1.5\",\"profile\":\"octo/v2\",\"card_seq\":3,\"transient\":true}" }
// content_edit = full replacement type-17 envelope (D6), optional card_seq (D9),
// optional transient (D10 — progress frames stay out of revision history)
// errors: not-owner, cross-type mutation, whitelist/size/scheme violation,
// card_seq ≤ stored (D9 conflict code)
```

⚠️ Send-handle correction (POC-verified against official WuKongIM v2.2.4):
`/v1/bot/sendMessage`'s passthrough response carries **`message_id` only** —
`message_seq` is assigned by WuKongIM's async persistence and is 0 in the send
response. The progress-rewrite handle is therefore **message_id**; the bot
edit path already resolves the seq server-side via `IMSearchMessages` when
`message_seq` is omitted, so bots never need to poll for it.

**`GET /v1/message/card/revisions?message_id=&limit=&full=`** (new, D10 —
Auth → Space → SharedUIDRateLimiter + membership):

```json
// response (summary mode; full=1 adds the complete frame envelopes)
{ "revisions": [
    { "card_seq": 2, "plain": "审批单 #42:✅ 已通过", "editor_uid": "bot_x", "edited_at": 1751791500 },
    { "tombstone": true, "cleared": 3, "editor_uid": "bot_x", "edited_at": 1751791400 },
    { "card_seq": 1, "plain": "审批单 #42:待审批", "editor_uid": "bot_x", "edited_at": 1751791234 } ] }
```

**`GET /v1/bot/card/profile`** (new, D12 — bot-token auth):

```json
// response — additive-only manifest (fields may be added, never renamed/removed)
{ "enabled": true, "card_version": "1.5",
  "profiles": ["octo/v1", "octo/v2"],
  "limits": { "max_payload_bytes": 524288, "max_nodes": 200, "max_depth": 16,
              "max_input_text_bytes": 4096, "max_inputs_bytes": 16384 } }
// enabled=false (rollout flag off) still returns 200 with the full manifest —
// feature detection is the point; only the send/edit paths reject.
```

**`card_action` bot event** (via existing `POST /v1/bot/events` polling): shape
frozen below (Load-bearing list) — implementation may add fields but never
rename or remove them.

## Load-bearing list

<!-- touches tags: wire-contract, trust-boundary, bot-api, rate-limit, space,
     isolation, error-response, i18n, testing -->

- **New authenticated write endpoint** (`rate-limit`, `space`, `isolation`,
  `trust-boundary`): `/v1/message/card/action` — the first client-initiated
  card write path. Mount order (`AuthMiddleware` → Space → `SharedUIDRateLimiter`)
  per repo rule; **stored-message channel binding (D3 anti-IDOR), membership,
  sender-identity, stored-action checks, the D11 input whitelist, and the
  64 KiB body cap** are the P1 trust model's layer (c) and MUST NOT be
  weakened.
- **Bot event queue wire contract** (`wire-contract`, `bot-api`): the
  `card_action` `event_data` shape is **frozen by this brief** — the
  implementation may add fields but never rename or remove them:

  ```json
  {
    "event_type": "card_action",
    "event_data": {
      "message_id": "8234567890123456789",
      "channel_id": "g_9f2c...",
      "channel_type": 2,
      "space_id": "sp_01H...",
      "action_id": "approve_btn",
      "data": { "action": "approve", "record_id": 42 },
      "inputs": { "comment": "LGTM" },
      "operator_uid": "u_123",
      "client_token": "b7a0…",
      "acted_at": 1751791234
    }
  }
  ```

  Additive-event-type tolerance documented for bot SDKs. `client_token` here
  is the D4 correlation id — consumers MUST NOT treat it as part of any
  idempotency identity; bot-side idempotency keys on `event_id` per D8.
  `inputs` is D11-shape-checked before enqueue (declared ids only, typed,
  size-capped) — content remains untrusted user text. `data` is the matched
  `Action.Submit`'s **static author object, server-extracted from the stored
  frame** (D11) — not client-supplied, so it is trusted-as-authored and never
  forgeable; it is present only when the action declared a `data` object.
  `space_id` (**P1-3, PR#548**) is the card's **authoritative origin Space**,
  server-resolved from the stored row — GROUP/COMMUNITY_TOPIC via the group's
  `SpaceID`, PERSONAL via the `space_id` the send path injected into the stored
  payload — **never** the acting operator's request-context Space (which
  `SpaceMiddleware` only membership-validates, so an operator who belongs to both
  Space A and Space B could otherwise mislabel an A-card action as B). It is
  **omitted (fail-closed)** when no authoritative Space resolves, mirroring the
  send path's client-space strip; consumers treat it as optional.
- **Capability manifest wire contract** (`wire-contract`, `bot-api`): the D12
  `GET /v1/bot/card/profile` response is **additive-only** — bot SDKs and the
  first-party adapter key feature detection and limits off it, so renaming or
  removing a field is a breaking change on par with touching `event_data`.
  Values must be sourced from the `pkg/cardmsg` constants (single authority),
  never duplicated literals.
- **`content_edit` semantics shared with RichText edit** (`bot-api`,
  `wire-contract`): `botMessageEdit` (`send.go:647`) gains a type-17 branch
  next to the RichText branch (P1 ships this branch as a blanket reject —
  Decision 7 of the sibling brief; P2 replaces the reject with `cardmsg`
  validation + D9 CAS); the user-facing `/v1/message/edit`
  (route reg `modules/message/api.go:307`; handler + RichText-only normalization
  at `:772`/`:856`) must NOT accept type-17 edits, permanently
  (users don't own bot cards; assert, don't assume).
- **Idempotency store** (`trust-boundary`): Redis dedup keys
  `(message_id, action_id, operator_uid)` + the D4 claim→enqueue→confirm
  ordering are a correctness mechanism — TTL/key-shape/ordering changes are
  wire-contract-adjacent (replay window semantics) and belong in the
  protocol doc.
- **CMD fanout scale**: card refreshes ride `/extra/sync` CMDs — same volume
  class as existing edits/reactions; no new mechanism, but note bursty bots
  (progress-frame updates) are bounded by the bot API's existing quotas —
  protocol doc recommends **milestone-cadence rewrites (≥ 2–5 s or ≥ 25 %
  steps)**, not per-second progress bars.
- **Error responses** (`error-response`, `i18n`): all new rejections via
  `httperr.ResponseErrorL` + registered codes + zh-CN translations; i18n
  make-target suite green; guard-test lists updated.
- **Protocol doc**: `docs/card-protocol.md` (P1 deliverable) already contains
  this full action contract (incl. the D4 `action_id` logical-instance rule,
  the D8 client timeout constant, the D9 `card_seq` semantics, the
  milestone-cadence guidance, and the rule that **the message list / sync
  response is the authoritative card state** — `content_edit` overlay over
  payload, no separate card-state fetch API exists or will exist);
  implementation must not drift from it — the doc and this brief are amended
  together or not at all.

## Integration note — OpenClaw channel adapter (repo-verified 2026-07-06)

`Mininglamp-OSS/openclaw-channel-octo` is the org's OpenClaw channel adapter
and the first expected consumer of this contract. Source-verified state and
the mapping onto this brief:

- **Send path exists**: `src/api-fetch.ts` (`sendRichTextMessage`) already
  POSTs `/v1/bot/sendMessage` with the bot token; a card send is the same
  call with a type-17 envelope body (a `sendCardMessage` sibling — no new
  auth or transport; the adapter's internal message-type enum, 1–14 today,
  gains 17).
- **Agent progress frames** (thinking / tool_call / tool_result) map onto D6
  full-frame rewrites via `/v1/bot/message/edit`, keyed by the `message_id`
  returned from send (see the send-handle correction above). Progress frames
  SHOULD set `transient: true` (D10) and SHOULD carry `card_seq` (D9), since
  agent runtimes emit concurrent tool updates.
- **The only net-new mechanism is the event consumer**: the adapter has no
  `/v1/bot/events` polling loop today (verified — its channel layer is
  send/receive-message only), so interactive cards (D3–D5) require adding
  the cursor-polling consumer. Display-only agent-state cards need none of
  it and ship on send+edit alone.
- **Feature detection**: the adapter SHOULD call the D12 capability manifest
  at startup (enabled / profiles / limits) and fall back to its existing
  richtext path when `enabled:false` or the required profile is absent —
  no 400-probing, no hardcoded limits.
- Adapter-side work is tracked in that repo; nothing there changes this
  contract — the adapter is a normal bot producer behind the same ingress
  validation.

## Out of scope

- **P3**: `Action.Execute`/auto-refresh, templating/data-binding,
  `Action.ShowCard`/`ToggleVisibility`, ephemeral (仅本人可见) responses,
  multi-step forms, cross-ecosystem card mapping, designer tooling,
  bot-side real-time event delivery (D5), **per-element `fallback`**
  (AC-standard element-level degradation — the rendering half of capability
  evolution: a future out-of-profile element, e.g. a chart, carrying a
  whitelisted fallback inside the same frame; validation rule to be specced
  when the first post-v2 element lands. Until then, out-of-profile chart
  content ships as a producer-rendered `Image` — zero protocol change).
  (Card revision history moved INTO scope as D10 by maintainer decision.)
- Capability discovery for **incomingwebhook** producers (D12 is bot-token
  only) — fire-and-forget webhook callers detect via response codes; giving
  the webhook URL a query surface is a separate decision.
- Webhook-sender card interactivity (D7) and any incomingwebhook callback
  mechanism.
- OBO×card (stays rejected per P1 Decision 2b).
- Client renderers (input controls, loading states) — client repos, per the
  P1 Responsibility split.
- **octo-im**: zero code changes (refresh rides existing CMD channel).
- Synchronous action responses (card returned in the action HTTP response).
- **Stream text messages** (known boundary, source-verified 2026-07-06): the
  octo-im fork's public HTTP API does not expose `/streammessage/start|end`
  (octo-lib `IMStreamStart` would 404; `modules/robot/api.go:340`'s stream
  path status against the fork needs separate confirmation). Card progress
  display does NOT depend on streams — it rides D6 rewrites; combining a
  final card with streamed text requires re-enabling streams first, tracked
  outside this task.

## Acceptance

All machine-checkable unless noted:

- `go test ./pkg/cardmsg/... ./modules/message/... ./modules/bot_api/... ./modules/robot/...` pass; i18n suite (`make i18n-extract && make i18n-extract-check && make i18n-lint`) green with zh-CN entries.
- **Happy path e2e**: operator in channel taps action on a bot card →
  endpoint acks → `getEvents` returns one event with
  `event_type="card_action"` and the frozen `event_data` shape → bot calls
  `botMessageEdit` with a type-17 `content_edit` → `message_extra` updated,
  CMD emitted on `/extra/sync` channel.
- **Idempotency (D4)**: same `(message_id, action_id, operator_uid)`
  submitted twice **with different `client_token`s** → second call acks as
  replay, bot event queue contains **exactly one** event; **enqueue-failure
  recovery**: with event enqueue failing (injected), the endpoint returns the
  5xx internal envelope and the dedup key is released — an immediate retry
  succeeds (no 24h lockout).
- **Event lifecycle (D8)**: a bot polling from a stale `event_id` cursor
  receives all in-window `card_action` events in order (at-least-once — a
  test asserts re-polling the same cursor re-delivers, documenting the
  bot-side event_id idempotency requirement); the retention window and the
  D4 idempotency TTL are one shared constant (asserted by test); a contract
  test pins the `event_data` shape to the frozen example (additive fields
  only).
- **Trust model**: non-member of the channel → 403-class i18n envelope;
  target message sender is a human (non-bot) → 400 (layer-c assertion);
  `action_id` absent from the **effective** card → 400; **a tap on an
  action_id that existed in the original payload but was removed by a
  `content_edit` rewrite → 400 (D3 stale-tap fail-closed)**; action on an
  `iwh_`-sent card → 400 (D7); **cross-channel IDOR (D3)**: an operator who
  is a member of channel A but not of channel B submits the `message_id` of
  a card in B with A's `channel_id` in the request → rejected (binding
  derives from the stored row; membership is checked against the stored
  channel), asserted for both person and group channels.
- **Input validation (D11)**: `inputs` carrying a key not declared as an
  `Input.*` id in the effective frame → 400; `Input.ChoiceSet` value outside
  the declared choices → 400; `Input.Text` value > 4 KiB → 400; valid
  declared inputs arrive in `event_data.inputs` verbatim; the matched
  `Action.Submit`'s static `data` from the effective frame arrives in
  `event_data.data` (a `data` field supplied in the request is **ignored** —
  the server uses the stored frame's copy); request body over
  64 KiB → rejected pre-decode (D3).
- **Update path (D6/D9)**: non-owner bot editing another bot's card →
  rejected; type-17 `content_edit` failing whitelist/size/scheme → 400 with
  no `message_extra` write; **cross-type mutation (type-17 message edited to
  a non-17 body, or vice versa) → 400**; two consecutive rewrites with
  entirely different structures (e.g. progress FactSet frame → result
  ColumnSet frame with octo/v2 actions) both accepted (heterogeneous-frame
  test); `content_edit_hash` dedup asserted; user `/v1/message/edit` with
  type-17 body → rejected; **D9**: edit carrying `card_seq` ≤ stored → 409
  conflict i18n code, nothing stored; edit without `card_seq` →
  last-write-wins unchanged.
- **Rate limiting**: route-mount test asserts `SharedUIDRateLimiter` is
  mounted after auth on the action route (bucket reset in test setup per
  testing rule).
- **Capability discovery (D12)**: `GET /v1/bot/card/profile` returns the
  manifest with values asserted **against the `pkg/cardmsg` constants** (not
  re-typed literals); with the rollout flag off it returns 200 +
  `enabled:false` while the send path keeps rejecting (one test asserts
  both halves together); unauthenticated call → existing bot-auth rejection;
  a contract test pins the field set (additive-only evolution).
- **Whitelist**: `pkg/cardmsg` accepts `Action.Submit`/`Input.Text`/
  `Input.Toggle`/`Input.ChoiceSet` (including `selectAction` carrying
  `Action.Submit`) only under `profile: "octo/v2"`; `Action.Execute` still
  rejected; an octo/v2 card is NOT accepted where the caller pinned octo/v1;
  a frame with duplicate `Action.Submit` or `Input.*` ids → rejected (D1
  frame-uniqueness).
- **Revision history (D10)**: a non-transient card edit appends a revision
  row (frame + plain + editor + time) while `content_edit` still holds only
  the latest frame; a `transient: true` edit applies but appends nothing;
  cap eviction at 20 non-transient frames; revision clearing writes a
  tombstone row visible in the listing; the revisions API rejects
  non-members (same gate assertions as `card/action`) and its `full=1`
  frames render through the standard envelope validator.
- Guard tests + `golangci-lint run ./...` clean.
- `docs/card-protocol.md` action-contract section matches this brief
  (human-reviewed).
