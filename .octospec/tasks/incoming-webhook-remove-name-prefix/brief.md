---
type: Task
title: "Task: incoming-webhook-remove-name-prefix"
description: Remove the server-enforced "Webhook-" name prefix on non-admin members' incoming webhooks
tags: ["incomingwebhook", "webhook"]
timestamp: 2026-07-02T00:00:00Z
# --- octospec extension fields ---
slug: incoming-webhook-remove-name-prefix
upstream: none
source: self
---

# Task: incoming-webhook-remove-name-prefix

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal
Non-admin members (regular group members / bots) creating or renaming an
incoming webhook currently have the server force a `Webhook-` prefix onto
their chosen display name (`memberWebhookNamePrefix` in
`modules/incomingwebhook/api.go`). Product wants this forced prefix removed:
members should be able to set any name, same as admins, without the server
rewriting it.

User was explicitly informed this control was added as an anti-impersonation
guard (PR #340 review, yujiawei P1/P2 — prevents a member naming a webhook
"HR Announcement" or a colleague's name to impersonate a real sender in the
group) and confirmed removal anyway.

## Background
- `memberWebhookNamePrefix = "Webhook-"` (api.go) + `prefixedWebhookName()`
  force the prefix onto any non-admin-submitted name in `create()` and
  `update()`.
- `autoWebhookName()` generates the server default name when none is
  submitted (`Webhook-<id suffix>`) — this default-naming behavior is
  unrelated to the *forced* prefix on member-submitted names and stays as-is
  (still a reasonable default label, not a security control by itself).
- `resolveFromIdentity()` at push time ignores `Username`/`AvatarURL`
  override for non-admin-owned webhooks specifically *because* the prefix
  lock existed — comment there says "没有这道闸，管理面的 Webhook- 前缀与头像锁就会被
  push 路径整体绕过". The avatar lock is a separate, still-desired control
  (members cannot set custom avatars at all) — only the *name* override
  block should be reconsidered, and only for what changes once the prefix
  itself no longer exists.
- Frontend `WebhookEditModal.tsx` shows a hint (`memberPrefixHint`) below the
  name field for non-admin users explaining the forced prefix.
- `README.md` documents the forced-prefix behavior in 3 places.
- Tests asserting the forced prefix: `api_member_test.go`,
  `api_test.go`, `richtext_test.go`.

## Load-bearing list
- webhook, trust-boundary (modules/incomingwebhook is in trust-boundary's
  inject_when.paths — reviewed; this change is about display-name policy,
  not content escaping/adapter parity, so trust-boundary's escaping rules
  don't constrain this change, but noting the read).
- wire-contract: `create`/`update` request handling for `name` field no
  longer rewrites non-admin input.

## Out of scope
- Avatar lock for non-admin webhooks (`req.Avatar != ""` → 400 for
  non-admin) — stays. Only the name prefix goes.
- `autoWebhookName()` default naming when no name is submitted — stays
  as the server default label.
- `mention_uids` / broadcast capability bits — untouched.
- octo-web: only the now-inaccurate hint text/comment referencing the forced
  prefix is updated; no behavior change needed since the frontend never
  enforced the prefix itself (it only bound whatever the server returned).

## Acceptance
- Non-admin `create`/`update` with a custom `name` no longer gets
  `Webhook-` prepended; the name is stored/returned as submitted (still
  subject to the existing 64-char length cap and empty→auto-name fallback).
- Push-time `resolveFromIdentity` behavior for non-admin webhooks: still
  ignores `Username`/`AvatarURL` override and uses the stored `m.Name`
  (no regression — arbitrary names are now settable through the *management*
  API only, not through the push endpoint, keeping the push-path override
  block that PR #340 P1 called out).
- Existing tests updated to reflect: non-admin custom names persist
  unprefixed; the "already has prefix → idempotent" and "bare prefix →
  treated as empty" cases are removed since prefixing no longer happens.
- README.md updated to drop the forced-prefix claims.
- octo-web `WebhookEditModal.tsx`: remove the now-inaccurate
  `memberPrefixHint` hint block and its doc comment; i18n keys unused after
  removal are cleaned up if project convention says so (check other unused
  key handling first).
- `go build ./...`, `go vet ./...`, and the incomingwebhook test package pass.
