# Permission bundle server components

This Draft implements bounded server components for XIN-2793 / issue #950.
The Owner-approved v8 design and choice B govern it. It is not a complete
permission product or native authority proof. Source base:
`79701d4912eba79a8bcb02c58ed86609c38335e2`;
target: `chore/mcp-integration-20261103`.

The `pkg/permissionbundle/contracts` directory carries the exact unchanged
wire, managed identity, native-fact and private resolver schemas, plus the
unchanged release **template**. The template's rejecting root remains intact;
neither it nor the historical candidate is a supported runtime catalog.
`handoff-v1.json` records the sibling-facing routes, error contract and source
hashes. `permission-prepare-v1.schema.json` specifies the approved bounded
Owner preparation interface. No model-side evaluator or policy tool is added.
`contract-manifest-v1.json` commits each contract's exact bytes and SHA-256;
`synthetic-examples-v1.json` supplies positive and negative component fixtures.

## Supported component behavior

The parser rejects decoded duplicate keys, invalid UTF-8/surrogates, unknown
members, coerced versions, nulls, overflow and trailing JSON. Canonical output
sorts set values/references/rules, fills only the group descendant default and
hashes the entire ten-field bundle excluding only `bundle_digest`. Both stages
share revision/CAS; accepted no-ops advance revision. Invocation U-to-E advances
the persistent epoch. Deactivation erases rules; reactivation cannot recover
them. Catalog migration requires explicit complete capability rules.

Choice B skips current reference revalidation only for an exact stored deny
with the same stage, ID, effect, caller, source/defaulted descendants and
capability intent under the authorized key/incarnation/expected tuple. A changed
or copied rule, any allow and any new intent require current eligibility.
Migration retention additionally requires explicit old/target semantic
equivalence from the supported release integration. Removing a deny remains an
explicit policy edit.

`Store` uses the existing DBR pool and a fresh writer RR transaction. Current
native locks precede a final permission header suffix. A preflight policy read
is used only to plan unchanged-deny/reference validation; the final current
header must match its digest and expected tuple before a conditional update.
There is no automatic retry, cache, read provisioning, upsert reset or header
deletion. Unknown COMMIT/acknowledgement requires complete GET reconciliation.

`SQLNative` implements current canonical robot/user/creator/Space/member
configuration checks and exact human/DM/group/thread reference checks.
All-status app identities conflict; an inactive peer robot alone does not.
Native unique equality lookups retain their absence ranges; returned bytes must
match exactly. SQL failures are never treated as missing/eligible. DM
preparation returns only ordered kind/id pairs after whole-batch success and
save independently validates them. Equal CRC32 and native IDs containing `@`
remain unavailable representation gaps, not invented supported domains.

Owner routes use existing human authentication, the shared UID limiter,
preliminary Space middleware and the existing creator/not-found boundary.
Self routes use actual Bot authentication and the existing Bot limiter and
require `bf_` User Bot identity. Neither another Bot nor a Space administrator
gets owner access. Every store operation rechecks current native scope.
Responses are private/no-store; new semantic errors preserve D14 wire HTTP400.
Self returns only P; Owner returns the complete bundle/attribution/descriptor.

Identity verification opts into the minimal v2 projection using
`identity_version:2`. It current-locks the token and native identity rows,
omits display/owner/first-membership Space fields and preserves the separate
legacy context-policy opt-in. Omitted identity version retains the existing
unmanaged behavior. Identity verification does not establish exact-Space
permission or an acknowledged-position recovery guarantee.

## Explicit integration prerequisites

The default composition has no `FencedAuthority` or supported `Releases`
implementation. Owner/self routes therefore remain unavailable. The
`ConfigurePermissionBundle` composition methods accept those reviewed adapters;
no environment flag, fabricated nonce, cached pool or candidate catalog enables
them. Synthetic test adapters are confined to test files.

| Dependency / next owner | Exact missing interface/source and affected acceptance |
| --- | --- |
| Current SQL authority / native operations + backend | `FencedAuthority.Begin`, old-reader AND old-writer fencing, commit/read position acknowledgement and recovery at least through all acknowledged positions. The pinned pool/store exposes none of these. Required for serving, AC15–18 and positive owner/self/CAS acceptance. |
| Supported catalog / CLI + endpoint, backend comparison | One final immutable supported manifest with exact shared digest, capability membership and explicit migration equivalence. Historical 827-ID candidate/template is not a release. Required for provisioning, save/self/catalog and AC04/05/11/12/19/21. The CLI task currently reports its prescribed account wrapper unavailable; no shared release was delivered in the sibling discussions read for this task. |
| Association commit coverage / backend + native operations | `ProvisionSuffix` is implemented but is **not wired** into the 82-site/17-file association census. Every outer native commit must retain its prefix, finish registered steps, then append sorted header suffixes under real authority and the supported release. Requires the preceding dependencies. No GET/backfill or fake fresh nonce substitutes. AC01–04/08–10/18 remain unverified. |
| Original native facts / native IM + backend | `InspectDeliveredHeaderV1`, `ReadCurrentAccessV1`, `ListHistoryHeadersV1`, `IssueHistoryDisclosureV1` and `ReadDisclosedBodyV1` are absent. Existing `bot_api/sync.go`, webhook payloads, message mirrors and directed friend/block checks do not attest original recipient/Space/version/visibility. The private fact route is intentionally **not registered**; unchanged schema bytes are supplied for integration. No fabricated false/positive profiles. AC06/08/13/14/17/18/21/22 NOT_RUN. |
| Delivery/host / native IM + endpoint | `GetReplayCheckpointV1`, `FenceThroughV1`, stable stream incarnation/sequence and coordinated pre-ACK durable reservation are absent in the server/native carrier seams. Endpoint owns reservation/binding/context integration. AC13/18/21 NOT_RUN. |
| Cross-repository dispatch / CLI + endpoint | Same-policy Stage1/Stage2 checks, context generation, process revision watermark and owned physical sends remain sibling work. This server never evaluates model permission decisions. AC05/11/12/15/16/19–22 NOT_RUN. |

These are engineering blockers, not Owner waivers or completion by permanent
refusal. Actual native/read/send/content/restore tests and V-A–V-E/V-D2 were
not run. No shared DB, business content, provider, credentials, runtime or global
configuration was changed.

## Migration and rollback

`modules/robot/sql/20261108000001_bot_permission_scope.sql` adds the permanent
binary-key header table without seed/backfill/cascade. It was not applied to a
business database. Native association and initial U/U revision0 epoch0 header
must eventually commit together. Existing anchors preserve denies across a
temporary rejoin; changed/ambiguous anchors remain non-serving under O2.

Rollback disables composition/consumers while preserving header rows,
incarnations, epochs, erased grants, anchors and recovery fences. Down migration
is intentionally non-destructive. Catalog rollback is an explicit new higher
revision with a still-supported release, not database restoration or revision
reversal. Retirement/transfer/key reuse and broader O3 retention remain open.

## Local verification

`go test -race ./pkg/permissionbundle ./pkg/mcppolicy -count=1` exercises strict
wire/bytes, lifecycle, CAS, exact deny retention/migration, native SQL predicates,
private projections and actual disposable SQLite file persistence/restart.
SQLite uses an in-package test-only lock override and proves no InnoDB gap-lock,
isolation, topology, native delivery or recovery guarantee. A concurrent same-CAS
fixture permits one update; an acknowledgement failure after actual COMMIT
requires readback and preserves the committed state.

The first deny-retention tests failed because sealing reused rule maps; the fix
unmarshals into a fresh value, preserving original stored intent. This is
red-to-green component evidence, not runtime acceptance.

Focused robot/Bot route and i18n source guards, the legacy policy tests,
`go build ./...`, `make i18n-extract-check` and `make i18n-lint` are the bounded
regression checks. The user package's normal `TestMain` unconditionally starts a
MySQL-dependent rollout session; full package/integration tests remain NOT_RUN.
The selected identity/parser/legacy verification tests can run without that
entry point using `scripts/test-permission-identity.py`.

The next reviewer is the independently assigned backend/security reviewer via
PjM, reviewing the exact Draft head and these explicit incomplete integrations.
No formal review, deployment, ready conversion, merge or product acceptance is
claimed.
