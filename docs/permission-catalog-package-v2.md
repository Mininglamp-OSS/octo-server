# Verified catalog packages — v42 server delivery

The server now independently verifies `permission-catalog-package-v2`, stores an
immutable in-process snapshot, serves private manifest/listed-file reads, and
compares representation-neutral semantic views during explicit catalog migration.
V1 artifacts remain readable through their original validator and no-format
route. No production package or support pin is included or enabled by default.

Authority is task comment `42cf053f-adf9-43ba-b7e2-0f4af4e8f73e`, implementing the
Owner-approved corrected addendum, attachment
`01a1bfb4-aeef-7497-8f7f-268335e0b0fa`: 70,457 bytes, SHA-256
`f8bcf1af183fcb69b5c40575d26e21b8a37e7e8fb6720b53e4013924ad078725`.
The exact addendum, change map and source ledger were authenticated-downloaded
and hash-verified before edits. Approval supersedes their historical pending
language only within this packaging amendment. V8, PRD2.1, v28 deny retention,
native ACL, no-OBO and the three original source pins remain unchanged.

## Verifier and immutable storage

`VerifyCatalogPackage(ctx, PackageInput, SlotValues)` validates the same exact
uncompressed bytes independently of a producer/other consumer's pass report:

- Closed manifest/index/module/shared grammar, canonical metadata, exact limits,
  source pins, inventory, lengths/hashes, shard continuity and all 827 immutable
  identities/targets/aliases. The identity oracle is a source-cited projection of
  the approved candidate, not a supported release or a replacement index digest.
- Complete typed Entry/profile/Content/SharedRow/Root closure. Index.source_files
  contributes a `_global` root label; all resources are in `_global`; transitive
  Content belongs to the lowest complete referring module. Orphans, wrong target
  types, duplicate placement, dependency cycles and hidden blob padding reject.
- One-level raw slices, checked ranges/sums, raw Root hashes and descriptor hash
  rules. Binary resources remain opaque. JSON is parsed only for declared JSON
  consumers, including parameter truth. Inherited `eq.literal:null` remains valid;
  missing literal and null in new required packaging fields reject. Raw member
  order, whitespace, number spelling, literal strings and bytes are not rewritten.
  Raw JSON work accounting uses the inherited renderer's JSON syntax domain,
  counting duplicate member values without metadata scalar/duplicate filters.
  Slot pointers must still be unambiguous. Schema validation uses bounded numeric
  projections without changing committed inherited values or raw bytes.
- Complete disjoint 56,592 schema, 156 discovery and 1,248 static-binding cells.
  Each discovery inventory contains its exact operation set; source row order
  remains intact. No first-match, model selector, default or omitted variant.
- Whole-value slot pointers/markers and the two unchanged domains. The inherited
  RawMessage/two-space/HTML-escape renderer preserves member order and number
  spelling. Pre-slot and conservative joint-assignment post-slot byte/node
  ledgers count repeated semantic payload uses, not cell visits. Decoding charges
  nodes before allocation; expansion and formatted-output sizes are checked.

The schema is separately versioned at
`pkg/permissionbundle/contracts/permission-catalog-package-v2.schema.json`.
The old rejecting release template remains byte-identical. Structural validation
is not source-equivalence, fixed-program source reconciliation or support approval.
Those remain prerequisites to the trusted support registry.

`ReadCatalogDirectory` accepts only an operator-owned private staging root. It
pins that root with `os.OpenRoot`, bounds inventory and reads, rejects nonregular
files/symlinks/hardlinks/extra paths and directories, and checks identities and
metadata before/after the complete read. A platform without link-count evidence
cannot use that directory adapter; embedded immutable maps remain supported.
No request path reaches this filesystem adapter.

`CatalogRegistry.Replace` validates every source and directed pair before one
atomic pointer swap. Failure at a final file or pair leaves the prior registry
intact. Version rebinding rejects, including a version removed earlier in the
process. Reads return copies from an exact pin; unsupported/removed pins never
substitute an old snapshot. The serving registry contains no mutable caller data.
Startup passes that registry through the existing `Store.Releases` and trusted
`Install` seam; absent real authority dependencies still prevents installation.

## Exact producer seam

XIN-2794 remains the sole production package publisher. Supply `SupportedSource`
with the exact reviewed pin, explicit format and complete raw file map (or a
directory snapshot). No source program, archive size or pass report substitutes
for file verification. Archives are carriers only; the ledger counts every
uncompressed file including the manifest.

For templates with slots, also supply the independently source-reconciled private
`SlotValues(context.Context, SlotCell)` binding. `SlotCell` contains pin, exact
Entry ID, requested schema facade, resources boolean, managed mask 0–7 and server
facade `three/two/both`. One call returns a **joint** assignment for
`facade_hint` and/or `server_managed_argument_names` for that same cell. Names
must be sorted distinct scalar-valid strings; hints are scalar-valid strings.
The server visits every matching cell, enforces each declared max_length, renders
the output and calculates its own maxima. A package cannot supply that oracle,
declare its own worst-case result or invent source-valid values. Missing source
bindings refuse slot-bearing admission; slot-free packages need no callback.
The synthetic binding in tests is not installed as a production source binding.

Required producer evidence remains the final complete package, stored/pre/post/
node/depth/part/selector ledgers, actual source-renderer equality for all cells,
discovery's eight-mask invariance and reviewed support pins/pairs. Parameter truth,
fixed-program/source validity and actual renderer equivalence cannot be inferred
from this synthetic fixture or a matching package hash. Existing docs provenance,
search maximum and physical-action contracts remain independently open.

## Private reads and migration

The existing authenticated owner/self `/catalog` route accepts v2 only with one
each of `space_id`, `format=permission-catalog-package-v2`, `catalog_version` and
`catalog_digest`. They must match fresh current P and the supported pin. It returns
exact manifest bytes. `/catalog/file` additionally requires `path`, decoded once
into the finite grammar and current manifest inventory. JSON files use JSON media;
blobs use `application/octet-stream`; responses are private/no-store and unencoded.
Every request independently checks current owner/self/native eligibility under
the existing limiter and one request deadline. A manifest gives no later-read
lease. There is no filesystem/URL resolver, redirect or model-visible tool.

No-format v1 requests retain whole-artifact behavior. A no-format request against
a v2 scope returns unsupported, never a disguised manifest or downgrade.
`permission-wire-catalog-v2.schema.json` changes only the closed descriptor's
permitted catalog_format value; policy fields/schema_version/limits are unchanged.
OwnerResponse chooses its discriminator from the fresh supported policy pin.

Directed pairs require complete validated semantic-view equality, backed by full
view comparison as well as hashes. Physical paths, pools, slices, shard layout,
packaging constants and representation-only descriptor IDs are removed; raw
hashes, slots/renderers, all finite selections, profiles, source and helper
bindings remain. Legacy descriptors are interpreted only when their complete old
selection/static maps are unambiguous. A single textual ID or approval flag
cannot override changed meaning. The internal legacy normalizer publishes no
new package or pin and does not alter v1 serving behavior.

Actual SQLite tests migrate v1→v2, retain a departed-member deny under exact CAS,
check complete GET readback, reject stale CAS/new stale allow, and explicitly
roll back at a new higher revision. Invocation state and context epoch stay
unchanged. Package installation itself writes no policy. Preserve old referenced
packages, headers, nonces, epochs, erased grants and recovery fences; complete
package recovery remains part of the real authority/operator obligation.

## Bounded positive conformance evidence

The committed test archives are deliberately **synthetic**, not a supported
catalog or a source-equivalent product release. A separate Python fixture builder
uses literal raw/renderer byte oracles and independent accounting. Both packages
contain 18 files, all 827 identities, ten inherited profile specimens, 393 truth
records, 1,179 base variants and all 57,996 selection cells. They exercise
transitive/shared ownership, index-only sources, inherited null, empty/binary
roots and slices split inside UTF-8. Gzip is only a convenient test carrier.

| Ledger | Slot-free fixture | Joint-slot fixture |
| --- | ---: | ---: |
| Entire stored bytes, including manifest | 1,220,366 | 1,220,604 |
| Pre-slot expanded bytes | 96,539 | 96,601 |
| Conservative post-slot bytes | 96,583 | 96,673 |
| Metadata nodes | 37,848 | 37,858 |
| Pre-slot nodes | 44,144 | 44,158 |
| Conservative post-slot nodes | 44,144 | 44,159 |
| Maximum depth | 10 | 10 |
| Parts / Roots / SharedRows / Content | 791 / 791 / 393 / 1,603 | 791 / 791 / 393 / 1,603 |

The Go consumer independently matches these ledgers and exact renderer hashes.
Negative cases include missing/extra/tampered files, pins, duplicates, new nulls,
missing inherited literal, identity/selector/typed-edge errors, ranges, padding,
slot/source errors and aliases. Exact/+1 depth, aggregate node and repeated-use
expansion checks complement the complete fixtures. Concurrent registry readers,
last-file/pair failure, representation-only repacking, reader skew, fresh private
reads and real local CAS/rollback are covered. The old schemas/handoffs are retained.

Native/current-writer/acknowledged-position/restore providers and production
startup binding remain incomplete, with the precise unchanged prerequisites in
the prior source-cited producer map. This implementation supplies no fake provider
and makes no native, full MCP, OS, restore or product acceptance claim. Independent
review remains deferred until complete source delivery; no deployment is performed.

The prior automated review also identified post-commit acknowledgement handling.
`ErrReconcile` now distinguishes uncertain commit/failed acknowledgement from
pre-commit refusal while retaining the public unavailable envelope. Bot creation
preserves the exact durable Bot/scope instead of compensating it away. Enabled
association begin/commit helpers share the original context/deadline, including
authority acknowledgement; request-aware native callers can pass their parent
through `BeginAssociationContext`. Disposable SQLite tests prove the native row
and permanent header survive failed acknowledgement and remain readable through
fresh authority. [Review disposition](permission-review-disposition-v5.md) records
the remaining static assessment and its native concurrency limitations.
