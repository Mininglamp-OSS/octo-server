# Card action callback dispatch operations

This runbook covers the first-party callback branch of `card_action`. External
and third-party Bots continue to consume `/v1/bot/events`; do not migrate those
queues to this worker.

Consumer implementers should start with
[`card-action-callback-consumer.md`](./card-action-callback-consumer.md) for the
wire contract, TypeScript verification example, idempotency, and retry rules.

Standard approve/deny consumers need a signed decide endpoint and one route
entry with `notify_token_env`. That entry authorizes both the callback and an
owner-bound generic `approval_card` ingress. Docs is bound to its specialized
resource-card finalizer; every other registered `(owner, action_type)` uses
octo-server's standard approval terminal card and requester notification. A
custom terminal visual remains a reviewed octo-server extension, not
callback-authored card JSON.

## Required configuration

Routes are static startup configuration. Card data can select only a registered
`(stored sender_uid, owner, action_type)` tuple and never carries a URL.

```bash
export OCTO_CARD_ACTION_ALLOWED_URLS='https://docs.internal/v1/card-actions/decide'
export OCTO_CARD_ACTION_ROUTES='[{"sender_uid":"notification","owner":"docs","action_type":"access_request.decision","url":"https://docs.internal/v1/card-actions/decide","secret_env":"OCTO_DOCS_CARD_ACTION_SECRET","timeout_ms":3000,"max_attempts":5,"base_backoff_ms":1000,"max_backoff_ms":60000,"max_in_flight":8}]'
export OCTO_DOCS_CARD_ACTION_SECRET='<at least 32 random bytes>'
export OCTO_DOCS_NOTIFY_TOKEN='<dedicated docs ingress token>'
export OCTO_DOCS_APPROVAL_CARD_ENABLED=true
export OCTO_CARD_MESSAGE_ENABLED=true
```

Startup fails if a route is malformed, not in the exact HTTPS allowlist, has a
missing/short secret, or if the docs pilot is enabled without its exact route.
Callback redirects and environment HTTP proxies are disabled.

For example, a second service can add a route without adding a finalizer or Bot
worker:

```json
{
  "sender_uid": "notification",
  "owner": "tasks",
  "action_type": "task.decision",
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

Set both referenced values to distinct random secrets of at least 32 bytes and
add the exact callback URL to `OCTO_CARD_ACTION_ALLOWED_URLS`:

```bash
export OCTO_TASKS_CARD_ACTION_SECRET='<at least 32 random bytes>'
export OCTO_TASKS_NOTIFY_TOKEN='<different, at least 32 random bytes>'
```

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
    "action_type": "task.decision",
    "title": "Deploy release",
    "description": "Approve production deployment",
    "data": {"task_id": "task-1"}
  }
}
```

The token supplies the authoritative owner; the request cannot choose it. The
server adds approve/deny actions and reserved metadata. `data` accepts at most
32 lower-case keys with string values up to 500 runes; `owner`, `action_type`,
and `decision` are reserved. No callback URL or card JSON is accepted.

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

For a generic owner, deploy the decide endpoint first, then add the allowlist,
route, callback secret, and notify token in one restart. Verify a test
`approval_card` before switching business traffic. Roll back by stopping new
approval-card calls, draining ready/leased events, and only then removing the
route and secrets.
