---
type: Task
title: "Task: card-gate-hardening"
description: Close three gaps left by PR #712's persistence and truncation gates — a fail-open in the x-octo-constraints parser, zero test coverage for the two modules/bot_api guards, and a doc comment that under-counts the write paths the persistence judge covers.
tags: [card, cardtmpl, cardmsg, carddispatch, bot-api, trust-boundary, test, testing]
timestamp: 2026-08-08T09:30:00+08:00
# --- octospec extension fields ---
slug: card-gate-hardening
upstream: "follow-up to Mininglamp-OSS/octo-server#712 (review P2-1 / P2-2 / P2-4)"
source: review
---

# Task: card-gate-hardening

## Goal

Three findings from the approving reviews on #712, none of which blocked that merge and all
of which are in mutable engine code. No artifact bytes change; no published card version is
touched.

1. **A fail-open in the `x-octo-constraints` parser.** Optional string fields were read with
   `v, _ := entry[k].(string)`, which maps "present but wrong type" onto the same `""` the
   absent case produces — so a declaration silently changes meaning instead of failing to
   compile.
2. **Both guards added to `modules/bot_api` had zero test coverage in that package.** All
   coverage sat in `internal/carddispatch`, behind that layer's fakes.
3. **The persistence judge's doc comment listed three write paths; there are five.**

## Background

### Why (1) is a fail-open and not a style nit

`truncateStrings` has two genuinely optional fields, and that is exactly what makes the
`v, _ :=` idiom unsafe for them:

- a non-string `arrayField` becomes `""`, which re-scopes the clamp from `phases[].thought`
  to a top-level `thought` that does not exist — the declared display ceiling then simply
  **stops applying**, with no error at compile time and no signal at render time. Since
  applying that ceiling is the entire purpose of the declaration, this is a silent, total
  failure of the feature.
- a non-string `ellipsis` becomes `""`, silently dropping the ellipsis so a truncated value
  becomes indistinguishable from a short one.

Measured before the fix: both cases **compile successfully**. The three *required* string
fields (`field`, `parentArray`, `childArray`) were already caught downstream by a `== ""`
comparison — so they fail closed, but by accident, and they report `is required` for what is
actually a type error. Fixing the diagnosis is worth the same change.

Not reachable today: the runtime-publish path is not enabled, and no shipped artifact has a
wrong-typed constraint. It is reachable the moment either changes.

### Why (2) is the same shape as #712's P0

The P0 that took three review rounds to find was a guard applied in one package and tested
through another package's fake: `fakeBotCardMutator` records the request and never
re-validates, so the entire second validation pass was uncovered. After #712 the same shape
existed again — the raw-card-edit column gate and the template-send precheck both live in
`modules/bot_api`, and `grep` for their identifiers across `modules/bot_api/*_test.go`
returned nothing.

### Why (3) matters more than a stale comment usually would

`NormalizeFrameForPersistence`'s comment asserted that *all* pre-write revalidation paths go
through it. That assertion being false is precisely what caused rounds 5 and 6: the raw edit
branch and `WriteCAS` did not. The comment was corrected when those were wired in, but the
enumeration itself will drift again.

## Load-bearing list

- `pkg/cardtmpl/json_artifact.go` — `parseStringTruncations` / `parseAggregateArrayLimits`.
  Optionality must be preserved: omitting `arrayField` (top-level scope) and `ellipsis`
  (no ellipsis) stays legal. Only *present-but-wrong-typed* becomes an error.
- `internal/carddispatch/mutation.go` — the doc comment is the only change; no behaviour.
- `modules/bot_api` — tests only; no production change.

## Out of scope

- The 92.6%-of-column margin on an all-fields-at-maximum CJK frame, and its two durable
  fixes (`MEDIUMTEXT` widening, or display ceilings on `tool`/`detail` — the latter needs
  nested-path support in `truncateStrings` first). Recorded in #712's D3a.
- The `[图片]` placeholder lines `BuildPlain` emits for every `Image` regardless of
  `isVisible`, which land in push previews. Needs a product decision and touches either the
  push text surface (`altText` is unvalidated) or search indexing.
- A distinguishable error code for "too large" versus "invalid card".
- Promoting the two pending learnings into `rules/`.

## Acceptance

- [ ] A present-but-wrong-typed `arrayField` / `ellipsis` / `field` / `parentArray` /
      `childArray` fails compilation with a message naming the field **and** saying it must
      be a string. Proven to be a change: the same cases compile successfully with the
      production fix reverted.
- [ ] Omitting `arrayField` and omitting `ellipsis` both still compile, and truncation still
      clamps — the tightening must not remove the optionality.
- [ ] `modules/bot_api` drives the raw-card-edit column gate through the **real** HTTP
      handler, with a positive control first (a normal-sized edit succeeds on the same setup)
      so the subsequent rejection is attributable to width rather than to auth, lookup or
      policy. Asserted: HTTP 400, the `card_invalid` code's message, and that the stored
      `content_edit` still holds the control frame.
- [ ] Removing the gate makes that test fail — and fail with the store error, showing the
      frame reached MySQL.
- [ ] `modules/bot_api` proves the template-send precheck is not dead code: a schema-valid
      worst-case payload renders to a frame that passes `cardmsg.MaxPayloadBytes` and exceeds
      the column.
- [ ] A source guard counts the persistence judge's write paths and fails when the count
      diverges from the number the doc comment claims. Proven to fire by removing one.
- [ ] `go build ./...`, `go vet ./...`, gofmt, `git diff --check`, `golangci-lint`, `-race`
      on the three touched packages, and the DB-backed `modules/bot_api` suite all pass.
