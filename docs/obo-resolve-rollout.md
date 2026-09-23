# Bot Resolve / generic OBO rollout

Deploy the `bot_api` migration `20260921000001_obo_generic_scope.sql` before
serving the new endpoints. It adds a separate generic Scope binding table;
historical Channel grants do **not** gain `ALL` automatically. The old
`/v1/auth/verify-bot` response contract remains unchanged; credential
comparison is now byte-exact, so case-variant tokens that previously matched
under a case-insensitive database collation no longer authenticate. Channel
OBO also honors a Grant's `expires_at` when that field is set by the new
management API.

`POST /v1/auth/resolve` accepts the same `bf_` Bot credential as the legacy
Bot verifier; no additional service token is configured or sent. Its OBO
Action is checked against the server's Action registry. Built-in Project
Actions are registered by code; additional downstream Actions can be
configured with `OCTO_OBO_ACTION_SCOPES_JSON`:

```json
{
  "task.read": ["ALL"]
}
```

The Action registry is loaded at startup; invalid values fail startup. It
does **not** authenticate which downstream service submitted an Action. The
calling backend must derive Action from a controlled route, never accept a
Bot/CLI-provided Action. Adding an Action that accepts `ALL` expands what
existing `ALL` grants may delegate, so review and test each addition before
deployment. If service-level Action isolation becomes necessary, add a trusted
caller identity mechanism rather than reintroducing a token without an
explicit deployment decision.

Both `AS_BOT` and `OBO` requests must include the target `space_id`. `OBO`
also requires a registered Action, an active and globally enabled
owner-to-Bot Grant, and an explicit `ALL` binding. The Resolve result chooses Actor/Subject; the
caller still checks the Subject's current business permissions. The new Bot
Project routes do that check in the existing Project read service. The status
endpoint `/v1/bot/obo-grants/self` is advisory and must not be used as an
authorization decision. Resolve decisions are not cached in this release.

## Release gates still open

- The new management routes are Grantor-scoped. The document's cross-user
  "controlled Admin" role needs an explicit authorization policy before it can
  be enabled safely.
- `expires_at` uses UTC DATETIME. The one-shot delegation PUT may set it with
  an RFC3339 timestamp or clear it with JSON `null`; omission preserves the
  current deadline. Existing rows default to `NULL`, which means the Grant
  never expires. The deadline also stops legacy Channel sends and fan-out
  because both paths share the Grant; the
  legacy Grant PUT has not been extended with this field.
- The one-shot PUT requires `scope_codes`: `["ALL"]` binds the currently
  supported generic Scope, while `[]` explicitly unbinds it. Omitted or
  `null` `scope_codes` is rejected. An active PUT idempotently reauthorizes a
  revoked Grant and clears its legacy Channel `persona_prompt`; a disable-only
  PUT retains both the revocation timestamp and prompt.
- `GET /v1/obo/grants/:id/audits` currently returns transactional generic
  Grant/`ALL`-binding policy records. Legacy Channel `obo_scopes` changes are
  intentionally outside this timeline and do not change the generic
  `policy_version`; OBO decision records are emitted as structured
  Actor/Subject logs, not stored in this management table.
- Generic management updates the same Grant that controls legacy Channel OBO.
  Channel cache invalidation remains best-effort after commit; the document's
  `policy_postprocess_pending` response is not implemented for that legacy
  cache path. The Web management UI must warn about shared-state effects.
- Run the database-backed server integration suite and a staging migration /
  rollback rehearsal before production rollout. Local compilation and SQL-mock
  tests do not substitute for those checks.
