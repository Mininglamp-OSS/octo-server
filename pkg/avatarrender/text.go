package avatarrender

import "unicode"

// extractAvatarText derives the ≤limit-glyph display text for a default avatar
// from a free-form name, script-aware (first match wins):
//  1. strip invisible (space/Cc/Cf); empty → "" (caller falls back to an icon)
//  2. any CJK glyph (Han / Hangul / Hiragana / Katakana) → those glyphs only
//     (drop Latin/digits/symbols/other scripts), clamp to limit
//  3. else pure digits → clamp to limit
//  4. else has a letter → initials over the *original* name: whitespace and
//     punctuation split tokens (plus camelCase), while zero-width/control chars are
//     ignored — so a space separates words but a zero-width char inside one does not;
//     ≤limit, uppercase. (The CJK/digit branches above run on the invisible-stripped
//     runes; only initials needs the raw name, for the whitespace split.)
//  5. else (pure symbol / emoji) → "" (icon)
//
// fromEnd picks trailing glyphs in the CJK/digit cases (personal 后N); initials
// are always leading. Known limitation: a cased-but-non-Latin alphabet *without*
// word spaces — a single Cyrillic / Greek / Arabic / Thai word — falls into the
// initials branch and collapses to one glyph (e.g. "Анна"→"А"); that is outside
// the zh-CN/en-US + CJK scope this rule targets. The result may still contain a
// rune with no glyph in the avatar font (rare Han); callers pair this with
// Renderable and fall back to an icon when it is not renderable.
func extractAvatarText(name string, fromEnd bool, limit int) string {
	rs := visibleRunes(name)
	if len(rs) == 0 {
		return ""
	}
	// CJK ideographic/syllabic scripts (no word spaces) → take those glyphs only.
	cjk := make([]rune, 0, len(rs))
	for _, r := range rs {
		if isCJKGlyph(r) {
			cjk = append(cjk, r)
		}
	}
	if len(cjk) > 0 {
		return string(clampRunes(cjk, fromEnd, limit))
	}
	allDigit := true
	for _, r := range rs {
		if !unicode.IsDigit(r) {
			allDigit = false
			break
		}
	}
	if allDigit {
		return string(clampRunes(rs, fromEnd, limit))
	}
	for _, r := range rs {
		if unicode.IsLetter(r) {
			// Pass the original name (whitespace preserved) so initials splits on it.
			return initials(name, limit)
		}
	}
	return ""
}

// isCJKGlyph reports whether r is a CJK ideographic/syllabic glyph that should be
// rendered 1:1 (taken N-at-a-time) rather than abbreviated to an initial: Han
// (Chinese / Japanese kanji), Hangul (Korean), and Japanese Hiragana / Katakana.
func isCJKGlyph(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r)
}

// clampRunes returns at most limit runes, trailing when fromEnd else leading.
func clampRunes(rs []rune, fromEnd bool, limit int) []rune {
	if len(rs) <= limit {
		return rs
	}
	if fromEnd {
		return rs[len(rs)-limit:]
	}
	return rs[:limit]
}

// isZeroWidth reports whether r is a non-whitespace control/format rune (Cc/Cf, e.g.
// ZWSP U+200B or BOM U+FEFF) — an invisible character that should be ignored rather
// than treated as a word boundary. Whitespace (which IS a word separator) is excluded.
func isZeroWidth(r rune) bool {
	return !unicode.IsSpace(r) && (unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r))
}

// initials returns up to limit uppercase first-letters, one per token. Tokens split
// on whitespace and on any other non-letter/digit run (punctuation/symbols), plus on
// camelCase boundaries (a lowercase letter immediately followed by an uppercase one).
// Zero-width / control chars (Cc/Cf, e.g. ZWSP/BOM) are ignored, not word breaks —
// matching the invisible-stripping in the CJK/digit branches — so a zero-width char
// inside a word does not split it. A digit is not a separator either: it only
// preserves the previous letter's case, so a subsequent lower→Upper boundary still
// splits ("Web3Team"→"WT") but a digit between same-case letters does not
// ("dev2team"→"D"). An all-uppercase acronym has no camelCase boundary, so it stays
// one token ("API2Gateway"→"A", "APIGateway"→"A"). A token with no letter contributes
// nothing. Examples: "Backend Team"→"BT", "dev team"→"DT", "my-team"→"MT",
// "myCoolGroup"→"MC".
//
// The returned initials are upper-cased and may include a non-renderable letter (a
// single Cyrillic/Greek word → one glyph); callers pair this with Renderable and fall
// back to an icon, the same as the other extractAvatarText branches.
func initials(name string, limit int) string {
	out := make([]rune, 0, limit)
	var prevLetter rune // last letter in the current token; 0 at a token boundary
	took := false       // already emitted an initial for the current token
	for _, r := range name {
		if isZeroWidth(r) {
			continue // zero-width / control: ignored, not a word break
		}
		if unicode.IsSpace(r) || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			took, prevLetter = false, 0 // whitespace or punctuation → end token
			continue
		}
		if unicode.IsUpper(r) && unicode.IsLower(prevLetter) {
			took = false // camelCase boundary → new token
		}
		if !took && unicode.IsLetter(r) {
			out = append(out, unicode.ToUpper(r))
			took = true
			if len(out) == limit {
				return string(out)
			}
		}
		if unicode.IsLetter(r) {
			prevLetter = r
		}
	}
	return string(out)
}

// GroupNameText derives a group's default-avatar text from its NAME: script-aware,
// leading 2 glyphs —— CJK(汉字/假名/谚文)前2 / 纯数字前2 / 纯拉丁等取首字母缩写
// (≤2、大写) / 否则空(回退群组图标)。混排有 CJK 时只取 CJK(忽略拉丁/数字/符号)。
//
// 仅用于「群名自动取字」。用户显式设置的自定义头像文字走 GroupText(原样渲染、≤4),
// 不经过本规则。返回结果可能仍含本字体无字形的字符(罕见生僻字),调用方应配合
// Renderable 判断,对不可渲染的结果回退到群组图标。
func GroupNameText(name string) string {
	return extractAvatarText(name, false, 2)
}

// IndividualText 返回个人默认头像应显示的文字:script 感知、**后** 2 字 ——
// CJK(汉字/假名/谚文)取后2(混排时只取 CJK)、纯数字后2、纯拉丁等取首字母缩写
// (≤2、大写)、否则空(调用方回退 ASCII 兜底)。
//
// 个人取**后**两字(区别于群名 GroupNameText 取前2):中文昵称后缀(名)更具辨识度
// (张三丰→三丰、王小明→小明,同钉钉/飞书)。空白/控制/零宽字符在计数前剔除。结果可能
// 含本字体无字形的字符(如 emoji),调用方应配合 Renderable 判断并回退。
func IndividualText(nickname string) string {
	return extractAvatarText(nickname, true, 2)
}

// GroupText 规范化用户**显式设置**的自定义群头像文字:取可见字符前 4(PRD:中/英文
// 最多 4 字符)。空白/控制/零宽字符在计数前剔除。
//
// 注意:本函数用于「自定义 avatar_text」的清洗(写入校验 + 渲染原样),**不是**群名
// 自动取字 —— 后者走 GroupNameText(script 感知、前2)。返回结果可能仍含本字体无字形
// 的字符,调用方应配合 Renderable 判断,对不可渲染的结果回退到群组图标。
func GroupText(name string) string {
	cleaned := visibleRunes(name)
	if len(cleaned) > 4 {
		cleaned = cleaned[:4]
	}
	return string(cleaned)
}

// VisibleRuneCount 返回 s 去除不可见字符后的可见 rune 数,供调用方校验自定义头像
// 文字长度(PRD:最多 4 个中文/英文字符)。
func VisibleRuneCount(s string) int {
	return len(visibleRunes(s))
}

// visibleRunes 返回 s 中剔除不可见字符后的 rune 序列。
func visibleRunes(s string) []rune {
	cleaned := make([]rune, 0, len(s))
	for _, r := range s {
		if isInvisible(r) {
			continue
		}
		cleaned = append(cleaned, r)
	}
	return cleaned
}

// isInvisible 报告 r 是否为不应在头像上占位的不可见字符:空白(含全角空格、
// 不间断空格)、控制字符、Unicode 格式字符(零宽连接符/BOM 等)。
func isInvisible(r rune) bool {
	return unicode.IsSpace(r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}
