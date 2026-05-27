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
// request context. User-language override is intentionally left for a later
// late-stage middleware once the DB/Redis/token source of truth is available.
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
		c.Next()
	}
}
