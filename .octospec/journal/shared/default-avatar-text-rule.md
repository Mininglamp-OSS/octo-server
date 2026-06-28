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

## PR #494 评审轮:批量修复(4 位 reviewer)
4 位 reviewer(Jerry-Xin / Octo-Q / OctoBoooot / yujiawei)全 Approve(非阻塞)。按
用户指示「等一波齐了一次性修」,合并为一轮:

**`initials` 两处真 bug(测试碰巧没盖到)**
- **空格未在切词**:`visibleRunes` 在分词前剥了空格,只剩驼峰/标点在切 → `dev team→D`
  (应 `DT`)。修:`initials` 改吃**原始 name**(保留空格)。`Backend Team→BT` 原是靠
  驼峰边界蒙对的。
- **数字阻断驼峰**:`prev` 记成了数字 → `Web3Team→W`(应 `WT`)。修:跟踪上一个**字母**
  的大小写(顺带消除 Octo-Q 的 prev-scope nit)。

**D — 非汉字脚本塌成 1 字(产品定:日韩一起修)**
`extractAvatarText` 的 CJK 分支从 `unicode.Han` 扩到 **Han + Hangul + Hiragana +
Katakana**(`isCJKGlyph`):`김철수→김철`/`철수`、`さとう→さと`/`とう`、片假名同。拉丁仍走
缩写;西里尔/阿拉伯/泰文等「无空格的有大小写字母」单词仍塌 1 字,文档标注为已知限制
(超出 zh-CN/en-US+CJK 范围)。

**无需再 bump 版本**:v4/v5 尚未发布,B/C/D 都并入这首个携带新规则的版本,旧 v3/name-v4
缓存在部署时一并失效,中间没有 v4-without-BCD 上过线。

**测试/注释补强**
- `TestGroupAvatarGetCustomTextNotTruncated`:4 字自定义 `研发中心` 原样渲染、不被截成
  前 2(Jerry-Xin 🟡,守 custom/auto 分流)。
- `TestGroupNameText`/`TestIndividualText` 增 D(日韩假名)/B(`dev team`、`HR BP`)/
  C(`Web3Team`)/`张123`/西里尔限制 用例。
- 清理 stale「群名前 4 字」注释 → 「前 2 字」(service.go/db.go/const.go/api.go/swagger;
  自定义 ≤4 的「4 个字符」措辞不动;迁移文件历史记录不改)。

**验证**(测试环境本地真起:MySQL8 + Redis + WuKongIM):取字单测 + 群/个人端点(含 A1
+ pin v4/v5)全绿;#480 饿死 21.8x→0.9x 无回归;go vet 0、golangci-lint 0、i18n-lint、
NoLegacy 守卫全过。

**无 action**:Octo-Q `2024春招群→春招`(digit+Han 设计,已签)。

### PR #494 评审第三批:ZWSP 修复 + 甄别两条 false finding
重审 doc-sweep 头(fa4c8d8a)又出几条,逐一甄别:
- **ZWSP 真 bug(且是 doc-sweep 自己引入的 doc-vs-code 矛盾)**:`initials` 把零宽符
  (Cc/Cf 非空白)也当词分隔符 → `dev<ZWSP>ops→DO`(本该 D),既和我加的「invisible
  chars are ignored」注释相反、也和 CJK 分支(剥零宽符)不一致。修:`initials` 里零宽符
  改成**忽略**(新增 `isZeroWidth`;空格/标点仍分词)→ `dev<ZWSP>ops→D`,对齐文档 + CJK
  分支语义。加 `dev<ZWSP>ops→D` pin(双 suite)。无需 bump 版本(v4/v5 未发布)。
- **甄别掉两条 false finding**(没动代码):Octo-Q P2-1「`API2Gateway→AG`、测试该 FAIL」
  —— 实测 `→A`、子测试 PASS(重跑确认),**不能改成 AG**(改了才会挂);OctoBoooot 自己
  随后也更正。Octo-Q P2-3「custom/auto 共用 CacheKey 碰撞」—— `text` 是 key 因子,文字
  相同→像素相同→复用本就正确,CacheKey 长度分帧单射,非问题(OctoBoooot byte-verify)。
- doc nit:`initials` 注释补 `APIGateway→A`、`group.go:18` 区分自动名(≤2)/自定义(≤4)。
- 验证:全 avatarrender 单测(含新 ZWSP 用例)+ 群/个人端点 + #480 饿死(0.9x)全绿;
  vet/lint/i18n/NoLegacy 守卫过。
