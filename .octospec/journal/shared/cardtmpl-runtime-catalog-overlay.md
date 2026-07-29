---
type: Journal
title: "Journal: cardtmpl-runtime-catalog-overlay (roadmap E3 PR-B server)"
description: Add the dark RuntimeCatalog overlay, audited activation state machine, bounded cache, startup recovery, and runtime consumer migration on top of PR-A.
tags: [cardtmpl, runtime-catalog, activation, rollback, cache, database, trust-boundary, observability, testing, e3]
timestamp: 2026-07-29T07:20:03+08:00
# --- octospec extension fields ---
task: cardtmpl-runtime-catalog-overlay
issue: 672
stacked_on_pr: 670
source: self
---

# Journal: cardtmpl-runtime-catalog-overlay (roadmap E3 PR-B server)

## What was done

- Installed one process-wide `RuntimeCatalog` in the composition root before
  module factories are constructed. It overlays the frozen built-in Registry
  with the PR-A MySQL artifact store while retaining a narrow read-only
  interface for render, exact/default metadata, fallback text, and action
  context.
- Added an authoritative activation table and append-only state audit fields.
  Activate, explicit-target rollback, and one-way block use revision CAS and
  commit the pointer/block change with the success audit in one transaction.
  Rollback only accepts a target proved previously active; blocking the active
  version atomically switches to a valid fallback or disables the template.
- Added default resolution, dynamic exact loading, source/identity/hash
  revalidation, and a compiled-artifact LRU bounded by entry count and retained
  canonical bytes. Cold loads use bounded `singleflight`; each waiter may cancel
  independently, while shared DB/compile work owns a 10-second deadline.
- Added super-admin detail/audit/activate/rollback/block routes behind Auth and
  the shared UID limiter. Forward activation and dynamic new-send gates default
  false; production installs no downstream dynamic grant, so every business
  dynamic purpose remains fail-closed.
- Moved Bot template send/edit, notify template rendering/preflight/fallback,
  `CardUpdater`, and message action-context onto the catalog interface with
  server-authored principal, purpose, and Space context. Explicit stored
  versions are preserved for historical edit/action behavior.
- Moved static reconciliation and active-target validation into asynchronous
  module startup. Transient MySQL failure leaves DB-backed paths unavailable
  and retries without crashing the whole process; proven integrity failure is
  sticky until restart. Explicit static exact reads remain available for a
  transient outage.
- Updated the production runbook for activate, rollback, emergency block,
  startup diagnostics, exact-key collision recovery, and the post-first-dynamic
  binary compatibility floor.

## Self-review closures

- Static built-in interactive targets are no longer forced through the dynamic
  `RouteSpec` activation precondition. This restores valid static fallback and
  prevents an active `ai.reasoning-process@0.2.0` pointer from poisoning startup;
  dynamic interactive artifacts still require a matching route.
- Manager detail and audit stay readable for a super-admin while readiness is
  pending or integrity-poisoned. State-changing and runtime data paths remain
  fail-closed, and the diagnostic DB calls retain their server deadline.
- Notify default/preflight/fallback calls now carry a server-owned 10-second
  deadline. Moving static defaults behind the overlay introduced a real DB read;
  an unbounded `context.Background()` would otherwise strand request goroutines
  during a MySQL stall.
- Runtime schemas with non-empty `patternProperties` are rejected before
  publication/activation. `additionalProperties=false` does not close that
  keyspace in JSON Schema, so bounded value schemas under a regex were still
  able to admit an unbounded number of object keys.
- The real-MySQL integration database now uses a process-unique name and a
  bounded connection pool, avoiding destructive cross-workspace collisions in
  parallel Conductor runs.

## Current-head review closure

- Kept default/dynamic resolution fail-closed until startup validation succeeds
  and wired the same state into `/v1/ready`. Pending or integrity-poisoned
  replicas now return 503 while `/v1/health` remains dependency-free.
- Bounded startup to 128 active targets (`LIMIT 129` overflow detection), gave
  each target an independent 10-second validation deadline, removed the shared
  30-second whole-list budget, and exposed the observed count through
  `dmwork_card_catalog_active_targets` including the overflow case.
- Preserved runtime catalog causes through notify preflight, classified catalog
  safety failures separately, and made block/disable/auth/integrity/unavailable
  fail closed between preflight and render instead of degrading to successful
  text delivery. Notify catalog calls now inherit request cancellation while
  retaining the server-owned 10-second ceiling.
- Bounded Bot manifest construction lookups to five seconds and added audit
  indexes for manager paging plus prior-active proof lookups.
- Recorded trusted producer provenance as a PR-C prerequisite. Existing stored
  messages do not carry enough server-authored information to infer `bot` versus
  `internal_producer`, so the action-context principal must not be guessed from
  sender, template ID, or owner.

## Verification

- `go test -count=1 ./pkg/cardtmpl/... ./modules/card_template_catalog/...`
  passed, including real MySQL migration Up/Down and multi-replica
  activate/rollback/block/restart coverage.
- After recreating the known-polluted shared `test` database with its required
  `utf8mb4_general_ci` collation, the Bot, notify, message, carddispatch, and
  cardactiondispatch packages all passed when run serially with a clean database
  per package.
- Targeted `-race` passed for RuntimeCatalog/cache/CardUpdater, Bot template
  send/edit, message action-context, catalog startup/CAS, and the multi-replica
  DB integration flow.
- `pkg/cardtmpl` and `modules/card_template_catalog` whole-package coverage were
  rechecked at 80.5% and 81.4%. The legacy consumer packages remain below 80%
  as whole packages (Bot 48.3%, notify 68.7%, message 21.6%); their changed
  runtime paths have focused and race tests, but the brief's literal
  whole-package 80% line remains open and is not represented as complete.
- `go build ./...`, `go vet ./...`, `golangci-lint run ./...`,
  `make i18n-extract-check`, `make i18n-lint`, source guards, and
  `git diff --check` passed locally.

## Structural learnings / gotchas

- A fail-closed readiness switch must not also remove the authenticated,
  bounded read path needed to diagnose why readiness failed.
- A runtime dependency that deliberately closes a serving path must also mark
  process readiness down. Keeping liveness green is useful for diagnostics, but
  keeping readiness green leaves the poisoned replica in normal rotation.
- Activation capability checks are source-sensitive. Trusted built-ins are
  reviewed with their binary; applying a current runtime route configuration to
  them can invalidate a legitimate rollback target. Untrusted dynamic artifacts
  need the stronger precondition.
- Adding an overlay can turn formerly in-memory static-default calls into DB
  calls. Every migrated caller must be re-audited for cancellation and a
  server-owned deadline, even if its functional result remains static.
- `additionalProperties=false` is not a complete object-cardinality proof when
  `patternProperties` is present. Reject unsupported open-keyspace constructs or
  pair them with a deliberately bounded policy before runtime use.
- This repository's integration packages do not share an identical migration
  inventory. Reusing one database across independently built package test
  binaries produces legitimate "unknown migration" failures; clean databases
  must be isolated per package/lane.

## Remaining boundary

- PR-A squash-merged through PR #674 as `68e8134d`. This branch has been
  rebased/retargeted to `main`; post-rebase CI and current-head approvals must
  pass before PR #673 can merge.
- This journal covers only Issue #672's octo-server work package. OpenClaw Model
  A selection/release (E1d), real multi-instance stop/retry (E1e), PR-C grants
  and discovery, the cross-repository version matrix, and production gate
  enablement are not complete. PR-C also owns trusted producer-provenance
  persistence before principal-kind-aware grants can be enabled.
