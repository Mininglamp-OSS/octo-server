---
type: Task
title: "Task: cardtmpl-provenance-markers"
description: Server-authored catalog provenance markers on stored card frames — authored where they cannot be invalid, refused at raw ingress, and preserved across every replacement.
tags: [card, cardmsg, cardtmpl, provenance, wire-contract, trust-boundary, notify, bot-api, testing]
timestamp: 2026-08-06T00:00:00+08:00
slug: cardtmpl-provenance-markers
upstream: "split out of cardtmpl-runtime-catalog-grants-discovery (E3 PR-C); parent brief holds D3"
source: self
---

# Task: cardtmpl-provenance-markers

This is the **ungated** half of E3 PR-C, split out for review after round 7.

The parent task —
[`cardtmpl-runtime-catalog-grants-discovery`](../cardtmpl-runtime-catalog-grants-discovery/brief.md)
— owns the contract of record, including **D3**, which this task implements.
Nothing here is a scope change; the split exists because the two halves need
different merge arguments:

- **This task takes effect on merge.** It sits behind the pre-existing
  `OCTO_CARD_MESSAGE_ENABLED` switch, not behind either runtime-catalog gate,
  so wherever cards are already on, merging changes stored bytes and can make
  one previously-succeeding send fail. That deserves line-by-line review.
- **The parent's remaining half is inert on merge.** Grants, one-snapshot
  authorization, discovery/export and the pilot are all behind
  `OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED` / `_NEW_SEND_ENABLED`, both
  default false. It can merge on that argument once this lands.

## Scope

A dynamic card frame carries two server-authored top-level markers,
`template_ref` and `catalog_provenance`, so historical edit and action-context
read the producer from the frame instead of inferring it from `msg.FromUID`.

Three properties, each with a test that fails when its fix alone is reverted:

1. **A marker cannot be authored invalid.** Both authoring boundaries validate
   before stamping. `cardmsg.Validate`'s marker hook runs before the keys exist
   and `Finalize` does not re-check, so the authoring site is the last point
   that can refuse. An untrimmed target Space otherwise produced a frame every
   reader rejects — permanently unclickable and uneditable.
   `TestSendRefusesToAuthorAMarkerItsReadersWouldReject`.
2. **A marker cannot be forged.** No external input can carry the two keys in.
   The entry points do not all achieve that the same way, and the difference is
   worth stating because a single sentence hid a real gap for three rounds (the
   normative per-ingress list is under Acceptance): bot raw send and robot
   ingress **reject by key**; bot template
   mode has no such field in its request shape and the server authors the
   markers at the rendering boundary; the incoming webhook builds its envelope
   from a fixed field allowlist so caller keys never reach the top level; and
   the thread source-message copy **refuses type-17 payloads outright**, because
   there the caller's bytes are persisted under another sender's identity and
   the identity the action route trusts lives inside the card body, where
   stripping the top-level keys does not reach.
3. **A marker cannot be dropped.** `CardMutator.Mutate` refuses a replacement
   that loses markers the stored frame carries, so no call site can silently
   downgrade a marked card into the legacy population and leave the identity
   guards inert. `TestCardMutatorRefusesToDropStoredCatalogMarkers`,
   `TestDocsActionFinalizerRoutesEveryStateThroughTheRegistry`,
   `TestStandardActionFinalizerCannotSilentlyDowngradeAMarkedCard`.

## Behaviour changes worth an owner's attention

- **A thread created from a card message no longer copies a first message.**
  The copy path persists caller-supplied bytes under the source message's
  sender and never checks the id against the payload, so for a card that is a
  way to send a card as another user — which the user ingress bans outright.
  Refusing type-17 there matches that ban. The thread is still created and
  `source_message_id` is simply not announced, so nothing claims a first
  message that does not exist. Owner: whoever owns `modules/thread` should
  confirm the product is willing to lose the card preview on that flow; the
  alternative is authorizing the copy through the same visibility gates a
  single-message read applies, which is a larger change than this slice.

## Out of scope

Grants, the grant table and its migration, one-snapshot authorization, the Bot
capability manifest, B1/B2 discovery and export, the localized Space
middleware, and the pilot. All of those stay in the parent task.

## Acceptance

- **No external input can author or persist either marker.** That is the
  invariant; "rejected by key" is only one of the ways it is met, and stating it
  as if it were the only one is what hid a real gap for three rounds. Per
  ingress:
  - bot raw send / raw edit, and robot ingress — **reject the request** when
    either key is present;
  - bot template mode — the request shape has no such field, and the server
    authors both markers itself at the rendering boundary;
  - incoming webhook — builds its envelope from a fixed field allowlist
    (`buildCardPayload`), so caller keys never reach the top level; a request
    carrying them is **not rejected for that reason**, the keys are simply not
    copied. The allowlist covers the two markers only: the caller still owns the
    whole `card` node including `metadata.octo.template`. That is unchanged
    legacy behaviour and not an impersonation — a webhook sends under its own
    identity, and a markerless frame takes the static-`ActionContext` branch,
    which authorizes no principal. It is the same in-body identity that made
    key-stripping insufficient one bullet down, where the sender *is* someone
    else's;
  - thread source-message copy — **refuses type-17 payloads outright**, because
    key-stripping does not reach the in-body identity
    (`card.metadata.octo.template`) that the action route falls back to;
  - user send / user edit — never reaches a marker question at all: cards are
    refused as a message type (`modules/message/api.go`), both when the edit
    body is one and when the target is.
- Both authoring boundaries refuse to write a marker their readers reject.
- A replacement frame cannot drop markers the stored frame carries, and the
  refusal is enforced at the mutation boundary rather than per call site.
- Every docs finalizer state renders through the Registry, so the route cannot
  be selected by a state allowlist that a later state would silently escape.
- `docs.access-request@0.3.0` declares `cancelled` and `unavailable`, the two
  outcomes the pre-Registry fallback used to render.
