---
type: Task
title: "Task: vcs-webhook-card-forge-align"
description: Align every server-authored GitHub/GitLab incoming-webhook card with octo-web's Forge visual profile and docs-card layout language, using one reviewed neutral inline icon and preserving event, trust, URL, fallback, and delivery contracts.
tags: ["incomingwebhook", "adapter", "webhook", "card", "wire-contract", "render-profile", "trust-boundary", "external-content", "url-destination", "testing"]
timestamp: 2026-08-08T12:00:49+08:00
# --- octospec extension fields ---
slug: vcs-webhook-card-forge-align
upstream: self
source: self
---

# Task: vcs-webhook-card-forge-align

> This brief is based on `octo-server` main at
> `75edcf3869a0f06e0521c0d8d8d27bb9cd4296dc` and a fresh checkout of
> `Mininglamp-OSS/octo-web` main at
> `5269665bc904e54ca3b89d08c5d2258f916f9a26`. Re-check both heads before
> implementation because the client render-profile and width contracts are
> cross-repository dependencies.

## Goal

Refresh all existing **server-authored GitHub and GitLab incoming-webhook
InteractiveCards** so they use octo-web's Forge visual profile and the same
compact header / semantic badge / accent surface hierarchy as the current docs
cards.

The user-visible result is one coherent VCS card family:

- a compact header with a reviewed neutral VCS icon, visible `GitHub` or
  `GitLab` source text, repository/project context, and an event/status badge;
- a neutral headline and structured content surface instead of coloring the
  whole Pipeline title;
- readable facts and long values, including English Pipeline labels and one
  job name per line;
- the existing localized `Action.OpenUrl` navigation, text fallback, event
  semantics, and trust boundary unchanged.

This is a presentation change to the existing VCS producer. It is **not** a
`cardtmpl` migration and does not make externally supplied webhook cards
platform-owned.

## Verified background

### octo-web rendering contract

- Current octo-web fixes every InteractiveCard outer width at
  `width: min(480px, 100%)` and makes both legacy/Forge mount roots fill that
  width (`InteractiveCard/index.css`). Equal desktop width and narrow-screen
  shrink are therefore client-container behavior; the server must not add an
  empty stretch column solely to simulate a fixed outer width.
- `render_profile` is active, not reserved. Missing/empty permanently selects
  `legacy`; explicit `octo-chat/v1` selects the packaged Forge HostConfig
  (`renderProfile.ts`, `renderOctoCard.ts`, and `renderDecision.test.tsx`).
- Incoming-webhook senders (`iwh_*`) are trusted for display but remain
  submit-read-only. This task adds no interactive actions or inputs.

### Inline-image validation contract

- `pkg/cardmsg` does **not** generically allow `data:image/svg+xml`.
  `checkImageURL` accepts only an exact match from `vettedInlineImages`; every
  other image URI falls back to the absolute HTTP(S) allowlist.
- Existing tests explicitly reject an arbitrary harmless SVG, a one-byte
  mutation, surrounding whitespace, appended content, and a base64-encoded SVG.
  Therefore an embedded VCS icon must be deliberately added to the exact-byte
  allowlist; merely copying a `data:` URI into `adapter_card.go` would make every
  VCS card fail validation and degrade to text.
- The current docs v3 cards use HTTPS Iconify/Lucide URLs; they do not prove
  that arbitrary or base64 SVG data URIs pass server validation.
- octo-web can safely render bounded SVG data URIs after its own sanitizer, but
  the server's stricter exact-byte gate remains authoritative and must not be
  weakened to the client's broader policy.

### Icon source and license decision

- Do not ship GitHub or GitLab logo artwork in this task. Those marks carry
  separate brand/trademark usage conditions and no approved repository asset
  was found in current octo-web/main.
- Use the neutral Lucide `git-branch` icon for both sources; visible source text
  differentiates GitHub from GitLab. The reviewed source is Lucide
  `0.577.0`, official repository `lucide-icons/lucide`, licensed under ISC.
- Vendor one compact, URL-encoded `data:image/svg+xml,...` constant with a
  fixed neutral stroke suitable for the Forge card header. Do not use base64,
  runtime Iconify/CDN fetches, arbitrary SVG registration, or caller-supplied
  icon bytes.
- Add the Lucide copyright, source/version, and ISC license notice to the
  repository's third-party notice material. The icon constant must document
  the upstream file and any byte-level modifications (size/color/minification).

## Proposed design

### 1. Shared VCS card anatomy

Keep `vcsCardData.card` as the single GitHub/GitLab assembler and evolve it to
produce one shared structure:

1. **Header `ColumnSet`**
   - auto column: 16px reviewed neutral `git-branch` Image;
   - stretch column: small/bolder visible source label plus subtle repository or
     project context, with wrapping/truncation bounded by existing helpers;
   - optional auto column: small/bolder event or status badge.
2. **Accent content `Container`**
   - `style: "accent"`, `bleed: true`, separator and explicit spacing;
   - neutral bolder headline (never status-colored as a whole);
   - existing title/commit/comment lines, FactSet, and quote content using the
     current escaped and rune-bounded leaves.
3. **Navigation footer**
   - when a valid destination exists, render a full-bleed `emphasis` Container
     with separator and one `ActionSet` carrying the existing localized
     `Action.OpenUrl`;
   - remove the duplicate top-level `actions` form so each card still exposes
     exactly one navigation action;
   - the current destination must still pass `httpURLForCard` and
     `cardmsg.Validate`; an invalid/missing URL omits the whole footer rather
     than rejecting delivery.

The shared assembler must keep both sibling adapters structurally identical.
Event builders supply only event-specific content and safe semantic literals.

### 2. Event and status presentation

- Cover all current builders, not only the screenshot's Pipeline card:
  - GitHub: push/tag-via-push, pull request, issue, issue comment, release;
  - GitLab: push, tag push, merge request, issue, note, pipeline.
- Pipeline maps known statuses to semantic badge colors (`Good`, `Attention`,
  `Warning`, `Accent`) while the headline remains neutral. Unknown statuses use
  the default badge color and remain visible as escaped text.
- Pipeline fact titles are always the English literals `Branch`, `Status`,
  `Duration`, and `Jobs (N)`, independent of outbound language.
- Pipeline jobs retain current ordering, non-blank count, per-item rune cap,
  maximum rendered count, and overflow marker, but render one job per line.
  Do not change the shared slash-joined representation used by MR/PR/Issue
  `Labels (N)`.
- Existing user-visible action localization remains unchanged.

### 3. Server-owned Forge selection

- VCS adapter cards explicitly emit top-level
  `render_profile: "octo-chat/v1"` so octo-web selects the Forge HostConfig.
  This field is a sibling of `profile`, not part of the Adaptive Card document
  or `metadata.octo`.
- Author the value through a package-internal, non-JSON field on the internal
  `pushPayloadReq` (or an equivalently closed server-only path). `vcsPushReq`
  sets the constant; `buildCardPayload` copies it before
  `cardmsg.Validate`/`Finalize`.
- Native callers sending `msg_type:"card"` cannot provide or override this
  internal value and continue to omit `render_profile` unless a separate
  contract explicitly changes them. Do not default every incoming card to
  Forge based only on message type.
- `profile` remains `octo/v1`; no `card_profile` field and no Submit/Input
  capability is introduced.

### 4. Exact vetted inline icon

- Add one exported, immutable VCS icon constant to `pkg/cardmsg` and include
  that exact string in `vettedInlineImages`; the shared card assembler reuses
  the same constant instead of duplicating bytes.
- Keep `checkImageURL`'s model unchanged: exact vetted bytes or absolute
  HTTP(S). Do not add a registration API, caller trust mode, SVG parser, broad
  MIME allowlist, or adapter-specific bypass.
- The icon stays below the existing 1 KiB icon limit. A one-byte mutation,
  whitespace variant, base64 re-encoding, or any other SVG remains rejected.
- `httpURLForCard` remains the gate for navigation destinations. It is not the
  image gate and must not be relaxed to accept `data:`.

## Load-bearing list

- **Shared card producer:** `modules/incomingwebhook/adapter_card.go` owns the
  common structure, leaf escaping, URL preflight, size/count limits, status
  colors, metadata, and card self-validation.
- **GitHub/GitLab event builders:** `adapter_github.go` and
  `adapter_gitlab.go` retain every current event/action/status decision and
  supply only the existing bounded data to the shared assembler.
- **Server-owned envelope:** `pushPayloadReq`, `vcsPushReq`, and
  `buildCardPayload` must distinguish server-authored VCS cards from external
  native `msg_type:"card"` requests before adding `render_profile`.
- **Trust boundary:** actor, repository/project, ref, title, commit, job,
  label, action/status, and comment text remain attacker-influenced. Every leaf
  continues through the existing clamp/escape helpers; every actionable URL
  remains absolute HTTP(S).
- **Image security:** only reviewed exact bytes may bypass the general HTTP(S)
  image URL rule. Client sanitization is defense in depth, not server authority.
- **Authoritative plain:** `Finalize` must still derive a non-placeholder,
  readable `plain` containing the meaningful event content after the body is
  rearranged.
- **Degrade and delivery:** flag-off text bytes, card-build failure to text,
  ping/skip/no_event, audit, auth, mention expansion, and final payload-size
  checks remain unchanged.
- **Client compatibility:** explicit `octo-chat/v1` is required for the Forge
  layout; current octo-web main is the verified client baseline.
- **Third-party attribution:** vendoring the Lucide-derived icon requires the
  upstream copyright and ISC notice to ship with this repository.

## Out of scope

- No GitHub mark, GitLab tanuki, Simple Icons brand asset, or other trademarked
  platform artwork without a separately approved asset and brand review.
- No arbitrary SVG ingestion, general `data:` URL support, base64 exception,
  remote icon configuration, CDN dependency, or new SVG sanitizer.
- No change to supported GitHub/GitLab event types, action/status acceptance,
  token/signature authentication, audit outcomes, rate limits, or input caps.
- No change to markdown text fallback bytes or to
  `OCTO_CARD_MESSAGE_ENABLED` behavior/default.
- No `cardtmpl` migration, template/version registry entry, persistence
  migration, card revision history, callback route, Input.*, or Action.Submit.
- No octo-web implementation change: current main already owns width,
  render-profile selection, Forge CSS/HostConfig, sender trust, and client-side
  SVG sanitization.
- No redesign of WeCom, Feishu, Multica, native webhook cards, bot cards, or
  platform docs/AI templates.

## Acceptance

### Structural and wire assertions

- Every one of the 11 current GitHub/GitLab card builders returns a non-nil
  card for its valid fixture, uses the same header/content/action anatomy,
  passes `cardmsg.Validate`, and derives a non-placeholder `plain`.
- Header tests assert the exact vetted icon URI, visible `GitHub`/`GitLab`
  label, repository/project context, and appropriate event/status badge.
- Pipeline headline has no semantic `color`; its status badge carries the
  mapped semantic color. Known, in-progress, unknown, and missing statuses keep
  their current render/skip semantics.
- Both `en-US` and `zh-CN` Pipeline cards contain English `Branch`, `Status`,
  `Duration`, and `Jobs (N)`. Job values use newline separators while MR/PR/
  Issue label values remain slash-joined.
- A production envelope built from a valid VCS adapter request contains exactly
  `profile:"octo/v1"` and `render_profile:"octo-chat/v1"`; the value is present
  before validation/finalization and survives transport serialization.
- A native external `msg_type:"card"` JSON body cannot set the package-internal
  render profile and does not gain `render_profile` by default.

### Image and trust-boundary assertions

- The new Lucide-derived VCS URI passes `pkg/cardmsg.Validate` only at
  `Image.url` / `ImageSet.images[].url` and stays below 1 KiB.
- Arbitrary SVG, a one-byte icon mutation, whitespace/appended-content variants,
  and base64 re-encoding still fail with `ErrCardBadURLScheme`; the vetted URI
  remains forbidden in action URL/iconUrl and background-image fields.
- Existing hostile GitHub/GitLab fixture coverage remains green: external
  markdown cannot create links/emphasis/lists, non-HTTP(S) destinations cannot
  become actions, and malformed icon/navigation data degrades safely.
- `NOTICE` (or the repository's canonical third-party notice location)
  contains the Lucide 0.577.0 source, copyright, and ISC license notice.

### Compatibility and visual verification

- With `OCTO_CARD_MESSAGE_ENABLED` off, representative GitHub and GitLab events
  produce the historical text payload byte-for-byte.
- With it on, invalid server-built cards still log and degrade to text rather
  than turning a valid webhook delivery into a new 4xx.
- Render representative GitHub Push/PR/Issue and GitLab Push/MR/Pipeline cards
  against current octo-web/main in both light and dark themes. Capture PR
  evidence showing:
  - equal 480px desktop outer width and responsive shrink below 480px;
  - no horizontal overflow for long repository, branch, title, labels, jobs,
    or comment values;
  - Forge header/accent/badge hierarchy and neutral Pipeline headline;
  - readable missing-optional-field states and a visible OpenUrl action when a
    valid destination exists.
- The visual check must use a final type-17 envelope containing
  `render_profile:"octo-chat/v1"`; rendering the card document alone is not
  sufficient evidence.

### Verification commands

```bash
go test ./pkg/cardmsg -run 'TestInlineImage|Test.*Image' -count=1
go test ./modules/incomingwebhook/... -count=1
go test ./pkg/cardmsg/... -count=1
golangci-lint run ./pkg/cardmsg/... ./modules/incomingwebhook/...
git diff --check origin/main...
```

If implementation adds a new handler-bearing Go file, update
`TestIncomingWebhookNoLegacyResponseError`'s source list. No i18n extraction is
required unless user-facing error codes or localized error behavior change.

## Rollout and rollback

- Use the existing `OCTO_CARD_MESSAGE_ENABLED` gate; add no second rollout flag.
- Before rollout, verify the deployed octo-web version supports
  `octo-chat/v1` and the 480px wrapper contract. Older clients that do not
  accept the explicit render profile may show their existing update/fallback
  behavior, so client-version readiness is a release gate.
- Monitor the existing “built VCS card failed self-validation; degrading to
  text” warning and card-render fallback/client error telemetry. A rise after
  deployment indicates icon/profile/layout incompatibility.
- Roll back the octo-server image (or disable the existing card-message gate)
  to restore the prior card/text behavior. No database or historical-message
  rollback is required because these cards are fire-and-forget and this task
  adds no persistence schema.
