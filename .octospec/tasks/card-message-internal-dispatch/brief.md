---
type: Task
title: "Task: card-message-internal-dispatch"
description: Add the only supported in-process path for business modules to send trusted interactive cards.
tags: ["message", "card", "internal", "trust-boundary", "wire-contract", "auth", "space", "isolation", "acl", "rate-limit", "observability", "testing"]
timestamp: 2026-07-13T16:17:40+08:00
# --- octospec extension fields ---
slug: card-message-internal-dispatch
upstream: self
source: self
---

# Task: card-message-internal-dispatch

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add `internal/carddispatch` as the only supported path for an in-process
business module to originate an `InteractiveCard` (`type=17`) message.

The dispatcher binds a reviewed producer capability to one configured active
bot identity, authorizes the exact Space and target using live database state,
owns the card envelope, applies the global and per-producer gates, runs
`cardmsg.Validate` / server enrichment / `cardmsg.Finalize` / final serialized
size recheck in a fixed order, makes at most one `SendMessageWithResult`
transport attempt per invocation, and emits bounded metrics and structured
logs.

This closes the architectural hole where a future business module could call
`config.Context.SendMessageWithResult` directly with a hand-built type-17 map
and accidentally skip sender trust, Space/target ACLs, authoritative `plain`, or
the final post-mutation 512 KiB check.

No HTTP endpoint is added. Existing Bot API, legacy Robot API, and Incoming
Webhook producers keep their current ingress-specific paths in this task; the
new boundary governs server-internal business producers.

## Background

### Existing authoritative pieces

- `pkg/cardmsg` is the card protocol authority:
  - `cardmsg.Enabled()` reads `OCTO_CARD_MESSAGE_ENABLED`;
  - `Validate` enforces the profile/version, structure, URL, depth/node and
    complete-payload limits;
  - `Finalize` overwrites untrusted `plain` and rechecks the enriched payload;
  - `RecheckPayloadSize` checks the final bytes after any later mutation.
- `modules/botidentity` resolves live bot authority:
  `robot.status=1` or `app_bot.status=1`; `user.robot=1` is presentation only;
  ambiguous or failed lookups fail closed.
- The three current external ingress producers already implement their own
  trust and permission boundaries before calling the IM transport:
  - `modules/bot_api/send.go`;
  - `modules/robot/api.go`;
  - `modules/incomingwebhook/api.go` + `card.go`.
- `pkg/cardmsg/producer_completeness_test.go` already proves every producer that
  expands `mention.ais` performs a final size recheck. It does not prevent a new
  internal producer from bypassing all of the other gates.
- `config.MsgSendReq` has no caller-supplied `client_msg_no`, and
  `SendMessageWithResult` has no context-aware overload. It returns the
  server-generated `message_id`, `message_seq`, and `client_msg_no` only after a
  successful transport response. Exactly-once retry cannot be truthfully
  promised by this task.

### Why this is a separate P0

The App Bot trust task fixes consumers of already-sent cards. It deliberately
does not authorize new in-process producers. Internal dispatch is a separate
load-bearing boundary: a server module is not authenticated by Bot API
middleware, so it needs an explicit producer capability and cannot inherit
HTTP-layer bot/Space checks by accident.

### Implementation gate: pilot producer must be named

Before `/octospec-go card-message-internal-dispatch` enables a pilot producer, a
maintainer must replace the following `TBD` values in this brief. A deliberately
dormant foundation may leave them unresolved, but it must register no production
producer. Do not implement a mutable generic "send as any UID" service while
they are unknown.

| Required input | Value |
| --- | --- |
| Producer ID (stable, low-cardinality) | `TBD` |
| Owning module / constructor | `TBD` |
| Sender bot UID configuration source | `TBD` |
| Allowed channel types | `TBD` |
| Allowed card profiles / action-event owner | `TBD` |
| Required Space source | `TBD` |
| Expected peak concurrency / QPS | `TBD` |
| Business retry/idempotency requirement | `TBD` |
| Process replicas / required cluster-wide cap | `TBD` |

The foundation may be implemented with no enabled production registration, but
it must not be presented as end-to-end useful until one concrete producer is
reviewed against this table.

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
   User Bots require creator/friend authorization for DMs; group/thread sends
   require an active internal bot member, a normal parent group, a non-deleted
   active thread, and exact Space equality. DB errors fail closed. OBO is not
   supported by internal dispatch.
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
    security boundary.
13. **Request-time authorization, no lock across transport.** Identity and ACL
    state are queried without a decision cache on every invocation. The
    dispatcher does not hold database locks or a transaction open across the IM
    network call. A concurrent unpublish, membership removal, or Space disable
    can therefore race with the already-authorized in-flight request; later
    requests fail closed. If the pilot requires linearizable revocation of an
    in-flight send, that stronger cross-system protocol must be designed before
    implementation rather than claimed by this package.

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

- Selecting or implementing the first business producer while the pilot table
  above remains `TBD`.
- Replacing or refactoring `/v1/bot/sendMessage`, legacy Robot API send,
  Incoming Webhook push/card send, or their public error contracts.
- Incoming Webhook card actions/callbacks; webhook identities still have no bot
  event consumer.
- A new internal `card_action` callback/event bus. Interactive profiles retain
  the existing sender-bot event ownership and poll/ACK contract.
- OBO, fan-out, streams, subscribers, transient/no-persist cards, mention
  expansion, broadcast cards, typing/read receipts, or card edits/revisions.
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

- The pilot-producer table has no `TBD` values before implementation starts, or
  the implementation is explicitly limited to a disabled foundation with no
  claim of end-to-end production usefulness.
- The chosen sender UID/config source, target kinds, Space source, concurrency,
  card profile/action owner, replica multiplier, and retry requirements receive
  maintainer sign-off in this brief.

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
- DM tests cover User Bot creator/friend allow, stranger deny, active Space
  membership, wrong/missing Space, App Bot platform/scope-space rules, and App
  Bot group/thread denial.
- Group/thread tests cover exact authoritative Space match, normal vs
  disabled/disbanded group, active/internal/blacklisted/deleted/non-member bot,
  valid/malformed/archived/deleted thread, and DB failure. All denials fail
  closed before card finalization/transport.
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
  - `go test ./pkg/cardmsg/... ./modules/botidentity/...`
  - `go run ./tools/lint-card-dispatch ./modules ./internal`
  - `go vet ./internal/carddispatch/...`
  - `make i18n-extract-check && make i18n-lint`
  - `golangci-lint run ./...`
  - `git diff --check`
- Broader package/full tests run with the repo's per-package MySQL/Redis reset
  discipline where local infrastructure permits.
- With zero enabled production registrations, deployment is behaviorally inert.
  Enabling the pilot is a separate reviewed config/code change with rollback by
  disabling/removing that one registration; existing public producers continue
  unaffected.
- Finish writes a shared journal/log entry and opens a separate PR with Linked
  Spec and substantive COMPREHENSION answers.
