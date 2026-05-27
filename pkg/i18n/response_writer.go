package i18n

import "github.com/gin-gonic/gin"

// contentLanguageWriter is a gin.ResponseWriter shim that refreshes the
// Content-Language header right before the response is committed.
//
// EarlyMiddleware writes a tentative Content-Language based on the pre-auth
// negotiation (Accept-Language / default). AuthMiddleware later hydrates
// wkhttp.UserInfo on the request context, after which LanguageFromContext
// performs the read-side late-stage merge and may return a higher-priority
// LanguageSourceUser decision (D9). Without this wrapper, success responses
// would keep advertising the pre-auth Content-Language even when the body
// was already localised to the user's preferred language — observably wrong
// per RFC 9110 and inconsistent with ErrorRenderer, which already
// re-evaluates the language at render time.
//
// Why a writer wrapper rather than a post-auth gin middleware: AuthMiddleware
// is mounted per route group, not globally, and there are ~36 mount sites.
// Wrapping the writer once in EarlyMiddleware avoids touching every group
// and keeps the late-merge invariant local to pkg/i18n.
//
// Header mutation timing: gin's responseWriter buffers headers until
// WriteHeaderNow is called (either explicitly or via Write/WriteString).
// Overriding those three methods to first stamp Content-Language guarantees
// the header reflects the final merged decision regardless of how the
// handler writes the body (c.JSON, c.String, c.Data, …).
type contentLanguageWriter struct {
	gin.ResponseWriter
	c         *gin.Context
	earlyLang string
}

func newContentLanguageWriter(c *gin.Context, earlyLang string) *contentLanguageWriter {
	return &contentLanguageWriter{ResponseWriter: c.Writer, c: c, earlyLang: earlyLang}
}

// applyLanguage stamps the late-merged Content-Language onto the response.
// Idempotent and best-effort: once Written() is true the header has already
// shipped and any update would be silently ignored, so the method short-
// circuits. Reading LanguageFromContext lazily — rather than capturing a
// snapshot at wrap time — is essential because AuthMiddleware replaces
// c.Request to inject the UserInfo-bearing context.
func (w *contentLanguageWriter) applyLanguage() {
	if w.Written() {
		return
	}
	decision, ok := LanguageFromContext(w.c.Request.Context())
	if !ok || decision.Language == "" || decision.Language == w.earlyLang {
		return
	}
	w.Header().Set("Content-Language", decision.Language)
}

func (w *contentLanguageWriter) Write(data []byte) (int, error) {
	w.applyLanguage()
	return w.ResponseWriter.Write(data)
}

func (w *contentLanguageWriter) WriteString(s string) (int, error) {
	w.applyLanguage()
	return w.ResponseWriter.WriteString(s)
}

func (w *contentLanguageWriter) WriteHeaderNow() {
	w.applyLanguage()
	w.ResponseWriter.WriteHeaderNow()
}
