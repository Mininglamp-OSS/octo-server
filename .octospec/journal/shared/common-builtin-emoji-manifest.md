---
type: Journal
title: "Journal: common-builtin-emoji-manifest (octo-server)"
description: Record of adding a public, cacheable GET /v1/common/emojis built-in emoji manifest endpoint (embedded JSON) so clients stop hardcoding the custom emoji list.
tags: ["common", "emoji", "wire-contract", "public-endpoint", "cache"]
timestamp: 2026-06-29T11:00:00Z
# --- octospec extension fields ---
task: common-builtin-emoji-manifest
upstream: octo-web EmojiService hardcoding (internal request)
source: self
---
# Journal: common-builtin-emoji-manifest (octo-server)

## What was done

The built-in **custom** emojis (`[使命必达] / [崇尚行动] / [有品位] / [尚方宝剑]`)
were hardcoded in every client (token→image map + bundled PNG). Adding or renaming
one forced web / iOS / Android / desktop to each re-release. This moves the **list**
(keys, labels, order, version) to a server single source of truth.

Added a public, cacheable `GET /v1/common/emojis` returning
`{ version, list:[{key, name, url}] }`, mirroring the freshly-merged
`GET /v1/group/avatar_palette` (#500) precedent:

- `modules/common/emojis/manifest.json` — the single source of truth, embedded via
  `//go:embed` (`modules/common/emoji.go`). `version` lives inside the JSON; editing
  the file and bumping `version` is the whole change to add/rename an emoji.
- `modules/common/emoji.go` — one-time parse (panic on corrupt embedded asset, like
  other startup asset checks → **no runtime error path, so no new errcode/i18n**),
  content-based weak ETag via the shared `pkg/avatarrender.ETag` / `IfNoneMatch`
  helpers, `Cache-Control: public, max-age=300, must-revalidate`, `If-None-Match`→304.
- Route registered in the existing **public** `commonNoAuth` group
  (`modules/common/api.go`), alongside `/countries` and `/changelog`. Public is
  documented in the handler: the manifest is non-sensitive, touches no user data and
  no Space, and is needed before login.
- Swagger entry + `modules/common/emoji_test.go` (infra-free handler/loader/304 tests
  that run anywhere, plus a `testutil.NewTestServer` test proving the real route is
  reachable without a token).

### Wire contract — `url` is optional per item (Option B, step 1)

This step is embedded-JSON only (chosen over the DB+manager+upload Option A). The
key design choice: `url` is **optional**. Built-in emojis ship `url: ""` and clients
reuse their already-bundled PNG (looked up by the client's own fallback map), so:
- no image hosting was needed server-side in this step (the source PNGs are 0.3–1.2 MB
  each — embedding them in the binary was explicitly rejected);
- any **future** emoji added on the server carries a non-empty `url` (object storage /
  file module) and renders on all clients with **zero client re-release**.

The wire contract is identical to the eventual DB-backed Option A, so phase A is a
drop-in source swap behind the same endpoint.

## Verification

- `go test ./modules/common/...` green against live MySQL/Redis/WuKongIM (4 emoji
  tests incl. the public-no-auth route test; full module suite passes).
- `go vet`, `golangci-lint`, `make i18n-lint`, `make i18n-extract-check` all clean
  (no new error codes introduced, as designed).
- Web counterpart (octo-web `DefaultEmojiService`) refactored to fetch + cache this
  manifest with built-in fallback; its vitest suite passes.

## Learning

A public GET that exposes a server-side single source of truth + content ETag +
`must-revalidate` + 304 (the `avatar_palette` shape) is now an established repo
pattern for "stop hardcoding shared data on every client". When the payload is a
compiled-in asset with no runtime failure mode, the handler needs **no** errcode/i18n
machinery — keep it that way rather than adding a code "just in case". Making the
image `url` optional (empty = client reuses its bundle) lets the manifest pipeline
ship and prove out without first solving image hosting, while staying forward
compatible with a later DB/object-storage source.
