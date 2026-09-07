---
type: Journal
title: "Journal: internal-token-registry"
description: Extract pkg/internaltoken — one registry for the fixed internal-token envs, with registration order as a precedence order, so cross-capability collision coverage is complete by construction instead of by each author remembering the sibling list.
tags: ["auth", "trust-boundary", "config", "internal", "testing"]
timestamp: 2026-09-07T18:00:00+08:00
# --- octospec extension fields ---
task: internal-token-registry
source: self
---

# Journal: internal-token-registry

## What was done

Four fixed internal-token envs each hand-rolled their own "must differ from
sibling X" check, and each author compared only against what existed when they
wrote it: `NOTIFY_INTERNAL_TOKEN` against 0 siblings, `OCTO_DOCS_NOTIFY_TOKEN`
against 1, `OCTO_DOCS_BOT_MENTION_TOKEN` against 2, `OCTO_DRIVE_INTERNAL_TOKEN`
against 3.

- `pkg/internaltoken` (new) — owns the registry, `Resolve(env, getenv)`, the
  `X-Internal-Token` header name, and `DefaultMinBytes`. Resolve enforces the
  spec's length floor and compares the value against every env registered
  *before* it. Refusal returns `("", *Error)` with a typed `Reason`; the empty
  token is what makes each caller's auth middleware fail closed.
- `modules/notify`, `modules/bot_mention`, `modules/internal_resolve` — the
  three hand-rolled resolvers collapse to one call each. `notify` splits its log
  level on `Reason` (unset → WARN, collision/too-short → ERROR); the other two
  now do the same instead of logging every refusal at ERROR.
- `internal/cardactiondispatch/registry.go` — its two hardcoded `32` literals
  now read `internaltoken.DefaultMinBytes`, so "one bar operators have to
  remember" is one value rather than a claim in a doc comment.
- `main.go` — `ValidateNotifyTokenExclusions` is fed from
  `internaltoken.Values(os.Getenv)` instead of a hand-written argument list, and
  a new `reportFixedInternalTokenCollisions` logs one line per colliding pair
  naming the survivor and the disabled env.

## The structural point: registration order is a precedence order

The first implementation made `Resolve` symmetric — compare against every
*other* env, disable both sides of a collision. It read well ("when one secret
opens two doors, neither should stay open") and it was wrong.

Under the pre-existing behaviour, `NOTIFY_INTERNAL_TOKEN == OCTO_DOCS_NOTIFY_TOKEN`
disabled docs and left the legacy ingress serving. Symmetric resolution turns
that same misconfiguration into `internalToken == "" && docsToken == "" &&
actionService == nil`, which 401s **every** request to the `/v1/internal` notify
group. A deployment that had been serving for months goes dark on the next
rolling deploy, for a config that was already contained.

Making the registry order a *precedence* order recovers both properties at once:

- **Coverage is still complete by construction.** Every unordered pair `{i, j}`
  is compared exactly once, by whichever side was registered later. Appending a
  Spec guards it against all existing envs and them against it, with no edit
  anywhere else.
- **Which side yields is decided, not arbitrary.** The incumbent keeps serving;
  the newcomer that duplicated an existing secret is the one that goes dark —
  which is exactly what the four hand-rolled ladders already did.

So the ladder the modules had built by accident was the right semantics. What
was wrong was that it lived in four places and depended on memory. The fix was
to make it a property of the data, not to replace it.

## Gotchas worth remembering

- **A "complete by construction" rewrite can quietly widen the blast radius.**
  The symmetric version was strictly more coverage and strictly worse
  operationally. Ask what a *misconfigured but currently working* deployment
  does on the next deploy, not just whether the invariant holds.
- **Length floors are a rollout decision, not a refactor decision.** Three of
  the four envs ship without the 32-byte bar. Applying it uniformly would have
  disabled any live ingress running a shorter secret — and `pilote2e` proves
  short values are in use. The waivers are explicit `MinBytes: 0` entries pinned
  by a test, so lifting one is a deliberate one-line change in two places.
- **`Values`/`Collisions` panic on a nil `getenv`.** Returning an empty slice
  would make `ValidateNotifyTokenExclusions(Values(nil)...)` — the one gate here
  that *aborts* startup — a silent no-op. Same posture as `mustLookupSharedCode`
  in the i18n helpers.
- **A source-grep guard is only as strong as the expression it pins.** The
  pre-existing `main_wiring_test.go` grepped for one literal argument. Replacing
  it with `internaltoken.Values(` alone would have been weaker — a stub lookup
  passes. It pins `internaltoken.Values(os.Getenv)` plus a runtime assertion
  that the registry contains the drive env.

## Verification

`go build ./...`, `go vet ./...`, `golangci-lint`, `make i18n-extract-check`,
`make i18n-lint` clean. Full suite run locally the way CI runs it
(`ci/run-unit-tests.sh` + `ci/run-e2e-shard.sh 1 1`, with MySQL 8.0 / Redis 7 /
WuKongIM v2.2.4-20260313): **102 packages, 0 failures**.

`pilote2e` (build-tagged, not in either CI lane) has 3 failing tests —
`cardtmpl registry ... default catalog not wired`, because the package has no
TestMain wiring the registry the way `modules/notify/testmain_test.go` does.
Verified pre-existing by checking out the parent commit and reproducing the
identical three failures.
