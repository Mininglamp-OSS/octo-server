---
type: Task
title: "Task: card-message-internal-dispatch"
description: Add the only supported in-process path for business modules to send trusted interactive cards, grounded in the summary-forward and docs-share scenarios.
tags: ["message", "card", "internal", "trust-boundary", "wire-contract", "auth", "space", "isolation", "acl", "rate-limit", "observability", "testing"]
timestamp: 2026-07-13T17:00:00+08:00
# --- octospec extension fields ---
slug: card-message-internal-dispatch
upstream: octo-server#571
source: self
---

# Task: card-message-internal-dispatch

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add `internal/carddispatch` as the only supported path for an in-process
business module to originate an `InteractiveCard` (`type=17`) message, so that
server-side scenarios — smart-summary results delivered/forwarded into chats as
cards, docs share-link cards, and future system cards — have one reviewed,
trusted origination boundary instead of ad-hoc sends.

The dispatcher binds a reviewed producer capability to one configured active
bot identity, authorizes the exact Space and target using live database state,
owns the card envelope, applies the global and per-producer gates, runs
`cardmsg.Validate` / server enrichment / `cardmsg.Finalize` / final serialized
size recheck in a fixed order, makes at most one `SendMessageWithResult`
transport attempt per invocation, and emits bounded metrics and structured
logs.

This closes the architectural hole where a business module (or an internal
S2S caller relaying through one) could reach `config.Context.SendMessage` /
`SendMessageWithResult` with a hand-built type-17 map and skip sender trust,
Space/target ACLs, authoritative `plain`, or the final post-mutation 512 KiB
check. The cross-repo research below shows this hole is not hypothetical:
`POST /v1/internal/notify` already forwards a caller-supplied payload map
verbatim as the trusted `notification` User Bot (Decision 14 closes that
specific gap in this task).

No new HTTP endpoint is added. Existing Bot API, legacy Robot API, and
Incoming Webhook producers keep their current ingress-specific paths in this
task; the new boundary governs server-internal business producers. The only
behavior change to an existing endpoint is the fail-closed card-payload
rejection on `/v1/internal/notify` (Decision 14).

## Background

### Cross-repo scenario research (2026-07-13)

The two motivating scenarios were traced end-to-end across the four repos.

**Scenario A — 智能总结投递/转发到会话 (smart-summary → chat):**

- `octo-smart-summary` is a standalone Go sidecar (separate binaries
  `summary-api` / `summary-worker`), not an octo-server module. After a summary
  task terminates, the worker pushes a **plain-text DM** itself:
  `POST {OCTO_API_URL}/v1/internal/notify` with header `X-Internal-Token`
  (`SUMMARY_NOTIFY_TOKEN`), body payload hardcoded to `{"type":1,"content":…}`
  (`internal/notify/notify.go:283-291`, `internal/notify/client.go:83,156`).
  Per-recipient dedup/retry state lives in its `summary_notification` table.
- Delivery back into the **origin group/thread** is explicitly a future
  enhancement (`internal/notify/target.go:34-37`); today every origin falls
  back to a creator DM. The repo has zero card/type-17 code.
- On the octo-server side, `modules/notify` owns that ingress:
  `POST /v1/internal/notify` and `/v1/internal/notify/batch`
  (`modules/notify/api.go:90-95`), gated by `NOTIFY_INTERNAL_TOKEN`
  (fail-closed when unset, constant-time compare). It verifies Space
  membership, then fans out **DM-only** sends via
  `config.NewPersonalMsgSendReq` from the static system bot
  `notification` with a bounded 20-goroutine pool (`api.go:243-303`).
- The `notification` sender is a **full User Bot**: `ensureNotifyBot`
  provisions the `user` row, `app` row, and a `robot` row with `status=1`
  (`modules/notify/bot_manager.go:52-87`). `modules/botidentity` therefore
  resolves it as an active bot and `modules/cardtrust` trusts it as a card
  sender.
- In `octo-web`, "转发到聊天" for a summary sends the summary text as plain
  `MessageText` chunks via the WuKongIM JS SDK
  (`packages/dmworksummary/src/pages/SummaryDetailPage.tsx:1018-1043`).

**Scenario B — docs 文档分享链接卡片 (docs share-link card):**

- `octo-docs-backend` (TypeScript/Hocuspocus) has **no outbound IM capability
  at all** — stated in source (`src/api/routes/accessRequests.ts:13-17`); its
  octo-server integration is identity-only (token→uid, uid→profile). Share-to-
  chat is split: docs-backend authorizes recipients (`forward-grant`, invite
  tokens); the message itself is sent by the web client.
- In `octo-web`, doc sharing sends a plain `MessageText` whose body is
  Markdown `**title**\n[url](<url>)` via the JS SDK
  (`packages/dmworkbase/src/Components/WKBase/index.tsx` `runDocForward`,
  `ForwardModal/forwardMessageText.ts:55-58`). There is no link-card/unfurl
  rendering.

**Established platform facts relevant to both:**

- The web client is **receive/render-only** for type-17 by design ("波 1 web
  不发送 type-17", `packages/dmworkbase/src/Messages/InteractiveCard/`
  `InteractiveCardContent.ts:22,59`); only re-forwarding an already-received
  display-only card exists. So every card origination for these scenarios must
  happen server-side — exactly the boundary this task builds.
- Inside octo-server, only three modules can emit type-17 today, each with its
  own ingress trust: `modules/bot_api/send.go`, `modules/robot/api.go`,
  `modules/incomingwebhook`. All other `SendMessage*` call sites send fixed
  non-card types from static system identities (`notify`, `botfather`,
  `app_bot` welcome, group/thread/user system tips).
- **Live gap:** `modules/notify.deliverNotification` forwards the
  caller-supplied payload map to transport without any type restriction. A
  holder of `NOTIFY_INTERNAL_TOKEN` can thus emit a hand-built `type=17` map
  as the **trusted** `notification` bot, skipping `cardmsg.Enabled`,
  `Validate`, `Finalize`, authoritative `plain`, and the 512 KiB gates. The
  planned AST guard cannot see this shape (no card construction in
  octo-server source — the map arrives over HTTP), so it needs the runtime
  gate in Decision 14.

### Existing authoritative pieces

- `pkg/cardmsg` is the card protocol authority:
  - `cardmsg.Enabled()` reads `OCTO_CARD_MESSAGE_ENABLED`;
  - `Validate` enforces the profile/version, structure, URL, depth/node and
    complete-payload limits;
  - `Finalize` overwrites untrusted `plain` and rechecks the enriched payload;
  - `RecheckPayloadSize` checks the final bytes after any later mutation;
  - `IsCardPayload` / `IsCardRawPayload` are the single card-shape detectors;
  - profiles: `octo/v1` is display-only, `octo/v2` is the only interactive
    profile (`profiles.go:19-22`).
- `modules/botidentity` resolves live bot authority:
  `robot.status=1` or `app_bot.status=1`; `user.robot=1` is presentation only;
  ambiguous or failed lookups fail closed.
- The three current external ingress producers already implement their own
  trust and permission boundaries before calling the IM transport:
  - `modules/bot_api/send.go`;
  - `modules/robot/api.go`;
  - `modules/incomingwebhook/api.go` + `card.go`.
- `modules/notify` owns the internal S2S notification ingress
  (`/v1/internal/notify*`, `X-Internal-Token`) and the static `notification`
  User Bot (see research above). It is an existing trust surface of this
  boundary, not a card producer.
- `pkg/cardmsg/producer_completeness_test.go` already proves every producer that
  expands `mention.ais` performs a final size recheck. It does not prevent a new
  internal producer from bypassing all of the other gates.
- `config.MsgSendReq` has no caller-supplied `client_msg_no`, and
  `SendMessageWithResult` has no context-aware overload. It returns the
  server-generated `message_id`, `message_seq`, and `client_msg_no` only after a
  successful transport response. Exactly-once retry cannot be truthfully
  promised by this task.

### Why this is a separate P0

The App Bot trust task (card-message-appbot-trust, PR #570 lineage) fixes
consumers of already-sent cards — display masking and the `card_action` gate.
It deliberately does not authorize new in-process producers. Internal dispatch
is a separate load-bearing boundary: a server module is not authenticated by
Bot API middleware, so it needs an explicit producer capability and cannot
inherit HTTP-layer bot/Space checks by accident.

### Industry practice alignment

The design was cross-checked against the public behavior of major IM
platforms (Slack Block Kit, Feishu/Lark interactive cards, DingTalk robots,
Microsoft Teams Adaptive Cards — the same card family octo uses). The core
decisions match established practice:

- Arbitrary rich interactive messages are bot/app-only; human clients cannot
  forge them — matches Decision 2. The separately planned user-share card is a
  fixed, display-only resource-share exception minted by the authenticated
  server with verifiable provenance, not a generic human card ingress.
- Channel authorization is checked live at send time (Slack `not_in_channel`;
  Feishu/DingTalk bots must be members of a group before posting into it) —
  matches Decision 4. For posting **without** membership, DMWork adopts the
  reviewed member-exempt mode analogous to Slack's `chat:write.public`
  (no-join posting to public channels), scoped to normal in-Space groups with
  a recorded triggering member action since DMWork has no public/private
  split; Feishu/DingTalk reach the same user-visible end state by keeping
  group robots outside the member list/count.
- Fallback text next to the rich body is app/platform-authored (Slack `text`
  alongside blocks; Teams/Feishu card summaries on push surfaces) — matches
  authoritative `plain`.
- Interaction callbacks route only to the app that owns the message, and
  webhook-only identities cannot receive interactions (Teams Incoming Webhook
  cards cannot carry `Action.Submit`; Feishu custom group bots send
  non-interactive cards only) — matches Decision 5 and the existing `iwh_`
  restriction.
- The platform boundary does schema validation plus hard size caps (Teams
  ≈28 KB per card, Slack block-count/length limits) — the 512 KiB
  `pkg/cardmsg` cap is permissive by comparison, not aggressive.
- First-party traffic goes through the same gateway as third-party traffic (no
  internal bypass) — matches Decisions 12 and 14.
- postMessage-style send APIs are not idempotent; retry semantics live in the
  caller — matches Decision 8.

Two places this design is deliberately weaker than public platforms, declared
here rather than discovered later:

- **No per-channel send-rate control** (Slack enforces ≈1 msg/s/channel;
  DingTalk webhooks 20 msg/min/group). Acceptable for the DM-only pilot; a
  reviewed per-channel rate rule is a **precondition of widening any producer
  to group/thread targets**.
- **No cluster-wide quota** — Decision 9's semaphore is per process, while
  public platform quotas are global. The cluster-cap decision is likewise part
  of the group/thread widening review, not this task.

## Method: how a scenario onboards (方法概览)

The supported end-to-end path for "some internal service wants to send a card"
is fixed by this task:

```
external sidecar service            octo-server process
(smart-summary, docs-backend, …)
        │  internal S2S ingress            ┌─────────────────────────────┐
        └─ X-Internal-Token endpoint ────▶ │ owning business module      │
           (or the feature is already      │  holds a producer-bound     │
            in-process: no ingress hop)    │  carddispatch.Sender        │
                                           └──────────────┬──────────────┘
                                                          ▼
                                           internal/carddispatch pipeline:
                                           producer gate → live bot identity
                                           → live Space/target ACL → envelope
                                           → Validate → enrich → Finalize
                                           → size recheck → ONE transport call
```

- The **producer** is always an octo-server module (the ingress endpoint's
  owner for sidecar-driven scenarios; the feature module itself for in-process
  scenarios). Sidecar services never hold card-send authority directly; their
  S2S token authenticates them to their ingress module only.
- The **sender identity** is always a configured active bot resolved live via
  `modules/botidentity` — never a human UID, never caller-supplied.
- The web client stays receive-only for type-17; no client card-send path is
  introduced for these scenarios.

This onboarding method governs **internal Bot producers**. The separately
specified user-initiated summary share is not an internal service asking to
send as a Bot: it is a user-authenticated server-minted message with the user as
sender, and therefore has its own trust boundary and brief.

Applied to the motivating scenarios (Scenario A splits into two sub-paths
with different readiness):

- **Pilot — `summary-notify` (Scenario A1, worker-driven notification):**
  `modules/notify` becomes the first producer. The existing summary
  completion/failure DM upgrades from plain text to a display-only (`octo/v1`)
  card. **Decision amended 2026-07-14:** the producer reuses the existing
  `notification` User Bot, and its text fallback uses the same identity, so the
  user sees one system-notification DM conversation. This supersedes the
  dedicated `summary` Bot choice recorded on 2026-07-13; capability isolation
  remains on the producer even though the sender identity is shared.
  The ingress, token, membership verification, and smart-summary's retry/dedup
  state machine all stay as they are. When smart-summary later implements
  origin group/thread回发, the same producer widens its allowed channel types
  in a separate reviewed change — that review must also settle the per-channel
  rate rule and cluster-cap questions (see Industry practice alignment), and
  the delivery policy is **member-exempt posting to the origin group**
  (Decision 4 mode, confirmed 2026-07-13): the bot never joins — member list
  and 群人数 are untouched — consent comes from the creating member having
  bound the task to that group, the dispatcher re-verifies group lifecycle
  and exact Space at send time, and delivery falls back to the creator DM
  when the group is no longer eligible. The dispatcher's group/thread
  authorization rules (Decision 4) are specified now so that widening is a
  config review, not a design change.
- **Separate platform — user-initiated resource share (Scenario A2 and B):**
  user-selected DM/group/thread cards are a generic platform capability, not a
  smart-summary-specific transport and not a Bot producer. The sharing user is
  the stored sender in the selected conversation; the resource owner supplies
  a signed structured intent; octo-server owns target authorization, template
  finalization, provenance, quotas, idempotency, audit, and transport. The
  generic contract is `../user-resource-share-card/brief.md`; smart-summary is
  the initial use case, while a future docs user-share card onboards as another
  provider and does not require a docs Bot. Automated docs notifications/
  actions, if desired, remain a separate Bot producer problem. The generic
  path is **not Bot API OBO**: no S2S caller
  supplies `actor_uid`, no request contains `from_uid`, and arbitrary card
  payloads remain forbidden.

### Interactive cards (octo/v2): how future action scenarios fit

Foreseeable scenarios where the recipient acts on the card — merge a PR,
close an issue, approve an access/authorization request — are deliberately
accommodated by this contract and require **no new dispatch machinery**. The
pilot's cards are display-only (`octo/v1`) with a deep link, but nothing in
the pilot blocks a later interactive producer. What changes for an
interactive producer is what it must bring, not how it dispatches:

- **The Decision 5 gate is the whole difference.** A producer registered for
  `octo/v2` must name the existing bot event consumer that owns `card_action`
  for its sender bot; the registry refuses the interactive profile otherwise
  (already in Acceptance › Capability and configuration). Sending an
  interactive card goes through the exact same pipeline.
- **The action loop is the existing one**, hardened by
  card-message-appbot-trust: recipient taps → `POST /v1/message/card/action`
  → live sender-trust check → `card_action` event on the **sender bot's**
  event queue → the bot's owning service polls `/v1/bot/events` and ACKs via
  Bot API. The dispatcher never adds a second callback route (Out of scope
  holds).
- **Sender identity choice is therefore scenario-driven.** An interactive
  producer's bot must be backed by a service that actually runs the event
  poll loop: e.g. a GitHub-integration bot whose sidecar executes merge/close
  against GitHub, or a docs bot whose backend executes the approval, then
  reflects the outcome by **editing the card** through the existing card-edit
  path (`card_seq` / `pkg/cardrevision`) instead of sending a new card.
  Display-only producers (the pilot) may use consumer-less bots; upgrading a
  producer to `octo/v2` later means giving its bot an event consumer first —
  or registering a separate producer bound to a bot that has one.
- **Executor responsibilities stay outside the dispatcher**: authorize the
  acting user in the target system at execution time (a visible button is
  NOT authorization — the event's actor identity must be checked against the
  external system's ACL), and handle the poll/ACK at-least-once delivery
  idempotently (dedup on event/card identity before executing side effects
  like a merge).

These scenarios (`devops` PR/issue cards, `docs` approval cards) are far
candidates: each onboards with its own producer table row, a named event
consumer, and its own brief for the executor side. They are listed here so
the registry/config schema is designed with the action-owner field from day
one rather than retrofitted.

### Card template and deep-link (designed, confirmed 2026-07-13)

**Template — a `ResourceCard` family in a small server-side shared library**
(e.g. `pkg/cardtmpl`), consumed by producer modules only; the carddispatch
core never imports it (Decision 11 acyclicity — the dispatcher validates via
`pkg/cardmsg`, it does not know templates). Sidecar services keep sending
structured fields (the decided ingress contract); template knowledge lives in
exactly one place. Signature:

```
ResourceCard{ icon, title, attribution?, excerpt, facts[],
              primaryAction{title,url}, localActions[]?, lang }
```

Rendered with octo/v1 whitelist elements only (`ColumnSet` icon+title header,
optional attribution `TextBlock`, excerpt `TextBlock`, `FactSet` metadata,
`ActionSet` with `Action.OpenUrl` "查看详情" plus optional
`Action.CopyToClipboard`). One template family instantiates A1 completion/
failure notifications, user-authored A2 share cards (which use the normal chat
sender UI instead of a forwarder-attribution header), and the future docs share
card. Constraints by construction: URLs are absolute https
(Decision 3d positive allowlist); the excerpt is server-truncated (~300 chars)
so payloads stay far under `MaxPayloadBytes` and today's chunked-forward hack
disappears; `plain` needs no template work (`Finalize` recomputes it); labels
render per recipient via `i18n.OutboundLanguage` (same discipline as email
templates). Guard tests: JSON snapshot per instantiation plus a test that
every template output passes `cardmsg.Validate` under `octo/v1` — template
drift fails CI.

**Deep-link — standalone web route `/s/:taskId?sp={spaceId}`**, mirroring the
battle-tested `/d/:docId` machinery (cold-load → login bounce → multi-session
sid recovery, XIN-398 test suite). Root cause of the historical removed link
(`octo-smart-summary internal/notify/notify.go:396`): summary detail has only
in-app `WKApp.route` panels (`/summary/detail`), no browser URL route — the
design closes exactly that hole. Logged-in behavior: enter the app and call
the existing `WKApp.openSummaryDetail(taskId)`; no standalone renderer is
needed in wave 1. The URL is built by the octo-server template layer from
`External` config (reuse `External.WebLoginURL`'s origin or add
`External.WebBaseURL` — decided at the enablement config review);
smart-summary passes `taskId` only and never builds URLs. Anti-regression
contract: octo-web ships a route-contract test asserting `/s/:taskId` is
registered, octo-server ships a template test pinning the link shape — a path
change is an explicit cross-repo contract change, so the "link points
nowhere" failure cannot silently recur.

## Implementation gate: pilot producer table

Before `/octospec-go card-message-internal-dispatch` enables a pilot producer,
a maintainer must confirm this table. All pilot decisions are now
**maintainer-confirmed (2026-07-13)**: concurrency, duplicate tolerance,
replicas, sender bot, message ownership, member-exempt group posting, and the
card template + deep-link design (Method › Card template and deep-link). The
remaining work on the template/deep-link is **verification, not decision**:
the pilot enablement PRs must land the `/s/:taskId` route-contract test in
octo-web and the template snapshot/Validate guard tests in octo-server before
the producer is enabled. A deliberately dormant foundation may ship first,
but it must register no production producer while any row is unconfirmed. Do
not implement a mutable generic "send as any UID" service.

| Required input | Pilot: `summary-notify` |
| --- | --- |
| Producer ID (stable, low-cardinality) | `summary-notify` |
| Owning module / constructor | `modules/notify`; it obtains its producer-bound `Sender` from the single registry composed at server bootstrap (exact wiring resolved in implementation per Decision 11 — must fit the `register.AddModule` module system without mutable package-global registration) |
| Sender bot UID configuration source | **Amended 2026-07-14: reuse the static `notification` User Bot** provisioned by `ensureNotifyBot` (`user` + `app` + `robot.status=1`). Summary cards, generic text notifications, and summary-card text fallback share one DM identity. This supersedes the dedicated `summary` Bot decision from 2026-07-13; the producer-bound capability still prevents generic notify callers from originating cards |
| Allowed channel types | DM (person) only at pilot; group/thread widening is a separate reviewed change tied to smart-summary origin回发, and that review must include a per-channel rate rule and the cluster-cap decision. Group/thread sends use the member-exempt posting mode (Decision 4): the bot joins no group, member list and 群人数 unchanged |
| Allowed card profiles / action-event owner | `octo/v1` (display-only) only; `octo/v2` forbidden — the notification Bot has no compatible summary action-event consumer polling `/v1/bot/events`, so no one could own `card_action` (see Method › Interactive cards for the upgrade path) |
| Required Space source | `NotifyReq.space_id` supplied by the internal caller and member-verified by notify's `memberCache`; the dispatcher independently re-verifies live Space/membership (Decision 3). DM policy = system-notification mode (space-member DM, Decision 4) |
| Expected peak concurrency / QPS | max-in-flight **20** per process (mirrors notify's existing bounded send pool) — **confirmed 2026-07-13** |
| Business retry/idempotency requirement | smart-summary already retries per recipient with `summary_notification` dedup state ⇒ at-least-once from the caller side; a transport-ambiguous failure may duplicate a card; dispatcher stays single-attempt (Decision 8). **Confirmed 2026-07-13: duplicates are acceptable for notification cards; no outbox** |
| Process replicas / required cluster-wide cap | **Confirmed 2026-07-13: per-process bound accepted, no cluster-wide cap required.** Record the actual replica count in the deployment/config review that enables the pilot |

## Decisions locked by this task

1. **Capability-bound API, no arbitrary sender.** A caller obtains a
   producer-specific `Sender` from an immutable registry/factory. `SendRequest`
   contains no `from_uid` and no free-form producer string. The registry binds a
   known `ProducerID` to its configured sender UID, allowed channel types,
   allowed card profiles, Space policy, enable flag, and concurrency budget.
   Unknown/unconfigured producers fail closed. No mutable package-global
   registration from module `init()` is allowed.
2. **Live bot authority.** Every send resolves the bound sender with
   `modules/botidentity`; disabled/unpublished/deleted/missing, ambiguous, and
   lookup-error identities are rejected before target queries or transport.
   Synthetic `iwh_` identities and human UIDs are never accepted as internal
   action-owning card senders. Dispatch authorization also needs the current
   User Bot `creator_uid`, or App Bot `scope` and `space_id`; extend the unified
   resolver result (or add a resolver method) so identity kind and this policy
   metadata come from one authoritative live statement/snapshot. Do not create
   a second, competing bot-kind resolver in `carddispatch`.
3. **Explicit Space, never default-by-accident.** Every request carries a
   trusted `SpaceID` obtained by the owning module, not from card content. The
   dispatcher verifies that the Space is active, the sender is authorized in
   that exact Space, and the recipient is an active member for DMs; groups and
   threads must have the same authoritative Space. It does not choose the first
   membership for a multi-Space bot. The verified value is the only `space_id`
   placed in the envelope and is injected into DM payloads.
4. **Target permission is live and at least as strict as Bot API self-send.**
   App Bots are DM-only, require the existing friend opt-in, and a scope=space
   App Bot may only target an active member of its own active Space. Platform
   App Bots require an active target Space and active recipient membership.
   User Bots require creator/friend authorization for DMs — except a producer
   whose registered Space policy is the reviewed **system-notification DM
   mode**, which instead authorizes DMs by active membership in the verified
   Space (the semantics `modules/notify` delivers today; industry analog:
   Slack app DMs and Feishu in-tenant application messages need no
   friendship). The mode is per-producer registry config, never
   caller-selectable, and the pilot uses it. Group/thread sends
   require an active internal bot member, a normal parent group, a non-deleted
   active thread, and exact Space equality — except a producer with the
   reviewed **member-exempt group posting mode** (confirmed for the summary
   producers 2026-07-13): the bot posts to a normal group or valid thread in
   the exact verified Space **without a membership row**, so it never appears
   in the member list and never increases the member count
   (`QueryMemberCount` today counts `robot=1` rows, so joining would inflate
   群人数); every such send must be attributable to a recorded triggering
   member action in the owning module (task creation, forward click), and
   group/thread lifecycle plus Space equality remain verified fail-closed at
   send time. Member-exempt is *no-membership-required*, not
   *membership-ignored*: an **explicit ban is still honored** — a bot carrying
   a blacklisted group-member row (`group_member.status=2`) was deliberately
   removed by an admin and is denied even in this mode. Transport needs no
   membership (group system tips already post
   member-less, `modules/group/event.go:36`); this is policy, and the
   industry analog is Slack `chat:write.public` no-join posting, scoped to
   normal in-Space groups because DMWork has no public/private split. The
   dispatcher never mutates membership to make a send succeed (no auto-join);
   a producer without this mode that is not an active internal member fails
   closed. DB errors fail closed. OBO
   is not supported by internal dispatch — consistent with the card protocol's
   Bot-producer prohibition of OBO cards (card-message-protocol Decision 2b:
   Bot API rejects card + `on_behalf_of` with
   `err.server.bot_api.card_obo_forbidden`, `modules/bot_api/send.go:92-105`;
   the separately reviewed user-authenticated server-minted share authority is
   not Bot OBO and is not an internal-dispatch option).
5. **Dispatcher owns the envelope.** The public request accepts a card document
   plus a supported profile, not an arbitrary message payload. The dispatcher
   sets `type`, `card_version`, and profile; caller-supplied `plain`, `space_id`,
   OBO keys, sender fields, subscribers, stream fields, or other top-level
   transport metadata are impossible through the API. Initial scope rejects
   mentions; a later mention feature must extend the dispatcher and retain the
   post-expansion recheck rather than bypass it.
   An `octo/v2` producer must name the existing bot event consumer that owns
   `card_action`; a producer with no such consumer is restricted to the
   non-interactive profile. The dispatcher does not create a second callback
   route for internal modules.
6. **Dispatcher owns an immutable input snapshot.** The API accepts a standard
   Adaptive Card document as bytes (or defensively deep-copies an equivalent
   typed value) before validation. It never mutates or retains caller-owned maps
   or slices. Concurrent caller mutation therefore cannot race validation,
   final size checking, or transport serialization.
7. **Fixed validation order.** The order is part of the contract:
   structural request check -> global feature gate -> producer policy/gate ->
   acquire producer in-flight slot -> snapshot input -> live bot identity ->
   live Space/target authorization -> build envelope -> `cardmsg.Validate` ->
   authoritative Space enrichment -> `cardmsg.Finalize` ->
   `cardmsg.RecheckPayloadSize` on the exact wire map -> serialize that map ->
   recheck context -> at most one transport call. No mutation is allowed after
   the final recheck; the serialized bytes passed to `MsgSendReq.Payload` are
   the bytes whose map passed that check.
8. **One transport attempt, no hidden retry.** The dispatcher calls
   `SendMessageWithResult` once and returns its typed result/error. It does not
   start a goroutine, retry an ambiguous transport failure, or claim
   exactly-once delivery. Callers must not blindly retry transport-ambiguous
   failures. If the pilot requires retries, stable `client_msg_no` support and
   an outbox/idempotency design must be added to this brief before implementation.
9. **Bound internal blast radius.** Each producer has a small, reviewed
   in-process max-in-flight limit. Saturation returns a typed `busy` error and
   never waits unboundedly. This is business concurrency control, not an HTTP
   rate limiter and not a hand-written Redis request-frequency counter. The
   limit is per process; effective cluster concurrency is replicas multiplied
   by this value. If the pilot needs a cluster-wide QPS/concurrency cap, that is
   a separate explicit distributed-control requirement, not an implied property
   of this semaphore.
10. **Bounded observability.** Emit attempt/result counters, duration, and
   in-flight metrics with only reviewed low-cardinality labels (`producer`,
   normalized target kind, bounded result). Structured logs include request ID,
   producer, sender kind, Space, target kind and returned message IDs on success;
   never card JSON, user inputs, tokens, or channel/user IDs as metric labels.
11. **Single composition point and acyclic dependencies.** Construct one
    registry per application context, then inject only the producer-bound
    `Sender` into the owning module. Multiple registries must not create
    independent copies of the same producer's semaphore/metrics. The dispatcher
    may depend on narrow repository/transport interfaces and
    `modules/botidentity`, but must not import a potential producer module;
    production adapters are composed outside the core package to avoid cycles.
12. **Mechanized no-bypass rule.** Add an AST/source guard to CI. The exact
    existing external ingress producers and `internal/carddispatch` are the only
    allowlisted type-17 transport owners. Allowlist exact existing symbols or
    call sites with reasons, not an entire directory that could hide a new
    bypass. A new non-test package that combines card construction/type-17
    references with direct `SendMessage` or `SendMessageWithResult` fails the
    guard and must onboard through the dispatcher (or add a narrowly reviewed
    external-ingress exemption). The guard is a review backstop for known Go
    syntax/data-flow shapes, not a claim that static analysis is a runtime
    security boundary. It cannot see payload-passthrough shapes where the card
    map arrives over HTTP — that class is closed at runtime by Decision 14.
13. **Request-time authorization, no lock across transport.** Identity and ACL
    state are queried without a decision cache on every invocation. The
    dispatcher does not hold database locks or a transaction open across the IM
    network call. A concurrent unpublish, membership removal, or Space disable
    can therefore race with the already-authorized in-flight request; later
    requests fail closed. If the pilot requires linearizable revocation of an
    in-flight send, that stronger cross-system protocol must be designed before
    implementation rather than claimed by this package.
14. **Existing internal notify ingress fails closed on card payloads.**
    `modules/notify.deliverNotification` rejects any request whose payload
    `cardmsg.IsCardPayload` reports as a card (covering `/v1/internal/notify`
    and `/batch`) before membership verification and with zero transport
    calls, responding through a registered `pkg/errcode` code (e.g.
    `err.server.notify.card_not_allowed`, 400, non-Internal) via
    `httperr.ResponseErrorL`. Today's only known caller (smart-summary) sends
    `type=1` text exclusively, so no legitimate traffic changes. This gate is
    the runtime complement to Decision 12 for the trusted `notification`
    sender; it may only be removed by onboarding notify's card path through
    `internal/carddispatch` (the `summary-notify` pilot).

## Proposed internal contract

Names may change during implementation, but these authority boundaries may not:

```go
type ProducerID string

type Target struct {
    SpaceID    string
    ChannelID  string
    ChannelType uint8
}

type Card struct {
    Profile  string
    Document json.RawMessage // standard Adaptive Card document; copied on entry
}

type Sender interface {
    Send(ctx context.Context, target Target, card Card) (*Result, error)
}

type Result struct {
    MessageID   int64
    MessageSeq  uint32
    ClientMsgNo string
}
```

`context.Context` must be checked before expensive DB work and again immediately
before transport, and used for tracing/log correlation. The current octo-lib
transport is not cancellable; cancellation after the call starts cannot stop the
request. The implementation must document that boundary and must not fake
cancellation by leaking a goroutine. A context-aware transport extension is a
separate octo-lib change unless accepted into this task before implementation.

Fan-out stays caller-side: a producer sending one card to N recipients calls
`Send` N times inside its own loop (as notify does today), each call
individually authorized and bounded by the producer's in-flight budget. The
dispatcher does not accept target lists.

Internal errors are typed Go errors with a stable bounded category:
`invalid_request`, `feature_disabled`, `producer_disabled`,
`identity_untrusted`, `target_denied`, `card_invalid`, `payload_too_large`,
`busy`, and `dispatch_failed`. They are not HTTP/i18n envelopes. A future HTTP
caller must map them through that endpoint's registered `errcode` facade.

## Load-bearing list

<!-- touches: trust-boundary, wire-contract, auth, space, isolation, acl,
     rate-limit, testing -->

- **Producer trust (`trust-boundary`, `auth`)** — only immutable reviewed
  producer capabilities can obtain a sender; live bot tables remain the
  lifecycle authority. The send request has no caller-controlled sender field;
  in-repository misuse remains covered by code review and the source guard.
- **Tenant/target authorization (`space`, `isolation`, `acl`)** — explicit
  Space binding, active membership, group/thread lifecycle, DM relationship,
  App Bot scope and channel-type restrictions all fail closed before dispatch.
- **Card wire contract (`wire-contract`, `trust-boundary`)** — `pkg/cardmsg`
  remains the sole profile/schema authority; `plain` and `space_id` are
  server-authored, and the exact serialized wire payload stays <=512 KiB.
- **Internal notify ingress (`trust-boundary`, `wire-contract`)** —
  `/v1/internal/notify*` keeps its text-notification contract for existing
  callers (smart-summary sends `type=1`); its auth stays `X-Internal-Token`
  fail-closed; it additionally stops relaying card-shaped payloads
  (Decision 14). The `summary-notify` card producer and fallback reuse the
  existing `notification` Bot provisioning/readiness path.
- **Existing ingress behavior (`bot-api`, `wire-contract`)** — Bot API, Robot
  API, and Incoming Webhook validation order, error envelopes, OBO behavior,
  rate limits, and metrics do not change merely because the internal package is
  added. Their allowlist entries are explicit and source-tested.
- **Transport failure semantics** — one call, no automatic retry, no false
  exactly-once guarantee. A non-OK/ambiguous IM response returns
  `dispatch_failed` and records a bounded result metric.
- **Authorization consistency** — every invocation reads current identity and
  ACL state without a decision cache, but no DB lock spans the IM call; rollback
  disables new sends and does not retract an already-authorized in-flight send.
- **Overload control (`rate-limit`)** — producer concurrency is bounded in
  process and configurable/reviewed; the replica multiplier is explicit and no
  new public route or Redis frequency counter is introduced.
- **Observability** — all terminal branches increment exactly one bounded result
  outcome; duration/in-flight are correct under error/panic-safe defers; content
  and high-cardinality identifiers never become metric labels.
- **Testing (`testing`)** — policy matrix, fail-closed storage paths, exact
  validation order, final bytes, no-dispatch-on-denial, one-call success/error,
  overload behavior, metrics, and the direct-send guard require tests.

## Out of scope

- Enabling the `summary-notify` pilot (switching the summary notification body
  from text to a card) while any ⚠️ row in the pilot table lacks maintainer
  sign-off. The foundation plus the Decision 14 gate may ship dormant first.
- Implementing smart-summary origin group/thread回发 (its `target.go` routing
  or widening the pilot's channel types) — a separate task in
  octo-smart-summary + a reviewed config change here, whose review must settle
  per-channel rate control and the cluster-wide cap (see Industry practice
  alignment).
- A platform-level "group robots" presentation change — segmenting `robot=1`
  members out of the member list and `QueryMemberCount` (Feishu/DingTalk
  style, also fixing existing AI bots inflating 群人数). It is the long-term
  alternative to member-exempt posting and a separate product task.
- User-initiated resource-card sharing: this is a separate provider-based,
  user-authenticated server-minted platform supporting DM/group/thread with the
  sharing user as sender. It does not weaken this dispatcher's static-Bot
  sender contract; see `../user-resource-share-card/brief.md`. Smart-summary
  and docs user shares onboard through separate provider briefs rather than
  creating new Bot producers.
- Any client-side card sending in octo-web; the web stays receive-only for
  type-17.
- Replacing or refactoring `/v1/bot/sendMessage`, legacy Robot API send,
  Incoming Webhook push/card send, or their public error contracts. Beyond the
  Decision 14 card gate, `/v1/internal/notify*` behavior (text payload
  passthrough, DM fan-out, membership filtering, legacy error envelope) is
  unchanged.
- Incoming Webhook card actions/callbacks; webhook identities still have no bot
  event consumer.
- A new internal `card_action` callback/event bus. Interactive profiles retain
  the existing sender-bot event ownership and poll/ACK contract.
- OBO, fan-out inside the dispatcher, streams, subscribers, transient/no-persist
  cards, mention expansion, broadcast cards, typing/read receipts, or card
  edits/revisions.
- New card profiles/elements/actions, renderer changes, or changes to
  `pkg/cardmsg` limits.
- New HTTP/gRPC routes, auth middleware, Space middleware, or client SDKs.
- Durable outbox, retries, caller-supplied/stable `client_msg_no`, exactly-once
  delivery, or transport cancellation. These require an explicit contract
  change, not an invisible dispatcher retry.
- Linearizable revocation across MySQL authorization state and the WuKongIM
  network call, or recall of an already-dispatched card.
- A distributed/cluster-wide rate or concurrency limiter. The initial
  max-in-flight budget is per process unless the pilot planning gate requires a
  stronger cap.
- Database migrations solely for the dormant dispatcher foundation.
- A dynamic admin UI or runtime API for registering producers. Registration is
  code/config reviewed and immutable for the process lifetime.

## Acceptance

### Planning gate

- All `summary-notify` pilot decisions are maintainer-confirmed (2026-07-13,
  with the sender amended 2026-07-14): concurrency, duplicate tolerance,
  replicas, sender bot (shared `notification` User Bot),
  member-exempt group posting, and the card template + deep-link design.
  Registering/enabling the pilot additionally requires the design's guard
  tests to exist and pass (see Acceptance › Card template and deep-link).
  Until then the implementation is explicitly limited to a disabled
  foundation plus the Decision 14 gate, with no claim of end-to-end
  production usefulness.
- User-initiated resource providers are governed by the separate generic share
  platform and never become dynamic-sender registrations in this Bot-only
  dispatcher.

### Capability and configuration

- `internal/carddispatch` exposes a producer-bound `Sender`; send requests have
  no sender UID or arbitrary payload/transport fields.
- Registry tests prove unknown, duplicate, disabled, and missing-config
  producers cannot obtain/use a sender. The registry is immutable after
  construction, exactly one production instance owns all producer budgets, and
  test instances do not share mutable global state.
- Enabled producer startup/config validation checks non-empty bounded IDs,
  configured sender UID, allowed target kinds/profiles, positive max-in-flight,
  and Space policy. An interactive profile also requires a named compatible
  action-event owner. Invalid config fails only that producer closed and emits
  one safe log plus a bounded configuration-error metric; it never falls back to
  a caller UID or a default producer.

### Identity and target authorization

- Table-driven unit tests plus DB-backed tests cover active/inactive/missing
  User Bot and App Bot, presentation-only `user.robot=1`, cross-table ambiguity,
  live User Bot creator and App Bot scope/Space policy metadata, and DB errors.
  No failed branch reaches the transport capture.
- DM tests cover User Bot creator/friend allow, stranger deny,
  system-notification-mode allow for a non-friend active Space member and deny
  outside the verified Space, active Space membership, wrong/missing Space,
  App Bot platform/scope-space rules, and App Bot group/thread denial.
- Group/thread tests cover exact authoritative Space match, normal vs
  disabled/disbanded group, active/internal/blacklisted/deleted/non-member bot,
  valid/malformed/archived/deleted thread, and DB failure. All denials fail
  closed before card finalization/transport.
- Member-exempt-mode tests prove a producer with the mode posts to a normal
  same-Space group with **no membership row and no member-list/count side
  effects**, is still denied on disbanded/disabled groups, wrong Space,
  invalid threads, and when the bot carries an explicit blacklist row
  (`group_member.status=2`); a producer without the mode is denied as a
  non-member; and no dispatch code path creates or mutates group membership.
- Multi-Space tests prove the explicit request Space is verified and the
  dispatcher never silently selects the first membership.

### Payload pipeline

- The dispatcher constructs `type=17`, pinned `card_version`, and selected
  profile itself. Tests prove caller data cannot set/retain `plain`, `space_id`,
  sender, OBO, stream, subscribers, mention, or transport headers.
- Tests prove the dispatcher copies the input document before decoding and does
  not mutate caller-owned bytes/maps; the same private snapshot feeds validation,
  plain derivation, final size checking, serialization, and transport.
- Call-order tests freeze the required sequence. Invalid card, disabled feature,
  identity/ACL failure, and enriched/final payload overflow each produce zero
  transport calls.
- Success tests prove `plain` is recomputed, verified `space_id` is injected,
  `cardmsg.Validate` and `Finalize` accept the final envelope, exact serialized
  bytes are <= `cardmsg.MaxPayloadBytes`, and the captured `MsgSendReq` has the
  bound bot UID and normalized target.
- A boundary test starts below the size limit, grows during authoritative
  enrichment, and is rejected by `Finalize`/the final size gate. No mutation
  occurs after the explicit recheck before serialization/dispatch.

### Internal notify card gate (Decision 14)

- Integration tests prove a card-shaped payload (`type=17` map, per
  `cardmsg.IsCardPayload`) to `/v1/internal/notify` and
  `/v1/internal/notify/batch` is rejected with the registered error code and
  produces zero `SendMessage` calls; a `type=1` text payload on the same
  requests still delivers (existing smart-summary contract unchanged).
- The new error code passes `make i18n-extract-check` + `make i18n-lint` and
  has a zh-CN translation.

### Card template and deep-link (pilot enablement)

- Every `ResourceCard` template instantiation has a JSON snapshot test and
  passes `cardmsg.Validate` under `octo/v1`; the excerpt truncation bound is
  tested; template labels resolve via `i18n.OutboundLanguage`.
- The deep-link is built only by the octo-server template layer from
  `External` config; a template test pins the `/s/{taskId}?sp={spaceId}`
  shape. smart-summary never constructs the URL.
- octo-web registers the standalone `/s/:taskId` route with a route-contract
  test (mirroring the `/d/:docId` cold-load/login/sid patterns); logged-in
  navigation reaches the existing summary detail view.

### Dispatch, overload, and observability

- Success returns the exact `message_id`, `message_seq`, and `client_msg_no`
  from one captured `SendMessageWithResult` call. Transport failure is returned
  without retry; cancellation before dispatch produces zero calls.
- Concurrency tests prove each producer never exceeds its configured in-flight
  bound, saturated calls fail fast with `busy`, and slots are released on every
  terminal branch. Tests and docs state that this bound is per process.
- Metrics tests use an isolated Prometheus registry and prove exactly one
  terminal result increment per attempt, duration/in-flight correctness, and a
  fixed label vocabulary. A source test rejects UID/channel/Space/message ID as
  labels and rejects logging serialized card content.

### No-bypass guard

- A new AST-based tool/test scans non-test Go sources and fails fixtures for:
  direct internal type-17 `SendMessage`, direct
  `SendMessageWithResult`, literal/constant card construction paired with a
  transport call, package-local wrapper/alias use, and splitting
  construction/call across files in one package.
- The allowlist contains only exact existing external ingress symbols/call sites
  and `internal/carddispatch`, with a reason for each. Adding an exemption is a
  load-bearing review change; fixtures also prove that adding a second bypass in
  an otherwise allowlisted package still fails.
- The guard is wired into CI and the existing
  `TestType17ProducerSizeRecheckCompleteness` remains green.

### Verification and rollout

- TDD RED/GREEN checkpoints remain reachable from the implementation branch.
- New dispatcher/authorizer critical business logic has >=90% statement and
  branch-oriented matrix coverage; overall new package coverage is >=80%.
- Focused commands pass:
  - `go test -race -cover ./internal/carddispatch/...`
  - target-authorizer DB integration tests;
  - `go test ./pkg/cardmsg/... ./modules/botidentity/... ./modules/notify/...`
  - `go run ./tools/lint-card-dispatch ./modules ./internal`
  - `go vet ./internal/carddispatch/...`
  - `make i18n-extract-check && make i18n-lint`
  - `golangci-lint run ./...`
  - `git diff --check`
- Broader package/full tests run with the repo's per-package MySQL/Redis reset
  discipline where local infrastructure permits.
- With zero enabled production registrations, deployment is behaviorally inert
  except the Decision 14 rejection of card-shaped internal notify payloads
  (no known legitimate caller sends them). Enabling the `summary-notify` pilot
  is a separate reviewed config/code change with rollback by disabling/removing
  that one registration; existing public producers continue unaffected.
- Finish writes a shared journal/log entry and opens a separate PR with Linked
  Spec and substantive COMPREHENSION answers.
