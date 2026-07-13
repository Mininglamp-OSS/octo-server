---
type: Journal
title: "Journal: card-message-p2-capability-manifest (PR-D, card message P2 D12)"
description: Record of PR-D — the D12 producer capability manifest GET /v1/bot/card/profile (bot-token, existing bot_api chain, no new limiter/Space middleware), values sourced from a single pkg/cardmsg authority (AcceptedProfiles) with a drift-guard test; enabled:false still returns 200; additive-only wire contract; no new errcode/i18n/DB.
tags: ["card", "wire-contract", "bot-api", "space", "isolation", "testing"]
timestamp: 2026-07-08T15:00:00Z
# --- octospec extension fields ---
task: card-message-p2-capability-manifest
upstream: self
source: self
---

# Journal: card-message-p2-capability-manifest (PR-D, card message P2 D12)

## What was done

Shipped D12 of the `card-message-interaction` contract — **producer capability
discovery**. A read-only `GET /v1/bot/card/profile` lets card producers (bot
SDKs, the OpenClaw channel adapter) feature-detect the deployment's card
capabilities instead of send-probing, where a `400` cannot distinguish "cards
disabled" from "card invalid". Independent of PR-B (#548) / PR-C (#549), both
already merged to main.

- **`pkg/cardmsg/profiles.go`**: `acceptedProfiles` — the single authority for
  the accepted profile set + their interactive tier — and `AcceptedProfiles()
  []string` (returns a fresh copy). `interactiveByProfile` (validate.go) now
  derives its accepted set from the same slice instead of a hardcoded switch.
- **`modules/bot_api/card_profile.go`**: `botCardProfile` handler on the
  existing `authBot()` group — no new rate limiter, no Space middleware.
  Every manifest value is sourced from a `pkg/cardmsg` constant
  (`Enabled()` / `CardVersion` / `AcceptedProfiles()` / the five `Max*` limits),
  not a re-typed literal.
- **Route reg**: one line in `bot_api.go`.
- **Docs**: `docs/card-protocol.md` D12 enumeration synced to include
  `card_version`.

## Structural learnings

- **Manifest/capability endpoints must source every field from a single
  in-package authority, and prove non-drift with a test.** The `profiles` set
  could trivially have been a hardcoded `["octo/v1","octo/v2"]` in the handler;
  instead it comes from `AcceptedProfiles()`, derived from the *same*
  `acceptedProfiles` slice the validator's `interactiveByProfile` consumes.
  `TestAcceptedProfilesSingleAuthority` asserts every advertised profile is
  accepted by the validator (and unknowns are rejected), so the wire contract
  can never advertise a capability the server doesn't actually honor. This is
  the reusable pattern for any "server tells clients what it supports" endpoint.
- **`enabled:false` must still return 200 with the full manifest.** The rollout
  gate (`cardmsg.Enabled()`) governs *send/edit*, not the manifest read — the
  whole point of feature detection is that a producer can read `enabled:false`
  and fall back. `TestBotCardProfile_DisabledStillReturnsManifestAndSendRejects`
  pins both halves in one test (manifest 200 + `enabled:false`, while a card
  send still rejects via the same `Enabled()` gate) so the two can't drift.
- **The smallest of the three P2 PRs by design.** Unlike PR-B/PR-C, D12 needed
  **no new errcode / i18n / DB / migration / rate limiter / Space middleware** —
  a read-only manifest over frozen constants. The only rejection path is the
  existing `authBot()` bot-token check. Calling this out in the brief (D12.5)
  kept the reviewer from expecting an `errcode` diff.

## Gotchas

- Additive-only wire contract (same rule as `event_data`): a contract test
  (`TestBotCardProfile_AdditiveContractFieldSet`) pins the exact top-level +
  `limits` field set, so a future rename/removal fails CI. New capabilities are
  added as new fields, never by re-typing.
- `c.Response(map)` emits the map as the top-level JSON body (no envelope
  wrapper) — the additive-contract test relies on this (verified against the
  sibling `getMentionPref` decode pattern).
