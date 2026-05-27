package i18n

import (
	"net"

	"github.com/gin-gonic/gin"
)

// MiddlewareOptions controls request language negotiation.
type MiddlewareOptions struct {
	DefaultLanguage        string
	TrustedLangHeaderCIDRs []*net.IPNet
	TrustedProxyCIDRs      []*net.IPNet
}

// EarlyMiddleware negotiates language before auth and stores the decision in
// request context. The user-language override is applied lazily on the read
// side by LanguageFromContext (see ctx.go) so handlers and renderers see the
// merged decision without per-route post-auth middleware. The wrapped
// response writer keeps the Content-Language header consistent with that
// merged decision for successful responses (the ErrorRenderer path already
// re-evaluates at render time).
func EarlyMiddleware(opts MiddlewareOptions) gin.HandlerFunc {
	return func(c *gin.Context) {
		decision := NegotiateLanguage(c.Request, LanguageNegotiationOptions{
			DefaultLanguage:        opts.DefaultLanguage,
			TrustedLangHeaderCIDRs: opts.TrustedLangHeaderCIDRs,
			TrustedProxyCIDRs:      opts.TrustedProxyCIDRs,
		})
		c.Request = c.Request.WithContext(WithLanguage(c.Request.Context(), decision))
		c.Writer.Header().Set("Content-Language", decision.Language)
		addVary(c.Writer.Header(), "Accept-Language", HeaderOctoLang, "Cookie")
		c.Writer = newContentLanguageWriter(c, decision.Language)
		c.Next()
	}
}
