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
2. **A marker cannot be forged.** Every raw ingress — bot, robot, incoming
   webhook, user edit — rejects both keys.
3. **A marker cannot be dropped.** `CardMutator.Mutate` refuses a replacement
   that loses markers the stored frame carries, so no call site can silently
   downgrade a marked card into the legacy population and leave the identity
   guards inert. `TestCardMutatorRefusesToDropStoredCatalogMarkers`,
   `TestDocsActionFinalizerRoutesEveryStateThroughTheRegistry`,
   `TestStandardActionFinalizerCannotSilentlyDowngradeAMarkedCard`.

## Out of scope

Grants, the grant table and its migration, one-snapshot authorization, the Bot
capability manifest, B1/B2 discovery and export, the localized Space
middleware, and the pilot. All of those stay in the parent task.

## Acceptance

- Raw ingress carrying either key is rejected on every path (bot, robot,
  incoming webhook, user edit).
- Both authoring boundaries refuse to write a marker their readers reject.
- A replacement frame cannot drop markers the stored frame carries, and the
  refusal is enforced at the mutation boundary rather than per call site.
- Every docs finalizer state renders through the Registry, so the route cannot
  be selected by a state allowlist that a later state would silently escape.
- `docs.access-request@0.3.0` declares `cancelled` and `unavailable`, the two
  outcomes the pre-Registry fallback used to render.
