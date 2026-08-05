# Handoff — E3 PR-C grants, discovery and controlled consumption

> Forward-looking implementation guide. `brief.md` in this directory is the
> contract of record; this file says where implementation starts, which
> repository files belong to each slice, and what must never be committed.
> Last updated 2026-08-05 (round 2 — brief "Plan refinement log" applied to the
> Slice 1/4/6 ledgers below). All paths below are repository-relative.

## TL;DR

- PR-A/#674 and PR-B/#675 are merged. PR-C business code has not started on
  this checkout; only the OctoSpec documents are modified/untracked.
- PR-C is one reviewable milestone implemented as six ordered RED→GREEN slices.
- Slice 1 is provenance, not grants or discovery. It closes the trusted identity
  gap before any grant can be consumed.
- A test file or fixture belongs to PR-C only when it proves a named PR-C
  invariant. “Suggested test artifacts” are not an independent deliverable.
- The docs pilot bundle is a Slice 5 input, not a Slice 1 prerequisite and not a
  new built-in/static template. Its exact version is allocated only after the
  dedicated non-production database proves the candidate key is unclaimed.
- Production runtime-catalog gates remain false. PR-C merge and the
  non-production pilot do not authorize production activation.

## Authority and path convention

Use this order when documents disagree:

1. `brief.md` — authorization, consistency, visibility and rollout contract.
2. this `HANDOFF.md` — execution order, repository file ownership and gates.
3. current production code and tests — exact existing API and package shape.
4. `docs/card-template-runtime-catalog-runbook.md` and
   `docs/card-protocol.md` — operator/client documentation updated in Slice 6.

Paths in this handoff are repository-relative and never include a Conductor
workspace prefix. `[new]` means the planned file does not exist at handoff time;
`[modify]` means it exists on `main@40627cc0`. If implementation discovers that
an existing package boundary makes a planned new file unnecessary, keep the
same ownership and tests and update this ledger in the same change.

## Baseline and start conditions

Verified at handoff time:

- base: `origin/main@40627cc0`;
- runtime catalog already provides immutable claim/artifact/audit, activation,
  rollback, block, cache and default-off control/new-send gates;
- `CatalogAccess` already carries purpose plus `bot|internal_producer|system`,
  but the production dynamic authorizer has no grant store and rejects business
  use;
- Bot frames keep `template_ref`; internal `carddispatch` frames do not keep
  producer/template provenance;
- B1/B2 and `card_template_grant` do not exist;
- no Go tests or business-code validation have run for this PR-C checkout yet.

Before each slice, re-check `git status`, preserve unrelated worktree changes,
and write the focused RED test before production code. No slice may enable the
production gates.

## Deliverable boundary

PR-C must land all of the following together:

1. server-authored, stored and validated dynamic-card provenance;
2. template-ID grant persistence, CAS, tombstones, audit and control API;
3. one-snapshot runtime authorization for new-send/edit/action;
4. request-scoped Bot capability plus authoritative target-Space resolution;
5. visibility-aware B1/B2 with bounded safe export;
6. one repeatable non-production docs-notify pilot and operational evidence.

PR-C does not add Bot self-service publishing, production activation, new
RouteSpecs/secrets/finalizers, per-Space active versions, L2b `ext.*`, or an
OpenClaw stop/retry implementation.

## Ordered file ledger

### Slice 1 — stored provenance

Purpose: make principal/template/Space identity trustworthy before grants are
introduced.

Production files (ledger updated at Slice 1 GREEN, 2026-08-05 — the wire
value object landed in `pkg/cardmsg` because `pkg/cardtmpl` imports
`internal/carddispatch` (updater), so carddispatch could not import a
cardtmpl-owned marker type without a cycle; envelope-level parsing also
belongs with `CardTemplateContext`):

- `[new] pkg/cardmsg/provenance.go` — canonical marker wire type, strict
  parse/marshal, `CatalogFrameMarkers` frame extraction with
  `template_ref`↔`metadata.octo.template` cross-check, `Validate()`
  defense-in-depth hook (see `[modify] pkg/cardmsg/validate.go`).
- `[new] pkg/cardtmpl/provenance.go` — provenance→CatalogPrincipal mapping
  (the only bridge from stored frames into authorization input).
- `pkg/cardtmpl/catalog.go` — no change was needed; the mapping lives in
  `pkg/cardtmpl/provenance.go` and access construction stays at call sites.
- `[modify] pkg/cardtmpl/updater.go` — Snapshot before ReplaceView, stored
  identity/Space pinning, marker preservation, provenance-derived edit
  principal for both ReplaceView and Append.
- `internal/carddispatch/types.go` / `mutation.go` — no change was needed:
  markers are authored from already-validated card metadata (no new Card
  field), and `Snapshot` already exposes the effective frame.
- `[modify] internal/carddispatch/registry.go` — read-only
  `ProducerBinding` fact table (disabled specs keep their identity binding;
  duplicates/invalid shapes never bind).
- `[modify] internal/carddispatch/context.go` — `ProducerBindingFromContext`
  (never returns send capability).
- `[modify] internal/carddispatch/dispatch.go` — author producer-bound
  markers from `ProducerSpec.ID` + authorized target Space, only for
  documents carrying validated Registry metadata.
- `[modify] modules/bot_api/card_template_catalog.go`
- `[modify] modules/bot_api/send.go` — author Bot marker and reject raw forgery
  on send/edit.
- `[modify] modules/robot/sanitize_robot_ingress.go` — reject the server-only
  template/provenance fields on the legacy raw robot card ingress. Add a
  separate reject helper; do not reuse or change the semantics of the existing
  `__obo_*` silent-strip function (its file-header rationale does not apply to
  keys this PR introduces).
- `[modify] modules/message/api_card_action.go` — `cardActionFrameOrigin` +
  `validatedFramePrincipal`; provenance-derived access with sender/binding/
  Space consistency, legacy sender fallback for unmarked frames.
- `[modify] internal/cardactiondispatch/contract.go` — persist validated
  provenance in additive durable `CardContext` fields (the frozen
  octo-card-v1 callback envelope does not grow the principal).
- `modules/notify/card_via_registry.go` — no change was needed: its send
  access is already the composition-injected internal producer, and markers
  are authored inside `carddispatch.producerSender`.
- `[modify] modules/notify/action_finalizer.go` — `docsResultTemplateVersion`
  consumes the event's stored exact identity; foreign template IDs fail
  closed; legacy zero-context events keep static V3.

Focused tests extend the existing package suites:

- `pkg/cardtmpl/updater_test.go`
- `internal/carddispatch/dispatch_test.go`
- `internal/carddispatch/mutation_test.go`
- `modules/bot_api/card_template_catalog_test.go`
- `modules/bot_api/card_template_send_test.go`
- `modules/bot_api/card_template_edit_test.go`
- `modules/robot/sanitize_robot_ingress_test.go`
- `modules/robot/card_p1_test.go`
- `modules/message/api_card_p1_test.go` — retain the absolute user type-17
  rejection before persistence.
- `modules/incomingwebhook/util_test.go` and
  `modules/incomingwebhook/card_test.go` — prove caller `extra` cannot cross the
  server-built webhook envelope and no dynamic marker is forged.
- `modules/message/card_action_template_context_test.go`
- `internal/cardactiondispatch/contract_test.go`
- `modules/notify/card_via_registry_test.go`
- `modules/notify/action_finalizer_v3_test.go`

Add `[new] pkg/cardmsg/provenance_test.go` (wire matrix) and
`[new] pkg/cardtmpl/provenance_test.go` (principal mapping) only for the new
value object's table-driven unit matrix. Do not create one new test file per
attack case. `modules/message/api_card_p1_test.go` and the robot/user absolute
rejection suites are MySQL/Redis-gated: their existing assertions are the
regression coverage and must run in CI, not be reported green locally.

Exit gate: raw Bot/robot/user/incoming-webhook forge, cross-bot,
cross-producer, cross-Space, missing/malformed marker, `template_ref` mismatch
and no-Snapshot mutation are RED then GREEN; static historical frames remain
compatible. User and incoming-webhook production paths need no new capability:
their existing absolute card rejection/server-built envelope remains the
boundary and receives regression coverage only.

### Slice 2 — grant migration, store and manager API

Production files (ledger updated at Slice 2 GREEN, 2026-08-05):

- `[new] modules/card_template_catalog/sql/20260805000001_card_template_catalog_grant.sql`
  — grant table with CHECK-enforced D2 shapes (active⇒discover; tombstone⇒no
  permissions; space⇒discover-only + global sentinel) plus additive audit
  principal/permission columns.
- `[new] modules/card_template_catalog/store_grant.go` — CAS upsert/revoke with
  same-tx audit and the claim FOR UPDATE target guard; `resolveGrantRows` is
  the single exact-over-global/tombstone precedence implementation Slice 3
  must reuse.
- `[new] modules/card_template_catalog/api_grant.go` — PUT/DELETE handlers,
  `validateGrantPrincipal` seam (production: botidentity Active + carddispatch
  ProducerBinding + active-space query; revoke skips existence so stale
  principals stay revocable).
- `internal/carddispatch/context.go` — no further change; the Slice 1
  read-only binding resolver is consumed as-is.
- `modules/card_template_catalog/store.go` — no change was needed (shared
  helpers `boundedRequiredText`/`isDuplicateKey`/limits already exist).
- `[modify] modules/card_template_catalog/api.go` — routes + principal
  validator wiring.
- `[modify] modules/card_template_catalog/api_state.go` — detail carries the
  bounded grant summary (limit 32) when the store implements the grant
  interface.
- `[modify] modules/card_template_catalog/store_read.go` — audit page exposes
  the redacted grant principal/permission columns.
- `[modify] modules/card_template_catalog/api_i18n.go` — `respondCatalogGrantInvalid`.
- `modules/card_template_catalog/metrics.go` — no change was needed; the
  existing operation/db counters take grant/revoke as label values.
- `[modify] pkg/errcode/card_template_catalog.go` — one new code
  `err.server.card_template_catalog.grant_invalid`.
- `[modify] pkg/i18n/locales/active.zh-CN.toml` (+ regenerated i18n markers).

Focused tests:

- `[new] modules/card_template_catalog/store_grant_test.go`
- `[new] modules/card_template_catalog/store_grant_mysql_integration_test.go`
- `[new] modules/card_template_catalog/api_grant_test.go`
- `[modify] modules/card_template_catalog/api_test.go` — add `api_grant.go` to
  `TestCatalogNoLegacyErrorResponses` and keep the authenticated route guard.

Exit gate: create/update/revoke CAS, exact-over-global, exact tombstone, ABA,
permission/principal matrix, concurrent winner, same-transaction audit,
superAdmin/auth/rate-limit/i18n and strict-body behavior are proven. Migration
Down exists for test rollback; production rollback does not rely on Down.

### Slice 3 — atomic authorization and Bot merge

Landed (`feat: resolve card catalog authorization in one snapshot`):

- `[new] pkg/cardtmpl/runtime_authorization.go` — the D4 contract:
  `RuntimeGrant` (+`Allows(purpose)`), `RuntimeAuthorizationQuery/…Authorization`,
  the indivisible `RuntimeAuthorizationStore`, `RuntimeAdvertiser`,
  `RuntimeAuthorizationSource` (readers + the new-send gate) with
  `SetDefaultAuthorizationSource`/`DefaultAuthorizationSource`, and the single
  `ActivationVersion` rule both catalog and store call.
- `[modify] pkg/cardtmpl/runtime_catalog.go` — `resolveVersion` +
  `resolveDynamic`'s split reads replaced by one `resolve()` that performs at
  most one snapshot; `verifyAuthorization` rejects an incoherent receipt;
  `RuntimeDynamicAuthorizeFunc` now carries the grant. Static/explicit reads
  still return before any store call, so a gates-off deployment gains no DB
  dependency. `versionMode` distinguishes new-send (may follow the pointer)
  from historical-edit/action-context (pinned, never follows it).
- `pkg/cardtmpl/catalog.go` — no change was needed; `Catalog` stays the render
  surface and the resolver seam lives beside it.
- `[new] modules/card_template_catalog/store_authorization.go` — one
  REPEATABLE READ read-only tx reading activation + claim/artifact/block +
  exact/global grant rows, reduced through the existing `resolveGrantRows`;
  plus bounded `ListAuthorizedTemplates` for the Bot advertised set.
  Compile-time assertion that `*store` is the resolver.
- `[modify] modules/card_template_catalog/store_runtime.go` — activation and
  artifact scans take a `rowQuerier` so the standalone and in-snapshot reads
  share one copy of the shape validation.
- `[modify] modules/card_template_catalog/runtime_install.go` — the authorizer
  consumes the snapshot grant (purpose→permission, no IO of its own) and the
  composition root publishes `runtimeAuthorizationSource` (store + gates).
- `[new] modules/bot_api/card_template_runtime.go` — `botCatalogPrincipal`,
  the strict `resolveBotGrantSpaceID` (every ambiguity refuses; an
  unauthorized/unverifiable `X-Space-ID` never falls through), the
  target-authoritative `botSendCatalogPrincipal` (group row / thread parent
  row), the D6 `resolveSendRef`/`decideSendRef` matrix, `advertisedSendRefs`,
  request-scoped `CapabilityFor`, and `requireSendableRef`.
- `[modify] modules/bot_api/card_template_catalog.go` — per-template-ID single
  advertised send version guard (was per exact ref); `staticSendByID`/
  `staticSendIDs`; `renderPayload` takes a ref-resolver seam so new send
  re-resolves while historical edit stays pinned to the static allowlist;
  `templateCapabilityFromMeta` shared by boot and request-scoped manifests.
- `[modify] modules/bot_api/card_profile.go` — manifest resolved per
  authenticated bot + Space; a runtime read failure returns a typed error
  instead of advertising a possibly-shadowed static version.
- `[modify] modules/bot_api/bot_api.go` — `/card/profile` moved to its own
  group `authBot → botActorUID → SharedUIDRateLimiter`; the other legacy Bot
  routes are deliberately left unlimited.
- `[modify] modules/bot_api/send.go` — new send resolves the grant principal
  from the target and fails closed on `errBotTemplateRuntimeUnavailable`.
- `[modify] modules/bot_api/{db.go,space_inject.go}` — `queryGroupSpaceID`
  added to `botSpaceQuerier`.
- `modules/bot_api/{resolve_targets.go,card_profile route consts}` and
  `main.go` — no change was needed: `InstallRuntimeCatalog` already runs before
  the module factories, and publishing the resolver there avoids a second
  grant truth without new wiring in `main.go`.

Focused tests:

- `[new] pkg/cardtmpl/runtime_authorization_test.go` — one-snapshot counting,
  pinned reads never follow the pointer, incoherent-snapshot rejection,
  static-exact needs no snapshot, a grant-unaware store can never produce an
  allow, `Allows`/`ActivationVersion` tables.
- `[new] modules/card_template_catalog/store_authorization_test.go` — tx shape
  via sqlmock, precedence table, ungrantable principals issue no grant query.
- `[new] modules/card_template_catalog/store_authorization_mysql_integration_test.go`
  — the linearization point (a held snapshot is unaffected by a concurrent
  revoke; the next snapshot is already denied), exact tombstone shadowing a
  live global grant, block/unknown reported from the snapshot.
- `[new] modules/bot_api/card_template_runtime_test.go` — the eleven-row shadow
  matrix, manifest/send agreement, stale-ref rejection, granted-dynamic
  advertisement, list-failure fail-close, the two-versions-per-ID constructor
  guard, the strict Space resolver table, target-authoritative group/thread
  Space.
- `[modify] modules/card_template_catalog/runtime_gates_test.go` — authorizer
  purpose→permission matrix.
- `[modify]` signature migrations in `pkg/cardtmpl/runtime_catalog_test.go`,
  `modules/card_template_catalog/runtime_db_integration_test.go`,
  `modules/bot_api/{card_template_catalog_test.go,card_template_edit_test.go,
  space_inject_test.go}`.

Exit gate: active/block/grant are read in one primary-DB snapshot; profile and
send share the same resolver but make independent decisions; dynamic active
shadows static same-ID new-send; revoke/DB failure/invalid or ambiguous Space
never falls back; static gate-off behavior does not gain a DB dependency.

Deferred to Slice 6 (documentation reconciliation): `resolve_targets.go` needs
no code change, but the runbook must state that a Bot's advertised catalog is
now request-scoped, so an operator reading `/card/profile` sees that Bot's
view rather than the deployment's.

### Slice 4 — B1/B2 discovery and safe export

Landed (`feat: add card template discovery and safe export`):

- `[new] pkg/cardtmpl/export.go` — `SafeExport`, the immutable B2 projection.
  Carries manifest/schema/reports plus allowlisted samples; never templates,
  goldens or canonical bundle bytes. Deterministic export hash (the static
  ETag), 2 MiB cap, visibility fails closed to private, deep `Clone`.
- `[modify] pkg/cardtmpl/template.go` — `TemplateMeta.export/contractVersion`
  plus `Export()`/`ExportHash()`.
- `[modify] pkg/cardtmpl/registry.go` — projection built at registration from
  the same reviewed bytes; `rawSchemaBytes`/`rawInteractionReports`;
  `SetCatalogVisibility` (pre-Freeze composition-root seam) and
  `StaticCatalog()` for B1.
- `[modify] pkg/cardtmpl/json_artifact.go` — manifest gains a strict
  `export.samples` opt-in allowlist; projection built during compile so an
  allowlist naming a missing sample fails publish rather than serving a hole.
- `[modify] pkg/cardtmpl/catalog.go` — `CatalogPurposeDiscover`,
  `CatalogPrincipalSpace`.
- `[new] modules/card_template_catalog/store_discovery.go` — visibility, block
  state and the caller's discover grant are all inside the SQL predicate, so
  they settle before LIMIT; `ErrDiscoveryNotVisible` is the single outcome for
  unknown/invisible/blocked.
- `[new] modules/card_template_catalog/api_discovery.go` — B1/B2 handlers, the
  Space-bound opaque cursor, `{id}@{version}` exact-only refs, ETag/304,
  `Cache-Control: private, no-cache` for private reads, and the manager
  template index.
- `[modify] modules/card_template_catalog/store_read.go` — manager keyset index.
- `[modify] modules/card_template_catalog/api.go` — B1/B2 route group
  `Auth → SharedUIDRateLimiter → LocalizedSpaceMiddleware`; manager `GET ""`.
- `[modify] modules/card_template_catalog/api_i18n.go` — `respondCatalogNotFound`.
- `[modify] modules/card_template_catalog/runtime_install.go` — public
  templates are discoverable without a grant; the new-send gate does not apply
  to discovery.
- `[new] pkg/space/middleware_localized.go` — same membership rule, cache keys
  and TTLs as `SpaceMiddleware`; only the failure responses move to `httperr`.
- `[modify] main.go` — `applyStaticCatalogVisibility`, deliberately an empty
  list: all five existing L2a cards stay private.

Focused tests: `[new] pkg/cardtmpl/export_test.go`,
`[new] modules/card_template_catalog/api_discovery_test.go`.

Exit gate: filtering precedes paging; a cursor cannot be replayed across
Spaces; unknown/invisible/blocked are indistinguishable; samples are opt-in;
static cards stay private.

### Review follow-ups (post-`/code-review`, folded into Slice 4's commit)

Eight confirmed correctness findings fixed; one finding (MetaDefault losing
`rejectIntegrity`) verified as a false positive — the unpinned path calls
`requireReady`, whose error set is a superset.

1. Bot grant Space is resolved only when the dynamic catalog is enabled, so a
   gates-off deployment keeps `/v1/bot/card/profile` free of a DB dependency.
2. New-send provenance is stamped with the *authorized* Space (target group or
   thread parent), not `env.SpaceID`, which send.go fills only for DMs — the
   downstream D3 guards are `!= ""` and were no-ops for group cards.
3. `ListAuthorizedTemplates` reads one row past its budget and drops the
   template the cut landed in, so an exact tombstone can never be separated
   from its global row.
4. `dynamicPageFollows` peeks with a budget of one instead of asserting
   `has_more` at the static/dynamic boundary.
5. `privateTemplateIDs` deduplicates the grant probe by template ID.
6. Manager detail serves the rest of the response with `grants_unavailable`
   when the grant summary read fails, and `ListGrants` orders active rows
   before tombstones.
7. `respondCatalogGrantInvalid` logs the rejection reason it previously dropped.
8. `setupBotCardProfile` resets the `ratelimit:uid:*` bucket now that the route
   mounts `SharedUIDRateLimiter`.

Still open, tracked for Slice 6: the docs.access-request 0.2.0 result-edit
guard (`docsResultTemplateVersion` plus the hardcoded V3 in
`card_mutate_api.go`) — unreachable today because nothing sends 0.2.0, but the
two call sites need one shared version-selection helper; and the quality
findings (ReplaceView's extra Snapshot round trip, the provenance double
marshal, and the two intentional Space/middleware duplications).

### Slice 5 — docs-notify dynamic pilot

Landed (`feat: add the controlled docs catalog pilot`):

- `[new] modules/card_template_catalog/testdata/pilot/docs.access-request@0.4.0-pilot.20260805/bundle.json`
  — owner `docs`, producer `internal_producer/docs-notify`, the existing
  `docs/access_request.decision` RouteSpec, visibility private, and an
  `export.samples` allowlist with one synthetic sample so the B2 export path is
  actually exercised. Same template ID as the static card, new prerelease exact
  version: that is what makes the shadow behaviour observable.
- `[new] modules/card_template_catalog/testdata/pilot/README.md` — states in one
  place that the fixture is publish input only: not registered, not embedded,
  not an activation, not permission to open the production gates.
- `[new] modules/card_template_catalog/pilot_mysql_integration_test.go` —
  `requirePilotVersionUnclaimed` (the preflight gate, in code so it cannot be
  skipped, failing with the remedy) plus the D7 loop: static baseline →
  publish → prove publish is neither activation nor grant → grant → prove a
  grant does not move the pointer → activate → dynamic shadows static for new
  send → private discovery needs a Space grant and another Space sees nothing →
  rollback while a pinned historical read is unaffected → revoke denies the very
  next decision and leaves a tombstone. `TestPilotBundleCompilesAndProjectsSafely`
  runs with no database so a malformed fixture fails everywhere.
- `[modify] modules/notify/card_via_registry.go` + `card.go` —
  `preflightDocsAccessRequestSchema` takes the Space. Template resolution became
  Space-aware in Slice 3, so preflighting against an empty Space would authorize
  a different decision than the send that follows it.
- `modules/notify/action_finalizer.go` — no further change: Slice 1 already made
  the result edit use the stored `template_id@version`.
- `main.go` — no change was needed. The pilot fixture is never registered, so
  the composition root has nothing to wire; treating "merged" as "wired" is
  exactly the confusion the README exists to prevent.

Exit gate: the pilot proves publish ≠ activate ≠ grant, dynamic-over-static
shadowing, private discovery, historical pinning across a rollback, and
immediate denial after revoke — with production gates untouched.

### Slice 6 — verification, operations and PR closeout

Documentation files:

- `[modify] docs/card-template-runtime-catalog-runbook.md` — grant/rollout
  procedures, plus a note that `l2aOwnerAllowlist` (Registry registration) and
  `approvedRuntimeOwners` (runtime publish/authorization) are intentionally
  different lists: a grant for an unapproved runtime owner never enables send.
- `[modify] docs/card-protocol.md` — §1 additive top-level
  `template_ref`/`catalog_provenance`; state explicitly that the client render
  gate still keys on `from_uid` only (provenance is server-side authorization
  input, not a client rendering contract).
- `[modify] docs/platform-card-base.md` — §9 actual B1/B2 fields and
  authorization semantics; §10 new fail-close row for dynamic
  unauthorized/blocked/DB-unavailable (no fallback-text degradation); §2.2-5
  note that L2b threshold ④ is not advanced by PR-C.
- `[modify] .octospec/tasks/cardtmpl-runtime-catalog/brief.md`
- `[modify] .octospec/tasks/cardtmpl-runtime-catalog-grants-discovery/brief.md`
- `[modify] this HANDOFF.md` with final SHAs, commands and evidence boundaries.

Exit gate: focused and widened tests, race, build, vet, lint, i18n, source
guards, real MySQL concurrency, dedicated non-production two-replica
Redis/WuKongIM pilot, restart/DB-outage and rollback evidence are complete.
The English PR body links this spec and answers COMPREHENSION; gates stay false.

## Review and commit boundaries

Keep all slices in one PR-C branch, but make each slice independently
reviewable. Recommended Conventional Commit boundaries are:

1. Slice 1 RED evidence, then `feat: persist trusted card catalog provenance`;
2. Slice 2 RED evidence, then `feat: add card template producer grants`;
3. Slice 3 RED evidence, then `feat: enforce runtime card catalog grants`;
4. Slice 4 RED evidence, then `feat: add card template discovery APIs`;
5. Slice 5 RED evidence, then `feat: add controlled docs catalog pilot`;
6. `docs: complete E3 PR-C rollout handoff` after all verification.

RED evidence may be a separate `test:` commit or recorded in the PR when the
repository is expected to keep every commit buildable. Do not merge or deploy
an intermediate slice by itself. Slice 2's migration is not a standalone
release, and no commit may change production gate defaults.

## Repository fixture policy

The following are PR-C repository deliverables because automated tests consume
them:

- Go `_test.go` regression/integration tests named above;
- synthetic compiler/export fixtures under `testdata/` with no real tenant,
  user, token, document or callback data;
- the Slice 5 pilot `bundle.json`, only after exact-version preflight and only
  as publish input.

The following are runtime state/evidence and must not be committed:

- real `card_template_grant`, activation, audit or artifact rows;
- real Space IDs, user/Bot identities, access tokens, callback secrets or
  production samples;
- database dumps, API response captures containing identities, Redis keys or
  raw service logs;
- generated canonical-bundle/cache bytes.

Redacted command output, selected hashes/revisions and test summaries may be
kept under `.context/` for agent collaboration; `.context/` is gitignored and
is not a PR deliverable.

## First implementation move

Start Slice 1 only. The first RED change should cover the existing boundaries,
not create grant tables or a pilot fixture:

1. add the provenance value-object table tests;
2. add raw Bot send/edit forgery tests;
3. add internal dispatch producer/template/Space authorship tests;
4. add action durable-context and updater Snapshot tests;
5. run the focused packages and record expected RED failures;
6. implement the smallest Slice 1 GREEN without opening gates.

Focused validation after GREEN:

```bash
go test ./pkg/cardtmpl/... ./internal/carddispatch/... ./internal/cardactiondispatch/...
go test ./modules/bot_api/... ./modules/message/... ./modules/notify/...
go test -race ./pkg/cardtmpl/... ./internal/carddispatch/... ./internal/cardactiondispatch/...
gofmt -w <edited-go-files>
git diff --check
```

The MySQL/Redis/WuKongIM environment is not required to author the pure value
object tests, but DB/transport-backed package tests must not be reported green
unless their dependencies actually ran rather than skipped or failed setup.

## Stop conditions

Stop the current slice and update the contract instead of improvising if any of
these occur:

- authorization would need caller-supplied principal or `space_id`;
- active, block and grant cannot be resolved in one primary-DB snapshot;
- a dynamic same-ID denial would fall back to static new-send;
- B2 would expose templates, goldens, canonical storage, grants/audit or real
  samples;
- the pilot needs a new RouteSpec, callback secret, finalizer or production
  Space/data;
- a candidate pilot exact version is already claimed with any source/content;
- completing a slice requires enabling production control/new-send gates.
