# Bot token single-instance binding

## Goal

Prevent one Bot API token from registering more than one OpenClaw installation.
The first client that supplies an `instance_id` owns that token permanently;
re-registration by the same instance is idempotent, while a different or legacy
instance receives an explicit HTTP 409 conflict.

## Load-bearing behavior

- Binding acquisition is atomic across octo-server replicas.
- Raw Bot API tokens never appear in binding keys or diagnostic details.
- A bound token returns credentials only to its owning `instance_id`.
- `instance_id` is a persisted UUID v4 shared by all Bot accounts in one
  OpenClaw installation.
- Existing clients without `instance_id` remain compatible until a new client
  claims the token; after that, omission is a conflict rather than a bypass.
- `force_refresh=true` does not bypass ownership.
- User-facing failures use the localized error envelope and a real HTTP 409.
- The IM credential issued for a newly bound token is distinct from the Bot API
  token, so a rejected legacy client cannot reconnect directly with the bearer
  token after a binding is established.
- Startup restoration treats the binding table as authoritative and re-checks
  after each external token update, so a concurrent first claim cannot be
  overwritten by a stale Bot API token.
- A legacy registration re-checks ownership after its external token update;
  if a first claim raced it, the bound credential is restored and the legacy
  request receives HTTP 409.

## Out of scope

- Changing WuKongIM's Master connection replacement semantics.
- Time-based leases or automatic ownership expiry.
- Adding a new unbind endpoint. Rotating the Bot Token creates a fresh binding
  identity and is the explicit recovery/transfer mechanism.

## Acceptance

- The first non-empty `instance_id` atomically creates a binding.
- The same `instance_id` can register repeatedly and receives the same IM token.
- Once claimed, a different or missing `instance_id` receives HTTP 409 with
  `err.server.bot_api.instance_conflict`.
- Legacy registration remains unchanged while no binding exists.
- User Bot and App Bot registration share the same binding behavior.
- Focused unit tests cover validation, idempotency, conflict, legacy transition,
  startup reconciliation, the legacy/claim race repair, and the migration's
  uniqueness constraints.

## Rollout

- Release and upgrade `openclaw-channel-octo` first. Older servers safely ignore
  the new `instance_id` request field.
- Deploy this server change only after supported clients persist and send an
  installation ID. During a multi-replica rollout, old and new registration
  logic must not remain active together because an old replica can overwrite
  the binding-scoped IM token with the Bot API token.
- Existing legacy clients can register only until a modern client claims the
  token. After the claim, requests without the owning `instance_id` receive 409.
