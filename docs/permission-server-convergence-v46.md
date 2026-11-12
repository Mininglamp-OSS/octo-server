# Permission Server Convergence (v45 03:41 Continuation Slice — XIN-2793)

This document records the Owner-authorized 03:41 continuation slice on `Mininglamp-OSS/octo-server` Draft PR [#951](https://github.com/Mininglamp-OSS/octo-server/pull/951) (`feat/xin-2793-permission-server` targeting `chore/mcp-integration-20261103` at `79701d4912eba79a8bcb02c58ed86609c38335e2`), continuing from `767cce7bcc27b6b6babdcb394f0ebd0553ab1ac6`.

## 1. Opt-In Startup Composition with Real `modules/message.NewServerMessageFactHooks`

- `main.go:installPermissionBundle(ctx)` now invokes `permissionbundle.InstallFromEnvWithHooks(ctx, nil, nil, message.NewServerMessageFactHooks(ctx))`.
- `pkg/permissionbundle/install.go` (`InstallConfig`, `InstallFromEnvWithHooks`, `InstallOptIn`) wires `ServerMessageFactProvider` with the real `modules/message.NewServerMessageFactHooks(ctx)` when `OCTO_PERMISSION_BUNDLE_FACT_PROVIDER_MODE=octo_server_message_store_v1`, reading:
  - `OCTO_PERMISSION_BUNDLE_STORE_AUTHORITY_ID` (optional, default `octo-server-message-store`),
  - `OCTO_PERMISSION_BUNDLE_STORE_INCARNATION` (required 32-hex lineage label),
  - `OCTO_PERMISSION_BUNDLE_PERMIT_KEY_HEX` (required >=32-byte hex HMAC key),
  - `OCTO_PERMISSION_BUNDLE_DURABLE_PERMIT_LEDGER` (optional boolean enabling SQL table `bot_permission_disclosure_permit`).
- **Live runtime activation remains fail-closed**: `main.go` passes `authorityDriver = nil` and never installs a dummy/no-op authority or self-approves the unapproved Architect `4831fa26` / `7b11211f` SQL recovery/history proposal. If `OCTO_PERMISSION_BUNDLE_ENABLED=true` is set without an approved `Authority` / `ExternalAuthorityDriver`, startup fails closed with `ErrUnavailable`.
- When valid dependencies are supplied in opt-in composition, `/v1/bot` mounts:
  - `GET /v1/bot/permission-bundle/v1`
  - `GET /v1/bot/permission-bundle/v1/catalog`
  - `GET /v1/bot/permission-bundle/v1/catalog/file`
  - `POST /v1/bot/permission-facts/v1`
  - `POST /v1/bot/permission-facts/v1/bind-delivery`

## 2. Authenticated Metadata-Only Message Reference/Version Binding Seam

- Implemented `ServerMessageFactProvider.BindDeliveredMessageV1`, `SQLNativeFactAdapter.BindDeliveredMessage`, `Runtime.BindDeliveredMessage`, and `POST /v1/bot/permission-facts/v1/bind-delivery?space_id=<space_id>`.
- **Why a current-version lookup alone does NOT prove the delivered version**:
  - `octo-server` persists initial message rows in `message_<shard>` and mutable edits/revocations in `message_extra` (`version`, `content_edit`, `content_edit_hash`, `revoke`, `is_deleted`), without keeping a per-version historical payload table.
  - Looking up `(channel_type, channel_id, message_id)` alone would return the current `x<version>_<content_edit_hash|0>` even if a concurrent edit occurred between WKSocket delivery and the endpoint's binding call, silently binding a pre-edit delivery frame to post-edit metadata (or vice versa).
- **Required trusted ingress evidence (`DeliveryBindingRequestV1`)**:
  - `channel_type` (`1` = DM/person, `2` = group, `5` = thread), `channel_id`, `message_id`, `message_seq`, `from_uid`, `observed_payload_sha256` (64-hex lowercase SHA-256 of the raw payload bytes observed on the WKSocket delivery frame before any body parsing/release), and optional `expected_native_version`.
- **Deterministic refusal reasons (`status: "refused", current: false`)**:
  - `"mirror_lag"`: row absent in sharded `message` table (not yet persisted or `no_persist`).
  - `"expired_reference"`: `message.is_deleted != 0`, `message_extra.revoke != 0`, `message_extra.is_deleted != 0`, thread deleted (`status == 3`), or group missing/disabled.
  - `"parent_unavailable"`: thread's parent group missing or not in Normal/Disband state.
  - `"identity_mismatch"`: stored `from_uid != request.from_uid` or stored `message_seq != request.message_seq`.
  - `"payload_digest_mismatch"`: SHA-256 of current effective stored payload (`content_edit` when edited, else `message.payload`) does not match `request.observed_payload_sha256` (e.g. concurrent edit race).
  - `"version_mismatch"`: optional `request.expected_native_version` does not match current `x<version>_<content_edit_hash|0>`.
  - `"visibility_denied"`: known space mismatch (`!MessageSpaceUnknown && !MessageSpaceExact`) or `!BotCurrentRead` (`visibles` exclusion, per-user delete, channel offset clear, or bot not active member).
  - `"predicate_unsupported"`: foreign DM channel (`dmPeer` mismatch) or missing sender user row.
- **Metadata-only & non-fabrication guarantees**:
  - Zero message, quote, or media body bytes are ever returned in `"bound"` or `"refused"` responses.
  - Group and thread bindings without a `visibles` exclusion report `unknown_predicates: ["bot_original_visible", "caller_original_visible"]` and `resolvable_purposes: ["reply"]`, never fabricating original visibility as `true`.
  - Untagged DM bindings (`payload.space_id == ""`) report `unknown_predicates: ["message_space_exact"]` and `resolvable_purposes: []`, never fabricating DM space membership as `true`.
  - Two-step history disclosure (`history_headers` -> `history_disclosure`) is preserved; no atomic header+body substitute is activated.

## 3. Own-Session Consistency & Restart-Durable Permit Ledger

- **Own-session `RepeatableRead` consistency (Resolved)**:
  - `SQLNativeFactAdapter.Read` and `SQLNativeFactAdapter.BindDeliveredMessage` attach the caller's `RepeatableRead` `*dbr.Tx` to `context.Context` via `withFactReadTx(ctx, tx, a.testNoLocks)`.
  - `ServerMessageFactProvider` executes all SQL reads (`loadMessage`, `loadExtra`, `groupFacts`, `activeMember`, `userHidden`, `ListHistoryHeadersV1`, and permit queries) directly on that `*dbr.Tx` with `FOR SHARE NOWAIT`, eliminating separate-session snapshot skew.
- **Restart-durable single-use permit ledger (`bot_permission_disclosure_permit`)**:
  - When `DurablePermitLedger` is enabled (`ServerMessageFactConfig.DurablePermitLedger: true` or `OCTO_PERMISSION_BUNDLE_DURABLE_PERMIT_LEDGER=true`), `IssueHistoryDisclosureV1` records issued permits in `bot_permission_disclosure_permit` (`ServerDisclosurePermitDDL`) and refuses to re-issue already-consumed permits; `ConsumeDisclosurePermit` / `ConsumeDisclosurePermitContext` atomically updates `consumed_at` (`WHERE consumed_at IS NULL`, requiring `RowsAffected() == 1`), preventing replay or re-issuance across process restarts.
  - Residual: live DB schema migration for `bot_permission_disclosure_permit` is out of scope of this slice; when `DurablePermitLedger` is false, single-use tracking falls back to the in-process `usedPmt` map.

## 4. Sibling Consumption & Private ABI Alignment

- **Consumed CLI (`XIN-2794`, Draft PR #234, head `d39072f8182a28ce2be5884151eb03f16cd205b2`)**:
  - `docs/permission/cli-component-contract-v6.json` (`8,998` B, SHA-256 `8da203c9e6b650f5bc94d81080f4ed7281bd2cc36755ff0027f8e2e6b0b9fe9d`)
  - `docs/permission/endpoint-abi-v2.json` (`6,955` B, SHA-256 `7f9556d0526cff30ef1c36d53d2457dbc4266b8912722717badebac796ad8909`)
  - `docs/permission/contract-manifest-v6.json` (`2,612` B, SHA-256 `7fbb9cece9dfa384181c2fc272cb6afc7f6667b32712aa2e71c8a1c04615301d`)
  - Semantic digest verified: `15c7ca2bf6df703979df529afbca6828f116b2463b1a818456edfad80bcad799`.
- **Consumed Endpoint (`XIN-2795`, Draft PR #102, head `d007962a7762ec42ae2279c673392121ad630e64`)**:
  - `docs/permission-endpoint-handoff-v6.json` (`12,897` B, SHA-256 `e7fe1cf23bf914eab7ba4429f51c477c63c7762eaff973326bfe5de4b2ba258c`)
  - Endpoint `d007962a` reconciled semantic digest `15c7ca2bf6df703979df529afbca6828f116b2463b1a818456edfad80bcad799` and removed `octo-im`/HA demands, but `src/permissions/endpoint-factory.ts` currently posts custom `op: "inspect_delivery" | "recover_fence" | "prepare_disclosure"` to `/v1/bot/permission-facts/v1`.
  - Exact private ABI call sequence and request/response payloads for `/v1/bot/permission-facts/v1/bind-delivery` (`operation: "bind_delivery"`) and `/v1/bot/permission-facts/v1` (`operation: "resolve" | "history_headers" | "history_disclosure"`) are published in `pkg/permissionbundle/contracts/handoff-v7.json`.
