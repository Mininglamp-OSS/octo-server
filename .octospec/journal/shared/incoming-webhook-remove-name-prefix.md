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

## Review follow-up 2 (GitHub PR #526, automated review round 1)
`code-reviewer`/`qa-engineer` flagged (P3, non-blocking) that the 64-byte
name cap on the member path — a boundary this PR's diff directly touches in
both `create()` and `update()` — had no test coverage, and asked for a
unicode/emoji verbatim-name case to confirm the byte-vs-rune distinction
doesn't silently mangle multi-byte names now that the forced-prefix
rewrite is gone. Added `TestMemberCreate_NameByteBoundaries` to
`api_member_test.go`: a 19-byte CJK+emoji name stored verbatim, and a
65-byte ASCII name rejected with 400 on **both** create and update
(independent checks, since this PR reshaped both call sites to the same
form). `go vet` + `golangci-lint` clean; could not run `go test` in this
sandbox (no MySQL/Redis).

The separate `check-sprint` CI question the same review raised is a
governance/merge-gate question for a human, not a code change — left
alone.

## Review follow-up 3 (GitHub PR #526, human review — yujiawei)
Full re-review at head `0c973ef9`, verdict APPROVED, three P2/advisory
findings:
- **README push-override wording** (`README.md:111`): the text implied
  member/bot webhooks always display a fixed "default avatar," but an
  admin can set a custom avatar on any webhook (including member-created
  ones) and that value is what's actually locked in at push time — the
  wording conflated "default" with "stored." Fixed: now says "存量
  Name/Avatar" and cross-references the avatar section for the
  admin-can-override-default case.
- **E2E push test asserts status only, not dispatched payload**
  (`TestPush_MemberWebhookOverrideSilentlyIgnored`): reviewer suggested
  asserting `from.name`/`from.avatar` end-to-end, not just "not
  rejected." **Not done** — the actual message dispatch goes through
  `ctx.SendMessageWithResult` (real WuKongIM call in this suite, per
  CLAUDE.md), and there's no existing capture/mock seam for it; every
  other `from.*` assertion in this package (`richtext_test.go`,
  `util_test.go`, `TestResolveFromIdentity`) tests `buildPayload`/
  `resolveFromIdentity` directly rather than through a live HTTP push.
  Building that capture path is new test infrastructure, not a small
  fix, and the property is already pinned at the unit level
  (`TestResolveFromIdentity` with a spoof attempt) — reviewer's own note
  confirms this. Left as a genuine follow-up suggestion, not applied.
- **Bare `"Webhook-"` accepted as literal name**: reviewer explicitly
  flagged "intentional and tested... awareness only" — no action.

A subsequent bot re-review (OctoBoooot, delta `0c973ef9`→`40ddac1`)
confirmed `api.go` is byte-identical to what was already reviewed and
reframed yujiawei's two 🟡 notes as pre-existing/out-of-scope for this
PR, not regressions it introduces — no further action triggered.

## Review follow-up 4 (GitHub PR #526, automated review — Octo-Q)
Full re-review at head `aebce7f5` (after the README fix above), verdict
APPROVED, one remaining Nit: the just-added README wording still hedged
with "通常" (avatar is "usually" the member-path default) where the
actual invariant is unconditional — whatever `m.Avatar` is stored at
create/update time (default or admin-set custom URL) is what push
renders, full stop. Trimmed the hedge: reworded to state the invariant
directly for both cases instead of qualifying one as the common case.

Reviews at heads `40ddac1`→`aebce7f5` (OctoBoooot) and `0c973ef9`→
`aebce7f5` (Jerry-Xin) both re-confirmed via blob comparison that no
`.go` production file changed since round 1 — no further code action
from either.

## Learning
When a handler is mounted by more than one module (here: incomingwebhook
management routes re-mounted under `/v1/bot/...` by bot_api), sweep for
behavior-pinning tests by grepping the *behavior* (the literal asserted
value, e.g. `"Webhook-"`) across the whole repo, not just the owning
module's directory.
