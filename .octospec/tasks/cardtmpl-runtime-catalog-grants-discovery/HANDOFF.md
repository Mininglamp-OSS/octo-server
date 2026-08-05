# Handoff — E3 PR-C grants, discovery and controlled consumption

> Forward-looking implementation guide. `brief.md` in this directory is the
> contract of record; this file says where implementation starts, which
> repository files belong to each slice, and what must never be committed.
> Last updated 2026-08-05. All paths below are repository-relative.

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

Production files:

- `[new] pkg/cardtmpl/provenance.go` — canonical marker type, strict parse,
  clone/equality and dynamic validation helpers.
- `[modify] pkg/cardtmpl/catalog.go` — carry validated provenance in exact/action
  context without changing raw caller request shape.
- `[modify] pkg/cardtmpl/updater.go` — Snapshot before dynamic ReplaceView/Append
  and preserve exact identity.
- `[modify] internal/carddispatch/types.go`
- `[modify] internal/carddispatch/registry.go`
- `[modify] internal/carddispatch/context.go`
- `[modify] internal/carddispatch/dispatch.go`
- `[modify] internal/carddispatch/mutation.go` — author producer-bound marker,
  retain `template_ref`, and expose read-only binding/Snapshot facts.
- `[modify] modules/bot_api/card_template_catalog.go`
- `[modify] modules/bot_api/send.go` — author Bot marker and reject raw forgery
  on send/edit.
- `[modify] modules/robot/sanitize_robot_ingress.go` — reject the server-only
  template/provenance fields on the legacy raw robot card ingress.
- `[modify] modules/message/api_card_action.go`
- `[modify] internal/cardactiondispatch/contract.go` — persist validated
  provenance in additive durable `CardContext` fields.
- `[modify] modules/notify/card_via_registry.go`
- `[modify] modules/notify/action_finalizer.go` — consume stored exact identity;
  do not infer producer from sender/owner.

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

Add `[new] pkg/cardtmpl/provenance_test.go` only for the new value object's
table-driven unit matrix. Do not create one new test file per attack case.

Exit gate: raw Bot/robot/user/incoming-webhook forge, cross-bot,
cross-producer, cross-Space, missing/malformed marker, `template_ref` mismatch
and no-Snapshot mutation are RED then GREEN; static historical frames remain
compatible. User and incoming-webhook production paths need no new capability:
their existing absolute card rejection/server-built envelope remains the
boundary and receives regression coverage only.

### Slice 2 — grant migration, store and manager API

Production files:

- `[new] modules/card_template_catalog/sql/20260805000001_card_template_catalog_grant.sql`
- `[new] modules/card_template_catalog/store_grant.go`
- `[new] modules/card_template_catalog/api_grant.go`
- `[modify] internal/carddispatch/context.go` — consume only the read-only
  producer-binding resolver installed in Slice 1; the request body cannot
  construct a `ProducerSpec`.
- `[modify] modules/card_template_catalog/store.go`
- `[modify] modules/card_template_catalog/api.go`
- `[modify] modules/card_template_catalog/api_state.go`
- `[modify] modules/card_template_catalog/api_i18n.go`
- `[modify] modules/card_template_catalog/metrics.go`
- `[modify] pkg/errcode/card_template_catalog.go`
- `[modify] pkg/i18n/locales/active.zh-CN.toml`

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

Production files:

- `[modify] pkg/cardtmpl/catalog.go`
- `[modify] pkg/cardtmpl/runtime_catalog.go`
- `[new] modules/card_template_catalog/store_authorization.go`
- `[modify] modules/card_template_catalog/store_runtime.go`
- `[modify] modules/card_template_catalog/runtime_install.go`
- `[modify] modules/bot_api/card_template_catalog.go`
- `[modify] modules/bot_api/bot_api.go` — mount only the card-profile route as
  `authBot → botActorUID → SharedUIDRateLimiter`; do not silently rate-limit
  unrelated legacy Bot routes.
- `[modify] modules/bot_api/card_profile.go`
- `[modify] modules/bot_api/send.go`
- `[modify] modules/bot_api/resolve_targets.go`
- `[modify] modules/bot_api/space_inject.go`
- `[modify] main.go` — inject the shared resolver/binding dependencies; do not
  create a second grant truth.

Focused tests:

- `[modify] pkg/cardtmpl/runtime_catalog_test.go`
- `[new] modules/card_template_catalog/store_authorization_mysql_integration_test.go`
- `[modify] modules/card_template_catalog/runtime_db_integration_test.go`
- `[modify] modules/bot_api/card_template_catalog_test.go`
- `[modify] modules/bot_api/card_profile_test.go`
- `[modify] modules/bot_api/card_template_send_test.go`
- `[modify] modules/bot_api/card_template_edit_test.go`
- `[modify] modules/bot_api/resolve_targets_test.go`
- `[modify] modules/bot_api/space_inject_multispace_test.go`

Exit gate: active/block/grant are read in one primary-DB snapshot; profile and
send share the same resolver but make independent decisions; dynamic active
shadows static same-ID new-send; revoke/DB failure/invalid or ambiguous Space
never falls back; static gate-off behavior does not gain a DB dependency.

### Slice 4 — B1/B2 discovery and safe export

Production files:

- `[new] pkg/cardtmpl/export.go`
- `[modify] pkg/cardtmpl/template.go`
- `[modify] pkg/cardtmpl/registry.go`
- `[modify] pkg/cardtmpl/json_artifact.go`
- `[new] modules/card_template_catalog/store_discovery.go`
- `[new] modules/card_template_catalog/api_discovery.go`
- `[modify] modules/card_template_catalog/api.go`
- `[modify] modules/card_template_catalog/api_i18n.go`
- `[modify] modules/card_template_catalog/metrics.go`
- `[new] pkg/space/middleware_localized.go`
- `[modify] pkg/errcode/card_template_catalog.go`
- `[modify] pkg/i18n/locales/active.zh-CN.toml`

Focused tests:

- `[new] pkg/cardtmpl/export_test.go`
- `[new] modules/card_template_catalog/store_discovery_mysql_integration_test.go`
- `[new] modules/card_template_catalog/api_discovery_test.go`
- `[new] pkg/space/middleware_localized_test.go`
- `[modify] modules/card_template_catalog/api_test.go` — add
  `api_discovery.go` to `TestCatalogNoLegacyErrorResponses`.

Exit gate: public/private/Space/superAdmin matrix, visible-only cursor,
cross-Space replay rejection, anti-enumeration, ETag/304/cache headers, 2 MiB
cap and synthetic-only samples are proven. Request handlers read immutable
projection bytes, never repository source paths.

### Slice 5 — docs-notify dynamic pilot

Production integration files, only where the generic slices do not already
complete the path:

- `[modify] modules/notify/card_via_registry.go`
- `[modify] modules/notify/action_finalizer.go`
- `[modify] modules/notify/card.go`
- `[modify] main.go`

Pilot fixture and test:

- `[new after version preflight] modules/card_template_catalog/testdata/pilot/docs.access-request@<selected-version>/bundle.json`
- `[new] modules/notify/runtime_catalog_pilot_integration_test.go`

`<selected-version>` is a placeholder, not a default. Before creating the exact
directory, query the dedicated non-production catalog and prove the candidate
has never been claimed. If it is already claimed, choose a new reviewed SemVer;
never overwrite/reuse the key. The fixture is input to publish/E2E only:

- do not add it to `pkg/cardtmpl/docs_access_request/handoff/`;
- do not register it in `DefaultRegistry`;
- do not `go:embed` it into production startup;
- do not treat its presence in Git as activation or authorization.

Exit gate: establish static `docs.access-request@0.3.0` activation baseline,
then publish→grant→activate→B1/B2→send→Action.Submit→same-version edit→rollback;
prove old dynamic historical edit while edit grant remains, then revoke and
prove rejection.

### Slice 6 — verification, operations and PR closeout

Documentation files:

- `[modify] docs/card-template-runtime-catalog-runbook.md`
- `[modify] docs/card-protocol.md`
- `[modify] docs/platform-card-base.md`
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
