# Permission Server Convergence — ACTIVE v52 (`[S-B1]` Minimal Mainline Repair)

## 1. Governance and Scope

- **Task**: `XIN-2793` (`6a92a547-cb82-4c3d-ac6c-8f5529028ba4`)
- **Charter & Review Baseline**:
  - Charter v52 registration: `32e49395-62f6-4aea-bd5b-4b583797d2a3` on `XIN-2323` (`17e81670-5b49-461d-9509-7c22ca127939`)
  - Independent exact-head code review: `9a49c03b-542a-4223-9ac3-3513fb717752` on `XIN-2834` (`46d23689-0cb4-4d33-b488-8d430ae854c3`)
  - Trigger comment: `57c2870a-f023-44bc-bef4-2ac5d8ca4836` on `XIN-2793`
- **Repository & PR**: `Mininglamp-OSS/octo-server` Draft PR `#951` (`boris-clark:feat/xin-2793-permission-server` -> `chore/mcp-integration-20261103` at `79701d4912eba79a8bcb02c58ed86609c38335e2`), continuing from reviewed head `e708d0235f0d0ecb2b2fd3966db9ccf9e0a8054d`.
- **Authorized Scope**: `[S-B1]` only. Preserve existing `octo-server` native message expiry (`message.expire`) across `BindDeliveredMessageV1`, `InspectDeliveredHeadersV1`, `ListHistoryHeadersV1`, and `IssueHistoryDisclosureV1` at read and revalidation time using `ServerMessageFactProvider`'s testable clock (`p.now()`), never issuing a disclosure permit for an expired message and never misapplying message TTL to replay-journal eviction.

---

## 2. Root-Cause Verification & Minimal Code Repair (`[S-B1]`)

### Source Verification
1. In `octo-server` (`modules/message/db.go:120`, `modules/webhook/sql/20230920000002_webhook_legacy01.sql:5-18`), each sharded `message` table (`message`, `message1..4`) stores `expire integer not null DEFAULT 0` alongside `timestamp`.
2. Existing `octo-server` read paths (`modules/message/api.go:2336-2338`, `3859-3863`) enforce message TTL dynamically at read time via:
   ```go
   if msg.Expire > 0 && time.Now().Unix()-int64(msg.Expire) >= int64(msg.Timestamp)
   ```
   while `message.is_deleted` remains `0` in SQL storage.
3. At `e708d0235f0d0ecb2b2fd3966db9ccf9e0a8054d`, `ServerMessageFactProvider.loadMessage` (`pkg/permissionbundle/server_message_facts.go:265-276`) selected only `message_seq,from_uid,COALESCE(timestamp,0),payload,COALESCE(is_deleted,0)` and omitted `COALESCE(expire,0)`, and `resolveOne` (`:430-432`) checked only `msg.deleted != 0 || extra.revoke != 0 || extra.deleted != 0`. In addition, `ListHistoryHeadersV1` (`:694`) prefixed `"p"` onto the 32-hex `page_id`, which violated `#/$defs/Hex32` when validated through `Runtime.Facts` (`nativeResponse`).

### Code Changes (`pkg/permissionbundle/server_message_facts.go`)
1. Added `expire uint32` and `func (m *serverMsgRow) isExpired(now time.Time) bool` (`return m != nil && m.expire > 0 && now.Unix()-int64(m.expire) >= m.timestamp`) on `serverMsgRow`.
2. Updated `ServerMessageFactProvider.loadMessage` to select `COALESCE(expire,0)` from `p.hooks.MessageTable(r.channelID)` into `&m.expire`.
3. Updated `ServerMessageFactProvider.resolveOne` to check `if msg.deleted != 0 || msg.isExpired(p.now()) || extra.revoke != 0 || extra.deleted != 0 { return un("expired_reference") }`.
4. Because `BindDeliveredMessageV1`, `InspectDeliveredHeadersV1`, `ListHistoryHeadersV1`, and `IssueHistoryDisclosureV1` all resolve through `resolveOne`:
   - `BindDeliveredMessageV1` (`POST /v1/bot/permission-facts/v1/bind-delivery`) refuses expired messages with `status: "refused", reason: "expired_reference", current: false`.
   - `InspectDeliveredHeadersV1` (`POST /v1/bot/permission-facts/v1`, `operation: "resolve"`) returns `status: "unavailable", reason: "expired_reference"`.
   - `ListHistoryHeadersV1` (`POST /v1/bot/permission-facts/v1`, `operation: "history_headers"`) returns `{"reference": "<delivery_ref>", "status": "unavailable", "reason": "expired_reference"}` for expired messages (and formats `page.page_id` as a 32-char lowercase hex string matching `#/$defs/Hex32`).
   - `IssueHistoryDisclosureV1` (`POST /v1/bot/permission-facts/v1`, `operation: "history_disclosure"`) re-evaluates `resolveOne` at step 2 and returns `status: "unavailable", reason: "expired_reference"` with no `permit` (and zero rows inserted into `bot_permission_disclosure_permit`) whenever a message is expired or expires between `history_headers` / `bind_delivery` and `history_disclosure`.

---

## 3. Contract & Sibling ABI Handoff Summary

- **Unchanged Wire Contracts**:
  - All HTTP route paths, authentication guards (`bf_` User Bot token + `ExplicitSpace`), JSON Schema definitions (`pkg/permissionbundle/contracts/permission-native-facts-v1.schema.json`), authority descriptor arrays (`[{"authority_id", "kind", "incarnation", "position", "read_nonce", "consistency", "started_at", "completed_at"}]`), `bind-delivery` boolean predicate map (`proven_predicates`: `map[string]bool`, `unknown_predicates`: `[]string`), decimal-string `context_epoch`, and metadata-only `history_disclosure` (`status: "resolved"` + `permit`) remain unchanged from `handoff-v7.json`.
  - Historical artifacts `handoff-v1.json` through `handoff-v7.json`, `remaining-producers-v1.json` through `remaining-producers-v5.json`, and `contract-manifest-v1.json` through `contract-manifest-v7.json` are preserved byte-for-byte.
- **Updated Behavioral Guarantee in `handoff-v8.json`**:
  - `expired_reference` in `bind_delivery`, `resolve`, `history_headers`, and `history_disclosure` now includes read-time and revalidation-time TTL expiration (`message.expire > 0 && now.Unix() - int64(message.expire) >= message.timestamp`).
  - `history_headers` `page.page_id` is a 32-character lowercase hex digest (`Hex32`) validated end-to-end through `Runtime.Facts`.
- **Sibling Alignment Notes (for `XIN-2795` / PjM)**:
  - `codex-channel-octo` (`createNativeEndpointDriver` in `src/permissions/endpoint-factory.ts`, `[A-B2]`) should consume `pkg/permissionbundle/contracts/handoff-v8.json`:
    1. Parse `authorities` as the 2-element JSON array emitted by `octo-server` (`kind: "im"` and `kind: "sql"` descriptors with `consistency: "ack_before_read_start"`).
    2. Parse `binding.proven_predicates` on `POST /v1/bot/permission-facts/v1/bind-delivery` as a boolean map (`message_version_exact`, `message_author_exact`, `message_source_exact`, `message_recipient_exact`, `caller_current_read`, `bot_current_read`, `bot_current_send`, etc.) and require `message_seq > 0`.
    3. Send `context_epoch` as a decimal string (`U64`) on `history_disclosure`, treat `history_disclosure` as metadata-only (`status: "resolved"` + single-use `permit`), use `operation: "resolve"` for contributor/reply revalidation, and verify all required boolean predicates in `resolved.predicates` are `true` (using `resolved.reference`) before allowing `publish` or `refuse`.
    4. When `bind-delivery` returns `status: "refused", reason: "expired_reference"` or `resolve` / `history_headers` / `history_disclosure` returns `status: "unavailable", reason: "expired_reference"`, fail closed before model admission/context use or reply send.

---

## 4. Focused Red-to-Green Verification & Test Boundaries

- **Red-to-Green Proof (`TestServerMessageFactsMessageExpireTTLBoundaryAndRevalidation`)**:
  - **Before fix (RED)**: `b9003` (`timestamp = 1700000000`, `expire = 100`, `now = 1700000100` at exact TTL boundary) returned `status: "bound"` instead of `"refused"`.
  - **After fix (GREEN)**: Tested through `Runtime.BindDeliveredMessage` and `Runtime.Facts` (`resolve` for `admit`/`contributor`/`reply`, `history_headers`, and `history_disclosure` with durable SQL permit ledger enabled):
    - `9001` (`expire = 0`): `bound` in `bind_delivery`, `resolved` in `resolve` and `history_headers`, and issues valid `permit` in `history_disclosure` at both `t = 1700000100` and `t = 1700000105`.
    - `9002` (`expire = 101`): `bound` / `resolved` / issues `permit` at `t = 1700000100` (`now - expire = 1699999999 < 1700000000`), then transitions to `unavailable` (`reason: "expired_reference"`, zero permit) after clock advances to `t = 1700000105` (`now - expire = 1700000004 > 1700000000`).
    - `9003` (`expire = 100`, at boundary `now - expire == 1700000000`) and `9006` (group `expire = 100`, at boundary): refused in `bind_delivery` (`reason: "expired_reference"`), `unavailable` in `resolve` and `history_headers`, and refused in `history_disclosure` (`reason: "expired_reference"`, `permit == nil`).
    - `9004` (`expire = 50`, past boundary `now - expire = 1700000050 > 1700000000`): refused/unavailable with `"expired_reference"` across all 4 operations, zero body leak, zero permit issued.
    - `9005` (`expire = 105`, revalidation case): bound and listed in `history_headers` while unexpired at `t = 1700000100`, then clock advances to exact boundary `t = 1700000105` before `resolve` and `history_disclosure` — revalidation refuses with `status: "unavailable", reason: "expired_reference"` and writes zero permit rows into `bot_permission_disclosure_permit`.
- **Component Scope Classification**:
  - `PASS` (synthetic isolated local author tests): `pkg/permissionbundle`, `modules/bot_api`, `modules/robot`, and root startup composition/token-registry tests.
  - `NOT_RUN` (by governance): live MySQL/Redis/WuKongIM deployment, cross-repo live 3-process execution, and independent re-review/acceptance gates.
