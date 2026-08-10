---
type: Task
title: "Task: cardtmpl-reasoning-bleed-successor"
description: Publish ai.reasoning-process@0.4.1 as the immutable successor to 0.4.0, adopting the incoming full-bleed presentation while preserving the server-owned bounded contract and Bot cutover rules.
tags: [card, cardtmpl, ai-reasoning-process, json-template, bot-api, wire-contract, test, testing, rollback]
timestamp: 2026-08-10T14:30:00+08:00
# --- octospec extension fields ---
slug: cardtmpl-reasoning-bleed-successor
upstream: "front-end handoff attachment ai.reasoning-process@0.3.1.handoff.zip sha256 c708905b18198b266479d5c7e4e2904d7c70a99fdd47f7c0117fcfc573c09433; supersedes no published artifact"
source: user
---

# Task: cardtmpl-reasoning-bleed-successor

## Goal

Publish the immutable built-in artifact `ai.reasoning-process@0.4.1` from the
attached front-end handoff. `0.4.0` is already published, so the handoff's
internal `0.3.1` identity must not be used to overwrite any historical card.

The release adopts the front-end's full-bleed root container (`bleed: true`) in
all three templates and records its `octo-chat@1.2.0-rc.3` render-profile
provenance. It registers V5 with the static registry, makes V5 the default and
the sole Bot new-send advertisement, while retaining V1–V4 for exact-version
historical edits.

## Load-bearing decisions

1. Start from the shipped `0.4.0` artifact, not from the attachment as a whole.
   Its V4 data schema, aggregate cap, thought truncation, reports, samples, and
   vetted percent-encoded SVG icon bytes remain authoritative.
2. Apply only the presentation delta that changes the card: `bleed: true` on
   the root `Container` of `active`, `result`, and `error`. Regenerate all five
   goldens from the server-owned templates and samples.
3. Use `renderProfile: octo-chat@1.2.0-rc.3` as manifest provenance only;
   `renderProfileCompatibility` remains `octo-chat/v1`. The server does not
   ship or validate front-end `render-profile/` resources.
4. Keep `owner=ai`, `protocol=octo-card@1.0`, the five-state/view/wire mapping,
   no manifest `submit_actions`, and no `actionType`.
5. Do not import the handoff's unbounded schema, optional unused `phaseState`,
   `reports/result.interaction.json`, or render-profile directory. Preserve the
   V4 `statusGlyph` binding; hardcoding `•` would erase the producer's per-tool
   success/failure signal. Preserve the existing vetted percent-encoded SVG
   bytes rather than broadening the inline-image allowlist for base64 variants.
6. This is an image-release cutover only. Do not use runtime activation or
   rollback pointers to switch the built-in exact version.

## In scope

- Add the V5 artifact and exact-version constants.
- Register V5, move the default and Bot `AdvertisedSend` to V5, and extend
  edit compatibility through V5.
- Update focused artifact, registry, composition-root, catalog, and Bot-profile
  assertions so a regression cannot advertise an unregistered version.

## Out of scope

- Changing V1–V4 artifacts or stored historical cards.
- Relaxing schema, node, payload, persistence, or inline-image security limits.
- Adding new producer fields, client controls, routes, migrations, error codes,
  runtime-catalog activation, or a consumer-plugin release.

## Acceptance

- [ ] V5 manifest has exact identity `ai.reasoning-process@0.4.1`, owner and
  protocol, contract `1.2.0`, render profile `octo-chat@1.2.0-rc.3`, and the
  unchanged five-state/view/wire shape with no Submit capability.
- [ ] All V5 root template containers carry `bleed: true`; no V5 template
  hardcodes a tool status glyph or changes the V4 toggle surface.
- [ ] The V5 schema is byte-identical to V4; it retains all bounds and
  `x-octo-constraints`, excludes `phaseState`, and V5 ships neither a
  render-profile directory nor a result interaction report.
- [ ] Goldens match server rendering for all five samples and the rendered V5
  frames remain valid through the production persistence normalization path.
- [ ] V1–V4 remain unchanged; registry contains V1–V5 and defaults to V5;
  Bot new sends advertise only V5 while exact-version edits accept V1–V5.
- [ ] Focused `pkg/cardtmpl/ai_reasoning_process`, `modules/bot_api`,
  `modules/card_template_catalog`, and composition-root tests pass, followed by
  `go build ./...`, `go vet ./...`, and `git diff --check`.

## Rollback

Roll back by deploying the previous image, whose static catalog advertises V4.
Do not mutate V5 or activate a runtime-catalog pointer: already-created V5
cards retain their exact artifact identity and must remain renderable on images
that keep V5 registered.
