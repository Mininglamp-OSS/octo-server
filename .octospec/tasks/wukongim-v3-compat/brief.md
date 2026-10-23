# WuKongIM v3 beta compatibility for an isolated companion stack

## Goal

Run the existing Octo Server against unmodified WuKongIM v3.0.0-beta.24
(`4d44b39dda6fdaa7dafb637ca6cfb718e9530f77`) in an isolated test deployment.
Keep the adapter in the Server repository so one draft PR identifies both the
Server build and the compatibility binary used by the deployment.

## Load-bearing paths

- Every existing octo-lib sender, including the independent Space welcome
  sender, receives the legacy send envelope without losing 64-bit message IDs.
- A v3 protocol rejection carried by HTTP 200 must become an unsuccessful
  legacy call. Neither proxying nor batch fanout retries ambiguous writes.
- Only the configured service token authorizes the Core API proxy. Public
  health probes expose no business data.
- Unsigned v3 callbacks are accepted only on a loopback listener in the IM Pod.
  The adapter validates bounded event bodies, restores Person peer IDs, signs
  the resulting bytes, and waits for the existing Server webhook acknowledgement.
- A read-only Manager account supplies the committed maximum sequence through
  an exact channel lookup. Missing or oversized sequence values never become 0.
- Batch sends retain per-recipient outcomes and report partial failure to old
  clients that otherwise only inspect the HTTP status.

These listeners implement private IM service protocols, not user-facing Octo
routes. The API uses the existing `token` service credential and upstream
authorization; the callback listener is restricted to the local network
namespace. User/Space middleware, localized user errors, and user rate limits
remain in the existing Server. Adapter protocol errors do not add user routes.

## Out of scope

- Changes to WuKongIM source, its durable format, or the pinned octo-lib module.
- Replacing or migrating existing live instances or v2 data.
- Durable webhook replay, v2 plugins/streaming, and emulation of `/varz`.
- A capacity or multi-node qualification claim from a single-node smoke test.

## Acceptance

- Focused tests exercise the actual octo-lib sender, IDs above 2^53, permission
  rejection, malformed responses, API authentication, bounded input, signed
  callbacks, Person/group/offline payloads, failed/partial acknowledgements,
  batch partial failure, and exact maximum-sequence lookup failures.
- The full Server and adapter build from the draft PR commit.
- A new companion Pod runs Server/MySQL/Redis/MinIO with fresh credentials and
  storage, and connects to a new unmodified beta.24 IM Pod with the adapter.
- Live smoke checks establish image/process identity, dependency readiness,
  basic send/receive and correct Server callback persistence.
- The PR stays draft and the deployment record states remaining limitations.
