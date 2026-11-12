# Permission Server Convergence — Round 2 Minimal Mainline Repair (`[S-B2]`, `[S-B3]`)

## 1. Governance and Scope

- **Task**: `XIN-2793` (`6a92a547-cb82-4c3d-ac6c-8f5529028ba4`)
- **Authority & Review Baseline**:
  - Owner mainline authority: `2120917153384140800` and `2120953081876090880`
  - Independent Round 2 exact-head review: `7827f70f-94e7-4efc-b5b7-da7b7496f2db` on `XIN-2834` (`46d23689-0cb4-4d33-b488-8d430ae854c3`)
  - Trigger comment: `0534e306-733c-40d7-bd4b-5248257a0c32` on `XIN-2793`
- **Repository & PR**: `Mininglamp-OSS/octo-server` Draft PR `#951` (`boris-clark:feat/xin-2793-permission-server` -> `chore/mcp-integration-20261103` at `79701d4912eba79a8bcb02c58ed86609c38335e2`), continuing from Round 1 repaired head `5c04569dd9bb562629c0cd44c367bab15bafc84f` (`[S-B1]` verified CLOSED in `XIN-2834` Round 2 review).
- **Authorized Scope**: Fix confirmed `octo-server` Round 2 blocking findings `[S-B2]` and `[S-B3]` only, with focused red-to-green regressions and exact sibling ABI handoff (`handoff-v9.json`, `remaining-producers-v7.json`, `contract-manifest-v9.json`).

---

## 2. Root-Cause Verification & Minimal Code Repairs

### `[S-B2]` `ListHistoryHeadersV1` Cross-Space and Untagged-DM Guard (`pkg/permissionbundle/server_message_facts.go:678-690`)
- **Verified Defect**: At `5c04569d`, `ListHistoryHeadersV1` checked only `if res.unavailable != "" || !res.delivery.BotCurrentRead` before emitting `IMHistoryHeaderItem{Status: "resolved", Reference: itemRef, Header: res.delivery.Header}`. Unlike `IssueHistoryDisclosureV1` (`:780-783`), it omitted `res.delivery.MessageSpaceUnknown || !res.delivery.MessageSpaceExact || !res.delivery.MessageSourceExact`. Consequently:
  1. When a group or thread belonging to `space_B` (where the same `bot_uid` is a member) or a DM tagged `space_B` was queried under `key.SpaceID = space_A`, `resolveOne` set `MessageSpaceExact = false` and `Header.SpaceID = "space_B"`, and `ListHistoryHeadersV1` emitted `Status: "resolved"` across the space boundary.
  2. When an untagged DM (`payload.space_id == ""`) was listed, `resolveOne` set `MessageSpaceUnknown = true` with `Header.SpaceID = key.SpaceID`, and `ListHistoryHeadersV1` emitted `Status: "resolved"`.
- **Minimal Repair**:
  - Updated `ListHistoryHeadersV1` to require `res.unavailable == "" && !res.delivery.MessageSpaceUnknown && res.delivery.MessageSpaceExact && res.delivery.MessageSourceExact && res.delivery.BotCurrentRead` before emitting `Status: "resolved"`.
  - When `res.delivery.MessageSpaceUnknown` is `true`, `ListHistoryHeadersV1` emits `Status: "unavailable", Reason: "predicate_unsupported"`; when `!res.delivery.MessageSpaceExact || !res.delivery.MessageSourceExact || !res.delivery.BotCurrentRead`, it emits `Status: "unavailable", Reason: "expired_reference"` with an empty `Header`.

### `[S-B3]` `readHistoryDisclosure` `evalScopeBaseFacts` Guard and Pre-`IssueHistoryDisclosureV1` `context_epoch` Short-Circuit (`pkg/permissionbundle/native_facts_adapter.go:1030-1085`)
- **Verified Defect**: At `5c04569d`, `SQLNativeFactAdapter.readHistoryDisclosure` queried only `SELECT context_epoch,retired FROM bot_permission_scope` without calling `a.evalScopeBaseFacts(ctx, tx, key)` (`botUserActive && botRobotActive && spaceActive && botSpaceMember`), and called `a.IMProvider.IssueHistoryDisclosureV1(ctx, key, contextID, contextEpoch, headerRefs)` (which inserts durable permit rows into `bot_permission_disclosure_permit`) before checking `storedEpoch == contextEpoch`.
- **Minimal Repair**:
  1. Added `base, err := a.evalScopeBaseFacts(ctx, tx, key)` and `if err != nil || !base.botUserActive || !base.botRobotActive || !base.spaceActive || !base.botSpaceMember { return nil, ErrUnavailable }` at the start of `readHistoryDisclosure` (matching `BindDeliveredMessage`, `readResolve`, and `readHistoryHeaders`).
  2. When `storedEpoch != contextEpoch`, `readHistoryDisclosure` now short-circuits **before** calling `IssueHistoryDisclosureV1`: it obtains the IM authority cut via `a.IMProvider.InspectDeliveredHeadersV1(ctx, key, "admit", nil)` (zero references inspected, zero permits minted, zero rows inserted into `bot_permission_disclosure_permit`) and returns `status: "unavailable", reason: "expired_reference"` for every requested `header_reference` inside a valid 2-authority (`im` + `sql`) `HistoryDisclosureResponse` envelope.

---

## 3. Focused Red-to-Green Proof

- **Test Added**: `TestServerMessageFactsRound2CrossSpaceHistoryAndDisclosureScopeAndEpochGuards` in `pkg/permissionbundle/server_message_facts_test.go`.
- **Before Fix (RED)**:
  - `server_message_facts_test.go:993`: `dmPage.Items[1].Status` for cross-space DM `9503` (`space_id: "other-space"`) was `"resolved"` instead of `"unavailable"`.
- **After Fix (GREEN)**:
  - `go test -v -count=1 ./pkg/permissionbundle -run "TestServerMessageFacts|TestSQLNativeFactAdapter|TestContractManifest"` -> **PASS**.
  - `go test -race -count=1 ./pkg/permissionbundle -run "TestServerMessageFactsRound2CrossSpaceHistoryAndDisclosureScopeAndEpochGuards"` -> **PASS**.
  - Verified both directly on `ServerMessageFactProvider` / `SQLNativeFactAdapter` and end-to-end through `Runtime.Facts`:
    1. Same-space tagged DMs (`9501`, `9505`) resolve in `history_headers`; cross-space tagged DM (`9503`) returns `status: "unavailable", reason: "expired_reference"`; untagged DM (`9502`) returns `status: "unavailable", reason: "predicate_unsupported"`; cross-space group (`group_space_b` in `other-space`) returns `status: "unavailable", reason: "expired_reference"` with zero cross-space metadata leaked.
    2. `history_disclosure` with mismatched `context_epoch: "999"` returns `status: "unavailable", reason: "expired_reference"` with `SELECT COUNT(*) FROM bot_permission_disclosure_permit == 0`; matching `context_epoch: "0"` returns `status: "resolved"` with a 43-char base64url HMAC `permit` and `1` durable permit row.
    3. Deactivating `space_member` (`status = 0`) or `space` (`status = 0`) while `bot_permission_scope.retired == 0` fails closed (`ErrUnavailable` in `SQLNativeFactAdapter.readHistoryDisclosure` and `ErrForbidden` in `Runtime.Facts`) with `0` permit rows written for `9505`.

---

## 4. Contract & Sibling ABI Handoff Summary (`handoff-v9.json`)

- **Wire Schema & Route Paths**: Unchanged from `handoff-v7.json` / `handoff-v8.json` and `pkg/permissionbundle/contracts/permission-native-facts-v1.schema.json`.
- **Consumed Immutable Round 2 Sibling Pins**:
  - `octo-cli` Draft `#234` @ `30a8991c0bbbdfd42399e07dadbb60c74b26c3b4`:
    - `docs/permission/endpoint-abi-v2.json` (`11,307` B, SHA-256 `7fc8534000d299fd3e03e85f9c22ba7b5a5f699e4009ee00b27ee10c19dbcb32`)
    - `docs/permission/contract-manifest-v6.json` (`2,876` B, SHA-256 `e8f916b7f0b7551691b4359e0643bcbe9e6f51b0d10763232dd6b11805ce8d38`)
  - `codex-channel-octo` Draft `#102` @ `6830c4050ccb7a299114933b7eb6afbea990abb2`:
    - `docs/permission-endpoint-handoff-v7.json` (`13,799` B, SHA-256 `8f67e01671080c41639d750ddec0e89e03bcab42af8ab9b86e51cae79c65ad7c`)
    - `docs/permission-v50-continuation.md` (`13,118` B, SHA-256 `b0c4e7865a46e86b31972f9439bb70c677e93e402177bab4a412fce0bff4f38c`)
- **Exact Producer ABI Note for `XIN-2795` (`[A-B6]`)**:
  - `octo-server` `ServerMessageFactProvider.permit` (`pkg/permissionbundle/server_message_facts.go:710`) emits `base64.RawURLEncoding.EncodeToString(m.Sum(nil))` — a 43-character unpadded base64url HMAC-SHA256 string matching `$defs/Token` (`^[A-Za-z0-9_-]{1,512}$` in `permission-native-facts-v1.schema.json`) without a `pmt1-` prefix.
  - `codex-channel-octo` `createNativeEndpointDriver.disclose` (`src/permissions/endpoint-factory.ts:576`) must validate `primary.permit` against `$defs/Token` (`^[A-Za-z0-9_-]{1,512}$` / 43-char base64url HMAC token) instead of requiring a synthetic `pmt1-` prefix.
