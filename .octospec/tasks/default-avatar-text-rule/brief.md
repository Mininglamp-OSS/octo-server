---
type: Task
title: "Task: default-avatar-text-rule"
description: Script-aware 2-glyph text rule for group + personal default avatars (Han-only on mixed, initials for English, icon/ascii fallback)
tags: [avatar, render, cache]
timestamp: 2026-06-27
slug: default-avatar-text-rule
upstream: self (post-#486 test-env feedback)
source: self
---

# Task: default-avatar-text-rule

## Goal
The default (un-uploaded) avatar text rule produces awkward output for
English / mixed-script / digit names: group takes the first 4 runes, personal
the last 2, with no script awareness — e.g. `Backend Team`→`Back`,
`Bug反馈群`→`Bug反` (cramped 2×2), `Alice`→`ce`. Replace it with a script-aware,
2-glyph rule shared by group and personal default avatars.

## Background
Post-#486 (transparent corners) test-env review surfaced these. Product/UI
decisions captured (sample-rendered + signed off in chat):
- 2 glyphs max.
- Mixed script: if any Han, take Han only (drop Latin/digits/symbols).
- Pure English (no Han): initials — first letter per token (whitespace/sep +
  camelCase split), max 2, uppercase; single word → 1 letter.
- Pure digits: 2 digits.
- Nothing renderable (empty / pure symbol / emoji): fall back to the icon —
  group: two-person icon; personal: ascii `generateDefaultAvatar`.
- Group takes **leading** glyphs (前2); personal keeps **trailing** (后2) for
  Han/digits (matches DingTalk/Feishu personal convention). Initials are
  direction-agnostic for both.
- Custom group `avatar_text` (user-set) is rendered AS-IS (its own ≤4-rune
  normalization), NOT through the auto rule.

## Load-bearing list
- **avatar text extraction** (`pkg/avatarrender/text.go`): add a shared
  script-aware core + `GroupNameText(name)` (前2) for group auto-derivation;
  rewrite `IndividualText(name)` (后2) for personal. **Keep `GroupText`
  unchanged** — it is the custom-`avatar_text` normalizer (≤4), used on write at
  `modules/group/api.go:793,1001` and for the custom branch of the avatar
  handler. Do NOT route custom text through the new rule.
- **render-cache identity** (ETag + CacheKey) [touches: wire-contract]: derived
  bytes change → bump `group-name-v3`→`v4` (`modules/group/api.go`) and
  `name-v4`→`v5` (`modules/user/api.go`) at BOTH the ETag and CacheKey sites.
  `group-icon-v3` and `ascii-v1` unchanged (their renderers are not touched).
- **avatar endpoint handlers** [touches: error-response]: `writeGroupDefaultAvatar`
  splits custom-text (→`GroupText`) vs auto-name (→`GroupNameText`); user avatar
  name/ascii branch keeps structure. No new raw error responses (keep existing
  fallbacks; `error-handling` rule).
- **version-pin tests** [touches: test]: `TestGroupAvatarGetPinsRenderVersion`
  (→`group-name-v4`, expected text via `GroupNameText`),
  `TestUserAvatarGetPinsRenderVersion` (→`name-v5`).

## Out of scope
- Personal ascii fallback white corners (#486 follow-up ①) —
  `generateDefaultAvatar` untouched, `ascii-v1` not bumped.
- CacheKey-version pin hardening (yujiawei P2 ②) — separate follow-up.
- Unnamed / auto-member-name groups using the icon instead of text (③, needs an
  `is_named` flag) — auto-named groups will still render Han前2 of the joined
  member names. Known follow-up.
- Personal avatar **direction** stays 后2 (confirmed); not switching to 前2.

## Acceptance
- `pkg/avatarrender` unit tests cover all 5 branches for both `GroupNameText`
  (前2) and `IndividualText` (后2); `GroupText` (custom ≤4) tests unchanged.
- Group endpoint: `后端架构讨论`→`后端`, `Bug反馈群`→`反馈`, `2024春招群`→`春招`,
  `Backend Team`→`BT`, `Sales`→`S`, `2024`→`20`, ``/`🎉🎉`→two-person icon;
  custom `avatar_text="研发中心"`→rendered as-is (NOT truncated to 2).
- User endpoint: `张三丰`→`三丰` (unchanged), `Alice`→`A`, `李雷Han`→`李雷`,
  emoji/empty→ascii fallback.
- ETag pins updated: group `group-name-v4`, user `name-v5`; both pin tests green.
- `go build ./...`; group+user avatar tests green (clean test DB); `golangci-lint`
  0; `make i18n-lint` pass; `Test{Group,User}NoLegacyResponseError` guards green.
