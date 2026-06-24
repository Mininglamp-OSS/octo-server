---
type: Task
title: "Task: bot-connect-plugin-package"
description: Serve the Bot connect plugin package + public API URL from the backend instead of hardcoding them in the frontend.
tags: ["app-bot", "botfather", "bot-connect", "plugin-package", "i18n", "wire-contract"]
timestamp: 2026-06-24T00:00:00Z
# --- octospec extension fields ---
slug: bot-connect-plugin-package
upstream: "octo-server#446"
source: self
---
# Task: bot-connect-plugin-package

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

Upstream: octo-server #446 — the App Bot "connect guide" (`npx -y <pkg> bind ...`)
is assembled entirely in the octo-admin frontend, which hardcodes two server-owned
facts: the channel-adapter npm package (`openclaw-channel-dmwork`, now unmaintained)
and an `api_url` that falls back to the **admin dashboard origin** rather than the
Bot API entry. Both must move to the backend so a package rename/canary or a
split admin/bot-api host never requires a frontend redeploy.

## Goal
The four App Bot read/create endpoints return a `connect` object so the frontend
stops owning these facts:

```jsonc
"connect": { "plugin_package": "create-openclaw-octo", "api_url": "https://<bot-api>" }
```

- `plugin_package` is backend-configured: default the **maintained** package
  `create-openclaw-octo` (the one BotFather already emits — the migration target
  off the dead `openclaw-channel-dmwork`), overridable via `OCTO_BOT_PLUGIN_PACKAGE`.
- `api_url` resolves to the **public Bot API entry** (`External.BaseURL`, fallback
  `http://<ip>:8090`), never the admin origin.
- The package name becomes a single backend source of truth (`pkg/botutil`) shared
  with BotFather's connect prompts, so a rename touches exactly one place.
- Data only: no localized guide prose, no token or other secret in `connect`.

## Background
- App Bot handlers: `modules/app_bot/app_bot.go` `createBot` (covers `POST
  /v1/admin/app_bot` + `POST /v1/space/:space_id/app_bot`) and `getBotDetail`
  (covers `GET /v1/admin/app_bot/:id` + `GET /v1/space/:space_id/app_bot/:id`).
  Two response builders cover all four endpoints.
- New source of truth: `pkg/botutil/connect.go` — `PluginPackage()` (env
  `OCTO_BOT_PLUGIN_PACKAGE` → default `create-openclaw-octo`) and
  `DeriveAPIURL(cfg)` (mirrors the existing `External.BaseURL`/`IP:8090` logic in
  `bot_api/register.go` and `botfather/command.go`).
- BotFather: the package name was frozen in the i18n templates
  `modules/botfather/templates/{zh-CN,en-US}/command.tmpl` (connect_prompt /
  created_prompt / quickstart / install) and rendered via `command.go`. These are
  the msgtmpl outbound-message catalog (#304, the "fourth i18n category"), NOT the
  HTTP error envelope.
- Issue: octo-server #446.

## Load-bearing list
- **wire-contract**: `connect{plugin_package, api_url}` is a NEW response field on
  4 endpoints consumed by octo-admin; it must be data-only, contain no secret, and
  always be present. It is a **success-response** field — it does NOT pass through
  the i18n error envelope, so no `pkg/errcode` code / `active.zh-CN.toml` /
  `i18n-extract` is involved. (rules: error-handling — wire-contract)
- **space**: `getBotDetail`/`createBot` keep their existing gates
  (`checkSpaceAdmin` for the space route, `CheckLoginRole` for the platform route)
  and the `botInRouteScope` cross-tenant IDOR guard. `connect` is identical
  regardless of scope/tenant and adds no per-tenant data, so isolation is
  unchanged — the field must not widen any query or gate. (rules: space-isolation)
- **i18n (msgtmpl catalog)**: BotFather connect/created/quickstart/install
  templates now render the injected `{{.PluginPackage}}`. The catalog runs with
  `missingkey=error`, so every render site (`command.go` ×4) AND the completeness
  probe must supply `PluginPackage`. (rules: error-handling — i18n)
- **secret-handling**: `connect` must never carry the bot token; the masked
  top-level `token` field stays unchanged.
- **api_url single-source-of-truth**: `DeriveAPIURL(cfg)` replaces every inline
  `External.BaseURL` / `http://<ip>:8090` copy across `app_bot`, `botfather`
  (`command.go` + `api.go`) and `bot_api` (`register.go` + `commands.go`) —
  behavior-identical, so the connect guide, BotFather docs, and `/bot/register`
  stay consistent and a future derivation change touches exactly one place.
  (rules: testing — `TestDeriveAPIURL`)

## Out of scope
- octo-admin frontend (`connectGuide.ts`) — `bot.connect?.plugin_package ?? …` /
  `?? getBotApiUrl()`; tracked separately under #446, ships after this.
- `modules/botfather/skill.go` — the deprecated, static skill-doc generator still
  hardcodes `create-openclaw-octo`; it is not part of the i18n connect-guide
  catalog and is left for a separate decision.
- octo-lib `config.Config` changes — the package name is a module-level env knob,
  no cross-repo change.
- Returning the localized guide prose from the backend (explicitly forbidden by #446).

## Acceptance
- All four endpoints return `connect{plugin_package, api_url}`; `plugin_package`
  is read from config (default `create-openclaw-octo`, honors
  `OCTO_BOT_PLUGIN_PACKAGE`); `api_url` is the public Bot API entry.
- `connect` carries no secret: exactly the two public keys, token never present.
- New tests pass:
  - `pkg/botutil`: default / env-override / `DeriveAPIURL` (BaseURL + IP fallback).
  - `modules/app_bot`: HTTP e2e (sqlmock+redis) — `connect` present, an
    `OCTO_BOT_PLUGIN_PACKAGE` override reaches the client, and no token leaks into
    `connect`.
  - `modules/botfather`: the connect-guide templates render the injected package
    (bind/install/quickstart), the completeness probe still renders, and a guard
    asserts the old `create-openclaw-octo` literal no longer appears when a
    different package is injected.
- `go build ./...`, `go vet ./...`, `golangci-lint run`, `make i18n-extract-check`,
  `make i18n-lint` all pass.
- No inline `api_url` derivation remains: a grep for the `External.BaseURL` /
  `http://<ip>:8090` fallback returns only `botutil.DeriveAPIURL`.
- Full affected suites green under the CI recipe (master key + per-package DB
  reset + `-race`): `pkg/botutil`, `modules/app_bot`, `modules/botfather`,
  `modules/bot_api`.
