---
type: Journal
title: "Journal: cloud-clone-per-user-limit"
description: Enforce one active Octo-hosted AI cloud clone per owning user, independently of Space and client process.
tags: ["botfather", "cloud-clone", "space", "concurrency", "wire-contract"]
timestamp: 2026-09-09T19:40:00+08:00
# --- octospec extension fields ---
task: cloud-clone-per-user-limit
upstream: user-request
source: user
---

# Journal: cloud-clone-per-user-limit

## Result

- `POST /v1/user/bots` accepts the server-owned `agent_hosting=octo_hosted`
  creation intent. Only one active hosted Bot is permitted per authenticated
  owner across every Space.
- `GET /v1/user/bots/hosted` is owner-only and returns the credential required
  to resume the existing cloud clone without exposing its Space membership.
- EVA now looks up the clone before creating it, uses the fixed display name
  `<display name>的 AI 分身`, and treats a concurrent conflict as a lookup-and-
  reuse result.

## Load-bearing decisions

- The lock anchor is the authoritative creator `user` row, rather than a Space
  row or a possibly absent robot row. The check and initial robot persistence
  therefore share one transaction and serialize an empty result set too.
- The quota query includes only active User Bots explicitly created with
  `octo_hosted`; ordinary/local Bots, App Bots, third-party hosting values and
  inactive historical records do not occupy the slot.
- The conflict response is detail-free. It tells an owner that its global slot
  exists without leaking the existing Bot identity or Space.

## Verification

- CUA created and then re-fetched the same hosted clone through EVA against the
  local Octo bridge; the returned access information was available in both
  paths.
- `go test ./modules/botfather -run '^$'`, `make i18n-extract-check`, and
  `git diff --check` passed.
- The full `go test ./modules/botfather/...` run is blocked before tests by the
  shared local `test` database containing migration `20201222000001_report_legacy01.sql`,
  which is not present in this worktree. It was not modified for this task.
