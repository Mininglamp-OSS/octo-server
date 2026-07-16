# Card action callback dispatch operations

This runbook covers the first-party callback branch of `card_action`. External
and third-party Bots continue to consume `/v1/bot/events`; do not migrate those
queues to this worker.

Consumer implementers should start with
[`card-action-callback-consumer.md`](./card-action-callback-consumer.md) for the
wire contract, TypeScript verification example, idempotency, and retry rules.

Standard action consumers need a signed decide endpoint and one route entry
with `notify_token_env`. That entry authorizes both the callback and an
owner-bound generic `approval_card` ingress. The card may use the compatible
localized approve/deny defaults or 1-5 bounded custom actions. Docs is bound to
its specialized resource-card finalizer; every other registered `(owner,
action_type)` uses octo-server's standard terminal card and requester
notification. A custom terminal visual remains a reviewed octo-server
extension, not callback-authored card JSON.

## Required configuration

Routes are static startup configuration. Card data can select only a registered
`(stored sender_uid, owner, action_type)` tuple and never carries a URL.

For example, a new service can add a route without adding a finalizer or Bot
worker:

```json
{
  "sender_uid": "notification",
  "owner": "tasks",
  "action_type": "task.execute.decision",
  "url": "https://tasks.internal/v1/card-actions/decide",
  "secret_env": "OCTO_TASKS_CARD_ACTION_SECRET",
  "notify_token_env": "OCTO_TASKS_NOTIFY_TOKEN",
  "timeout_ms": 3000,
  "max_attempts": 5,
  "base_backoff_ms": 1000,
  "max_backoff_ms": 60000,
  "max_in_flight": 8
}
```

Route fields:

| Field | Requirement |
| --- | --- |
| `sender_uid` | Use `notification` when `notify_token_env` enables the generic ingress |
| `owner` | Stable lowercase service owner, `[a-z][a-z0-9-]{0,63}` |
| `action_type` | Stable action contract, `[a-z][a-z0-9_.-]{0,127}` |
| `url` | Exact absolute callback URL under the transport policy below; this list is itself the allowlist |
| `secret_env` | Environment variable containing at least 32 bytes |
| `notify_token_env` | Optional; enables `approval_card`, also at least 32 bytes; it may be shared by action types of the same owner but must differ from callback secrets and other owners' or legacy/docs notify tokens |
| `timeout_ms` | Default 3000; allowed 100-10000 |
| `max_attempts` | Default 5; allowed 1-10 |
| `base_backoff_ms` | Default 1000; allowed 100-60000 |
| `max_backoff_ms` | Default 60000; between base backoff and 600000 |
| `max_in_flight` | Default 8; allowed 1-100, enforced per route per octo-server process |

Total callback concurrency scales with the number of octo-server replicas;
size the consumer and its downstream pool accordingly.

Set both referenced values to distinct random secrets of at least 32 bytes:

```bash
export OCTO_CARD_ACTION_ROUTES='[{"sender_uid":"notification","owner":"tasks","action_type":"task.execute.decision","url":"https://tasks.internal/v1/card-actions/decide","secret_env":"OCTO_TASKS_CARD_ACTION_SECRET","notify_token_env":"OCTO_TASKS_NOTIFY_TOKEN","timeout_ms":3000,"max_attempts":5,"base_backoff_ms":1000,"max_backoff_ms":60000,"max_in_flight":8}]'
export OCTO_TASKS_CARD_ACTION_SECRET='<at least 32 random bytes>'
export OCTO_TASKS_NOTIFY_TOKEN='<different, at least 32 random bytes>'
export OCTO_CARD_MESSAGE_ENABLED=true
```

`OCTO_CARD_ACTION_ROUTES` is a single JSON array containing every route and is
itself the exact URL allowlist. Startup fails if a route is malformed or has a
missing/short secret or an invalid cross-capability token reuse.

`OCTO_CARD_ACTION_ALLOWED_URLS` is deprecated and no longer consulted. If it is
still present in a rolling upgrade, octo-server logs a single structured
deprecation WARN at startup and continues; remove it from the ConfigMap when
convenient.

### Callback URL transport policy

- Both `https://` and `http://` are accepted. The scheme chosen in each route
  is the operator's explicit authorization for that destination.
- URLs must be absolute, contain a host, and carry no user credentials, query,
  fragment, or opaque form. Redirects and environment HTTP proxies remain
  disabled for both schemes.
- Hostname shape (Kubernetes Service DNS, docker service name,
  `host.docker.internal`, IP literal, or `localhost`) is not inspected;
  reachability is a network-layer concern, not a URL-parser concern.

Representative destinations for HTTP:

```text
http://smart-summary.dmwork-test.svc:8080/v1/card-actions/decide
http://smart-summary:8080/v1/card-actions/decide
http://host.docker.internal:8080/v1/card-actions/decide
http://127.0.0.1:8080/v1/card-actions/decide
```

HMAC authenticates the request but does not encrypt it, and callback responses
are not separately signed. Callback bodies may contain the operator UID, Space
ID, and business identifiers. When plain HTTP is used, restrict consumer
ingress and octo-server egress with cluster NetworkPolicy or a service mesh
that encrypts pod traffic; prefer HTTPS for anything that crosses a cluster,
a network zone, or a public boundary.

`notify_token_env` is optional for callback-only routes whose initial card is
produced elsewhere. When present, it dynamically installs an `octo/v2`,
DM-only producer bound to the route owner and the shared `notification` sender.
The token must differ from the callback secret, `NOTIFY_INTERNAL_TOKEN`, every
other owner token, and `OCTO_DOCS_NOTIFY_TOKEN`. `OCTO_CARD_MESSAGE_ENABLED`
must be `true`.

The service creates the standard initial card through the existing notify API:

```http
POST /v1/internal/notify
X-Internal-Token: <OCTO_TASKS_NOTIFY_TOKEN>
Content-Type: application/json

{
  "space_id": "space-1",
  "service": "tasks",
  "targets": ["approver-b"],
  "actor_uid": "requester-a",
  "approval_card": {
    "action_type": "task.execute.decision",
    "title": "Execute task",
    "description": "Choose how to handle this task",
    "data": {"task_id": "task-1"},
    "actions": [
      {"decision": "execute", "title": "Execute"},
      {"decision": "reject", "title": "Reject"},
      {"decision": "cancel", "title": "Cancel"}
    ]
  }
}
```

`space_id`, `service`, and `targets` are required. `targets` contains 1-200 user
IDs and is de-duplicated; `actor_uid`, when present, is excluded from delivery.
The token-bound owner is authoritative, not the caller-supplied `service`
label. Generic action cards are DM-only and every target must be a current
member of the Space. The batch notify endpoint does not accept
`approval_card`.

A successful request returns the actual delivery result:

```json
{"delivered":["approver-b"],"filtered":{}}
```

An HTTP 200 therefore does not imply every requested target received the card;
callers must inspect both `delivered` and `filtered`.

The token supplies the authoritative owner; the request cannot choose it. The
server adds reserved metadata and builds every action. `actions` is optional:
omitting the field (or sending explicit JSON `null` — equivalent on the wire)
preserves the existing localized approve/deny buttons. Sending `"actions": []`
(explicit empty array) is a caller bug and rejected — the fallback path is
nil, not an empty slice. When present, it must contain 1-5 items. Each
`decision` is unique and matches `[a-z][a-z0-9_.-]{0,47}`; the tokens
`approve` and `deny` are reserved for the legacy template and rejected as
custom decisions. Each trimmed `title` is 1-80 Unicode code points and cannot
contain control characters (tabs, newlines, BEL, etc.). octo-server derives
the action ID as `approval-<decision>` and the callback receives that decision
unchanged. `data` accepts at most 32 lower-case keys with string values up to
500 runes; `owner`, `action_type`, and `decision` are reserved. No per-action
data, URL, input, style, or arbitrary card JSON is accepted.

Custom decisions do not add callback result states. The consumer still returns
one authoritative `state`: `approved`, `denied`, `cancelled`, or `pending`.
Every valid typed response finalizes the current card; `pending` does not keep
the buttons interactive. Use a custom finalizer only when the standard terminal
status cannot represent the business outcome.

The callback receives `X-Octo-Timestamp`, `X-Octo-Event-ID`, and
`X-Octo-Signature: v1=<hex HMAC-SHA256>`. The canonical signed bytes are:

```text
v1\nMETHOD\nPATH\nTIMESTAMP\nEVENT_ID\nSHA256(EXACT_BODY)
```

The consumer must reject stale timestamps, verify HMAC before decoding business
fields, persist idempotency by `event_id`, re-check its current ACL, CAS the
domain request, and replay the same typed response for a repeated event.

## Queue and alerts

The worker uses Redis ready/leased/DLQ ZSETs with token-bound Lua transitions.
Finalization heartbeats extend only the matching lease token. Expired leases
are reclaimed; a stale worker cannot renew or ACK another worker's lease.
If a route has reached `max_in_flight`, its lease is atomically deferred for one
poll interval without consuming an attempt, so other routes and shutdown remain
unblocked.
Live retention equals `Robot.MessageExpire`; DLQ retention is 30 days.

Alert from these bounded-label metrics:

- `dmwork_card_action_dispatch_error_total{owner,category}`
- `dmwork_card_action_dispatch_retry_total{owner}`
- `dmwork_card_action_dispatch_duration_seconds{owner,result}`
- `dmwork_card_action_dispatch_leased{owner}`
- `dmwork_card_action_dispatch_ready_depth`
- `dmwork_card_action_dispatch_dlq_depth`
- `dmwork_card_action_dispatch_applicant_notify_failure_total{owner}`

Deployment-specific thresholds belong in the monitoring repository. At
minimum, alert on sustained DLQ depth above zero and sustained `consumer_5xx`,
`invalid_response`, or applicant notification failures.

## Manual DLQ replay

First inspect logs/metrics and fix the consumer or configuration. Record the
exact `event_id`; there is deliberately no bulk or automatic replay.

```bash
go run ./tools/card-action-dlq -config configs/tsdd.yaml -action depth
go run ./tools/card-action-dlq -config configs/tsdd.yaml -action replay -event-id 12345
```

Replay resets attempts and returns that one event to ready state. The consumer
must remain idempotent: its domain decision may already have committed. Terminal
card mutation is also idempotent (`card_seq=event_id`). Applicant notification
is at-least-once, so replay may duplicate it at the documented crash boundary.

## Rollout and rollback

Deploy the signed docs decide endpoint first. Configure its secret/route while
keeping `OCTO_DOCS_APPROVAL_CARD_ENABLED=false`; verify metrics, then enable the
pilot. Roll back by disabling the pilot, which restores the existing `octo/v1`
display card. Keep routes until ready/leased queues drain; removing a live route
sends affected events to DLQ rather than to the Bot pull queue.

For a generic owner, deploy the decide endpoint first, then add the route,
callback secret, and notify token in one restart. Verify every configured
action on a test `approval_card` before switching business traffic. Roll back
by stopping new approval-card calls, draining ready/leased events, and only then
removing the route and secrets.
