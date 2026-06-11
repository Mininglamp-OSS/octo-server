package avatarrender

import "unicode"

// IndividualText 返回个人默认头像应显示的文字：昵称中**可见字符**的后两个（按
// Unicode rune 计），不足两字时取全部可见字符。空白、控制字符、零宽/格式字符
// （如 U+200B ZWSP、U+FEFF BOM）在计数前会被剔除——它们在头像里不可见，不应占位。
//
// 返回结果可能仍含本字体无字形的字符（如 emoji），调用方应配合 Renderable 判断，
// 对不可渲染的结果回退到其它兜底图。
//
// 已知限制：按 rune 截取，对由多个 rune 组合而成的 emoji（带肤色修饰符、ZWJ
// 序列）可能切断；个人头像本期不要求 emoji 支持。
func IndividualText(nickname string) string {
	cleaned := make([]rune, 0, len(nickname))
	for _, r := range nickname {
		if isInvisible(r) {
			continue
		}
		cleaned = append(cleaned, r)
	}
	if len(cleaned) <= 2 {
		return string(cleaned)
	}
	return string(cleaned[len(cleaned)-2:])
}

// isInvisible 报告 r 是否为不应在头像上占位的不可见字符：空白（含全角空格、
// 不间断空格）、控制字符、Unicode 格式字符（零宽连接符/BOM 等）。
func isInvisible(r rune) bool {
	return unicode.IsSpace(r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}
