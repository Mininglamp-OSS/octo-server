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

The capability is controlled by the hot-reloaded `system_setting` keys
`message_reaction.read` and `message_reaction.write`.

## Load-bearing list

- `message_reaction.read` and `message_reaction.write` are the configuration source.
- The unset defaults are `read=true` and `write=false`.
- Read and write can be changed independently for staged rollout.
- Both the full appconfig response and the `version` short-circuit response carry
  the nested capability so an operator toggle is not hidden by client caching.

## Out of scope

- Bot-specific capability/profile responses.
- Bot identity detection or Bot write rejection.
- Server-side reaction read/write enforcement for the global switch.
- Web/iOS/Android client changes.

## Acceptance

- With no DB override, appconfig returns `read=true, write=false`.
- DB overrides for `message_reaction.read/write` are reflected independently.
- The version short-circuit response returns the same current capability.
- The setting is listed and writable through the existing system-setting manager
  surface without a schema migration.
