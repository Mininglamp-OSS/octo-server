// Package accesslog provides a gin access-log formatter that scrubs secrets
// embedded in request URL paths before they are written to logs.
//
// Motivation: the incoming-webhook push route is /v1/incoming-webhooks/
// {webhook_id}/{token} — a Discord-style URL with a PLAINTEXT bearer token in
// the path (#246). gin's default access logger writes the full request path,
// which would persist live tokens into access logs. Formatter mirrors gin's
// default line format exactly but masks the token segment.
package accesslog

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// maskedToken replaces a plaintext token segment in logged paths.
	maskedToken = "***"
	// incomingWebhookPrefix is the push route whose trailing path segment is a
	// plaintext webhook token.
	incomingWebhookPrefix = "/v1/incoming-webhooks/"
)

// ScrubPath masks secrets embedded in a request path before it is logged.
//
// It targets the incoming-webhook push route
// /v1/incoming-webhooks/{webhook_id}/{token}: the trailing {token} segment (and
// anything after it, e.g. a stray query string) is replaced with "***". The
// non-secret {webhook_id} is preserved so logs remain useful for correlation.
// Any path without the prefix, or with no token segment, is returned unchanged.
//
// The prefix match is case-INSENSITIVE: gin logs c.Request.URL.Path verbatim
// for every request including 404s (the access logger runs after c.Next()
// regardless of route match), so a request to /V1/INCOMING-WEBHOOKS/... that
// 404s on the case-sensitive router would otherwise still log the token
// unmasked. Scrubbing is the security control here, so it must survive
// path-casing variants. Original casing of the prefix/webhook_id is preserved
// in the output; only the token segment is replaced.
func ScrubPath(path string) string {
	if len(path) < len(incomingWebhookPrefix) ||
		!strings.EqualFold(path[:len(incomingWebhookPrefix)], incomingWebhookPrefix) {
		return path
	}
	rest := path[len(incomingWebhookPrefix):]
	// rest == "{webhook_id}/{token}[?query]". The token is everything after the
	// first '/'. No '/' means only {webhook_id} is present — nothing to mask.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return path
	}
	return path[:len(incomingWebhookPrefix)] + rest[:slash] + "/" + maskedToken
}

// tokenInText matches a webhook push path embedded anywhere in free-form log
// text and captures the prefix up to and including the webhook_id segment; the
// trailing token segment is everything up to the next whitespace / quote / ? .
// Used to scrub tokens from the gin.Recovery panic dump, which writes the full
// request line (httputil.DumpRequest) including the token-bearing path.
// Case-insensitive ((?i)) for the same reason as ScrubPath — a non-canonical
// path casing must not slip a token past the mask.
var tokenInText = regexp.MustCompile(`(?i)(/v1/incoming-webhooks/[^/\s?"']+/)[^\s?"']+`)

// scrubbingErrorWriter wraps an io.Writer and masks incoming-webhook tokens in
// everything written through it. ScrubPath only covers the access-log line;
// this covers the OTHER sink — gin.Recovery's panic dump, which bypasses the
// access logger entirely and would otherwise persist a plaintext token on any
// panic while serving the push route (#246).
type scrubbingErrorWriter struct{ w io.Writer }

// NewErrorWriter wraps w so that incoming-webhook tokens are masked before they
// reach it. Assign it to gin.DefaultErrorWriter BEFORE the recovery middleware
// is registered (gin.Recovery captures DefaultErrorWriter at registration time).
func NewErrorWriter(w io.Writer) io.Writer { return scrubbingErrorWriter{w: w} }

// Write masks tokens in p and forwards it. INVARIANT: the regex runs on each p
// independently with no carry-over buffer, so it assumes a token never splits
// across two Write calls. This holds for the only current caller —
// gin.Recovery → log.Logger.Output assembles the whole panic message (with the
// token-bearing request line from httputil.DumpRequest embedded) into a single
// Write. A future caller that writes in chunks (io.Copy, a chunked middleware)
// could split a token across Writes and bypass the mask; if that becomes a
// possibility, buffer until newline here.
func (s scrubbingErrorWriter) Write(p []byte) (int, error) {
	scrubbed := tokenInText.ReplaceAll(p, []byte("${1}***"))
	if _, err := s.w.Write(scrubbed); err != nil {
		return 0, err
	}
	// Report the original length: all of p was consumed (the byte-count change
	// from masking is internal and must not look like a short write).
	return len(p), nil
}

// Formatter is a gin.LogFormatter that reproduces gin's default access-log line
// byte-for-byte except the request path is run through ScrubPath. Wire it via
// gin.LoggerWithFormatter(accesslog.Formatter).
func Formatter(param gin.LogFormatterParams) string {
	var statusColor, methodColor, resetColor string
	if param.IsOutputColor() {
		statusColor = param.StatusCodeColor()
		methodColor = param.MethodColor()
		resetColor = param.ResetColor()
	}

	if param.Latency > time.Minute {
		param.Latency = param.Latency.Truncate(time.Second)
	}
	return fmt.Sprintf("[GIN] %v |%s %3d %s| %13v | %15s |%s %-7s %s %#v\n%s",
		param.TimeStamp.Format("2006/01/02 - 15:04:05"),
		statusColor, param.StatusCode, resetColor,
		param.Latency,
		param.ClientIP,
		methodColor, param.Method, resetColor,
		ScrubPath(param.Path),
		param.ErrorMessage,
	)
}
