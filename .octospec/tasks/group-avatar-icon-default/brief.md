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

1. **S2 — default avatar follows a *user-given* name; an auto-generated
   (member-concat) name renders the two-person icon.** Today an un-uploaded group
   with no custom `avatar_text` always renders its `name`'s leading 2 glyphs
   (`GroupNameText`), including the member-concatenated default name like
   `张三、李四、王五` (PR #494). Product (2026-06-29): the default avatar should take
   the name's first 2 glyphs **only when the user explicitly named the group**; a
   group left on the auto member-concat name renders the two-person icon (don't
   render a concat name as avatar text). A new `is_named` flag distinguishes the
   two (set at create from whether `name` was provided, and on rename). Priority:
   custom `avatar_text` > named-group name (`is_named=1`) > two-person icon.
   Existing groups are backfilled `is_named=1` (conservative — no existing avatar
   changes; only new auto-named groups get the icon).

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
- The default-avatar-text-rule journal flagged "unnamed/auto-member-name groups
  using the icon instead of text" as a deferred follow-up needing an `is_named`
  flag. S2 implements exactly that: `is_named` is added now (migration backfills
  existing rows to 1).
- Web research: there is no avatar dialog in octo-web today (image upload only);
  the create flow (`OrganizationalGroupNew`) sends members only — no name/avatar.
  Those are the web follow-up, not this task.

## Load-bearing list
- `writeGroupDefaultAvatar` text-selection branch (`modules/group/api.go`):
  `avatar_text` → as-is; else `is_named==1` → `GroupNameText(name)`; else icon.
- **`is_named` lifecycle.** New column (migration `20260629000001`, backfill
  existing → 1). The migration is **recovery-safe** (review #500, Jerry-Xin +
  OctoBoooot 🔴): because MySQL `ALTER TABLE` auto-commits, the backfill must NOT
  live inside the column-exists guard (a crash between the committed ALTER and the
  backfill would skip it on retry, leaving existing rows at `DEFAULT 0` =
  auto-named → icon, violating "no existing avatar changes"). Pattern: (1) add
  column **NULL** (no default) → existing rows = NULL sentinel; (2) `UPDATE ... SET
  is_named=1 WHERE is_named IS NULL` run unconditionally (idempotent, only touches
  not-yet-backfilled rows, converges on any partial-failure retry); (3) `MODIFY` to
  `NOT NULL DEFAULT 0` for new inserts. `CreateGroup`: `is_named = (trimmed req.Name != "")` computed
  *before* the member-concat fallback overwrites `groupName`. Rename
  (`UpdateGroupInfo`, `req.Name != nil`) → `is_named = 1`; persisted by adding
  `is_named` to `DB.UpdateTx`'s `SetMap` (it used an explicit column list).
- **All direct creation paths set `is_named` (review #500, Jerry-Xin + OctoBoooot
  🔴).** `CreateGroup` is not the only insert path: `event.go` system-group
  (`handleRegisterUserEvent`) + org/dept (`handleOrgOrDeptCreateEvent`) and
  `Service.AddGroup` insert named groups directly; left at the Go zero value they
  persist `is_named=0`. Set `IsNamed: 1` on the system/org/dept inserts (they carry
  an explicit configured name) and `AddGroup` infers it from `TrimSpace(name)!=""`.
  Note: system/`org_`/`dept_` groups are served *static PNGs* by the prefix-gated
  branches in `avatarGet` (`api.go:360-394`) and never reach the `is_named` render
  path, so this is data-correctness/robustness, not a live avatar regression — but
  the persisted flag must still be right (a removed prefix-branch would otherwise
  silently break them).
- **Combined `{name, invite}` update must not revert the rename / `is_named`
  (review #500 P1, yujiawei + OctoBoooot).** `groupUpdate` (`api.go`) loads the row
  once, then the name branch commits `name`+`is_named=1` via `UpdateGroupInfo`
  (its own fresh load) and the invite branch wrote the **stale** snapshot back via
  the full-column `UpdateTx` — clobbering both. Fix: invite branch uses a
  column-scoped `DB.UpdateInviteTx` (only `invite`+`version`); the handler no longer
  caches the full row (existence-check only). This also removes the pre-existing
  `name`-revert on the same path and the read-modify-write race.
- **`GroupResp.is_named` exposed (review #500 🟡).** Read-only `is_named` added to
  `GroupResp` (both `from`/`fromModel` populators) so web clients can locally
  predict name-text vs icon for an existing group when clearing `avatar_text`
  (preview parity). Additive, backward-compatible.
- **Avatar ETag / cache identity.** ETag is CRC32 over content *factors*
  (mode-version + group_no + color + text), not pixels. Named groups use the
  `group-name-v4` factor set (with text); auto-named/empty use `group-icon-v3`
  (no text) — different modes → different factor strings, so a rename or an
  is_named flip changes the text factor and the ETag on its own; clients
  revalidate to the fresh image. `RenderIcon`/`RenderGroup` bytes are untouched →
  **no version bump required** (contrast #486: there the icon *pixels* changed).
- `GroupNameText` (`pkg/avatarrender/text.go`) — still called by the handler for
  `is_named=1` groups; unchanged behavior.
- Comments in `modules/group/{api.go,db.go,service.go,const.go}` + swagger updated
  to the is_named rule ("空=按 is_named 回退：命名群群名/自动名群双人图标").
- **Wire contract (new):** `GET /v1/group/avatar_palette` (public, static design
  tokens — mirrors the already-public avatar render endpoint). Response:
  `{ "size": 10, "colors": [ { "index", "main", "fill", "icon_back" } ... ] }`,
  hex `#RRGGBB`, ordered by palette index. Pure read, no error path → no errcode.

## Out of scope
- Any change to custom `avatar_text`/`avatar_color` create/update APIs, validation,
  upload path, or the palette *values*/order.
- All octo-web work (修改头像 dialog, 发起群聊 dialog, local preview component,
  palette consumption) — separate task in octo-web.
- Render version bump (intentionally none — see load-bearing list).

## Acceptance
- `writeGroupDefaultAvatar` with no custom `avatar_text`: `is_named=1` group →
  `RenderGroup(GroupNameText(name))` (name first-2); `is_named=0` group (or empty
  name) → `RenderIcon` (two-person). Custom color honored in both. Custom
  `avatar_text` always overrides.
- `CreateGroup`: explicit `name` → `is_named=1`; omitted (member-concat) →
  `is_named=0`. `UpdateGroupInfo` with a name → `is_named=1` (persisted).
- `GET /v1/group/avatar_palette` returns `PaletteSize()` (=10) entries; each entry's
  `main`/`fill`/`icon_back` equals `avatarrender.PaletteHex()`; `colors[0].main == "#14C0FF"`.
- Tests: `TestGroupAvatarGetAutoNamedRendersIcon`, `TestGroupAvatarGetNamedRendersNameText`,
  `TestCreateGroup_{Success,AutoGenerateName}` is_named asserts,
  `TestUpdateGroupInfo_RenameMarksIsNamed`; custom-text/uploaded/404/disband + palette
  + version-pin regressions still pass.
- `go build ./...`, `go vet`, `golangci-lint`, `make i18n-lint` +
  `i18n-extract-check`, `TestGroupNoLegacyResponseError` guard all green.
  (Endpoint tests needing MySQL/Redis/WuKongIM run in CI per prior increments.)
