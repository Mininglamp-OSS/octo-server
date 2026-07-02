---
type: Journal
title: "Journal: incoming-webhook-remove-name-prefix"
description: Removed the server-enforced "Webhook-" name prefix on non-admin members' incoming webhooks.
tags: ["incomingwebhook", "webhook"]
timestamp: 2026-07-02T00:00:00Z
# --- octospec extension fields ---
task: incoming-webhook-remove-name-prefix
upstream: none
source: self
---
# Journal: incoming-webhook-remove-name-prefix

## What was done
Removed the anti-impersonation `Webhook-` prefix that was force-prepended to
non-admin (member/bot) submitted incoming-webhook display names. Originated
from PR #340 review (yujiawei P1/P2): the prefix stopped a group member from
naming their webhook "HR 公告" or a colleague's name to impersonate a real
sender. Product explicitly asked to remove it after being shown the tradeoff.

1. **`modules/incomingwebhook/api.go`** — deleted `memberWebhookNamePrefix`
   const + `prefixedWebhookName()` helper and their call sites in `create()`
   and `update()`. Renamed the remaining default-name constant to
   `autoWebhookNamePrefix` (still used only by `autoWebhookName()` to build
   the server-generated default label `Webhook-<id suffix>` when no name is
   submitted — that default-naming behavior is unrelated to the removed
   *forced* prefix and was left as-is). Updated the `resolveFromIdentity`
   doc comment: the push-path override block for non-admin webhooks
   (`allowOverride=false`) is **kept** — it now protects the
   authenticated/audited `create`/`update` endpoints' configured Name/Avatar
   from being bypassed per-push, independent of whether that Name happens to
   carry a prefix.
2. **Tests** — `api_member_test.go`: custom names now assert stored verbatim
   (no prefix), dropped the "already-prefixed is idempotent" /
   "bare-prefix-treated-as-empty" cases since prefixing no longer happens;
   a literal `"Webhook-"` name is now just a normal string. `richtext_test.go`:
   updated `TestResolveFromIdentity`'s locked-webhook sample name to a
   non-prefixed string to make clear the guard doesn't depend on prefixing.
3. **`README.md`** — dropped the three claims that names are forced to carry
   `Webhook-`; clarified members/admins are now the same on naming, only the
   avatar lock and push-time override block remain member-specific.
4. **octo-web** (`Mininglamp-OSS/octo-web`, same branch) —
   `WebhookEditModal.tsx`: removed the `memberPrefixHint` hint block (shown
   under the name field for non-admins) and its doc comment; deleted the
   now-unused `channelWebhook.form.memberPrefixHint` key from both
   `en-US.json` and `zh-CN.json`. No behavior change needed beyond that — the
   frontend never enforced the prefix itself, it only echoed the value the
   server returned.

## What stayed (out of scope, confirmed intentional)
- Avatar lock for non-admin webhooks (still 400 on `avatar` in
  create/update).
- `autoWebhookName()` default naming (`Webhook-xxxxxx`) when no name is
  submitted.
- Push-time `Username`/`AvatarURL` override still ignored for non-admin
  webhooks (`resolveFromIdentity`, `allowOverride=false`) — this is a
  separate control (per-push override vs. authenticated management-endpoint
  configuration) and was kept per the brief's acceptance criteria.

## Verification
- `go build ./...`, `go vet ./modules/incomingwebhook/...`,
  `golangci-lint run ./modules/incomingwebhook/...` all clean.
- Could not execute the incomingwebhook test suite (`go test`) — this
  sandbox has no MySQL/Redis running, and repo tests require them per
  CLAUDE.md. Test-file edits were reviewed by hand instead; recommend
  running `go test ./modules/incomingwebhook/...` in CI/local before merge.
- octo-web: could not run `pnpm test`/`pnpm lint` — `pnpm install` failed in
  this sandbox (configured `registry.npmmirror.com` returned 403 through the
  sandbox's proxy, and overriding the registry was blocked by the
  environment's auto-mode classifier as an unauthorized registry bypass).
  Verified the diff by hand (JSX/JSON only, no logic change) and validated
  both edited locale JSON files parse. Recommend running `pnpm lint` +
  `cd apps/web && pnpm test` before merge.

## Review follow-up (same branch, second commit)
An adversarial review pass over the first commit found:
- **P1**: `modules/bot_api/incoming_webhook_test.go` mounts the *same*
  `create()` handler via `MountManagementRoutes` and still asserted
  `"Webhook-ci-bot-wh"` — would have failed CI deterministically. Fixed to
  assert the verbatim name. The initial grep inventory only swept
  `modules/incomingwebhook/`; the handler is mounted cross-module.
- **P2**: stale "前缀/头像限制" comment at the `cachedCreatorMembership`
  call in the push handler — updated.
- **nit**: stale `display_locked` term in an `api_test.go` comment — updated.

## Learning
When a handler is mounted by more than one module (here: incomingwebhook
management routes re-mounted under `/v1/bot/...` by bot_api), sweep for
behavior-pinning tests by grepping the *behavior* (the literal asserted
value, e.g. `"Webhook-"`) across the whole repo, not just the owning
module's directory.
