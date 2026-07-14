---
type: Task
title: "Task: smart-summary-user-share-card"
description: Let an authenticated user share a server-minted summary card as themselves to a DM, group, or thread without opening generic user-authored type-17.
tags: ["summary", "card", "share", "auth", "space", "isolation", "acl", "trust-boundary", "rate-limit", "idempotency", "audit", "testing"]
timestamp: 2026-07-14T12:00:00+08:00
# --- octospec extension fields ---
slug: smart-summary-user-share-card
upstream: octo-server#571
source: self
---

# Task: smart-summary-user-share-card

## Goal

Let an authenticated user explicitly share a summary they can view into any
supported conversation target: a person-to-person DM, group, or thread. The
message appears as authored by the sharing user in the selected conversation;
it must not create a separate `notification` Bot DM or show a Bot as the author.

The card body remains server-minted from the summary resource. Neither web nor
an S2S caller can submit arbitrary type-17 JSON, choose `from_uid`, forge the
summary content, or use a generic “send as any user” capability.

## Distinction from origin delivery

| Capability | Trigger | Destination | Message author | Failure behavior |
| --- | --- | --- | --- | --- |
| Origin group/thread delivery | Automatic when a task reaches a terminal state | Immutable group/thread bound when the task was created | `notification` Bot | Permanently invalid origin falls back to creator DM |
| User-initiated share | Explicit authenticated user action | User-selected DM, group, or thread | Sharing user | Return per-target result; never silently redirect or change message type |

The destination sets overlap for groups and threads, but the trigger,
provenance, sender, authorization, fallback, idempotency, and audit contracts
are different. They are therefore separate tasks.

## Background and current blockers

Today web forwards summary content as user-authored plain-text chunks. The web
client is receive-only for type-17, and the current server deliberately enforces
Bot-only InteractiveCard origination:

- `modules/message` rejects every user-ingress type-17 request before target DB
  checks;
- `modules/cardtrust` trusts only active Bot or webhook senders and masks a
  human-sender type-17 card;
- `internal/carddispatch` binds every producer to one static active Bot UID and
  cannot send as the authenticated actor;
- Bot API OBO rejects type-17.

Consequently this feature is not implemented by widening `summary-notify`.
It needs a separate, narrowly scoped server-minted user-share authority while
the generic user, Bot-OBO, and client-authored type-17 gates remain closed.

## Contract and decisions

1. **Two-stage authenticated share intent.** Web first calls a user-authenticated
   summary-api endpoint with the summary, bounded target list, and idempotency
   key. After checking visibility, summary-api returns a short-lived, signed,
   single-use share intent bound to actor UID, summary ID/revision, Space,
   canonical targets, immutable template fields, expiry, nonce, and idempotency
   key. Web then calls a new user-authenticated octo-server share endpoint with
   that intent. octo-server requires `AuthMiddleware` and Space middleware;
   `login_uid` must equal the intent actor. A static S2S token plus caller-
   supplied `actor_uid` is insufficient.
2. **Summary authorization.** summary-api is the content authority and verifies
   current visibility before minting an intent. Deleted, expired, revoked,
   cross-Space, or unauthorized summaries fail closed. octo-server verifies the
   intent signature, audience, expiry, nonce, actor, Space, target binding, and
   content revision; request fields cannot override signed values.
3. **All three target types.** A bounded request may contain DMs, groups, and
   threads. Each target is normalized, authorized, rate-limited, dispatched,
   and reported independently; partial success is explicit, not transactional.
   - DM: verify actor and peer are valid participants under the existing Space/
     friendship policy, then send `from_uid=actor` to the peer so the card lands
     in their existing human-to-human conversation.
   - Group: verify active actor membership/post permission, active same-Space
     group, and explicit bans.
   - Thread: verify the canonical parent group, actor parent-channel access,
     same Space, and live thread lifecycle.
4. **User authorship, server-owned card.** octo-server builds the fixed
   display-only `octo/v1` summary ResourceCard, derives the deep link, computes
   authoritative `plain`, finalizes/size-checks it, and transports it with
   `FromUID=login_uid`. The request has no `from_uid`, card/document, URL,
   attribution, `plain`, OBO, subscriber, or transport fields. The normal chat
   sender UI is sufficient attribution; the card does not duplicate a fake
   “Alice shared” header.
5. **Narrow trusted-human-card provenance.** A human-sender type-17 card is
   renderable only when it carries a valid server-only share proof bound to the
   canonical finalized envelope, actor, Space, target, content revision, and
   idempotency nonce. The proof includes a key ID and detached signature.
   Generic user-ingress cards, unsigned cards, tampered cards, and Bot-OBO cards
   remain rejected/masked exactly as today. All server display surfaces and the
   web real-time renderer use one versioned verification contract; old
   verification keys remain available for historic-message rendering during
   key rotation. Private signing material comes from managed secret/config,
   never the repository or payload.
6. **Display-only exception.** The user-share authority permits only the fixed
   `octo/v1` template and local navigation/copy actions. It does not allow
   `octo/v2`, `Action.Submit`, inputs, card edits, revisions, or `card_action`.
   The existing human-sender action rejection remains unchanged.
7. **No generic OBO or dispatcher weakening.** This endpoint sends as the
   directly authenticated actor; it is not Bot API OBO and accepts no delegated
   actor identity. The existing Bot-bound `carddispatch.Sender` remains static.
   The new path must have its own source guard/allowlist entry and cannot expose
   a reusable arbitrary-card or arbitrary-sender API.
8. **Rate limits.** Apply the shared authenticated UID limiter plus a stricter
   endpoint-specific per-user share quota. Group/thread targets also use the
   Redis-backed cluster-wide per-channel and feature-wide outbound buckets
   defined by the origin-delivery brief; DM targets use a per-actor/peer
   cooldown. Quota values are bounded configuration chosen from observed
   volume. Limiter-store failure fails closed and returns a safe retry-after.
9. **Idempotency and audit.** Scope the idempotency key to actor + summary
   revision + canonical target. Persist intent consumption and per-target send
   result so retries do not intentionally redispatch successful targets; nonce
   replay with changed inputs fails closed. Audit actor, summary reference,
   Space, canonical target, timestamp, request/correlation ID, and outcome, but
   not summary text, card JSON, signatures, tokens, or credentials. Because the
   transport has no caller-controlled `client_msg_no`, an ambiguous transport
   timeout may still duplicate and is not described as exactly-once.
10. **No fallback target.** Authorization, quota, intent, or transport failure
    is returned for that selected target. The server never substitutes a Bot
    DM, plain text, creator DM, or another channel. The UI may offer an explicit
    retry or explicit plain-text share as a new user action.

## Load-bearing list

- Summary visibility, revision, retention, and same-Space isolation.
- Short-lived intent signing, verification, replay defense, key rotation, and
  secret/config management across smart-summary, octo-server, and web.
- User-authenticated share endpoint, Space middleware, anti-enumeration errors,
  and exact actor binding.
- DM friendship/Space checks, group membership/post permission, and thread
  parent access.
- Server-owned card template/finalization and the narrow human-card provenance
  exception across every display surface.
- Per-user, per-DM, per-channel, and feature-wide traffic control.
- Per-target idempotency, privacy-safe audit events, metrics, feature flags,
  and rollback behavior.

## Out of scope

- Automatic task-completion delivery to the origin conversation.
- Bot-authored or Bot-attributed user shares.
- Arbitrary client-authored cards, free-form Adaptive Card JSON, generic user
  type-17 ingress, Bot API OBO cards, or “send as any UID”.
- Cross-Space sharing, public links, or changing summary visibility grants.
- Interactive `octo/v2` actions, inputs, callbacks, card edits, or revisions.
- Exactly-once transport delivery or automatic duplicate recall.

## Acceptance

- Unauthenticated, actor-mismatched, expired, replayed, tampered, wrong-
  audience, unauthorized, revoked/deleted, stale-revision, and cross-Space
  intents fail closed before transport with the service's safe localized error.
- A DM success persists one `octo/v1` card with `from_uid=login_uid` in the
  existing actor/peer conversation; no `notification` Bot conversation is
  created. Friendship/Space denial produces zero sends.
- Group and thread successes also persist `from_uid=login_uid`; authorization
  tests cover non-member/removed/banned actor, wrong Space, disabled/disbanded
  group, invalid/archived/deleted thread, missing parent, and storage errors.
- A mixed bounded target request returns stable per-target delivered/denied/
  retryable results. Failure of one target does not roll back successes or send
  to a fallback destination.
- Card bytes, deep link, `plain`, Space, and sender are server-owned. Contract
  tests reject request fields for sender, arbitrary card/payload, URL,
  attribution, OBO, subscribers, or transport metadata.
- A valid share proof renders on all server surfaces and web real-time/cold-
  sync paths. Missing, forged, wrong-target, wrong-actor, wrong-Space, or
  payload-tampered proofs are masked; existing unsigned human type-17 fixtures
  remain untrusted. Key-rotation tests verify old messages with retained public
  keys while new messages use the active key.
- Generic user type-17 ingress and Bot-OBO type-17 tests remain rejected. The
  no-bypass source guard recognizes only the reviewed server-minted share call
  site, and the existing Bot-bound dispatcher cannot accept a dynamic sender.
- Repeating actor + summary revision + target + idempotency key returns the
  recorded result without a second intentional send; changed target/content
  with the same nonce is rejected. Ambiguous transport duplicates remain
  documented and observable.
- UID, DM, channel, and producer quota tests cover multi-replica behavior,
  bounded retry-after, configuration validation, and fail-closed store errors.
- Audit tests prove success and denial outcomes are recorded without summary
  text, card JSON, proof/token, or secrets; metrics use bounded labels only.
- Independent server/web feature flags can disable user-share cards without
  disabling summary notifications or origin delivery; rollback requires no
  destructive data migration.
