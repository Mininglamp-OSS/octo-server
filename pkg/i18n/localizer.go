package i18n

import (
	"github.com/nicksnyder/go-i18n/v2/i18n"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// Localizer 是 SDK 对外的翻译入口。renderer / 业务门面 / 中间件均通过此接口
// 把 (code, lang, params) 解析为最终展示给客户端的 message。
type Localizer interface {
	// Translate 将 code 解析为指定 lang 的本地化字符串。
	//
	// 入参：
	//   - code:   稳定 i18n key，必须在 codes.Register 已登记。
	//   - lang:   客户端协商出的 BCP-47 标签（"zh-CN" / "en-US"）。
	//             空字符串表示直接走 fallback chain。
	//   - params: 翻译模板插值数据（仅 string-safe key/value，敏感字段由
	//             调用方过滤；见 pkg/i18n/params.go——后续 step 实现）。
	//
	// fallback chain（D22 六级，简化为四级实测有效路径）：
	//   1. bundle: requested lang → fallback lang → source lang（go-i18n 自动）
	//   2. extreme fallback: codes.Code.DefaultMessages[lang]
	//   3. codes.Code.DefaultMessage（source 原文）
	//   4. code ID 本身（未注册 code，调用方应记 i18n_unknown_code_total）
	Translate(code, lang string, params map[string]any) string
}

// NewLocalizer 构造默认 Localizer。fallbackLang 通常对应 DM_DEFAULT_LANGUAGE
// （主方案 D5），空串则使用 SourceLanguage。
//
// Localizer 实例线程安全且无状态——所有数据从 Bundle 单例读取，
// 多 goroutine 可共享同一实例。
func NewLocalizer(fallbackLang string) Localizer {
	if fallbackLang == "" {
		fallbackLang = SourceLanguage
	}
	return &defaultLocalizer{fallbackLang: fallbackLang}
}

type defaultLocalizer struct {
	fallbackLang string
}

func (l *defaultLocalizer) Translate(code, lang string, params map[string]any) string {
	b, err := Bundle()
	if err != nil || b == nil {
		// Bundle 加载失败：完全脱离 go-i18n，走 codes registry 兜底。
		// 此分支理论上只在 embed FS 损坏或 init() 顺序错乱时触发。
		return fallbackMessage(code, lang, l.fallbackLang)
	}

	tags := buildLangTags(lang, l.fallbackLang)
	loc := i18n.NewLocalizer(b, tags...)

	msg, lerr := loc.Localize(&i18n.LocalizeConfig{
		MessageID:    code,
		TemplateData: params,
	})
	if lerr != nil || msg == "" {
		// go-i18n 找不到对应 message（code 未注册 / TOML 未含）→ 走兜底链。
		// 不向上抛错：客户端宁可看到 source 文案，也不应看到 500。
		return fallbackMessage(code, lang, l.fallbackLang)
	}
	return msg
}

// fallbackMessage 实现 bundle 不可用时的多级兜底。
//
// 顺序：
//  1. codes.Code.DefaultMessages[lang]   — 精确语言兜底
//  2. codes.Code.DefaultMessages[fallback] — 默认语言兜底
//  3. codes.Code.DefaultMessage          — source 原文
//  4. 入参 code 字符串                    — 未注册 code 的最后兜底
//
// 第 4 步返回 code ID 而非空串：调用方拿到 ID 可判定缺失并打日志/指标
// （i18n_unknown_code_total），同时客户端不会拿到空 message 触发 UI 异常。
func fallbackMessage(code, lang, fallback string) string {
	c, ok := codes.Lookup(code)
	if !ok {
		return code
	}
	if c.DefaultMessages != nil {
		if m, ok := c.DefaultMessages[lang]; ok && m != "" {
			return m
		}
		if fallback != "" && fallback != lang {
			if m, ok := c.DefaultMessages[fallback]; ok && m != "" {
				return m
			}
		}
	}
	return c.DefaultMessage
}

// buildLangTags 组装 go-i18n Localizer 的语言优先级链：
// requested → fallback → source，按出现顺序去重。
//
// 例：lang="zh-CN", fallback="zh-CN" → ["zh-CN", "en-US"]
//
//	lang="en-US", fallback="zh-CN" → ["en-US", "zh-CN"]（source 已在前列，去重后不再追加）
//	lang="",      fallback="zh-CN" → ["zh-CN", "en-US"]
func buildLangTags(requested, fallback string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	for _, t := range []string{requested, fallback, SourceLanguage} {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
