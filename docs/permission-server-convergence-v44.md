# Permission Server Convergence — Baseline v44 Delivery

This document records the Baseline v44 server convergence on Draft [#951](https://github.com/Mininglamp-OSS/octo-server/pull/951) (`feat/xin-2793-permission-server` targeting `chore/mcp-integration-20261103` at `79701d4912eba79a8bcb02c58ed86609c38335e2`), addressing the `XIN-2793` v44 charter and the `XIN-2830` review (`edd9ad84-f10b-4e1e-9161-19a598bbb153`) together with its corrective addendum (`28585cc5-4cc3-47e0-b3a5-17f81ac55b75`).

All historical v1–v4 contract schemas, manifests, handoffs, and synthetic fixtures listed in `pkg/permissionbundle/contracts/contract-manifest-v4.json` remain byte-for-byte unchanged.

## 1. Independent v2 Candidate and `source-slot-witness-v5.json.gz` Verification

`pkg/permissionbundle/package_witness.go` implements `ReadSlotWitnessFile`, `LoadSlotWitnessGzip`, and `LoadSlotWitnessJSON` for `permission-source-slot-witness-v1`:

- **Consumed `octo-cli` head**: `27b387f1b767a6e0cc200b8e23d074ad3bd28cc1` (PR #234).
- **Candidate manifest**: `docs/permission/catalog-package-v2/manifest.json` (`4,934` bytes, SHA-256 `ecc56c2ab0a2c520ab7e5203d2abab06671daf90c28d69f88fe1de8d154109fc`, `catalog_version`: `octo-capabilities-v1-xin2748-v4`).
- **Source slot witness**: `docs/permission/source-slot-witness-v5.json.gz` (`12,562` compressed bytes, SHA-256 `0c5da173bd0f53c64562a70a5c5feebc955ad111bbe84ed11f5588c08b4b9c0b`; `843,132` decoded canonical JSON bytes, SHA-256 `0dac894681aa7f4f8810f5fa5574d68ce071f2a401df4a99da4a92e7a43e7c5b`).
- **Candidate report**: `docs/permission/catalog-package-report-v5.json` (`1,914` bytes, SHA-256 `7257dfef2ed8d48844f9a97335926109f4b49e85b4eb233ba593202375e85caf`).
- **Independent verifier results**:
  - `stored_bytes = 7,823,327`, `files = 30`, `roots = 4,128`, `shared_rows = 1,179`, `content_records = 5,481`, `parts_total = 15,649`, `max_parts_per_root = 25`.
  - `metadata_nodes = 158,934`, `pre_bytes = 10,202,896`, `post_bytes = 11,603,961`, `pre_nodes = 399,778`, `post_nodes = 398,617`, `max_depth = 15`.
  - `schema_cells = 56,592` (`29,070` slot-bearing across `2,700` disjoint bitmask groups), `discovery_cells = 156`, `static_cells = 1,248`.
  - Representation-neutral `semantic_digest = 15c7ca2bf6df703979df529afbca6828f116b2463b1a818456edfad80bcad799`.
- **Boundary**: Packaging verification and witness loading establish candidate structural/render/ledger conformance against the supplied witness only. They do **not** claim `C1–C9` source-reconciliation proof (`C3` parameter truth, `C4` profile/reference closure, `C6` discovery 8-mask invariance, `C7` fixed-program non-empty runtime branches) or production allowlist support (`supported_catalog_digest` remains `null`).

## 2. Explicit Opt-In Runtime and Route Composition

`pkg/permissionbundle/install.go` provides `InstallOptIn(ctx, cfg)` and `InstallFromEnv(ctx, authorityDriver, imProvider)`, wired into `main.go` via `installPermissionBundle(ctx)` immediately before `module.Setup(ctx)`:

- **Default (`OCTO_PERMISSION_BUNDLE_ENABLED` unset or `0`/`false`)**: `installPermissionBundle(ctx)` is a no-op; `RuntimeFor(ctx)` remains `nil`, and `/v1/robot/:robot_id/permission-bundle/v1*`, `/v1/bot/permission-bundle/v1*`, and `/v1/bot/permission-facts/v1` stay unmounted.
- **Explicit opt-in (`OCTO_PERMISSION_BUNDLE_ENABLED=1` or `InstallConfig{Enabled: true}`)**:
  - Validates HTTPS `Origin`, exact `CatalogPin`, verified `Releases` (or operator-owned `CatalogDir` + `SlotWitnessPath`), `FencedAuthority` passing `RecoverAtLeast`, `NativeGuard` (`SQLNative`), and optional `NativeFactSource` (`SQLNativeFactAdapter`).
  - When all configured dependencies pass verification, `Install` populates `RuntimeFor(ctx)`, causing `robot.New(ctx).Route(r)` and `bot_api.NewBotAPI(ctx).Route(r)` to mount the Owner policy/prepare/catalog routes and Bot self/catalog/fact routes.
  - When any required authority, recovery fence, origin, catalog, or fact dependency is missing or invalid, `InstallOptIn` fails closed with an explicit error wrapped in `ErrUnavailable` and never fabricates a ready runtime.

## 3. `FencedAuthority` Lineage vs. `election_generation`

`pkg/permissionbundle/authority_fence.go` implements `RecoveryFenceV1`, `ElectedAuthoritySession`, `ExternalAuthorityDriver`, `FencedAuthorityAdapter`, and `RejectUnfencedAuthorityMode`:

- **Distinct lineage and election term**:
  - `AuthorityRead.Incarnation()` returns the 32-hex continuity-verified authority lineage (`RecoveryFenceV1.Incarnation`), which is persisted in `NativeAnchor.AuthorityIncarnation` on `bot_permission_scope` rows and emitted in `NativeFactSource` SQL authority cuts.
  - `FencedAuthorityRead.ElectionGeneration()` tracks the `uint64 > 0` HA writer term (`RecoveryFenceV1.ElectionGeneration`).
  - Across a normal primary failover where `RecoverAtLeast` proves `RecoverableScopeCheckpoint >= MinAckPosition` under the same continuity-verified `Incarnation`, `ElectionGeneration` advances while `Incarnation()` stays unchanged—preserving all existing `bot_permission_scope` rows across `Get`, `Self`, `Prepare`, and `Mutate`.
  - An in-flight session from a superseded `ElectionGeneration` is fenced before `COMMIT` (`Validate` returns `ErrUnavailable`).
  - A stale database restore (`RecoverableScopeCheckpoint < MinAckPosition`) or discontinuous store rebuild (`Incarnation` change without explicit reprovisioning) fails closed with `ErrUnavailable`.
  - Post-`COMMIT` fence recording failure (`RecordAcknowledgedPosition`) returns `ErrReconcile`.
- **Rejected substitutes**: `RejectUnfencedAuthorityMode` explicitly rejects local SQL writable variables (`@@read_only`, `@@super_read_only`, `sql_read_only_variables`), same-DB counters (`same_db_counter`, `local_sql_table`), and unverified pool/lease substitutes (`unverified_pool`, `cutover_floor`, `credential_writer_lease`, `session_rollout_floor`).

## 4. In-Repo Native Fact Adapter and Storage Seams

`pkg/permissionbundle/native_facts_adapter.go` implements `SQLNativeFactAdapter` and the `NativeIMFactProvider` seam (`InspectDeliveredHeadersV1`, `ListHistoryHeadersV1`, `IssueHistoryDisclosureV1`, `ValidateIMCut`):

- Inside `SQLNativeFactAdapter.Read(ctx, tx, key, requestBytes)`, the adapter evaluates all SQL principal, Space, membership, group, thread, directed friend, and blacklist predicates inside the caller's `RepeatableRead` `*dbr.Tx` using locking reads (`FOR SHARE NOWAIT` / `FOR SHARE`) and combines them with authoritative IM header/visibility/history/disclosure facts across all three operations (`resolve`, `history_headers`, `history_disclosure`) and all ten profiles (`dm_admit_v1`, `dm_contributor_v1`, `dm_reply_v1`, `group_admit_v1`, `group_contributor_v1`, `group_reply_v1`, `thread_admit_v1`, `thread_contributor_v1`, `thread_reply_v1`, `unsupported_principal_v1`).
- `ValidateCuts` verifies both `sql` (`FencedAuthorityAdapter.VerifySQLAuthorityCut`) and `im` (`NativeIMFactProvider.ValidateIMCut`) authority cuts under `ack_before_read_start`.
- `RejectPlainMessageSyncFactSource` explicitly rejects plain content-first `IMSyncChannelMessage` (`im_sync_channel_message`, `plain_message_sync`, `bot_api_sync`) and webhook/SQL message mirrors (`webhook_mirror`, `sql_message_mirror`), which return payload bytes with `uint32` sequences and cannot prove header-only inspection or original visibility.
- Owner `Prepare -> CAS Mutate -> Complete GET` readback and choice-B unchanged departed-member `DENY` retention remain intact and verified under `FencedAuthorityAdapter`.

## 5. Disposition of `XIN-2830` Findings

| Finding | Source | Disposition in Draft #951 |
| --- | --- | --- |
| `octo-cli` v2 candidate + `source-slot-witness-v5.json.gz` verification | `XIN-2830` `edd9ad84` / `XIN-2793` v44 §1 | Resolved in `pkg/permissionbundle/package_witness.go`, `package_slots.go`, `package_graph.go`, and `package_witness_test.go`. Independent verifier matches all 30 files, 16 ledger metrics, and semantic digest `15c7ca2b...` without claiming `C1–C9` or production allowlist support. |
| Opt-in `permissionbundle.Install` runtime and route composition | `XIN-2830` `edd9ad84` / `XIN-2793` v44 §2 | Resolved in `pkg/permissionbundle/install.go` and `main.go` (`installPermissionBundle`), with route-mounting tests in `pkg/permissionbundle`, `modules/robot`, and `modules/bot_api`. |
| `AuthorityRead.Incarnation()` lineage vs. `election_generation` and rejection of local SQL flags / same-DB counters | `XIN-2830` `28585cc5` corrective addendum / `XIN-2793` v44 §3 | Resolved in `pkg/permissionbundle/authority_fence.go` and `authority_and_adapter_test.go`. `Incarnation()` remains the 32-hex continuity-verified lineage distinct from `ElectionGeneration()`; normal failover preserves `bot_permission_scope` anchors; `sql_read_only_variables` and `same_db_counter` are explicitly rejected. |
| In-repo SQL/IM native fact adapter seams without claiming plain `IMSyncChannelMessage` proves header-only inspection or original visibility | `XIN-2830` `edd9ad84` & `28585cc5` / `XIN-2793` v44 §4 | Resolved in `pkg/permissionbundle/native_facts_adapter.go` and `authority_and_adapter_test.go`. `SQLNativeFactAdapter` evaluates locking SQL predicates in `tx` and binds to `NativeIMFactProvider`; `RejectPlainMessageSyncFactSource` rejects plain content-first sync and mirrors. |
| Current-vs-delivered dependency contract for CLI/endpoint handoff | `XIN-2793` v44 §5 | Published in `pkg/permissionbundle/contracts/handoff-v5.json`, `remaining-producers-v3.json`, and `contract-manifest-v5.json`. |
