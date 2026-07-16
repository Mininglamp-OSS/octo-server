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

An intent authorizes only the exact signed resource revision and target set. It
does not grant a general ability to send messages or read the resource. Every
provider must define whether sharing requires recipients to already have access
or atomically creates a provider-owned access grant; the platform never guesses
or widens resource visibility itself.

### Target and sender contract

- **DM:** verify the authenticated actor and peer under the existing
  friendship/Space policy, then send from the actor to the peer so the card
  lands in their existing human conversation.
- **Group:** verify active actor membership/post permission, exact active Space,
  group lifecycle, and bans.
- **Thread:** verify canonical parent group, actor parent-channel access, exact
  Space, and thread lifecycle.

Each bounded target is authorized, rate-limited, idempotency-claimed,
dispatched, and reported independently. Multi-target requests return explicit
per-target results; they are not all-or-nothing. No failure silently redirects
to a Bot DM, creator DM, another channel, or plain-text message.

`FromUID` is always the directly authenticated login UID and is not present in
the request or provider claims. The generic Bot-bound `carddispatch.Sender`
remains static and is not extended with a dynamic sender.

### Card and provenance contract

octo-server selects the registered template, derives the deep link from the
provider adapter, constructs the `octo/v1` card, recomputes authoritative
`plain`, validates/finalizes it, and performs the final serialized-size check.
Provider claims cannot inject arbitrary elements/actions, external URLs,
sender/attribution, OBO fields, subscribers, mentions, or transport metadata.

A human-sender type-17 card is renderable only with a valid platform share proof
bound to the finalized canonical envelope, actor, Space, target, resource type/
ID/revision, provider, and idempotency nonce. The proof carries a version and
key ID plus a detached signature. Generic user cards, unsigned shares, modified
payloads, and Bot-OBO cards remain rejected/masked.

All server display surfaces and web real-time/cold-sync rendering share the same
versioned verification vectors. Platform signing keys come from managed secret
configuration; public verification keys support overlap/rotation, and keys
needed for historic persisted messages are retained according to message
retention. Proofs, private keys, intents, and signatures are never logged.

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
- Idempotency is scoped to actor + provider + resource revision + canonical
  target. Intent nonce consumption and per-target results are durable. Changed
  inputs with a consumed nonce fail closed.
- Audit records actor, provider, resource reference, revision, Space, target,
  request/correlation ID, timestamp, and bounded outcome, but not resource
  content, card JSON, proof, intent, token, signature, or credentials.
- WuKongIM has no caller-controlled `client_msg_no`; an ambiguous transport
  timeout may duplicate despite request idempotency. Exactly-once is not claimed.

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
  stale resource revision, target mutation, nonce replay, and changed inputs;
  every denial produces zero transport calls.
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
- Proof conformance vectors pass in octo-server and web. Missing, forged,
  wrong-provider/resource/actor/target/Space, or payload-tampered proofs are
  masked. Old/new verification keys overlap safely during rotation.
- Generic user type-17 and Bot-OBO tests remain rejected; the no-bypass guard
  permits only the reviewed generic share transport site and proves the
  Bot-bound dispatcher still rejects dynamic senders.
- Mixed-target requests return stable per-target outcomes without rollback or
  fallback sends. Idempotent retry does not intentionally resend successful
  targets; changed nonce inputs fail closed; ambiguous duplicates are measured.
- Multi-replica rate tests prove per-DM, per-channel, per-provider, and global
  limits count targets correctly, return bounded retry-after, and fail closed
  when the limiter store is unavailable.
- Audit/log/metric tests prove content, identifiers in labels, intents, proofs,
  signatures, tokens, and secrets are not exposed.
- Global and per-provider feature flags disable new shares without affecting
  normal messages, Bot cards, or other providers; rollback requires no
  destructive data migration.
