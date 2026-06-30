---
type: Task
title: "Task: group-avatar-name-no-text"
description: New groups default to the two-person icon (group name is not avatar text; only custom avatar_text renders text). Existing groups are grandfathered via the is_named flag so they keep their current name-text avatar.
tags: [group, avatar, render, grandfather, product-pivot]
timestamp: 2026-06-29T12:00:00Z
# --- octospec extension fields ---
slug: group-avatar-name-no-text
upstream: group-chat-avatar-gen (client-coordination change, 2026-06-29)
supersedes: group-avatar-icon-default (S2 — is_named meaning is repurposed, not deprecated)
source: self
---

# Task: group-avatar-name-no-text

> Server change to match the client: a **newly created** group defaults to the
> two-person icon; the group **name is never rendered as avatar text**; text
> appears only when the user explicitly sets a custom `avatar_text`. Existing
> groups are grandfathered (kept on their current name-text avatar) so the change
> does not flip every historical group to an icon at once.

## Goal
Change **who gets `is_named=1`**, not the render rule.

- **Render rule is unchanged from #500:** `avatar_text` (custom) > `is_named==1`
  → `GroupNameText(name)` (first-2 glyphs) > two-person icon. `writeGroupDefaultAvatar`
  is untouched in logic.
- **`is_named` is repurposed** from "user explicitly named the group" to
  "**pre-cutover legacy group**":
  - **New groups → `is_named=0` always** (regardless of whether a name was given).
    So a freshly created group defaults to the icon; setting `avatar_text` is the
    only way to get text. Matches the client preview (icon when `avatar_text` empty).
  - **Existing groups → `is_named=1`** — already backfilled by #500's migration
    (`20260629000001`, conservative backfill-to-1). They keep rendering their name
    as before (grandfather), so no historical group flips to an icon.

This is the **甲案** chosen by the maintainer (2026-06-29): keep `is_named`
load-bearing (NOT deprecated) as the legacy/new discriminator. The alternative
(retire `is_named` + a one-time backfill of name→`avatar_text`) was rejected because
it requires Go-side script-aware derivation (not expressible in SQL) and freezes
legacy avatars (rename would no longer follow).

## Background
- #500 added `is_named` meaning "user named it" and gated name-text on it. The
  client then changed its create/edit preview to show the icon when `avatar_text`
  is empty. To match, the server must default new groups to the icon — but #500
  set `is_named=1` for any explicitly-named new group, so new named groups still
  rendered name-text. Flipping ALL groups to icon (the global approach, prior PR
  #503 head) was rejected by product because it changes every existing group's
  avatar at once. Grandfather is the resolution.

## Load-bearing list
- **`is_named` write-policy (the whole change).** New groups must persist
  `is_named=0`:
  - `Service.CreateGroup` (`service.go`): removed the `isNamedVal = (name != "")`
    computation; the insert sets `IsNamed: 0`.
  - `Service.AddGroup` (`service.go`): removed the name-based computation; insert
    sets `IsNamed: 0`.
  - `event.go` system-group + org/dept inserts: `IsNamed: 0` (they are served
    static PNGs and never hit the render path; set to 0 to keep the invariant
    "`is_named=1` ⟺ pre-cutover legacy row" clean).
  - `Service.UpdateGroupInfo` rename: removed `groupModel.IsNamed = 1`. Rename no
    longer flips the flag — it preserves the loaded value (legacy stays 1 and its
    name-text follows the new name; new stays 0 and stays an icon). `UpdateTx`'s
    full-row SetMap still writes the (unchanged) loaded `is_named` back.
- **Render rule unchanged (`writeGroupDefaultAvatar`, `api.go`).** Still
  `avatar_text > is_named==1 name-text > icon`. Only the comments are reworded to
  the legacy/new semantic. ETag factors unchanged: text present (legacy name or
  custom) → `group-name-v4`; no text (new group) → `group-icon-v3`.
- **`is_named` is read-only after creation.** Only set by #500's migration
  backfill (legacy → 1); every app insert writes 0; rename/invite never change it.
  So `is_named=1` strictly identifies pre-cutover groups.
- **`GroupResp.is_named` kept** (additive, #500). Re-documented as
  "1=legacy/name-text, 0=new/icon"; clients can still locally predict the default.
- **Migration `20260629000001` is reused as-is, not modified.** Its backfill-to-1
  is exactly the legacy marker this task needs. The file is already merged/applied;
  its in-file comments still describe the #500 "user-named" intent (now reinterpreted
  as "legacy"). Since several reviewers flagged the stale *live* column COMMENTs
  (schema introspection would carry old guidance), a follow-up **comment-only
  migration `20260629000002`** `MODIFY COLUMN`s `is_named` + `avatar_text` to refresh
  their COMMENTs to the legacy/new semantic (no type/constraint/data change,
  reentrant via INFORMATION_SCHEMA guard). The historical migration files' `--`
  comments stay as the record; the Go `Model.IsNamed` comment + the live column
  COMMENT are the source of truth.

## Follow-ups (out of scope — separate PR/issue)
- **`UpdateGroupInfo` full-row `UpdateTx` → column-level** (yujiawei, non-blocking):
  a rename's full-row writeback could clobber a concurrent `invite` toggle with a
  stale loaded value (the inverse of #500's P1, which was fixed for the invite
  branch only). Pre-existing; converting `UpdateGroupInfo` to a column-scoped write
  closes the remaining read-modify-write window.
- **Public avatar endpoint enumeration / name-prefix disclosure** (yujiawei,
  non-blocking): the unauthenticated `GET /:group_no/avatar` distinguishes
  existing vs nonexistent/disbanded groups and renders a legacy group's first-2
  name glyphs. Pre-existing and intentionally preserved by grandfather; hardening
  (auth or constant-shape response) is separate scope.

## Out of scope
- Any change to the render logic, `avatar_text`/`avatar_color` APIs, validation,
  upload path, palette endpoint/values.
- Dropping or re-migrating the `is_named` column (kept, repurposed).
- octo-web (already shows the icon when `avatar_text` is empty).

## Acceptance
- `CreateGroup` / `AddGroup` with or without a name → `is_named=0` (new groups
  default to the icon). `UpdateGroupInfo` rename → `is_named` unchanged (new stays
  0, legacy stays 1).
- `writeGroupDefaultAvatar`: `is_named=1` (legacy) + no `avatar_text` →
  `RenderGroup(GroupNameText(name))`; `is_named=0` (new) + no `avatar_text` →
  `RenderIcon`; custom `avatar_text` always overrides; custom color honored in both.
- Tests: `TestCreateGroup_Success`/`_AutoGenerateName` assert `is_named=0`;
  `TestUpdateGroupInfo_RenameDoesNotChangeIsNamed`;
  `TestRenameThenInviteUpdate_KeepsNameAndIsNamed` (P1 clobber regression, legacy
  group); `TestAddGroup_DefaultsIsNamedZero`; `TestGroupAvatarGetNamedRenders*`
  (legacy → name-text) and `...AutoNamedRendersIcon` (new → icon) pass.
- `go build ./...`, `go vet`, `golangci-lint`, `make i18n-lint` green. Endpoint
  tests needing MySQL/Redis/WuKongIM run in CI.
