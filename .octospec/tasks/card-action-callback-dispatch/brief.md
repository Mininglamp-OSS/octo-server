---
type: Task
title: "Task: card-action-callback-dispatch"
description: Build a sender-bound card-action callback dispatch layer in octo-server, so first-party services react to card actions by implementing one signed HTTP decide endpoint (no bot polling). Docs access-request approve/deny is the pilot consumer.
tags: ["message", "card", "internal", "bot-api", "trust-boundary", "wire-contract", "auth", "space", "isolation", "acl", "rate-limit", "i18n", "error-response", "observability", "testing"]
timestamp: 2026-07-15T00:00:00+08:00
# --- octospec extension fields ---
slug: card-action-callback-dispatch
upstream: "TBD — file issue; direct prior art octo-server#584 (docs-notify card producer + reserved metadata namespace), octo-server#548 (card_action loop)"
source: self
---

# Task: card-action-callback-dispatch

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add the **card-action callback dispatch layer**: octo-server consumes
`card_action` events **in-process** and delivers them to the owning first-party
service via a **signed, config-registered HTTP callback** carrying a typed
decision request. The consumer service implements **one authenticated HTTP
endpoint** that returns a decision result; octo-server owns everything else —
queue consumption, retry/backoff/DLQ, callback signing, and rendering the final
card state + applicant notification.

This replaces the rejected **pull model** (where each new consumer would have to
implement bot-event polling + ack + cursor + a bot token + card-edit + notify
calls, re-paid per service and re-implemented per language). The maintainer
confirmed docs is the **first of several** consumers (加群审批 / 权限授权 /
任务确认 …), so the recurring per-consumer cost must be paid **once, in
octo-server**, not by every downstream service.

Design intent, per new consumer:

```
新增消费方 = 实现一个带验签的 decide 端点 + 在配置里加一行路由
         (不轮询、不要 bot token、不碰 bot/IM API、语言无关)
```

This promise applies to consumers that accept the shared **standard approval
UI**. A route may bind an independent `notify_token_env`; that owner then sends
the initial card through the generic `approval_card` notify ingress without a
new producer registration or template. octo-server supplies the approve/deny
actions, owner metadata, generic fallback finalizer, and requester outcome.
Docs remains a specialized template binding. A consumer that needs a genuinely
different visual still adds one reviewed octo-server binding, but an
unrecognized owner must never run a callback and only then fail finalization.

**Pilot consumer — docs access-request approve/deny.** The docs
`access_requested` notification is upgraded from an `octo/v1` display card to an
`octo/v2` approve/deny card; the click is dispatched to docs-backend's decide
endpoint; octo-server renders the finalized card + notifies the applicant.

End-to-end (pilot):

```
A 申请文档 (docs-backend 创建 pending)
→ docs-backend 调 POST /v1/internal/notify (DocsCard kind=access_requested)
→ octo-server 用 notification bot 发 octo/v2 审批卡给审批人 B (approve/deny)
→ B 点击 → POST /v1/message/card/action (校验/claim 已现成)
→ card_action 按权威 (sender_uid=notification, owner=docs,
   action_type=access_request.decision) 命中路由注册表
   → 入内部分发队列 (registered internal owner) 而非外部 bot 事件队列
→ 进程内 dispatcher 弹出, 带 HMAC 签名 POST 到 docs 的 decide 端点
   请求: {event_id, action_id, decision, operator_uid(仅作身份断言), doc_id,
          request_id, inputs, data, channel/message ids, space_id, acted_at}
→ docs-backend 验签 → 幂等(event_id) → 重校验 B 仍为 admin → CAS pending→approved/denied
   返回: {disposition, state, requester_uid, display fields}
→ octo-server 按返回结果: 编辑 B 的卡为最终态 (走卡片信任边界 + card_seq CAS)
   + 给 A 发 v1 展示卡通知 (已获权 / 已拒绝)
→ 全部完成后 ack (从分发队列移除)
```

This brief scopes **octo-server's** deliverables. The docs-backend decide
endpoint is a hard cross-repo dependency required for E2E but out of this repo's
acceptance (§ Cross-repo dependency).

## Background

### Why callback (not pull), and the one honest cost

Three consume topologies were evaluated: standalone polling worker, pull
(consumer polls its own bot queue), and callback (octo consumes in-process +
pushes to registered endpoints). With docs confirmed as the first of several
first-party consumers, callback wins decisively on **recurring per-consumer
cost**:

| Per new consumer | pull | **callback (this brief)** |
|---|---|---|
| poll loop + event_id cursor + ack | each re-implements | octo does once |
| bot token provisioning | each needs one | none |
| card edit / notify | each calls bot/IM APIs | octo owns rendering |
| language | bot client per language | plain signed HTTP (polyglot) |
| integration shape | service bent into a bot | one inbound endpoint |

Callback also aligns better with the **existing trust boundary**: PR #577
(`internal/carddispatch`) made octo-server the *sole* in-process card
origination boundary.
In pull, docs would mutate the card via `/v1/bot/message/edit` directly; in
callback, the final-state card edit stays *inside* octo-server's card
origination/mutation boundary. Card trust, `plain`, i18n, and the 512 KiB
recheck all stay in one place. The existing dispatcher only exposes `Send`; this
task therefore extracts a small, shared internal card-mutation primitive from
the Bot API edit path rather than bypassing that path's ownership, lifecycle,
normalization, revision, CAS, and CMD-sync rules.

**The one honest cost:** octo-server has **no outbound delivery-reliability
layer today** (verified 2026-07-15: `modules/webhook/` is inbound-verify +
mobile push; `modules/webhook/hmac.go` verifies *incoming* signatures; the bot
event queue is pull-side only). So this task builds, net-new: internal dispatch
queue + retry/backoff/timeout + DLQ + outbound HMAC signing +
idempotent delivery. This is built **once** and amortized across all consumers —
the correct platform investment given the stated future.

### SSRF / trust posture

The rejected callback risks (SSRF, dynamic registration) applied to a
**data-driven** callback where the card carries a URL. This design is
**config-driven**: routes are `(sender_uid, owner, action_type) → {url,
secret_env, timeout, retry}` from server config, URLs validated against an exact
HTTPS allowlist. Cards never carry a callback URL; redirects are disabled. The
`owner` / `action_type` fields live in `Action.Submit.data` and are picked by the
reviewed octo-server template. They are still only "trusted as authored" at the
generic card protocol layer, so callback routing additionally requires the
stored message sender to equal the route's configured internal `sender_uid`.
An external Bot copying the same `data` therefore remains on the existing pull
path and cannot invoke a first-party callback.

The structured notify ingress is also producer-bound for interactive cards:
the docs pilot uses `OCTO_DOCS_NOTIFY_TOKEN`; generic routes use their own
`notify_token_env`. Each token resolves to a server-side `(sender_uid, owner)`
and only explicitly token-enabled action types. It cannot mint another owner's
card, select an owner in request JSON, or reuse the callback HMAC secret.

### Current state (verified 2026-07-15 against main @ acb12089)

- `card_action` pipeline is merged (PR #548): `POST /v1/message/card/action`
  does validate → idempotent claim → `EnqueueBotTypedEvent(msgM.FromUID, …)` →
  confirm, with `card_seq` CAS on edits (`modules/message/api_card_action.go`).
  Today it enqueues **every** action to the sender bot's event queue
  (`robotEvent:{botUID}`, Redis ZSET, `Robot.MessageExpire` TTL —
  `modules/robot/api.go:229`) for the sender bot to poll via `/v1/bot/events`.
- `docs-notify` producer (`main.go:375-388`) is bound to the `notification` User
  Bot, DM-only, **`ProfileV1` display-only**. `access_requested`
  (`modules/notify/model.go:64`, `card.go:274` `deliverDocsCardNotification`)
  builds a v1 `/d/{doc_id}` resource card — no interactive element today.
- `octo/v1` rejects interactive elements; `octo/v2` permits `Action.Submit` /
  `Input`. `carddispatch` gates `ProfileV2` producers on a non-empty
  `ActionEventOwner` (`internal/carddispatch/registry.go:154`).
- octo-docs-backend has **no outbound IM** and integration is identity-only
  (`accessRequests.ts:13-17`, per the `card-message-internal-dispatch` brief);
  it does have approve/deny domain logic. In this design docs-backend adds
  **only** the decide endpoint — no polling, no IM, no bot token.

### Web / mobile rendering

Out of scope per maintainer direction; this brief does not gate on the client
rendering of `octo/v2` interactive elements.

## Load-bearing list

Existing behaviors/contracts this change touches — review depth + rule
injection anchor here:

- **`card_action` ingest routing** (`modules/message/api_card_action.go`) — after
  the existing validate/claim, branch on `(stored sender_uid, owner,
  action_type)`: **registered internal route → internal dispatch queue**;
  external/non-internal sender → existing `EnqueueBotTypedEvent`. An action from
  a registered internal sender whose owner/action is absent or malformed fails
  closed and releases the pending claim; it never falls into an unconsumed Bot
  queue. All current validation, anti-IDOR, visibility, claim, and
  `event_data` shaping is preserved.
- **Route registry** (new, static config) — `(sender_uid, owner, action_type) →
  {url, secret_env, timeout, retry policy}`, with exact HTTPS URLs and no
  redirects. Config/env driven, never data driven. Bootstrap validates each
  internal `ProfileV2` producer's `ActionEventOwner` against a route bound to the
  same sender, so a v2 producer cannot start in a black-hole configuration.
- **Internal dispatch queue** (new, durable Redis) — one ZSET-based due queue +
  leased queue + payload hash, with Lua claim/ack/nack/reclaim/defer transitions.
  Lease ownership is token-bound; finalization heartbeats extend the matching
  lease, and expired leases are requeued. When a route is already at its
  per-route concurrency limit, the worker atomically returns the lease to ready
  with a short delay and rolls back the claim attempt; capacity deferral cannot
  burn retries, block shutdown, or starve an unrelated route. The live queue
  TTL is the same `Robot.MessageExpire` source used by D4, preserving the
  existing action-window/idempotency-window invariant.
- **In-process dispatcher** (new) — single logical consumer, multi-replica-safe
  via a Redis claim/lease (any replica pops; processing idempotent → no leader
  election). Per event: route lookup → signed callback POST (timeout) → on
  decision render card + notify → ack. Retry/backoff on transport failure;
  exhaustion → DLQ + alert.
- **Outbound delivery reliability** (new) — HMAC-SHA256 signing over a versioned
  canonical string containing method, path, timestamp, event_id, and SHA-256 of
  the exact body; retry with bounded exponential backoff, per-call timeout, and
  DLQ. HTTPS is mandatory and redirects are disabled. `event_id` is the replay
  key, so consumers need no second nonce store. Per-route concurrency is
  bounded; a general circuit-breaker platform is deliberately deferred.
- **typed-decision contract** (new wire contract) — request/response schema
  above; additive-only evolution. `event_id` is a decimal JSON string so
  JavaScript and other consumers preserve the full `int64` identity without
  precision loss. `operator_uid` is authenticated by octo but is not
  authorization: the consumer re-checks its own current ACL. Responses are
  schema/size-bounded and carry separate `disposition` and authoritative
  business `state`; the consumer must replay the same stored response for one
  `event_id`.
- **Card finalization inside the trust boundary** — extract a producer/sender-
  bound internal card-mutation primitive from the existing Bot edit path. It
  retains message ownership, revoke/delete, card-only replacement,
  `NormalizeContentEdit`, content-hash idempotency, `card_seq` CAS, revision
  append, and CMD sync. The callback path uses `event_id` as its deterministic
  `card_seq`. The consumer never calls `/v1/bot/message/edit`.
- **Standard approval finalizer** — finalization is routed by authoritative
  `(owner, action_type)`: docs uses its reviewed resource template; every other
  registered route falls back to a server-owned standard approval template.
  The fallback mutates the operator's card with `card_seq=event_id` and sends a
  v1 requester outcome from the shared notification identity. Therefore adding
  a standard approval consumer does not require an octo-server code change.
- **Standard approval ingress** — a route with `notify_token_env` dynamically
  installs one DM-only `octo/v2` producer for its owner. The owner-bound token
  may submit `ApprovalCard{action_type,title,description,data}` only for action
  types that explicitly repeat that token binding. octo injects owner,
  approve/deny decisions, labels, layout, and escaping; callers cannot submit a
  URL or arbitrary card JSON. Callback and notify secrets must be distinct.
- **Docs pilot producer** (`main.go` `cardDispatchProducerSpecs`, notify DocsCard
  path) — `access_requested` upgraded to an `octo/v2` approve/deny producer
  (`notification` sender, `ProfileV2`, `ActionEventOwner`="docs", DM); two new
  outcome kinds (`access_granted`/`access_denied`) as v1 display cards for the
  applicant. Fallback to v1 display when the flag is off.
- **Producer-bound notify ingress** — docs structured cards require
  `OCTO_DOCS_NOTIFY_TOKEN`; generic approval cards require the token named by
  their route. The legacy global token continues to authorize legacy/summary
  requests but cannot mint any interactive approval card.
- **Feature flag** — per-owner enable (e.g. `OCTO_DOCS_APPROVAL_CARD_ENABLED`)
  gating the pilot; interacts with global `OCTO_CARD_MESSAGE_ENABLED` (v2
  requires it on).
- **i18n** — approve/deny labels + outcome-card text localized per recipient
  (`i18n.OutboundLanguage`); any new error codes registered + zh-CN translated;
  denied card leaks no approver identity/reason.
- **Space isolation** — approval + outcome cards keep
  `SpacePolicySystemNotification`, DM-only, recipient membership verified.
- **Rate limiting** — `card_action` already carries `SharedUIDRateLimiter`; no
  new hand-rolled counter.

## Out of scope

- **pull / bot-polling for first-party consumers** — superseded by this layer.
  Genuinely external/third-party bots keep the existing `/v1/bot/events` pull
  path (this brief does not change it).
- **octo-docs-backend decide endpoint** — separate repo (§ below); required for
  E2E, not part of this repo's acceptance.
- **Web / mobile `octo/v2` rendering** — per maintainer direction.
- **Multi-approver card fan-out + sync** — phase 1 acts on a single approver's
  card (edited in place via the `card_action` event's own `message_id`); the
  `request_id → message_id[]` mapping is deferred.
- **Group / thread approval cards** — DM-only first.
- **Rich inputs** (permission-level select, reason text) — approve/deny only;
  `Input.*` deferred to `card-message-p3-rich-inputs`.
- **Dynamic / data-driven callback registration** — routes are static config
  only.
- **Approver selection policy** (who is B, offline/left fallback) — docs product
  decision, not octo rails.
- **Custom outcome templates for future owners** — this task ships docs plus a
  generic standard-approval template. New custom visual families remain a code
  review, but standard approval consumers do not.
- **Exactly-once applicant notification** — business decisions and terminal
  edits are idempotent, but the applicant notification remains at-least-once.
  A crash after transport success and before queue ack may produce a rare
  duplicate, consistent with the existing notify contract; no outbox is added.
- **General circuit-breaker framework** — bounded concurrency and scheduled
  retry already protect dependencies in v1.

## Acceptance

Machine-checkable on the octo-server side:

- **Routing**: only `(stored sender_uid, owner, action_type)` matching a
  registered route is enqueued to the internal queue. An external Bot copying a
  registered owner/action remains on its Bot queue. An unknown action from a
  registered internal sender fails closed and is not confirmed. Integration
  tests assert all three branches.
- **Dispatch happy path** (stubbed consumer returning `applied`): dispatcher
  POSTs a signed HTTPS request (valid versioned HMAC over method/path/timestamp/
  event_id/body hash, redirects disabled) to the
  registered URL with the typed-decision fields, then renders the final card
  (via the carddispatch boundary, `card_seq` incremented) and sends the
  applicant notification. Test asserts signature validity, request shape, card
  finalized, notify sent, event ack'd. Oversized or malformed callback
  responses are rejected before rendering.
- **Retry + DLQ**: consumer 5xx/timeout → dispatcher retries with backoff and
  does NOT ack; after configured attempts → event lands in DLQ + an alert
  metric fires; the card is NOT finalized. Test with a failing stub.
- **Route saturation**: if one route has no available concurrency slot, its
  claimed lease is token-bound deferred without consuming an attempt; another
  route remains dispatchable and dispatcher shutdown does not wait for the
  saturated slot. Tests assert all three properties.
- **Idempotency / replay**: re-delivering the same `event_id` (dispatcher crash
  before ack) re-calls decide (consumer returns the same stored typed result)
  and re-renders the byte-identical terminal frame (`card_seq=event_id`, CAS
  no-op). The applicant notification is at-least-once; tests assert no duplicate
  domain transition or card revision and explicitly allow transport duplication
  at the crash boundary.
- **SSRF guard**: non-HTTPS, credential-bearing, fragment-bearing, or
  non-allowlisted exact URLs are rejected at config load; redirects are never
  followed and proxy environment variables are ignored. Cards never carry a
  URL. Tests assert each rejection.
- **Docs pilot flag on**: `POST /v1/internal/notify` with
  `DocsCard{kind:"access_requested"}` sends an `octo/v2` card (sender
  `notification`, DM) with approve + deny actions. **Flag off**: unchanged
  `octo/v1` display card. Both tested.
- **Notify capability**: legacy `NOTIFY_INTERNAL_TOKEN` cannot submit a
  `DocsCard`; the docs token can submit only `DocsCard` requests. Missing docs
  token fails closed.
- **Generic approval capability**: `notify_token_env` is owner/action-bound;
  its token sends a server-authored v2 approval card but cannot send payload,
  summary/docs cards, another owner's action, or callback-only actions. Token
  reuse across owners or with callback/legacy/docs secrets fails startup.
- **Outcome cards**: `access_granted`/`access_denied` build v1 display cards;
  denied card body contains no approver uid/name/reason. Test asserts.
- **Second-owner onboarding**: a non-docs route (for example
  `tasks/task.decision`) obtains its initial v2 card through the generic
  `approval_card` ingress, reaches its callback, uses the standard approval
  finalizer, edits the operator card, sends the requester outcome, and ACKs.
  No owner-specific octo-server code or Bot API call is required. Integration
  coverage asserts the ingress capability and callback/fallback path.
- **Guard + tooling**: `TestMessageNoLegacyResponseError` and notify/docs guards
  pass with new files listed; `make i18n-extract-check` + `make i18n-lint` pass;
  new codes have zh-CN; `golangci-lint run ./...` clean; `go test -race` on
  touched packages green (dispatcher concurrency test with `-race`).
- **Observability**: dispatch latency, consumer error rate, retry count, leased
  count, DLQ depth, and applicant-notify failure use bounded per-owner labels.
  The repo ships metric definitions and a DLQ replay command/runbook; deployment
  alert thresholds are an operational follow-up, not claimed as code output.
- **Protocol consistency**: amend `card-message-interaction`,
  `card-message-p2-action-loop`, `card-message-internal-dispatch`, and
  `docs/card-protocol.md` to describe the sender-bound first-party callback
  branch while preserving the third-party Bot pull contract.

## Cross-repo dependency (octo-docs-backend — out of this repo's acceptance)

The **entire** docs-side work is one endpoint:

- Follow [`docs/card-action-callback-consumer.md`](../../../docs/card-action-callback-consumer.md)
  for the wire schema, canonical HMAC, TypeScript verification example,
  idempotency transaction, typed results, and retry contract.

- Implement `POST <decide-url>`: require HTTPS at deployment; verify the
  versioned HMAC signature + timestamp freshness + `event_id` replay key;
  idempotent by `event_id`; re-check `operator_uid` is still a document admin
  (octo authenticated it, but authorization stays local); `pending →
  approved/denied` CAS (concurrent clicks: first valid decision wins). Persist
  and replay the same typed response for one `event_id`:
  - `disposition`: `applied|replayed|forbidden|conflict|not_found`.
  - `state`: `pending|approved|denied|cancelled`.
  - bounded `requester_uid` is required for `approved` / `denied` (octo must
    notify the applicant); it may be omitted for other states. Display fields
    remain optional and are consumed only by reviewed octo templates.
- No polling, no bot token, no card/IM/notify code. Grant is source-of-truth;
  the applicant notification is octo's best-effort render (retry independent of
  the grant).

## Locked implementation decisions (maintainer-confirmed 2026-07-15)

1. **Sender** — shared `notification` User Bot; callback consumers need no Bot
   token. Sender binding is part of every internal callback route.
2. **Transport auth** — HMAC-SHA256 over a versioned canonical string, over
   HTTPS. `event_id` is the replay key; no second nonce store and no mTLS in v1.
   Secrets are referenced by env name and never stored in shared config/logs.
3. **Queue** — Redis ZSET ready/leased/DLQ state machine with token-bound Lua
   claim/ack/nack/reclaim/defer. Capacity defer is attempt-neutral and scheduled
   after a short delay. No leader election and no Redis Stream abstraction.
4. **Standard card lifecycle** — octo owns the generic initial approval
   template, final template, requester outcome, and shared mutation primitive.
   A route-bound notify token provides the only generic ingress. Docs binds a
   specialized template; all other routes use the standard fallback. A new
   standard consumer needs only secrets + route + decide endpoint; a genuinely
   new visual template still requires an octo-server code review.
5. **DLQ** — bounded retries, then DLQ retained 30 days; alert + manual replay
   command/runbook. No automatic DLQ replay in v1.
6. **Protection** — per-route concurrency + timeout + exponential backoff;
   general circuit breaker deferred.
7. **Retention** — live queue TTL equals `Robot.MessageExpire` (default 7 days),
   keeping D4 idempotency and actionable event windows aligned.
8. **Delivery semantics** — callback decision and terminal card mutation are
   idempotent; applicant notification is explicitly at-least-once.
