---
type: Journal
title: "Journal: default-avatar-text-rule"
description: Script-aware 2-glyph text rule for group + personal default avatars (Han-only on mixed, initials for English, icon/ascii fallback)
tags: [avatar, render, cache]
timestamp: 2026-06-27
slug: default-avatar-text-rule
---

# Journal: default-avatar-text-rule

## What was done
Reworked the default (un-uploaded) avatar text extraction for BOTH group and
personal avatars. Old rule: group = first 4 runes, personal = last 2 runes, no
script awareness → awkward output for English / mixed / digit names
(`Backend Team`→`Back`, `Bug反馈群`→`Bug反` cramped 2×2, `Alice`→`ce`).

New rule (product/UI signed off via sample renders), shared script-aware core in
`pkg/avatarrender/text.go` (`extractAvatarText`):

1. strip invisible (space/Cc/Cf); empty → "" → caller falls back to an icon
2. any Han → Han chars only (drop Latin/digits/symbols), clamp to 2
3. else pure digits → clamp to 2
4. else has a letter → initials (first letter per token, camelCase/sep split,
   ≤2, uppercase; single word → 1 letter)
5. else (pure symbol/emoji) → "" → icon

Direction: group `GroupNameText` takes **leading** 2 (前2); personal
`IndividualText` keeps **trailing** 2 (后2) for Han/digits (DingTalk/Feishu
convention — the given-name suffix is more distinctive). Initials lead for both.

Worked examples — group: 后端架构讨论→后端, Bug反馈群→反馈, 2024春招群→春招,
Backend Team→BT, Sales→S, 2024→20, 🎉🎉/空→two-person icon. Personal: 张三丰→三丰
(unchanged), Alice→A, 李雷Han→李雷, emoji/空→ascii fallback.

## Key decisions
- **Custom vs auto split.** `GroupText` (the custom `avatar_text` normalizer,
  ≤4) is KEPT unchanged — it is also used on the write path
  (`modules/group/api.go:793,1001`). The avatar handler now branches: user-set
  `avatar_text` → `GroupText` (rendered as-is); else group Name → `GroupNameText`
  (new rule). So an explicit custom text is never truncated to 2 or initial-ized.
- **Cache-version bump (the #486 lesson, extended).** Derived bytes change →
  bumped `group-name-v3→v4` and `name-v4→v5` at BOTH the ETag and CacheKey sites
  (verified consistent). `group-icon-v3` and `ascii-v1` unchanged — their
  renderers are not touched. The version-pin tests were updated to v4/v5.

## Scope / deferred
Out of scope (separate follow-ups): personal ascii white corners (#486 ①),
CacheKey-version pin hardening (#486 ②), unnamed/auto-member-name groups using
the icon instead of text (③, needs an `is_named` flag — auto-named groups still
render Han前2 of the joined member names).

## Verification
Local green: `go build ./...`; `go vet`; `pkg/avatarrender` text-rule units
(`GroupNameText`/`IndividualText`/`GroupText`); `golangci-lint` 0; `make
i18n-lint` + `i18n-extract-check`; `Test{Group,User}NoLegacyResponseError` guards.
**Deferred to CI**: group/user avatar **endpoint** tests + version-pin tests —
the local MySQL/Redis/WuKongIM infra was reclaimed mid-session (ephemeral env),
so `NewTestServer` can't run locally; CI provides a clean DB (as for #486).

## Learning
The render-version bump must accompany ANY change that alters derived render
bytes — not only pixel/style changes (#486 transparent corners) but **text-rule**
changes too (this task). The ETag is CRC32 over content *factors*, so a text-rule
change with unchanged factors would otherwise serve stale 304s. Encoded at the
ETag call sites; the endpoint version-pin tests guard it.
