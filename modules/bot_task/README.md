# Generic Bot Task ingress

`POST /v1/internal/bot-tasks` accepts source-authenticated work for an active
User Bot and enqueues the fixed `bot_task` event consumed by Octo channel
plugins. The server treats `source`, `task_type`, `prompt`, `context`, and
`metadata` as business-neutral transport data.

Configure sources with `OCTO_BOT_TASK_SOURCES`:

```json
{
  "loop": {
    "token": "replace-with-at-least-32-bytes",
    "enabled": true,
    "allowed_bot_uids": ["bot-uid"]
  }
}
```

Each source must have a unique token of at least 32 bytes. An empty or invalid
registry disables ingress. `allowed_bot_uids` should list explicit Bot IDs;
`"*"` permits that source to address every active Bot across all Spaces and
should only be used in controlled development environments.

The endpoint is protected by a strict per-IP rate limit. Defaults are 20
requests/second with a burst of 60 and can be changed with
`DM_BOT_TASK_IP_RPS` and `DM_BOT_TASK_IP_BURST`.
