package i18n

import (
	"context"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

type languageContextKey struct{}

// LanguageDecision 记录一次语言协商结果及其来源。
type LanguageDecision struct {
	Language string
	Source   LanguageSource
}

// WithLanguage 将语言协商结果写入 context.Context。
func WithLanguage(ctx context.Context, decision LanguageDecision) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, languageContextKey{}, decision)
}

// WithLanguageIfHigherPriority 仅当新来源优先级高于现有来源时覆盖 context。
// 这用于 D9 两段式中间件：Auth 后的 user.language 可以覆盖 Accept-Language/default，
// 但不能覆盖 trusted header、URL 或 cookie 这类显式选择。
func WithLanguageIfHigherPriority(ctx context.Context, decision LanguageDecision) (context.Context, bool) {
	current, ok := LanguageFromContext(ctx)
	if !ok || languageSourcePriority(decision.Source) > languageSourcePriority(current.Source) {
		return WithLanguage(ctx, decision), true
	}
	return ctx, false
}

// LanguageFromContext 读取 context 中的语言协商结果。
//
// 除了 EarlyMiddleware 在 ctx 中写入的早期协商决策（trusted header / query /
// cookie / Accept-Language / default），本函数还会顺带读取
// wkhttp.UserFromCtx() 注入的 UserInfo.Language——它是 AuthMiddleware 经由
// CacheTokenParser + UserLanguageResolver 解析出的"用户偏好真相源"。
//
// D9 两段式协商：EarlyMiddleware 不能在 Auth 前知道 user.language，所以这里把
// "用户偏好覆盖" 做成读侧延迟合并。只有当 user.language 非空且来源优先级
// （LanguageSourceUser=30）严格高于 ctx 中现有决策时（典型场景：现有为
// LanguageSourceAccept=20 或 LanguageSourceDefault=10），才让用户偏好生效。
// trusted header / query / cookie 这类显式选择仍然胜出（D6 优先级顺序）。
func LanguageFromContext(ctx context.Context) (LanguageDecision, bool) {
	if ctx == nil {
		return LanguageDecision{}, false
	}
	decision, ok := ctx.Value(languageContextKey{}).(LanguageDecision)
	if ok && decision.Language == "" {
		ok = false
	}
	if user, hasUser := wkhttp.UserFromCtx(ctx); hasUser && user.Language != "" {
		if !ok || languageSourcePriority(LanguageSourceUser) > languageSourcePriority(decision.Source) {
			return LanguageDecision{Language: user.Language, Source: LanguageSourceUser}, true
		}
	}
	if !ok {
		return LanguageDecision{}, false
	}
	return decision, true
}

// LanguageOrDefault 返回 context 中的语言；不存在时返回 fallback。
func LanguageOrDefault(ctx context.Context, fallback string) string {
	if decision, ok := LanguageFromContext(ctx); ok {
		return decision.Language
	}
	return fallback
}

// OutboundLanguage 解析「发出方内容」（邮件等）应使用的语言。
//
// 与请求响应不同，邮件常在脱离请求 ctx 的路径上生成（异步 goroutine、未登录
// 的验证码发送），此时 ctx 里没有早期协商决策，也没有 UserInfo。本函数把
// LanguageFromContext 的结果与 OCTO_DEFAULT_LANGUAGE 兜底合并：拿得到协商语言
// 就用它，否则退回部署默认语言。
//
// 这样调用点写法统一（一行拿 lang），且当未来把请求 ctx 或收件人语言接入这条
// 链路时无需改动发送层与模板层——结果会自动从 default 切换到真实语言。
func OutboundLanguage(ctx context.Context) string {
	def, err := DefaultLanguageFromEnv()
	if err != nil {
		def = DefaultLanguage
	}
	return LanguageOrDefault(ctx, def)
}

func languageSourcePriority(source LanguageSource) int {
	switch source {
	case LanguageSourceTrustedHeader:
		return 60
	case LanguageSourceQuery:
		return 50
	case LanguageSourceCookie:
		return 40
	case LanguageSourceUser:
		return 30
	case LanguageSourceAccept:
		return 20
	case LanguageSourceDefault:
		return 10
	default:
		return 0
	}
}
