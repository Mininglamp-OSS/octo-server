---
type: Task
title: "Task: bot-card-template-consumption"
description: Add Registry-backed template discovery and template_ref send/edit modes to the existing bot card capability and message APIs, while preserving the raw-card Model B path.
tags: [card, cardtmpl, bot-api, wire-contract, trust-boundary, space, isolation, auth, testing]
timestamp: 2026-07-24T13:51:54+08:00
# --- octospec extension fields ---
slug: bot-card-template-consumption
upstream: "roadmap E1b; depends on octo-server PR #657"
source: self
---

# Task: bot-card-template-consumption

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add the first **server-compiled Bot template consumption protocol (Model A)**
on top of the existing bot card protocol:

1. Extend the existing additive-only `GET /v1/bot/card/profile` manifest with
   one `templating` subtree that says this deployment can compile Registry
   templates, identifies the wire as `template-ref/v1`, and lists the
   explicitly Bot-callable `template id@version` entries and their views.
2. Add a Registry mode to `POST /v1/bot/sendMessage`: the bot sends
   `template_ref + state + data`; octo-server selects `state -> view/profile`,
   validates `data` against the template schema, renders through
   `cardtmpl.Registry.Render`, and sends the resulting type-17 envelope.
3. Add the symmetric Registry mode to `POST /v1/bot/message/edit`: the bot
   sends `template_ref + state + data + card_seq` (plus optional `transient`);
   octo-server renders the replacement frame and reuses the existing bot
   ownership, lifecycle, CAS, revision, and CMD-sync path.

The existing raw-card path remains supported as **Model B**. Old bot SDKs ignore
the additive `templating` manifest field and continue sending compiled
`card/content_edit`; new SDKs prefer Model A only when the advertised template
and wire are present.

The first advertised template is `ai.reasoning-process@0.1.0` from PR #657.
This task provides the server render/send/update protocol only; the OpenClaw
producer and the stop/retry business behavior are separate downstream work.

## Background

### Current state

- `pkg/cardtmpl.Registry` already owns registered templates and exposes
  `List`, `Lookup`, `Render`, and `RenderCardWithProfiles`.
- `GET /v1/bot/card/profile` currently advertises the Model B contract only:
  card enablement, card/profile versions, element/input/action allowlists, and
  payload limits. It does not advertise Registry compilation or templates.
- `POST /v1/bot/sendMessage` currently accepts a caller-compiled payload map.
  For type 17 it validates the supplied `card/profile/card_version`, injects
  authoritative Space metadata, recomputes `plain`, and dispatches it.
- `POST /v1/bot/message/edit` currently accepts a caller-compiled
  `content_edit` string. It validates/normalizes the full frame, verifies bot
  ownership and lifecycle, optionally applies `card_seq` CAS, records
  non-transient revisions, and emits message-extra sync.
- PR #657 registers `ai.reasoning-process@0.1.0` in the production Registry,
  but registration alone gives bots no runtime wire for selecting it.

### Why capability discovery and the wire land together

Advertising a Registry template before a bot can send or update it creates a
false capability. Conversely, landing an undiscoverable wire forces callers to
probe with requests. This task therefore adds the manifest subtree and both
send/edit modes atomically.

The existing manifest remains additive-only. `enabled:false` still returns
HTTP 200 and the complete capability manifest, including `templating`; the bot
checks top-level `enabled` before sending and can still learn what a future
enabled deployment supports.

### Capability manifest addition

The response adds one top-level field; all existing fields and meanings remain
unchanged:

```jsonc
{
  "enabled": true,
  "card_version": "1.5",
  "profiles": ["octo/v1", "octo/v2"],
  "elements": ["..."],
  "inputs": ["..."],
  "actions": ["..."],
  "limits": { "...": "existing fields unchanged" },
  "templating": {
    "supported": true,
    "wire": "template-ref/v1",
    "templates": [
      {
        "id": "ai.reasoning-process",
        "version": "0.1.0",
        "views": [
          {
            "name": "active",
            "states": ["answering", "reasoning"],
            "wire_profile": "octo/v2",
            "submit_actions": ["reasoning_stop"]
          },
          {
            "name": "error",
            "states": ["error"],
            "wire_profile": "octo/v2",
            "submit_actions": ["reasoning_retry"]
          },
          {
            "name": "result",
            "states": ["completed", "stopped"],
            "wire_profile": "octo/v1",
            "submit_actions": []
          }
        ]
      }
    ]
  }
}
```

Ordering is deterministic: templates by `id@version`, views by name, states and
submit action IDs lexicographically. Consumers must still treat every array as
an unordered capability set.

`templates` is **not** an unfiltered dump of `Registry.List()`. A small,
explicit, deployment-global Bot template catalog is the authorization boundary:

- only catalogued `id@version` pairs are advertised and accepted by Bot
  template-ref requests;
- startup validates every catalog entry against the frozen Registry and fails
  closed on a missing template, duplicate ref, empty view/state set, or
  interaction-report mismatch;
- internal notification templates such as `docs.access-request` and
  `summary.*` remain registered but are not Bot-callable;
- per-bot/per-owner template ACLs are deferred; v1 exposes the same explicit
  catalog to every authenticated bot.

`submit_actions` is derived from the registered view interaction report and
contains only `Action.Submit` IDs. It tells a producer which callback actions it
must understand before choosing the template. Local actions such as
`Action.ToggleVisibility` continue to be negotiated by the existing top-level
`actions` capability.

### Send wire: raw Model B XOR Registry Model A

Registry mode reuses the existing endpoint and type-17 envelope:

```jsonc
POST /v1/bot/sendMessage
{
  "channel_id": "g_xxx",
  "channel_type": 2,
  "payload": {
    "type": 17,
    "template_ref": {
      "id": "ai.reasoning-process",
      "version": "0.1.0"
    },
    "state": "reasoning",
    "data": { "...": "matches the registered data schema" },
    "mention": { "uids": ["u_xxx"] },
    "reply": { "message_id": "optional-existing-contract" }
  }
}
```

Rules:

- `template_ref.id`, `template_ref.version`, `state`, and `data` are required.
  The version is explicit; wire callers never silently follow a moving Registry
  default.
- `data` must be a JSON object. If that object contains a `state` field, it must
  be a string exactly equal to the outer wire `state`; a mismatch is a caller
  error. `ai.reasoning-process@0.1.0` requires this mirrored field, so the first
  producer sends both values identically. The outer field remains authoritative
  for Registry view selection.
- Callers send `state`, never `view` or `profile`. Registry metadata is the only
  authority for `state -> view -> wire_profile/render_profile`.
- Registry mode rejects caller-supplied render-owned fields: `card`, `plain`,
  `profile`, `card_version`, `render_profile`, and `space_id`.
- Existing orthogonal send-envelope features (`mention`, `reply`, and other
  already-accepted non-render fields) keep their current behavior. The handler
  carries only allowed existing fields onto the rendered payload before the
  normal server enrichment/finalization path.
- Raw Model B (`payload.card`) and Registry Model A (`payload.template_ref`)
  are mutually exclusive. Both present, neither present for type 17, or partial
  Registry fields all fail closed with the localized card-invalid response and
  zero dispatch.
- OBO/card exclusion, send authorization, Space resolution, mention rewrite,
  server-authoritative `plain`, final-size recheck, and dispatch behavior remain
  unchanged.

### Edit wire: raw content_edit XOR Registry replacement

Registry mode is additive to the existing edit request:

```jsonc
POST /v1/bot/message/edit
{
  "message_id": "8234567890123456789",
  "channel_id": "g_xxx",
  "channel_type": 2,
  "template_ref": {
    "id": "ai.reasoning-process",
    "version": "0.1.0"
  },
  "state": "answering",
  "data": { "...": "matches the registered data schema" },
  "card_seq": 1,
  "transient": true
}
```

Rules:

- `content_edit` raw mode and `template_ref` Registry mode are mutually
  exclusive; both present, both absent, or partial Registry fields fail closed.
- Registry edit requires a positive `card_seq`. It uses the existing CAS and
  returns the existing localized 409 conflict on stale/out-of-order frames.
- `transient:true` preserves the existing D10 meaning: apply and sync the frame
  but do not append it to revision history. Terminal frames omit/false it.
- The target must be an effective Registry-authored card whose stored
  `metadata.octo.template.{id,version}` exactly matches the requested ref.
  Cross-template and cross-version rewrites are rejected; a future migration
  protocol must be explicit rather than smuggled through this endpoint.
- Bot identity, original-message ownership, message-id/seq binding, channel and
  thread lifecycle, revoke/delete guards, card gate, CAS, revision append, and
  CMD sync reuse the existing edit path. Rendering must not bypass or weaken
  any of those checks.
- Every replacement is a complete frame rendered through
  `Registry.RenderCardWithProfiles`; the caller cannot supply a view/profile,
  metadata, raw card tree, or `plain` in Registry mode.

### Error contract

- Reuse `ErrBotAPICardDisabled`, `ErrBotAPICardInvalid`,
  `ErrBotAPICardSeqConflict`, and the existing generic internal-error facade;
  do not expose schema paths, template internals, or catalog membership through
  detailed errors.
- Unknown/unlisted template, unknown version/state, invalid data, malformed
  dual-mode requests, outer/data state mismatch, and stored-template mismatch
  map to the localized card-invalid envelope. A genuine server-side
  render/composition failure maps to the generic internal 5xx envelope
  (`Internal=true`), never to a caller 4xx. The exact cause is logged with
  bounded template identity and `zap.Error`.
- The frozen `ai.reasoning-process@0.1.0` schema lacks most string and array
  caps. Existing HTTP-body, JSON-expansion, card-node, depth, and final-payload
  budgets still fail closed before write, but a schema-valid oversized input can
  currently surface as a render 5xx rather than a precise schema 400. A separate
  hardened template version must land before production enablement; `0.1.0`
  itself is never edited in place.
- A render/validation failure is never downgraded to fallback text and never
  dispatches or edits a message. Model A caller mistakes are 400 + zero write.

## Load-bearing list

- **Capability wire contract (`wire-contract`, `bot-api`)** —
  `/v1/bot/card/profile` is additive-only. Existing keys are unchanged; the new
  `templating` subtree and `template-ref/v1` request shapes become cross-repo
  contracts consumed by bot SDKs and OpenClaw.
- **Bot template authorization (`auth`, `bot-api`, `trust-boundary`)** — only
  explicit Bot catalog refs may be advertised or rendered. `Registry.List()`
  is broader than Bot authority and must never be used as an implicit allow-all.
- **Dual-mode trust boundary (`trust-boundary`, `wire-contract`)** — raw and
  Registry modes are total XORs. Both-present, neither-present, and partial
  template requests fail closed; no branch silently ignores caller input.
- **Server-authoritative rendering (`wire-contract`, `trust-boundary`)** —
  callers choose only explicit template version, state, and schema data.
  Registry owns view/profile/render profile/card metadata; cardmsg owns final
  validation and authoritative `plain`.
- **Space and ownership (`space`, `isolation`, `bot-api`, `auth`)** — template
  send/edit reuse the existing bot-token identity, send authorization,
  authoritative Space injection, thread/group lifecycle, and edit ownership
  checks. No caller-provided Space or sender identity becomes authoritative.
- **Edit identity/CAS/revision contract (`wire-contract`, `bot-api`)** —
  Registry edits may only replace the same stored `id@version`, require
  monotonic `card_seq`, preserve transient revision semantics, and keep the
  existing message-extra/CMD synchronization path.
- **Localized error envelope (`wire-contract`)** — all new rejection branches
  use `httperr.ResponseErrorL` with existing registered codes; render/schema
  internals remain log-only.
- **Testing (`testing`)** — contract, security, source-of-truth, send/edit, and
  zero-side-effect failure tests are required across `pkg/cardtmpl` and
  `modules/bot_api`.

## Out of scope

- OpenClaw/channel-adapter changes, including field mapping, local-renderer
  removal, feature detection, retry/fallback behavior, or release packaging.
- `reasoning_stop` / `reasoning_retry` business semantics, active-run
  cancellation, retry orchestration, or automatic reasoning card session
  registration. Normal Bot actions continue to use existing `card_action`
  polling; no `cardactiondispatch.RouteSpec` is added here.
- Public `GET /v1/message/card/templates` or artifact/schema export endpoints
  (roadmap B1/B2). The Bot capability contains a bounded runtime summary, not
  manifests, JSON Schema, templates, samples, or goldens.
- Internal notify envelope mode (roadmap E2) and any change to
  `/v1/internal/notify`.
- L2b enablement, `ext.*` owners, `manifest.visibility`, callback-domain
  allowlists, or business-owned template upload.
- Per-bot, per-owner, per-Space, or dynamic database-backed template ACLs. V1
  uses one deployment-global, code-reviewed Bot catalog.
- Implicit/default version selection, cross-template/version message migration,
  template deletion lifecycle, or remote template loading.
- Removing or weakening raw Model B, changing existing raw-card validation, or
  changing client rendering behavior.
- New database tables or migrations, new routes, new rate limiters, or new
  error codes.

## Acceptance

### Capability manifest

- `GET /v1/bot/card/profile` retains every existing field and adds exactly one
  additive `templating` object with `supported`, `wire`, and `templates`.
- The first catalog entry is `ai.reasoning-process@0.1.0`; its three views,
  five states, wire profiles, and Submit action IDs match the frozen Registry
  metadata and interaction reports.
- Templates/views/states/action IDs have deterministic output ordering, and a
  contract test pins field names/types while allowing future additive fields.
- The catalog is an explicit allowlist: Registry-only docs/summary templates
  are not advertised and template-ref requests for them are rejected.
- Startup/catalog construction fails closed for missing or duplicate refs,
  missing view/state metadata, or interaction-report drift.
- `enabled:false` still returns HTTP 200 with the same full `templating`
  capability; unauthenticated/bad-token behavior is unchanged.

### Registry send mode

- A valid `ai.reasoning-process@0.1.0` `reasoning` request dispatches exactly
  one type-17 message rendered from the `active` view,
  `profile=octo/v2`, `render_profile=octo-chat/v1`, explicit template
  id/version, authoritative Space, and server-recomputed `plain`.
- `completed` renders the `result` view with `profile=octo/v1`; the caller never
  sends either the view or profile.
- Existing allowed mention/reply behavior survives Registry rendering and the
  final post-enrichment size check still runs on the actual wire payload.
- Unknown/unlisted id, missing/unknown version, unknown state, schema-invalid
  data, malformed JSON data, outer/data state mismatch, and partial template
  mode return the localized card-invalid envelope and dispatch zero messages.
  Internal render failures return the generic internal 5xx envelope and also
  dispatch zero messages.
- Raw+Registry both-present and type-17 neither-present are rejected; ordinary
  raw Model B requests remain byte/behavior compatible.
- Caller attempts to inject `card`, `plain`, profile fields, render metadata, or
  `space_id` in Registry mode fail closed. Existing OBO card rejection and send
  permission tests remain green.

### Registry edit mode

- A valid same-template update renders the requested state through Registry,
  preserves authoritative `metadata.octo.template`, and writes/syncs one
  complete replacement frame through the existing edit path.
- Cross-template/version ref, non-Registry target, non-owner bot, message
  id/seq mismatch, revoked/deleted target, disbanded group/deleted thread, and
  invalid template data all fail closed with zero message-extra/revision/CMD
  side effects.
- Registry mode requires positive `card_seq`; a stale frame returns the
  existing 409 code and does not overwrite the stored frame. An identical
  retry at the same seq retains existing idempotent behavior.
- `transient:true` updates the effective frame but appends no revision;
  terminal/false appends according to the existing cap and tombstone rules.
- Raw `content_edit` mode remains behavior compatible; raw+Registry
  both-present and both-absent are rejected.

### Verification

- Focused tests pass:
  `go test ./pkg/cardtmpl/... ./modules/bot_api/... -count=1`.
- Shared card/message tests pass:
  `go test ./pkg/cardmsg/... ./internal/carddispatch/... -count=1`.
- `go test -race ./pkg/cardtmpl/... ./modules/bot_api/...` passes in the
  required MySQL/Redis/WuKongIM test environment.
- `go build ./...`, `go vet ./pkg/cardtmpl/... ./modules/bot_api/...`,
  `make i18n-extract-check`, `make i18n-lint`, and `git diff --check` pass.
- No database migration, new route, new rate limiter, or raw response pattern
  is introduced.

## Decisions for human confirmation

- **D1 — atomic scope:** capability advertisement plus send and edit Registry
  modes land in one server PR; no advertised-but-unusable intermediate state.
- **D2 — same manifest:** extend `/v1/bot/card/profile`; do not create a second
  capability endpoint.
- **D3 — explicit catalog:** Bot availability is a code-reviewed allowlist over
  the frozen Registry, not all of `Registry.List()`.
- **D4 — explicit version:** every wire request pins `id@version`; Registry
  defaults are not a cross-repo wire contract.
- **D5 — state is caller input, view is server output:** callers never choose
  view/profile/render profile.
- **D6 — total XOR:** raw Model B and Registry Model A are mutually exclusive on
  both send and edit, including explicit both-present rejection.
- **D7 — immutable update identity:** Registry edit may change state/view but
  not template id/version.
- **D8 — mandatory edit CAS:** Registry edit requires positive `card_seq`; raw
  mode keeps its current optional behavior.
- **D9 — existing error codes:** detailed Registry/schema failures stay in
  logs; the public API reuses card-invalid/disabled/seq-conflict envelopes.
- **D10 — normal Bot action path:** advertised Submit IDs tell the producer what
  it must support; click-back remains the existing `/v1/bot/events`
  `card_action` flow, not a new RouteSpec.
- **D11 — one state authority:** the outer wire `state` selects the Registry
  view; when `data.state` exists it must exactly mirror the outer value, otherwise
  the request fails as card-invalid with zero side effects.
- **D12 — frozen schema hardening:** do not mutate published `0.1.0`; publish a
  bounded successor and advertise that version before enabling Model A in
  production. Cap values come from the producer/UX contract plus the platform
  card budgets, not from an arbitrary E1b default.
