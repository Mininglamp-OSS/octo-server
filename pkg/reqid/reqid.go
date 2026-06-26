// Package reqid threads a per-request trace id through one HTTP request so the
// otherwise-separate slow logs (GIN access log, auth parser, group handler,
// group service) can be stitched back to a single request. It mirrors the
// existing modules/oidc/logging.go newTraceID pattern but is request-scoped and
// reusable across packages (middleware + pkg/auth + modules/*).
//
// The id lives in two places per request:
//   - gin.Context key (GinKey) → readable by the access-log formatter via
//     gin.LogFormatterParams.Keys and by handlers via c.GetString(GinKey).
//   - request context.Context (via WithTraceID) → readable by code that only
//     sees a context.Context, e.g. pkg/auth's CacheTokenParser.Parse.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

type ctxKey struct{}

// GinKey is the gin.Context / LogFormatterParams.Keys key holding the trace id.
const GinKey = "trace_id"

// maxInboundLen bounds a client-supplied X-Request-ID so it cannot inflate log
// lines unboundedly.
const maxInboundLen = 128

// Sanitize cleans a client-supplied trace id (e.g. an inbound X-Request-ID)
// before it is trusted for log correlation: it trims whitespace, drops control
// characters (CR/LF/tab/etc.) that could forge or split log lines, and bounds
// the length. Returns "" when nothing usable remains, so callers fall back to
// New(). The value is only ever used for log correlation, never for auth or
// routing (PR#479 F2).
func Sanitize(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxInboundLen {
		s = s[:maxInboundLen]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// New generates a 16-hex-char (8 random bytes) trace id. On the (practically
// impossible) rand failure it returns a fixed sentinel rather than erroring —
// a non-unique id is strictly better than failing the request for a log field.
func New() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// WithTraceID returns a child context carrying id.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext extracts the trace id, or "" if absent.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}
