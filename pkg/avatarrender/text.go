package avatarrender

import "unicode"

// extractAvatarText derives the ≤limit-glyph display text for a default avatar
// from a free-form name, script-aware (命中即止):
//  1. strip invisible chars (space/Cc/Cf); empty → "" (caller falls back to icon)
//  2. any Han → Han chars only (drop Latin/digits/symbols), clamp to limit
//  3. else pure digits → clamp to limit
//  4. else has a letter → initials (first letter per token, ≤limit, uppercase)
//  5. else (pure symbol / emoji) → "" (caller falls back to icon)
//
// fromEnd picks trailing glyphs in the Han/digit cases (personal 后N); initials
// are always leading. The result may still contain a rune with no glyph in the
// avatar font (rare Han); callers pair this with Renderable and fall back to an
// icon when it is not renderable.
func extractAvatarText(name string, fromEnd bool, limit int) string {
	rs := visibleRunes(name)
	if len(rs) == 0 {
		return ""
	}
	han := make([]rune, 0, len(rs))
	for _, r := range rs {
		if unicode.Is(unicode.Han, r) {
			han = append(han, r)
		}
	}
	if len(han) > 0 {
		return string(clampRunes(han, fromEnd, limit))
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
			return initials(rs, limit)
		}
	}
	return ""
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

// initials returns up to limit uppercase first-letters, one per token. Tokens
// split on any run of non-letter/digit chars and on camelCase (lower→Upper)
// boundaries; a token with no letter contributes nothing. So "Backend Team" →
// "BT", "Sales" → "S", "myCoolGroup" → "MC".
func initials(rs []rune, limit int) string {
	var toks [][]rune
	var cur []rune
	var prev rune
	for _, r := range rs {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if len(cur) > 0 {
				toks = append(toks, cur)
				cur = nil
			}
			prev = r
			continue
		}
		if unicode.IsUpper(r) && len(cur) > 0 && unicode.IsLower(prev) {
			toks = append(toks, cur)
			cur = nil
		}
		cur = append(cur, r)
		prev = r
	}
	if len(cur) > 0 {
		toks = append(toks, cur)
	}
	out := make([]rune, 0, limit)
	for _, t := range toks {
		for _, r := range t {
			if unicode.IsLetter(r) {
				out = append(out, unicode.ToUpper(r))
				break
			}
		}
		if len(out) == limit {
			break
		}
	}
	return string(out)
}

// GroupNameText derives a group's default-avatar text from its NAME: script-aware,
// leading 2 glyphs —— 汉字前2 / 纯数字前2 / 纯英文首字母缩写(≤2、大写) / 否则空
// (回退群组图标)。混排有汉字时只取汉字(忽略拉丁/数字/符号)。
//
// 仅用于「群名自动取字」。用户显式设置的自定义头像文字走 GroupText(原样渲染、≤4),
// 不经过本规则。返回结果可能仍含本字体无字形的字符(罕见生僻字),调用方应配合
// Renderable 判断,对不可渲染的结果回退到群组图标。
func GroupNameText(name string) string {
	return extractAvatarText(name, false, 2)
}

// IndividualText 返回个人默认头像应显示的文字:script 感知、**后** 2 字 ——
// 汉字取后2(混排时只取汉字)、纯数字后2、纯英文取首字母缩写(≤2、大写)、否则空
// (调用方回退 ASCII 兜底)。
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
