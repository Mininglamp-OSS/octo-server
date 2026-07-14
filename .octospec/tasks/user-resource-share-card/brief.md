---
type: Task
title: "Task: user-resource-share-card"
description: Add a provider-based platform capability for an authenticated user to share a server-minted resource card as themselves to a DM, group, or thread.
tags: ["card", "resource", "share", "auth", "space", "isolation", "acl", "trust-boundary", "rate-limit", "idempotency", "audit", "testing"]
timestamp: 2026-07-14T12:00:00+08:00
# --- octospec extension fields ---
slug: user-resource-share-card
upstream: octo-server#571
source: self
---

# Task: user-resource-share-card

## Goal

Provide one reviewed platform path for an authenticated user to share a
server-minted resource card as themselves into a person-to-person DM, group, or
thread. Resource owners such as smart-summary, docs, tasks, or approval systems
onboard through an immutable provider registry and structured claims; they do
not create their own message sender, trust, target-authorization, rate-limit,
idempotency, audit, or transport implementations.

The message appears in the selected conversation with the sharing user as its
author. The client chooses a resource and targets but cannot submit Adaptive
Card JSON, choose `from_uid`, supply an arbitrary URL, or delegate another
actor. Cards are fixed, display-only resource representations built and
finalized by octo-server.

“Share” means creating a new message that represents an authoritative resource;
it is not a byte-for-byte forward of an existing message or card. Smart
summaries and documents are separate providers of the same platform capability:
the resource remains owned by its source service, and octo-server sends only a
reviewed representation and deep link. Sharing does not copy the underlying
resource into octo-server and does not grant access by default. A reviewed
provider may expose a public resource, a safe requestable-access card, or an
explicit atomic grant policy; the provider landing route authorizes the current
viewer on every open.

## Background

The current InteractiveCard boundary is intentionally Bot-oriented:

- ordinary user ingress rejects type-17;
- `modules/cardtrust` masks human-sender cards;
- `internal/carddispatch` binds a producer to one static active Bot UID;
- Bot API OBO rejects cards.

Those controls must remain the generic default. A user resource share is a
narrow first-party exception: the actor authenticates directly, the resource
owner attests what may be shared, octo-server authorizes the target and mints a
fixed card, and every consumer verifies server provenance. It is not a generic
human card composer and is not Bot OBO.

## Platform contract

### Provider registry

octo-server owns an immutable startup registry of reviewed resource providers.
Each provider specification binds:

- stable low-cardinality `resource_type` and accepted issuer/audience;
- intent verification keys and accepted intent versions;
- allowed template IDs/versions and a structured claim schema with byte/count
  bounds;
- server-owned deep-link builder and allowed route/origin policy;
- provider-specific enable flag and traffic budget;
- content-disclosure/access policy identifier and audit category.

Unknown, duplicate, disabled, or invalid providers fail closed at startup or
request time. External callers cannot register providers dynamically. Provider
adapters return a typed `ResourceCardInput`; they never return wire payloads,
HTML, markdown actions, or transport requests.

The registry also binds a required provider adapter. Given a fully verified
intent, the adapter revalidates the resource against its authoritative owner
before any target is dispatched: the actor must still be allowed to share it,
the signed revision must still be current and shareable, and the provider's
reviewed public/requestable/grant policy must still permit the disclosure. A
requestable card does not require every recipient to have access, but every
visible field must be safe for all current and future viewers of the target.
The adapter returns only typed `ResourceCardInput` and per-target disclosure
decisions. A timeout, unavailable owner, stale revision, partial grant, or
indeterminate result fails closed with zero transport calls for the affected
targets.

Intent verification algorithms, issuer keys, and `kid` values are pinned by the
reviewed specification and loaded from managed startup configuration. A JWS
header or request body can select only among those pre-registered keys; it can
never supply a key, JWKS URL, algorithm, template, or provider implementation.
Provider key rotation uses an overlapping verification ring. Runtime provider
registration and request-triggered key discovery are forbidden.

### Authenticated two-stage intent

1. The user calls the resource-owner service with the selected resource,
   bounded target list, and idempotency key.
2. The owner verifies current resource visibility/shareability and emits a
   short-lived, signed, single-use intent containing `iss`, `aud`, actor UID,
   Space, resource type/ID/revision, canonical targets, template ID/version,
   structured claims, expiry, nonce, and idempotency key.
3. The user calls the generic authenticated octo-server resource-share endpoint
   with that intent. `AuthMiddleware`, Space middleware, shared UID limiting,
   and a pre-decode body cap apply. The login UID and Space must equal the
   signed actor and Space. A static S2S token or caller-supplied `actor_uid` is
   insufficient.
4. octo-server verifies issuer, signature, audience, version, expiry, nonce,
   provider, revision, targets, claim schema, and replay state before building
   or dispatching a card.

The HTTP contract is `POST /v1/resource-shares`. `X-Space-ID` is required and
must have been validated by Space middleware. The JSON body contains exactly
one field, `intent`; targets, resource fields, actor, card content, and transport
metadata supplied outside the signed intent are rejected. The initial platform
limits are 128 KiB for the pre-decode HTTP body, 96 KiB for compact JWS, 32 KiB
for canonical structured claims, 20 canonical targets, five minutes maximum
intent lifetime, and 30 seconds maximum clock skew. Provider limits may be
lower, never higher. Nonce and idempotency values are opaque bounded strings and
are stored only as hashes where their original value is not operationally
required.

For a cryptographically and structurally valid intent, the endpoint returns
HTTP 200 with one result per signed target in canonical target order. Stable
outcomes are `sent`, `already_sent`, `denied`, `rate_limited`, `failed`, and
`unknown`; optional `message_id`, `message_seq`, and `retry_after_seconds` are
present only where applicable, with `message_id` encoded as a decimal string.
Target denial details are not exposed. A request-wide failure in authentication,
intent, provider, replay detection, body size, or configuration uses the
localized error envelope and produces no sends.
No response contains the built card, share proof, signing input, or raw intent.

An intent authorizes only the exact signed resource revision and target set. It
does not grant a general ability to send messages or read the resource. Every
provider must define whether a resource is public, requestable, access-required,
atomically granted, or forbidden. The platform never guesses or widens resource
visibility itself. Requestable links reauthorize the current viewer and open
the provider-owned permission flow when access is absent.

“Single use” is compatible with network retry through the durable intent
fingerprint. The first successful claim stores `hash(nonce)` and a canonical
fingerprint of all signed claims. A retry with the same login UID, Space, signed
intent, nonce, and fingerprint returns or resumes the same per-target records;
it is not treated as a new share. Reuse of the nonce with any different
fingerprint, actor, Space, provider, resource, revision, template, claims, or
target set is a replay conflict and performs zero transport calls. After intent
expiry, a retry may read already terminal results but may not start or resume a
delivery.

### Target and sender contract

- **DM:** verify the authenticated actor and peer under the existing
  friendship/Space policy, then send from the actor to the peer so the card
  lands in their existing human conversation.
- **Group:** verify active actor membership/post permission, exact active Space,
  group lifecycle, and bans.
- **Thread:** verify canonical parent group, actor parent-channel access, exact
  Space, and thread lifecycle.

Signed targets use typed forms rather than caller-controlled numeric channel
types: `{kind:"dm", peer_uid}`, `{kind:"group", group_no}`, or
`{kind:"thread", group_no, short_id}`. Canonicalization rejects duplicate
targets and non-canonical aliases. A DM proof binds the exact Space and the
order-independent actor/peer pair so both participants can verify the same
persisted message; group and thread proofs bind the exact canonical channel ID.

Phase one rejects empty/legacy Space IDs, external-group visibility fallbacks,
and all cross-Space targets. Group authorization requires a normal, non-deleted
member, enforces individual bans/mutes, and respects full-group posting rules
(only the existing manager/owner whitelist may post while the group is
forbidden). Thread authorization inherits those parent-group checks and
requires an active thread; archived or deleted threads are denied rather than
implicitly unarchived by a share.

Each bounded target is authorized, rate-limited, idempotency-claimed,
dispatched, and reported independently. Multi-target requests return explicit
per-target results; they are not all-or-nothing. No failure silently redirects
to a Bot DM, creator DM, another channel, or plain-text message.

`FromUID` is always the directly authenticated login UID. No request or intent
field named `from_uid` is accepted; the signed `actor_uid` is only an equality
assertion and can never override the login identity. The generic Bot-bound
`carddispatch.Sender` remains static and is not extended with a dynamic sender.

### Card and provenance contract

octo-server selects the registered template, derives the deep link from the
provider adapter, constructs the `octo/v1` card, recomputes authoritative
`plain`, validates/finalizes it, and performs the final serialized-size check.
Provider claims cannot inject arbitrary elements/actions, external URLs,
sender/attribution, OBO fields, subscribers, mentions, or transport metadata.

“Display-only” permits only reviewed, server-built navigation such as a single
`Action.OpenUrl`/deep link. It never permits provider-supplied URLs, inputs,
`Action.Submit`, callbacks, or any action that mutates the resource. The adapter
returns a typed resource-link input; octo-server applies the registered
origin/route policy and constructs the final URL. The route may open a public
resource or a permission-request page after current-viewer authorization; the
card never carries an access credential or performs the request inline.

A human-sender type-17 card is renderable only with a valid platform share proof
bound to the finalized canonical envelope, actor, Space, target, resource type/
ID/revision, provider, and idempotency nonce. The proof carries a version and
key ID plus a detached signature. Generic user cards, unsigned shares, modified
payloads, and Bot-OBO cards remain rejected/masked.

The proof is an ES256 detached JWS over an RFC 8785/JCS canonical signing object.
That object contains the finalized envelope without `resource_share_proof` plus
the authoritative actor, canonical target, Space, provider, resource reference,
revision, and delivery idempotency identity. octo-server calls
`cardmsg.Validate` and `cardmsg.Finalize`, attaches the proof, then performs one
last complete-payload size check. Verifiers reconstruct the same signing object
from the observed message context; verifying a signature without matching the
observed sender, Space, and channel is insufficient.

Platform proof signing uses a managed asymmetric private key and an active
`kid`; it does not reuse `OCTO_MASTER_KEY`. The public
`GET /v1/resource-shares/proof-jwks` endpoint publishes the active and retained
verification keys with cache headers. Old public keys remain available for at
least the persisted-message retention period. Missing or invalid signing
configuration disables new user shares fail-closed, while a valid retained
verification ring can continue to render historic shares.

All server display surfaces and web real-time/cold-sync rendering share the same
versioned verification vectors. Proofs, private keys, intents, and signatures
are never logged.

The exception is display-only: no `octo/v2`, inputs, `Action.Submit`, callbacks,
card edits/revisions, or `card_action`. Existing human-sender interactive-card
rejection remains unchanged.

### Abuse control, idempotency, and audit

- Shared authenticated UID limiting applies first; an endpoint-specific quota
  counts targets, not just HTTP requests.
- DM targets use a per-actor/peer cooldown. Group/thread targets use a
  Redis-backed cluster-wide channel bucket. A feature-wide cluster bucket and
  bounded in-process concurrency protect transport across providers.
- Provider-specific limits cannot exceed platform maxima. Invalid/unbounded
  configuration fails the provider closed. Limiter-store failure fails closed
  with a bounded retry-after.
- Idempotency is scoped to actor + provider + resource type/ID/revision +
  canonical target. Intent nonce consumption and per-target results are durable.
  Changed inputs with a consumed nonce fail closed.
- Audit records actor, provider, resource reference, revision, Space, target,
  request/correlation ID, timestamp, and bounded outcome, but not resource
  content, card JSON, proof, intent, token, signature, or credentials.
- WuKongIM has no caller-controlled `client_msg_no`; an ambiguous transport
  timeout may duplicate despite request idempotency. Exactly-once is not claimed.

### Durable delivery state

The module owns three append-safe/durable stores:

- `resource_share_intent` uniquely claims the nonce hash and stores the signed
  fingerprint, actor, Space, provider, resource reference, expiry, and bounded
  request state.
- `resource_share_delivery` uniquely claims the idempotency scope for each
  canonical target and stores its state, retry boundary, and transport result.
- `resource_share_audit` records bounded attempt and outcome events without
  resource content or cryptographic material.

The irreversible per-target transition is `claimed -> dispatching -> sent`.
Authorization failures become terminal `denied`. A limit or infrastructure
failure that occurs before transport may be retried only after its recorded
retry boundary and before intent expiry. Once a target reaches `dispatching`, a
timeout, process loss, or inability to durably confirm the returned message ID
becomes terminal `unknown`; automatic retry is forbidden because transport may
already have accepted the message. A stored `sent` result is returned as
`already_sent` and is never intentionally dispatched again.

Durable state is written before crossing the transport boundary. Audit/outbox
records that must accompany a transition are written in the same database
transaction. Database failure before dispatch fails closed; database failure
after transport is reported and measured as `unknown`, never disguised as a
safe failure.

### Implementation and rollout slices

1. **Platform foundation (octo-server):** add the immutable registry, intent and
   proof primitives, human target authorizer, durable state, distributed limits,
   generic endpoint, trust integration, public verification keys, observability,
   and source guards. It ships with the global feature flag off and zero enabled
   providers.
2. **Renderer companion (octo-web and any other display client):** verify the
   proof for real-time and cold-sync messages, bind it to observed sender/Space/
   target, consume shared conformance vectors, and mask failures to `[卡片]`.
3. **TODO — shared client share UX/SDK (Web and App):** ship one versioned
   resource-share component contract, with platform UI adapters where native
   controls differ, instead of letting each provider page or client rebuild the
   flow. It owns provider/resource selection hooks, signed-intent acquisition,
   one DM/group/thread target picker, `POST /v1/resource-shares`, stable
   `sent`/`already_sent`/`denied`/`rate_limited`/`failed`/`unknown` result
   mapping, loading/retry/partial-success UX, correlation and observability, and
   the shared proof-vector/JWKS cache integration. Provider pages inject only
   resource identity, presentation metadata, and the reviewed intent-acquisition
   adapter. They must not forward an existing card payload or accept caller-
   supplied card JSON, URL, sender, proof, or transport fields. The Web/App
   companion design and ownership must be recorded in their client adaptation
   tasks before enabling the first provider.
4. **Provider onboarding:** smart-summary, docs, tasks, and other resource owners
   each get a separate reviewed adapter/brief for issuer keys, claim schema,
   revision revalidation, disclosure/grant policy, template, deep-link route,
   traffic budget, and operational ownership. The shared integration contract
   is in `provider-onboarding.md`; the first provider briefs are
   `../smart-summary-resource-share-provider/brief.md` and
   `../docs-resource-share-provider/brief.md`.

No provider is enabled until the renderer companion, shared client share
UX/SDK, and that provider's onboarding are deployed. Rollback first disables
the provider, then the global share flag; historic proof verification remains
enabled so already persisted messages do not regress to untrusted content.

## Load-bearing list

- User authentication, Space middleware, localized/anti-enumeration errors, and
  exact login-actor binding.
- Provider registry, signed intent schema, replay state, key rotation, and
  secret/config management.
- Provider-owned resource visibility/disclosure policy and revision binding.
- DM, group, and thread authorization with no fallback target.
- Server-owned template/deep-link/finalization and trusted-human-card
  provenance across server and web display surfaces.
- Per-target distributed quotas, concurrency, idempotency, partial-result
  contract, audit, observability, feature flags, and rollback.
- Source guard proving the generic share service is the only human-sender
  type-17 transport owner.

## Out of scope

- Any provider-specific resource schema, visibility rule, template fields, or
  deep-link route; each provider gets a separate onboarding brief.
- Copying or forwarding an existing message/card payload. A share always mints
  a new resource representation from verified provider claims.
- Automatic system/Bot notifications or task-origin delivery.
- Arbitrary user-authored Adaptive Cards, generic type-17 user ingress, Bot API
  OBO cards, arbitrary senders, or runtime provider registration.
- Interactive cards, callbacks, edits/revisions, resource mutation, or access
  approval actions.
- Cross-Space sharing unless a future provider and platform policy explicitly
  define and review it.
- Exactly-once transport delivery or duplicate recall.

## Acceptance

- Registry tests cover unknown, duplicate, disabled, malformed, untrusted-
  issuer, unsupported-version/template, oversized-claim, and invalid-config
  providers; no invalid provider reaches template or transport.
- Intent tests cover actor/Space/audience mismatch, expiry, signature failure,
  stale resource revision as reported by required provider revalidation, target
  mutation, same-fingerprint retry, changed-fingerprint nonce replay, and
  changed inputs; every denial produces zero transport calls.
- DM/group/thread authorization matrices cover all allow and deny states,
  including friend/Space rules, removed/banned actor, wrong Space, disabled/
  disbanded group, invalid/archived/deleted thread, missing parent, and DB errors.
- Success in every target type persists an `octo/v1` message with
  `from_uid=login_uid` in the selected conversation. No Bot conversation or
  membership mutation is created.
- Request/provider fuzz and contract tests prove free-form card/payload, URL,
  sender, attribution, OBO, mention, subscriber, and transport fields cannot
  reach the wire. Every provider output passes `cardmsg.Validate`, `Finalize`,
  and final-size checks.
- Provider access-policy tests cover public, requestable, and forbidden
  resources. Requestable cards expose only reviewed safe metadata, carry no
  access credential, and open a provider route that reauthorizes the current
  viewer before showing the resource or permission-request flow.
- Proof conformance vectors pass in octo-server and web. Missing, forged,
  wrong-provider/resource/actor/target/Space, or payload-tampered proofs are
  masked. Old/new verification keys overlap safely during rotation.
- Web and App use the same versioned resource-share component contract/SDK;
  platform adapters may supply native UI, but provider pages only inject
  resource identity, presentation metadata, and signed-intent acquisition.
  They do not independently implement target selection, endpoint calls, result
  mapping, retry/partial-success behavior, proof-vector handling, or JWKS
  caching. Real-time and cold-sync rendering apply the same proof policy.
- Client contract tests prove no provider page or component can bypass the
  server-minted-card boundary by forwarding a stored card payload or supplying
  card JSON, URL, sender, proof, or transport fields.
- Generic user type-17 and Bot-OBO tests remain rejected; the no-bypass guard
  permits only the reviewed generic share transport site and proves the
  Bot-bound dispatcher still rejects dynamic senders.
- Mixed-target requests return stable per-target outcomes without rollback or
  fallback sends. Idempotent retry does not intentionally resend successful
  targets; an identical intent returns stored results, changed nonce inputs fail
  closed, and any target that crossed `dispatching` without durable confirmation
  returns terminal `unknown`; ambiguous duplicates are measured.
- Multi-replica rate tests prove per-DM, per-channel, per-provider, and global
  limits count targets correctly, return bounded retry-after, and fail closed
  when the limiter store is unavailable.
- Audit/log/metric tests prove content, identifiers in labels, intents, proofs,
  signatures, tokens, and secrets are not exposed.
- Global and per-provider feature flags disable new shares without affecting
  normal messages, Bot cards, or other providers; rollback requires no
  destructive data migration.
