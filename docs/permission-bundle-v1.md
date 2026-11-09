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
`handoff-v4.json` is the current sibling interface; earlier handoffs are retained
as immutable history. `permission-prepare-v1.schema.json` specifies the approved bounded
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

The default composition has no installed `FencedAuthority` or supported release.
Permission routes are therefore **not mounted**. Trusted startup
composition calls `permissionbundle.Install` once with real dependencies, before
constructing modules. Robot and BotAPI constructors consume that context-bound
runtime. The old unused `ConfigurePermissionBundle` hooks were removed. No
environment flag, fabricated nonce, cached pool or candidate catalog enables
the feature. Synthetic adapters remain confined to test files.

The installed runtime now participates in the identified association commit
owners: BotFather creation/binding (including the existing REST/OBO association
units), Space creation and outer owner-invite commit, initial join, direct add,
reactivation, both approval exits, per-member insert units, ignored inserts and
manager batch upsert. A fresh RR transaction has a deadline beginning before its
native prefix; native/project steps precede sorted header suffixes in the same
commit. Rejoins require the existing header and never seed missing state. Human
and built-in/legacy-token associations create no User Bot header. Default legacy
transactions retain their original behavior when no runtime is installed.

The optional private fact route now has a bounded server adapter for all three
unchanged native request arms and all ten profiles. It verifies complete ordered
results, request/key/recipient/profile binding, native widths, page bounds,
authority read-nonce references, SQL/IM coverage, SQL incarnation and disclosure
context. A real `NativeFactSource.Read` must supply the metadata and
`ValidateCuts` must recognize the actual authority/position domains and prove
acknowledgement-before-read semantics. Known false atoms are returned as facts;
the server does not evaluate model permission decisions. This protocol adapter
does **not** implement missing native original-message/content primitives.

| Dependency / next owner | Exact missing interface/source and affected acceptance |
| --- | --- |
| Current SQL authority / native operations + backend | `FencedAuthority.Begin`, old-reader AND old-writer fencing, commit/read position acknowledgement and recovery at least through all acknowledged positions. The pinned pool/store exposes none of these. Required for serving, AC15–18 and positive owner/self/CAS acceptance. |
| Supported catalog / CLI + endpoint, backend comparison | One final immutable supported manifest with exact shared digest, capability membership and explicit migration equivalence. Historical 827-ID candidate/template is not a release. Required for provisioning, save/self/catalog and AC04/05/11/12/19/21. Ken Draft #234 delivers materialized engineering content but still has a null supported catalog digest. |
| Association commit acceptance / backend + native operations | Identified association creation/rejoin commits now invoke `BeginAssociation`/`CommitAssociations`. Actual native writer/fence/index/collation/locking/recovery verification and installed runtime composition still require the preceding dependencies. The 82-site/17-file authority census is not a claim of dynamic/external-writer completeness. AC01–04/08–10/18 remain unverified. |
| Original native facts / native IM + backend | The wire/provider adapter is implemented, but actual `InspectDeliveredHeaderV1`, `ReadCurrentAccessV1`, `ListHistoryHeadersV1`, `IssueHistoryDisclosureV1` and `ReadDisclosedBodyV1` primitives remain absent. Existing `bot_api/sync.go`, webhook payloads, message mirrors and directed friend/block checks do not attest original recipient/Space/version/visibility. The fact route mounts only with an installed real source. No fabricated false/positive profiles. AC06/08/13/14/17/18/21/22 NOT_RUN. |
| Delivery/host / native IM + endpoint | `GetReplayCheckpointV1`, `FenceThroughV1`, stable stream incarnation/sequence and coordinated pre-ACK durable reservation are absent in the server/native carrier seams. Endpoint owns reservation/binding/context integration. AC13/18/21 NOT_RUN. |
| Cross-repository dispatch / CLI + endpoint | Same-policy Stage1/Stage2 checks, context generation, process revision watermark and owned physical sends remain sibling work. This server never evaluates model permission decisions. AC05/11/12/15/16/19–22 NOT_RUN. |

Exact producer/source/output requirements, consumed sibling pins and remaining
owned adapter work are in `contracts/handoff-v4.json` and
`contracts/remaining-producers-v2.json`. The old Jeff/wrapper blocker is stale:
XIN-2794 now has Ken Draft #234, with a still-null supported catalog digest.
These are engineering blockers, not Owner waivers or completion by permanent
refusal. Actual native/read/send/content/restore tests and V-A–V-E/V-D2 were
not run. No shared DB, business content, provider, credentials, runtime or global
configuration was changed.

The PjM continuation adds `ArtifactReleases`, a concrete immutable loader and
compatibility adapter against the approved release contract. It consumes an
explicit trusted support list from XIN-2794, verifies the exact canonical whole
artifact digest, structural fragment, ordering, content hashes and references,
and requires directed approved migration pairs. Any changed entry/program graph
requires current deny reference validation. It provides no supported artifact,
source/profile reconciliation or competing catalog. See the bounded,
[source-cited producer/consumer map](permission-producer-map-v2.md) for the
precise split between delivered server code, sibling outputs, native source
changes and missing deployment facts. `remaining-producers-v2.json` supersedes
the generic v1 dependency disposition; `contract-manifest-v3.json` records current
contract bytes/hashes and the readable map.

The approved v42 packaging continuation adds the independent v2 verifier,
atomic `CatalogRegistry`, private manifest/listed-file reads, closed reader
discriminator and complete semantic-pair/CAS integration. V1 artifacts retain
their original serving behavior. The exact new interfaces, independently matched
complete synthetic ledgers and remaining production-source requirements are in
[permission-catalog-package-v2.md](permission-catalog-package-v2.md),
`handoff-v4.json` and `contract-manifest-v4.json`. The old schemas/templates and
historical handoffs remain immutable. Package verification does not supply the
still-missing actual native/SQL/recovery bindings or a production support pin.

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

The continuation also fixes the review's unconditional production route exposure
and the inherited `my_bots` cross-Space listing gap. New regressions check absent
unconfigured routes and member/nonmember/query-failure/legacy listing behavior.
Core tests additionally cover atomic native association/header rollback, rejoin
without reseeding, all ten metadata profiles and all three native request arms.

Catalog tests include successful pinned load/migration/unchanged-deny retention,
copy isolation, unsupported/rebound pins, corrupt content and descriptor hashes,
invalid base64, open references, ordering, cancellation and unapproved migration.
All test support entries are explicitly synthetic and never installed by
production. Nil runtime checks are now explicit at constructors and route
registration; nil-safe receiver behavior was already covered. Disclosure remains
its closed permit-only arm, with no resolve Header/purpose; a regression checks
both the valid response and rejection of an added Header without a panic.

The source delivery remains incomplete. PjM coordinates the named producers;
independent review is deferred until complete source delivery. No formal review,
deployment, ready conversion, merge or product acceptance is requested.
