---
type: Task
title: "Task: message-reaction-appconfig"
description: Expose the global message reaction capability through appconfig, backed by system_setting.
tags: ["message", "reaction", "appconfig", "wire-contract", "testing"]
timestamp: 2026-07-22T00:00:00Z
slug: message-reaction-appconfig
source: user
---

# Task: message-reaction-appconfig

## Goal

Expose the ordinary Web/iOS/Android client's global message-reaction capability
through `GET /v1/common/appconfig` as:

```json
{"message_reaction":{"read":true,"write":true}}
```

The capability is controlled by the hot-reloaded `system_setting` key
`message_reaction.enabled`.

## Load-bearing list

- `message_reaction.enabled` is the single configuration source.
- The unset default is enabled to preserve the currently deployed user behavior.
- Enabled maps atomically to `read=true, write=true`; disabled maps to both false.
- Both the full appconfig response and the `version` short-circuit response carry
  the nested capability so an operator toggle is not hidden by client caching.

## Out of scope

- Bot-specific capability/profile responses.
- Bot identity detection or Bot write rejection.
- Server-side reaction read/write enforcement for the global switch.
- Web/iOS/Android client changes.

## Acceptance

- With no DB override, appconfig returns `read=true, write=true`.
- With `message_reaction.enabled=false`, appconfig returns both values false.
- The version short-circuit response returns the same current capability.
- The setting is listed and writable through the existing system-setting manager
  surface without a schema migration.

