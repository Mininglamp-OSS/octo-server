# Generic Bot Task ingress

`POST /v1/internal/bot-tasks` accepts source-authenticated work for an active
User Bot and enqueues the fixed `bot_task` event consumed by Octo channel
plugins. The server treats `source`, `task_type`, `prompt`, `context`, and
`metadata` as business-neutral transport data.

Configure sources with `OCTO_BOT_TASK_SOURCES`:

```json
{
  "loop": {
    "token": "<redacted>",
    "enabled": true,
    "allowed_bot_uids": ["bot-uid"]
  }
}
```

Each source must have a unique token of at least 32 bytes. An empty or invalid
registry disables ingress. `allowed_bot_uids` should list explicit Bot IDs;
`"*"` permits that source to address every active Bot across all Spaces and
should only be used in controlled development environments.

The endpoint authenticates the bearer token before reading the request body.
The authenticated source must match the body `source`; one source cannot spend
another source's quota or submit tasks under its name.

Two rate-limit layers protect the endpoint:

- A strict per-IP floor defaults to 100 requests/second with a burst of 200.
  Configure it with `DM_BOT_TASK_IP_RPS` and `DM_BOT_TASK_IP_BURST`.
- A Redis-backed per-source quota defaults to 20 requests/second with a burst
  of 60. Configure it with `DM_BOT_TASK_SOURCE_RPS` and
  `DM_BOT_TASK_SOURCE_BURST`.

Each process also keeps the same per-source token bucket locally. It remains
active when Redis is unavailable, so a Redis outage degrades cluster-wide
coordination without making the authenticated ingress unbounded.
