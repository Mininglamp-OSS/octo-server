# MCP Bot policy contract

XIN-2692 implements approved design v2, SHA-256
`d2405ece492b3faa96f974dfbb5cb9d18dbf35fc629e312eda7e350887b4aee7`.
All examples use synthetic identifiers. Successful responses use HTTP 200 and
`Cache-Control: private, no-store`.

## Identity verification

`POST /v1/auth/verify-bot` keeps the existing Bot token validation and network
ACL. It does not register identities. An omitted or false `include_mcp_policy`
leaves the five identity response fields unchanged and never accesses either
policy table. Null, non-Boolean and duplicate opt-in controls are rejected.

Synthetic request:

```json
{"bot_token":"synthetic-test-token","include_mcp_policy":true}
```

Explicit compatible exception under a strict stage default:

```json
{
  "bot_uid":"bot_B",
  "bot_name":"Example Bot",
  "owner_uid":"owner_U",
  "owner_name":"Example Owner",
  "space_id":"space_S",
  "mcp_policy":{
    "version":1,
    "context_mode":"compatible",
    "override":"compatible",
    "stage_default":"strict",
    "source":"bot",
    "generation":"7",
    "bot_revision":"6"
  }
}
```

Without an explicit override, `mcp_policy` is:

```json
{
  "version":1,
  "context_mode":"strict",
  "override":null,
  "stage_default":"strict",
  "source":"stage_default",
  "generation":"7",
  "bot_revision":null
}
```

`version` is an integer. Generations and revisions are canonical positive
decimal **strings**, at most `18446744073709551615`. A Bot revision cannot
exceed the snapshot generation. Null override/revision fields are present.
There is no code-default source or automatic debug fallback. A consumer of
the opt-in contract must reject older servers that omit policy entirely.

## Management

These endpoints use existing human login sessions (`token` header), shared
UID rate limiting, and creator/superadmin guards. They are not MCP tools.
An `Authorization: Bearer ...` Bot credential alone cannot authenticate here.

| Endpoint | Permission | Request | Response |
| --- | --- | --- | --- |
| GET `/v1/robot/bot_B/mcp-policy` | Active Bot's creator | None | Policy object above plus `bot_uid` |
| PUT same | Creator, checked again inside transaction | `{"context_mode":"compatible","if_generation":"7"}` | Committed policy plus `bot_uid` |
| DELETE same | Creator, checked again inside transaction | `{"if_generation":"8"}` | Committed policy, override/revision null |
| GET `/v1/manager/mcp-policy/default` | Superadmin | None | `{"version":1,"context_mode":"strict","generation":"9"}` |
| PUT same | Superadmin | `{"context_mode":"strict","if_generation":"8"}` | Committed default object |

Bot resources have no superadmin ownership exception. Route parameters must
match the actual robot ID byte for byte; Gin uses the existing `:robot_id`
wildcard name. A resource ID never changes the authenticated actor.

Mutation JSON is capped at 2048 bytes. Unknown/duplicate keys (including
escaped duplicates), nulls, wrong types, mode case/whitespace variants, missing
fields, zero/leading-zero/overflow generations and trailing JSON are rejected.
DELETE requires its explicit JSON body; the default resource has no DELETE.

## Failures

New errors retain the existing fixed-400 i18n envelope. Match `error.code` and
`error.http_status`; transport status alone does not distinguish a conflict
from a storage outage. Messages are localized and are not machine identifiers.

| Failure | Wire HTTP | `error.http_status` | `error.code` |
| --- | --- | --- | --- |
| Invalid management JSON | 400 | 400 | `err.server.mcp.policy_invalid` |
| Stale generation | 400 | 409 | `err.server.mcp.policy_conflict` |
| Missing/corrupt default, invalid override, DB/timeout/commit failure | 400 | 503 | `err.server.mcp.policy_unavailable` |
| Missing/disabled Bot or absent creator | 404 | 404 | `err.server.robot.not_found` |
| Noncreator, including another superadmin | 403 | 403 | `err.server.robot.creator_only` |

Default-admin denial uses existing `err.shared.auth.forbidden` semantics.
Login failures and rate limiting retain existing middleware behavior. Invalid
Bot verification tokens retain the existing generic 401 response.

English unavailable specimen:

```json
{
  "status":400,
  "msg":"Internal server error.",
  "error":{
    "code":"err.server.mcp.policy_unavailable",
    "message":"Internal server error.",
    "details":{},
    "http_status":503
  }
}
```

English CAS-conflict specimen:

```json
{
  "status":400,
  "msg":"MCP policy changed. Read the current generation and try again.",
  "error":{
    "code":"err.server.mcp.policy_conflict",
    "message":"MCP policy changed. Read the current generation and try again.",
    "details":{},
    "http_status":409
  }
}
```

Policy fault logs use a fixed category without driver errors, SQL parameters,
credentials, request identifiers or policy values.

## Storage and rollout constraints

The existing robot migration loader embeds the two dedicated InnoDB tables.
MySQL >= 8.0.16 with enforced CHECK constraints and strict SQL mode is required.
Before any separately approved deployment, verify actual table definitions,
constraints, indexes and seed; `IF NOT EXISTS` does not certify arbitrary
preexisting tables. A partial/incompatible migration is unready, and runtime
requests fail closed rather than creating tables or repairing the seed.

Reads use one context-bound, repeatable-read, read-only transaction for default
then override. Writes lock singleton → active robot (Bot writes) → override,
check global generation, and commit atomically within a two-second budget.
Every effective change increments generation; Bot revisions use that new value.
An explicit compatible value is stored even when it equals the stage default.
Repeated PUT of the same explicit value and DELETE of an absent override check
CAS and permissions without incrementing generation. No cache, automatic retry,
broadcast, scheduler or activation date is introduced. An uncertain commit
requires GET before deciding whether to submit another write.

Down succeeds only for the pristine seed without overrides. Any prior edit,
including creating then deleting an override, prevents destructive Down. Retain
tables when rolling back applications; destructive data migration requires
separate approval. Synthetic tests do not establish live environment readiness,
CLI/server integration, independent acceptance, or actual IM end-to-end behavior.

## Reproducing self-tests

Run `bash ci/test-mcp-policy.sh` for service-free mock/contract/race tests.
Set `MCP_POLICY_TEST_MYSQL_DSN` to a disposable MySQL 8 instance to also run
the migration, CHECK, CAS concurrency, snapshot, timeout and owner-handler
tests. Those tests create and drop only fresh `mcp_policy_test_*` databases.
The focused user mock tests compile all production user files while excluding
its existing service-dependent `TestMain`; full E2E remains in the regular lane.
