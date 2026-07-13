---
type: Task
title: "Task: card-message-p2-capability-manifest"
description: PR-D of the card message P2 protocol — D12 producer capability discovery. A read-only GET /v1/bot/card/profile (bot-token auth, existing bot_api middleware chain, no new rate limiter) that returns the deployment's card capability manifest — enabled / card_version / profiles / limits — with every value sourced from pkg/cardmsg constants (single authority, no re-typed literals). enabled:false still returns 200 with the full manifest (feature detection is the point; only send/edit paths reject). Additive-only wire contract. No new DB, no migration, no new errcode/i18n. Independent of PR-B/PR-C (both already merged to main).
tags: ["card", "wire-contract", "bot-api", "space", "isolation", "testing"]
timestamp: 2026-07-08T14:00:00Z
# --- octospec extension fields ---
slug: card-message-p2-capability-manifest
upstream: self
source: self
---

# Task: card-message-p2-capability-manifest

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Ship **D12 of the card message P2 contract** (`card-message-interaction`):
**producer capability discovery**. A read-only `GET /v1/bot/card/profile`
lets card producers (bot SDKs, the first-party OpenClaw channel adapter)
**feature-detect** the deployment's card capabilities instead of probing with
sends — where a `400` cannot distinguish "cards disabled" from "card invalid".

The manifest answers only the **negotiation** half of dynamic capability
delivery; the **rendering** half stays client-release-bound (P1 Decision B —
out-of-profile content degrades via the P1 `plain` chain, and per-element
`fallback` is the named P3 mechanism for finer-grained evolution).

```
producer startup → GET /v1/bot/card/profile   (bot-token, existing bot_api auth chain)
                 → { enabled, card_version, profiles, limits }   (values from pkg/cardmsg constants)
enabled:false    → still 200 + full manifest    (feature detection is the point)
                 → producer falls back to its richtext path; only send/edit paths reject
```

**Stacking**: none. PR-B (#548) and PR-C (#549) are **already merged to main**.
PR-D branches fresh from `origin/main` (`feat/card-message-p2-capability-manifest`),
base = `main`, and is independently mergeable. It is the smallest of the three
P2 PRs — a single read handler over frozen constants.

## Background

- **Authoritative contract**: `.octospec/tasks/card-message-interaction/brief.md`
  D12 (wire shape at brief `:125-135`, load-bearing at `:191-196`, acceptance at
  `:340-345`). `docs/card-protocol.md:79-81` carries the summary (`enabled` /
  `profiles` / `limits`, additive-only); the field-level detail (incl.
  `card_version`) lives in the master brief. Amend doc + master brief together
  or not at all.
- **Why it exists**: maintainer request 2026-07-07; mirrors P1 Decision 10
  (server-side profile negotiation). The OpenClaw adapter
  (`Mininglamp-OSS/openclaw-channel-octo`) is the first expected consumer — it
  SHOULD call this at startup and fall back to richtext when `enabled:false` or
  the required profile is absent (master brief `:248-251`), so it must not
  hardcode limits or 400-probe.
- **Source constants already on main** (P1 #543 + P2 #548), verified:
  - `cardmsg.Enabled()` (`pkg/cardmsg/cardmsg.go:158`) — reads
    `OCTO_CARD_MESSAGE_ENABLED` (Decision 2 rollout gate, default off).
  - `cardmsg.CardVersion = "1.5"` (`cardmsg.go:48`).
  - `cardmsg.ProfileV1 = "octo/v1"` (`cardmsg.go:46`),
    `cardmsg.ProfileV2 = "octo/v2"` (`interactive.go:21`).
  - Limits: `MaxPayloadBytes = 512<<10` (`cardmsg.go:52`), `MaxNodes = 200`
    (`cardmsg.go:64`), `MaxDepth = 16` (`cardmsg.go:66`),
    `MaxInputTextBytes = 4<<10` (`inputs.go:20`),
    `MaxInputsBytes = 16<<10` (`inputs.go:22`).
  - Accepted-profile **set** authority: `interactiveByProfile` switch
    (`validate.go:102-113`) accepts exactly `{octo/v1, octo/v2}` — currently
    **unexported**; D12.2 requires exposing it as the single authority the
    manifest reads (see below).
- **Mount point**: `bot_api.go:212` — `botAPI := r.Group("/v1/bot", ba.authBot())`.
  All `/v1/bot/*` routes hang here; D12 adds one `botAPI.GET("/card/profile", …)`.
  No Space middleware (bot-token, no user/space subject), no new rate limiter
  (master brief: "same middleware chain and quotas as other bot_api routes").

## Decisions (inherited from D12, with implementation-level bindings)

| # | Decision | Binding |
|---|---|---|
| D12.1 | **Endpoint = `GET /v1/bot/card/profile`**, mounted on the existing `botAPI` group (`ba.authBot()` bot-token auth). **No new rate limiter** — inherits the bot_api chain and quotas. **No Space middleware** — the manifest is deployment-global, carries no tenant/user/space data, so there is nothing to isolate. Handler is a pure read: assemble the manifest from `pkg/cardmsg` constants and `c.Response(...)`. | `bot_api.go` route reg + new handler (`card_profile.go`) |
| D12.2 | **Values sourced from `pkg/cardmsg` constants (single authority) — never re-typed literals.** For the scalar fields the handler references the exported consts directly. For **`profiles`**, the accepted set is authoritative in the `interactiveByProfile` switch (`validate.go`), so expose `cardmsg.AcceptedProfiles() []string` (returns `{ProfileV1, ProfileV2}`, ordered v1→v2) as the single source both the validator's acceptance and the manifest agree on. A guard test asserts `manifest.profiles == cardmsg.AcceptedProfiles()` **and** that every advertised profile is actually accepted by `Validate` (drift guard). | new `cardmsg.AcceptedProfiles()` + handler |
| D12.3 | **`enabled` = `cardmsg.Enabled()`.** With the rollout flag off, the endpoint **still returns 200 with the full manifest** (feature detection is the entire point — a producer must be able to read `enabled:false` and fall back). Only the send/edit paths reject when disabled; the manifest read never gates on it. | handler |
| D12.4 | **Wire contract is additive-only** — same evolution rule as `event_data`: fields may be added, never renamed / removed / re-typed. Bot SDKs and the adapter key feature detection and limits off it, so a rename/removal is a breaking change on par with touching `event_data`. A **contract test pins the exact field set** (`enabled`, `card_version`, `profiles`, `limits.{max_payload_bytes,max_nodes,max_depth,max_input_text_bytes,max_inputs_bytes}`) so accidental renames fail CI. | contract test |
| D12.5 | **No new errcode / i18n / DB / migration — deliberate.** The manifest read has no business failure path: the only rejection is missing/invalid bot-token, handled by the existing `ba.authBot()` (unchanged). This is the intended scope minimality vs PR-B/PR-C; the brief calls it out so a reviewer does not expect an `errcode` diff. | (none) |
| D12.6 | **Rendering half stays out of scope.** D12 delivers only negotiation (enabled / profiles / limits). Out-of-profile rendering degrades via the existing P1 `plain` chain; per-element `fallback` is the named P3 mechanism. The manifest does **not** advertise renderer capabilities. | scope guard |

## API surface (wire definition)

**`GET /v1/bot/card/profile`** (new — bot-token auth, existing bot_api chain):

```json
// response — additive-only manifest (fields may be added, never renamed/removed/re-typed)
{ "enabled": true, "card_version": "1.5",
  "profiles": ["octo/v1", "octo/v2"],
  "limits": { "max_payload_bytes": 524288, "max_nodes": 200, "max_depth": 16,
              "max_input_text_bytes": 4096, "max_inputs_bytes": 16384 } }
// enabled=false (rollout flag off) still returns 200 with the full manifest —
// feature detection is the point; only the send/edit paths reject.
// unauthenticated / bad bot-token → existing ba.authBot() rejection (unchanged).
```

Every numeric/string value above is the render of a `pkg/cardmsg` constant
(D12.2), not a literal duplicated in the handler. `profiles` reflects the
validator's actual accepted set — a P1-only deployment would advertise
`["octo/v1"]`; this build accepts both, so it advertises both.

## Load-bearing list

<!-- touches tags: card, wire-contract, bot-api, space, isolation, testing -->

- **Capability manifest wire contract** (`wire-contract`, `bot-api`): the D12
  response is **additive-only**. Renaming/removing/re-typing a field is a
  breaking change for bot SDKs and the adapter that key feature detection and
  limits off it. The contract test (D12.4) is the guard.
- **Single-authority sourcing** (`wire-contract`): manifest values MUST come
  from `pkg/cardmsg` constants (P1/P2 authority), never re-typed literals — a
  duplicated `512288`/`200`/`"octo/v2"` in the handler would silently diverge
  from the validator when a constant changes. `profiles` in particular must
  track the `interactiveByProfile` accepted set via `AcceptedProfiles()`.
- **Bot-token auth chain** (`bot-api`, `space`, `isolation`): the route mounts
  on the existing `botAPI` group (`ba.authBot()`); no new auth, no Space
  middleware, no new rate limiter. The manifest is deployment-global and leaks
  no tenant/user/space/message data — reviewers confirm the response body
  contains only static capability constants (nothing operator- or space-scoped).
- **`enabled:false` returns 200** (`wire-contract`): the rollout gate must NOT
  gate the manifest read (only send/edit). A test asserts both halves together:
  flag off → manifest 200 `enabled:false` **and** a send/edit still rejects.
- **Protocol doc**: `docs/card-protocol.md:79-81` + master brief D12 already
  carry this contract; implementation must not drift — doc + master brief are
  amended together or not at all.

## Out of scope

- **Any send/edit path change.** D12 is read-only. The `OCTO_CARD_MESSAGE_ENABLED`
  gate behavior on send/edit is unchanged and merely *reported* here.
- **New errcode / i18n / DB table / migration** — none needed (D12.5); adding
  any is a scope smell.
- **Space middleware / membership / anti-IDOR / rate limiter** — no user
  subject, no per-resource access, deployment-global read.
- **Action approval timeout / envelope `expires_at`** — a *separate* proposed
  P3 capability (evaluated 2026-07-08) that touches the frozen envelope + event
  contract and needs its own brief + doc amendment + maintainer sign-off. It is
  **not** part of D12. (If later shipped, the manifest may *additively* advertise
  it — but that is a future change under the additive-only rule, not this task.)
- **Rendering-half capability delivery** (per-element `fallback`, renderer
  feature flags) — P3, client-release-bound (D12.6).
- **incomingwebhook capability discovery** — D12 is bot-token-scoped; iwh
  producers have no bot token and no event consumer (D7). Out of scope by design.

## Acceptance

<!-- Machine-checkable where possible. -->

- **Manifest values asserted against `pkg/cardmsg` constants** (not re-typed
  literals): a test reads `GET /v1/bot/card/profile` and asserts
  `card_version == cardmsg.CardVersion`, each `limits.*` equals its constant,
  and `profiles` deep-equals `cardmsg.AcceptedProfiles()`.
- **Profile-set drift guard**: a test asserts every profile in
  `AcceptedProfiles()` is accepted by `cardmsg.Validate` and no other profile
  is — so the manifest can never advertise a profile the validator rejects (or
  omit one it accepts).
- **Rollout flag both-halves test**: with `OCTO_CARD_MESSAGE_ENABLED` unset/off,
  the endpoint returns **200 + `enabled:false` + full manifest**, while a
  send/edit path still rejects — asserted in one test so the two behaviors can't
  drift apart.
- **Auth**: an unauthenticated / bad-bot-token call hits the existing
  `ba.authBot()` rejection (assert non-200; reuse the bot_api auth test
  pattern) — no new auth code introduced.
- **Additive-only contract test**: pins the exact top-level + `limits` field set
  so a rename/removal fails CI (documents the additive-only evolution rule).
- **Guard tests + `golangci-lint run ./...` clean**; `go build ./...` green.
  (No i18n make-target delta expected — no new codes; if the guard suite for
  `bot_api` lists handler files, add `card_profile.go`.)
- **`docs/card-protocol.md`** D12 section matches this brief (human-reviewed);
  no drift from the master brief's wire shape.

## Notes for the implementer

- **Handler file**: `modules/bot_api/card_profile.go` (`ba.botCardProfile`),
  route reg one line in `bot_api.go` inside the `botAPI` group.
- **`AcceptedProfiles()`** lives in `pkg/cardmsg` (next to `interactiveByProfile`
  / the profile consts). Keep it the *only* place that enumerates the set;
  ideally derive `interactiveByProfile`'s acceptance and `AcceptedProfiles()`
  from a shared internal list so they cannot diverge.
- **No `SharedUIDRateLimiter`** — bot routes are token-throttled by the bot_api
  chain, not UID-limited; do not add one (master brief explicit).
- **Test infra reminders** (from prior P2 work): `export OCTO_MASTER_KEY=…`
  before `bot_api` tests; drop/recreate the test DB when switching binaries
  (cross-binary migration ledger drift). This PR adds no migration, so the DB
  surface is unchanged.
