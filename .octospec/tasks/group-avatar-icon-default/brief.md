---
type: Task
title: "Task: group-avatar-icon-default"
description: Default group avatar becomes the two-person icon (name-independent); expose the avatar color palette to clients for local preview parity.
tags: [group, avatar, render, cache, wire-contract]
timestamp: 2026-06-29T00:00:00Z
# --- octospec extension fields ---
slug: group-avatar-icon-default
upstream: group-chat-avatar-gen (product, 2026-06-29)
source: self
---

# Task: group-avatar-icon-default

> Server-side increment of the "group chat avatar" redesign. Web (octo-web) is a
> separate follow-up: the 修改头像 secondary dialog + 发起群聊 create dialog with
> live local preview consuming the palette endpoint below.

## Goal
Two server changes:

1. **S2 — default avatar is the two-person icon, independent of the group name.**
   Today an un-uploaded group with no custom `avatar_text` renders its name's
   leading 2 glyphs (`GroupNameText`), falling back to the two-person icon only
   when the name yields no renderable glyphs (PR #494). Product (2026-06-29) has
   reversed this: the default avatar must be the two-person icon **regardless of
   the group name**; text is rendered **only** when the user has explicitly set a
   custom `avatar_text` via the 修改头像 dialog.

2. **S1 — expose the avatar color palette over HTTP.** The 10-color palette
   (`main` / `fill` / `iconBack`) lives only in `pkg/avatarrender/palette.go`.
   The web color picker + local live preview must render the *same* colors as the
   server PNG. Add a read endpoint so the palette has a single source of truth and
   never drifts between server and client.

## Background
- Avatar render pipeline + custom `avatar_text`/`avatar_color` + the fixed palette
  shipped in PRs #478 / #486 / #494 (journals: `group-default-avatar`,
  `default-avatar-text-rule`). Default rendering decision is in
  `modules/group/api.go` `writeGroupDefaultAvatar`.
- The default-avatar-text-rule journal already flagged "unnamed groups → icon" as
  a deferred follow-up needing an `is_named` flag. S2 makes that flag unnecessary:
  **all** un-customized groups (named or not) use the icon.
- Web research: there is no avatar dialog in octo-web today (image upload only);
  the create flow (`OrganizationalGroupNew`) sends members only — no name/avatar.
  Those are the web follow-up, not this task.

## Load-bearing list
- `writeGroupDefaultAvatar` text-selection branch (`modules/group/api.go`) — the
  exact behavior being changed. Custom-text path (`GroupText`, ≤4, as-is) is
  unchanged; only the no-custom-text fallback flips from `GroupNameText(name)` to
  empty → icon.
- **Avatar ETag / cache identity.** ETag is CRC32 over content *factors*
  (mode-version + group_no + color + text), not pixels. Named groups switch from
  the `group-name-v4` factor set (with text) to the `group-icon-v3` factor set
  (no text) → the ETag **changes** on its own, so a stale `If-None-Match` cannot
  match → clients revalidate to the fresh icon. The `RenderIcon` bytes are NOT
  touched, so **no version bump is required** (contrast #486: there the icon
  *pixels* changed). `group-name-v4` is retained for the custom-text path.
- `GroupNameText` (`pkg/avatarrender/text.go`) — no longer called by the handler
  after S2; kept (still unit-tested, may serve future use). No behavior change to
  the function itself.
- Stale comments in `modules/group/{api.go,db.go,service.go}` that say
  "空=渲染时回退群名前 2 字派生" — updated to "空=双人图标(默认头像与群名无关)"
  so future readers don't re-introduce name derivation.
- **Wire contract (new):** `GET /v1/group/avatar_palette` (public, static design
  tokens — mirrors the already-public avatar render endpoint). Response:
  `{ "size": 10, "colors": [ { "index", "main", "fill", "icon_back" } ... ] }`,
  hex `#RRGGBB`, ordered by palette index. Pure read, no error path → no errcode.

## Out of scope
- Server default-render rule per named/unnamed distinction (`is_named`) — obviated.
- Any change to custom `avatar_text`/`avatar_color` create/update APIs, validation,
  upload path, or the palette *values*/order.
- All octo-web work (修改头像 dialog, 发起群聊 dialog, local preview component,
  palette consumption) — separate task in octo-web.
- Render version bump (intentionally none — see load-bearing list).

## Acceptance
- `writeGroupDefaultAvatar`: with no custom `avatar_text`, renders `RenderIcon`
  (two-person icon) for BOTH a named and an empty-name group; renders text ONLY
  when `avatar_text` is set. Custom color still honored for the icon.
- `GET /v1/group/avatar_palette` returns `PaletteSize()` (=10) entries; each entry's
  `main`/`fill`/`icon_back` equals `avatarrender.PaletteHex()`; `colors[0].main == "#14C0FF"`.
- Updated `TestGroupAvatarGet*` (named, no custom text → icon) and
  `TestGroupAvatarGetPinsRenderVersion` (text-render version pinned via a
  custom-text group, since name no longer triggers text). Custom-text not-truncated
  + uploaded-redirect + 404/disband regressions still pass.
- `go build ./...`, `go vet`, `golangci-lint`, `make i18n-lint` +
  `i18n-extract-check`, `TestGroupNoLegacyResponseError` guard all green.
  (Endpoint tests needing MySQL/Redis/WuKongIM run in CI per prior increments.)
