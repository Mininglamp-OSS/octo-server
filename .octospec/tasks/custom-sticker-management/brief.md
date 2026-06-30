---
type: Task
title: "Task: custom-sticker-management"
description: Per-user custom stickers (flat, no packs) with an admin-configurable per-user quota, end-to-end (octo-server + octo-web).
tags: ["sticker", "feature", "fullstack", "i18n", "system-setting"]
timestamp: 2026-06-29T00:00:00Z
# --- octospec extension fields ---
slug: custom-sticker-management
upstream: Mininglamp-OSS/octo-server#26
source: self
---

# Task: custom-sticker-management

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work, reviewed and confirmed by a maintainer.

## Goal

Let each user keep a **personal, flat collection of custom stickers** (no packs /
no categories) and use them in chat. Concretely:

- **octo-server** gains a new `modules/sticker/` package exposing user-scoped CRUD:
  - `GET    /v1/sticker/user` → `{ "list": [ {sticker_id, path, placeholder, format} ] }`
  - `POST   /v1/sticker/user` → add a sticker (the `path` comes from a prior
    `modules/file` upload of `type=sticker`); enforces the per-user quota.
  - `DELETE /v1/sticker/user/:sticker_id` → soft-delete one of **the caller's own** stickers.
- **Quota is admin-configurable, default 100 per user**, sourced from the existing
  `system_setting` registry as `sticker.user_max_count` (SuperAdmin-tunable via the
  existing `POST /v1/manager/common/system_setting`, 60s multi-instance reload, YAML
  fallback). Exceeding it returns HTTP 409 with a localized error carrying `max`.
- **Formats**: accept the raster image types users actually have — **GIF, PNG, JPEG,
  WEBP** (static + animated). Lottie/TGS is **not** required for user uploads. This
  requires widening `modules/file`'s sticker upload path, which today hardcodes a
  `.gif` extension.
- **octo-web** rebuilds the currently-dead sticker chain in `EmojiToolbar`: a fixed
  「我的贴纸」 tab backed by `GET /v1/sticker/user`, an upload (`+`) entry, a delete
  affordance, and **format-aware rendering** (`tgs` → `<tgs-player>`, otherwise
  `<img>`) in both the picker and the sent-message cell.

## Background

Issue #26 reported that `octo-web` called `GET /v1/sticker/user/category` (and
`/v1/sticker/user/sticker?category=`) but octo-server registered no such routes, so
every chat panel mount logged a 404. The issue was auto-closed as stale and never
implemented.

Current ground truth (verified in code):

- **Server**: there is **no** `modules/sticker/`. Sticker exists only as a *file
  bucket*: `TypeSticker = "sticker"` (`modules/file/const.go:111`), bucket allowlist
  (`modules/file/helpers.go:13`), and a sticker-specific upload path that hardcodes
  `.gif`: `modules/file/api.go:116-118` (`getFilePath`). `checkReq`
  (`modules/file/api.go:763-779`) already (a) lets `TypeSticker` skip the
  empty-path requirement and (b) includes `TypeSticker` in the allowed-type list.
- **Web**: the old `/sticker/*` chain is **dead code** — `requestStickerCategory()`
  (`packages/dmworkbase/src/Components/EmojiToolbar/index.tsx:119`) is defined but
  never invoked (disabled by the comment at `:115`); `stickerCategories` therefore
  stays empty, so the sticker tabs never render and `requestStickers()` (`:182`)
  never fires. **No live request hits either endpoint today**, so we are NOT bound
  by the legacy `category`-shaped contract and can ship a clean flat API.
- **Render constraint**: the picker (`EmojiToolbar:162`) and the sent-sticker cell
  (`Messages/LottieSticker/index.tsx`, `LottieStickerCell`, 208px) render stickers
  **only** via `<tgs-player>` (Lottie/TGS-only). A GIF/PNG/WEBP would not render —
  hence the required format-aware render branch.
- **Message protocol**: stickers are sent as the existing `LottieSticker` message
  type (`MessageContentTypeConst.lottieSticker = 12`) whose payload
  `{url, category, placeholder, format}` is embedded inline, so deleting a sticker
  does not break already-sent messages. We reuse this type unchanged.
- **Admin config**: octo-server already has a mature `system_setting` KV registry
  (`modules/common/system_setting_schema.go`, typed getters in
  `modules/common/system_settings.go`, SuperAdmin manager API in
  `modules/common/api_manager_system_setting.go`). The 「at most 20 categories」 limit
  in `modules/category` is a hardcoded constant; we deliberately do the configurable
  thing here instead and register `sticker.user_max_count` in that schema.

Decisions already settled with the maintainer: (1) stickers only, no inline custom
emoji; (2) flat, no packs/categories; (3) user-personal scope only — no
system/space packs; (4) admin-configurable per-user quota, default 100; (5) accept
GIF/PNG/JPEG/WEBP; (6) full-stack delivery.

## Load-bearing list

### octo-server (drives rule injection — tags mirror `.octospec/rules/_index.yaml`)

- **i18n error envelope & guard** (touches: `error-response`, `i18n`,
  `wire-contract`) — every error in `modules/sticker` must go through
  `httperr.ResponseErrorL` + codes registered in a new `pkg/errcode/sticker.go`
  (`err.server.sticker.*`, plus reuse of `err.shared.*` where apt). New module needs
  a `TestStickerNoLegacyResponseError` source guard; `make i18n-extract-check` and
  `make i18n-lint` must pass; zh-CN strings added to
  `pkg/i18n/locales/active.zh-CN.toml`. Empty-collection response must be `200 {list:[]}`,
  **never 404** (the original bug).
- **Per-user rate limiting** (touches: `rate-limit`, `throttle`) — the new
  `/v1/sticker/user` group mounts `appwkhttp.SharedUIDRateLimiter(r, ctx)` **after**
  `ctx.AuthMiddleware(r)`; no hand-rolled Redis counter. Module tests that hit these
  routes must reset `ratelimit:uid:*` in setup.
- **User-data isolation** (touches: `space`, `isolation`, `auth`, `acl`) — all
  sticker rows are keyed by the login uid (`c.GetLoginUID()`); list/add/delete
  operate only on the caller's own rows; `DELETE` must verify ownership before
  soft-deleting (deleting another user's sticker yields not-found/forbidden, never a
  cross-user delete). Scope is the **user**, not a space — no space packs in v1.
- **`modules/file` sticker upload contract** (touches: `wire-contract`) —
  `getFilePath` (`api.go:116-118`) and the allowed-type/path checks
  (`checkReq`, `api.go:763-779`) must be widened so a sticker upload can carry a
  `.png/.jpeg/.jpg/.webp/.gif` extension (derived from the requested filename /
  content-type) instead of a forced `.gif`. Must not regress existing `type=sticker`
  upload callers, the `sticker` bucket allowlist, or other file types. Enforce a max
  file size and reject unsupported formats.
- **`system_setting` registry** (touches: `wire-contract`) — register
  `sticker.user_max_count` (int, default 100) in
  `modules/common/system_setting_schema.go` with a typed getter
  (`StickerUserMaxCount()`) in `modules/common/system_settings.go`, injected into the
  sticker handler. Surfaces automatically in the SuperAdmin
  `GET/POST /v1/manager/common/system_setting` list; respects the snapshot/60s-reload
  and YAML-fallback semantics. Global default only — no per-space/per-user override
  table.
- **Module wiring** (touches: `commit`, `git`) — `modules/sticker` registered via
  `1module.go` (`init()` + `register.AddModule`, `SQLDir` embed) and blank-imported in
  `internal/modules.go`; SQL migration `modules/sticker/sql/<yyyymmdd>NNNN_sticker.sql`.
- **Module tests** (touches: `test`, `testing`) — `testutil.NewTestServer()` +
  `CleanAllTables`; cover empty-list 200, add, list-after-add, quota 409 at the
  boundary, quota override via system_setting, ownership-scoped delete, and
  format rejection.

### Cross-repo — octo-web (no octospec rules engine there; listed for completeness)

- **Sticker message protocol** — reuse `LottieSticker` (contentType `12`) unchanged;
  payload stays `{url, category, placeholder, format}`. `category` is now a constant
  sentinel (e.g. `"user"`) or dropped; do not break decode of already-sent messages.
- **Format-aware rendering** — branch in the picker (`EmojiToolbar`) and
  `LottieStickerCell` on `format`: `tgs` → `<tgs-player>`, else `<img>`.
- **Datasource contract** — replace the dead `userStickerCategory()` /
  `getStickers(category)` with calls to the new flat endpoints; route everything
  through `WKApp.apiClient` (no new fetch/axios instances); upload via the existing
  `modules/file` upload flow (`type=sticker`).

## Out of scope

- **Inline custom emoji** (text `[xxx]` tokens / the built-in `EmojiService` map) —
  a separate protocol and a separate epic; untouched here.
- **Packs / categories** of any kind, and **system or space-level (org/admin-curated)
  sticker sets** — user-personal only.
- **Favorites / recently-used / reorder (drag-sort)** lists — possible later phase;
  v1 list order is `created_at DESC`.
- **Per-space or per-user quota overrides** — only the global
  `sticker.user_max_count` default (admin-tunable) ships now.
- **Lottie/TGS authoring or upload** by end users, and **server-side
  transcoding/normalization** of uploaded images (e.g. forcing WEBP, resizing).
  Validate-and-store only.
- **Sticker file garbage collection** — soft-deleted stickers leave their object in
  storage (already-sent messages still reference it); no GC job here.
- **Content moderation /審核** of uploaded images.

## Acceptance

Server (machine-checkable via `go test ./modules/sticker/...` + manual curl):

- `GET /v1/sticker/user` for a user with no stickers returns **HTTP 200**
  `{"list":[]}` (regression guard for #26 — never 404).
- `POST /v1/sticker/user` with a valid `{path, format, placeholder?}` persists the
  sticker scoped to the caller; it appears in the next `GET /v1/sticker/user`.
- `POST /v1/sticker/user` with an unsupported `format` (not gif/png/jpeg/webp) returns
  a localized **400**.
- Adding past the quota returns **HTTP 409** with code `err.server.sticker.quota_exceeded`
  and detail `max` equal to the effective limit. With no override, `max == 100`.
- Setting `sticker.user_max_count` via `POST /v1/manager/common/system_setting`
  (SuperAdmin) changes the enforced quota within the reload window.
- `DELETE /v1/sticker/user/:sticker_id` soft-deletes the caller's own sticker;
  attempting to delete a sticker owned by another user does **not** delete it and
  returns not-found/forbidden.
- All four endpoints sit behind `AuthMiddleware` + `SharedUIDRateLimiter`.
- `make i18n-extract-check` and `make i18n-lint` pass; `TestStickerNoLegacyResponseError`
  passes; zh-CN entries exist for every new `err.server.sticker.*` code.
- `modules/file`: a sticker upload requested with filename `x.png` / `x.webp` /
  `x.jpeg` / `x.gif` yields an upload path with the corresponding extension (not a
  forced `.gif`); existing non-sticker upload behavior is unchanged
  (`go test ./modules/file/...` green).

Web (manual + `pnpm lint` / `cd apps/web && pnpm test`):

- The emoji/sticker panel shows a fixed 「我的贴纸」 tab; with zero stickers it shows
  an empty state + an add entry (no console 404).
- A user can upload an image and see it appear in the tab; can delete it.
- GIF/PNG/JPEG/WEBP stickers render (as `<img>`) in both the picker and the sent
  message bubble; any existing `tgs` sticker still renders via `<tgs-player>`.
- Sending a sticker posts a `LottieSticker` (type 12) message that round-trips and
  renders for the recipient.
- `pnpm lint` and the web test suite pass.
