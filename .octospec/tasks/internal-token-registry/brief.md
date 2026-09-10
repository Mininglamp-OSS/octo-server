---
type: Task
title: "Task: internal-token-registry"
description: Extract one shared registry (pkg/internaltoken) for the fixed internal-token envs so cross-capability collision coverage is complete by construction.
tags: ["auth", "trust-boundary", "config"]
timestamp: 2026-09-07T00:00:00Z
# --- octospec extension fields ---
slug: internal-token-registry
upstream: self
source: self
---

# Task: internal-token-registry

## Goal

One credential grants exactly one capability. Make that invariant hold *by
construction* for the fixed internal-token envs, instead of by each module
author remembering the full sibling list.

`pkg/internaltoken` owns the registry of those envs and exposes one resolve
function that takes the env being resolved, enforces its length floor, and
compares it against every env registered before it. Registration order is a
precedence order, so every unordered pair is compared exactly once — by
whichever side was registered later — and the junior side is the one disabled.
The capabilities that own one of these envs go through it, and it also owns the
`X-Internal-Token` header name and the shared 32-byte floor.

## Background

Each module hand-rolled its own "must differ from sibling X" comparison, and
each author compared only against what existed at the time:

| Env | Owner | Siblings it compared against |
|---|---|---|
| `NOTIFY_INTERNAL_TOKEN` | `modules/notify` | 0 |
| `OCTO_DOCS_NOTIFY_TOKEN` | `modules/notify` | 1 |
| `OCTO_DOCS_BOT_MENTION_TOKEN` | `modules/bot_mention` | 2 |
| `OCTO_DRIVE_INTERNAL_TOKEN` | `modules/internal_resolve` | 3 |

That ladder does cover all six of today's pairs — each newer env checks every
older one — but only by convention. The (N+1)th capability is protected solely
if its author remembers the whole list, and the N modules already in the tree
never learn the new env exists. The length floor drifted the same way: only
`OCTO_DRIVE_INTERNAL_TOKEN` enforces the 32-byte bar, and
`internal/cardactiondispatch` hardcoded its own copy of `32`. The
`X-Internal-Token` header was hand-copied in three modules, each with a comment
naming the other two.

The fix keeps the ladder's *semantics* and makes it a property of the data:
appending a Spec guards it against every existing env, and every existing env
against it, with no edit anywhere else. The guard is **directional** — the
appended env yields to all of them, and they are protected from the other
direction, by it disabling itself. Module tests must therefore branch on the
precedence index; a test that asserts the symmetric shape passes only while its
subject happens to be registered last, and goes red the moment the registry
grows (PR #853 review, P1).

`main.go` remains the only place that also sees the *dynamic* per-route notify
tokens and callback secrets from `OCTO_CARD_ACTION_ROUTES`.

## Load-bearing list

- **Collisions must not fail boot.** A collision between two *fixed*
  internal-token envs disables the affected capability module-locally (resolved
  token becomes `""`, so the auth middleware fails closed) and the server still
  boots. A collision with a *dynamic* route credential fails startup. Pinned by
  `TestCardActionDispatchScopesBotMentionTokenCollisionFailures`
  (`main_carddispatch_test.go`).
- **`main.go`'s cross-check with dynamic route credentials.**
  `cardactiondispatch.Registry.ValidateNotifyTokenExclusions` still runs, now
  fed from `internaltoken.Values` instead of a hand-written argument list.
  `modules/internal_resolve/main_wiring_test.go` pins that wiring.
- **Reasons never carry a token value.** They are logged, and in some callers
  surfaced at boot.
- **Existing length behaviour.** Only the drive token has a 32-byte floor.
  `pilote2e` and `internal/cardactiondispatch` integration tests run notify with
  short values; live deployments may too.
- Per-module collision tests: `modules/internal_resolve/api_test.go`,
  `modules/internal_resolve/boot_config_test.go`,
  `modules/bot_mention/config_test.go`.

## Out of scope

- **Promoting fixed-vs-fixed collisions to boot failures.** A separate rollout
  decision; the current contract is pinned by `main_carddispatch_test.go`.
- **Raising the length floor on the three legacy envs.** Doing so would disable
  a live ingress on the next deploy of any installation running a shorter
  secret. The waivers are explicit in the registry and pinned by
  `TestLegacyLengthWaiversAreExplicit` so lifting one is a deliberate one-line
  change.
- Dynamic `OCTO_CARD_ACTION_ROUTES` credentials — those stay owned by
  `internal/cardactiondispatch`.
- Any change to the wire header, route paths, rate limits, or auth middleware.

## Operator-visible changes

- **Boot log wording.** Refusal reasons are now generated from one template, so
  the four messages are reworded. The unset case keeps the substring `will
  reject all requests` that three of the four previously shared, so log-based
  alerting keyed on that phrase still fires.
- **Log level.** An unset env now logs at WARN (a capability that is simply not
  in use is a normal deployment shape); collisions and undersized values stay at
  ERROR. Previously `modules/internal_resolve` and `modules/bot_mention` logged
  both at ERROR.
- **Collision report.** `main.go` logs one ERROR line per colliding pair, naming
  the env that keeps serving and the one that was disabled. It runs before the
  card-dispatch installers so a malformed `OCTO_CARD_ACTION_ROUTES` cannot
  swallow the diagnostic.

No capability that was enabled before is disabled now, and no capability that
was disabled before is enabled now.

## Acceptance

- `pkg/internaltoken` table test enumerates the registry and asserts every
  unordered pair is covered exactly once, that the junior side is disabled and
  the senior side keeps serving (`TestResolveCoversEveryRegisteredPair`). The
  precedence order itself is pinned by `TestRegistryPrecedenceOrderIsStable`.
- **Appending a fifth `Spec` requires no edit to production code or to any
  test.** Verified the way a reader would: append a hypothetical entry, run
  `pkg/internaltoken` plus the three module packages, and confirm the module
  tests re-classify the new env as a junior (`outranks_*`) instead of failing.
- The boot collision report never names a disabled env as the survivor, and
  never reports a value refused for length as a collision
  (`TestCollisionsNeverNameADarkEnvAsTheSurvivor`,
  `TestCollisionsRespectTheLengthFloor`).
- `Values` / `Collisions` panic on a nil lookup rather than reporting an empty
  credential set, so the one gate that aborts startup cannot degrade into a
  no-op (`TestValuesPanicsOnNilGetenv`), and `main_wiring_test.go` pins that
  `main.go` feeds them `os.Getenv`.
- Error strings never contain a token value
  (`TestErrorsNeverContainTokenValue`).
- Per-module tests enumerate `internaltoken.Envs()` rather than a hand-written
  sibling list — including `boot_config_test.go`, which now builds the
  exclusion argument list from `internaltoken.Values` over its own getenv
  instead of reading the ambient process environment.
- `go build ./... && go vet ./...` clean; `golangci-lint run` clean on the
  touched packages; `make i18n-extract-check` + `make i18n-lint` pass.
- All previously-passing tests still pass (MySQL/Redis-backed integration tests
  are unrunnable in the sandbox and fail identically before and after).
