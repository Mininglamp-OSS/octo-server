// Package accesslog provides a gin access-log formatter that scrubs secrets
// embedded in request URL paths before they are written to logs.
//
// Motivation: the incoming-webhook push route is /v1/incoming-webhooks/
// {webhook_id}/{token} — a Discord-style URL with a PLAINTEXT bearer token in
// the path (#246). It is also reachable via the shorter /v1/webhooks/ alias
// (#455). gin's default access logger writes the full request path, which would
// persist live tokens into access logs. Formatter mirrors gin's default line
// format exactly but masks the token segment for BOTH prefixes.
package accesslog

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	"github.com/gin-gonic/gin"
)

const (
	// maskedToken replaces a plaintext token segment in logged paths.
	maskedToken = "***"
)

// webhookPushPrefixes are the push-route prefixes whose trailing path segment is
// a plaintext webhook token and therefore must be masked. The canonical path is
// /v1/incoming-webhooks/; /v1/webhooks/ is the shorter alias (#455). Both reach
// the same token-bearing handlers, so both must be scrubbed — masking only one
// would leak tokens via the alias (the #246 token-in-path concern).
var webhookPushPrefixes = []string{
	"/v1/incoming-webhooks/",
	"/v1/webhooks/",
}

// secretQueryInPath masks the VALUE of credential-bearing query parameters.
//
// gin builds the logged path as `URL.Path + "?" + URL.RawQuery` (logger.go), so a
// credential passed as a query parameter lands verbatim on the access-log line —
// the same #246 concern that created this package, just via the query string
// instead of a path segment.
//
//   - poll_secret: the scan-login poll credential. Holding it together with the
//     `uuid` (also on that line) is exactly what lets a caller read `auth_code`
//     out of GET /v1/user/loginstatus, so logging both defeats the gate for
//     anyone with log read access. Valid for scanLoginPollSecretTTL (12 min).
//   - auth_code: grant_login promotes this one-time authorization into a full
//     login credential after the authenticated scanner confirms.
//   - encrypt: Signal key material supplied by the scanner at confirmation.
//
// The value is everything up to the next separator; the parameter NAME is kept so
// the line stays useful for correlation. Case-insensitive for the same reason as
// ScrubPath — scrubbing is the security control, so it must survive casing
// variants that a router would 404 but the logger would still print.
// OAuth2/OIDC additions: the callback carries `code` and `state` as query
// parameters, and the token/userinfo calls of a plain-OAuth2 IdP carry
// `client_secret` / `access_token` in the query string (that IdP's own reference
// implementation puts them there and documents no alternative). All of these are
// redeemable credential material, so they must never reach a log sink:
//   - code           exchangeable for a token; on an IdP without PKCE its only
//     protection is the client_secret
//   - state          the single-use CSRF binding
//   - client_secret  long-lived credential; re-issuing it is a vendor process
//   - access_token / id_token / refresh_token  bearer material
//
// Alternation order matters: longer names must precede shorter ones that are
// their suffix, otherwise `auth_code=` would be matched by the `code` branch and
// only its tail would be masked.
var secretQueryInPath = regexp.MustCompile(`(?i)\b((?:poll_secret|auth_code|client_secret|refresh_token|access_token|id_token|encrypt|code|state)=)[^&\s?"']*`)

// authCodeInPath masks the redeemable scan-login auth code, which travels as a
// path segment.
//
// POST /v1/user/login_authcode/{code} treats {code} as one half of the login
// credential: the matching browser poll_secret is also required, and a valid
// code is atomically consumed before login side effects. Rejected requests can
// still contain an unspent code, so the path segment must always be masked.
var authCodeInPath = regexp.MustCompile(`(?i)(/v1/user/login_authcode/)[^/\s?"']+`)

// scrubSecretPatterns applies the non-webhook maskers. Split out so both the
// access-log path (ScrubPath) and the panic-dump path (scrubbingErrorWriter) run
// the identical set — the two sinks drifting apart is how #246 happened twice.
func scrubSecretPatterns(s string) string {
	s = secretQueryInPath.ReplaceAllString(s, "${1}"+maskedToken)
	s = authCodeInPath.ReplaceAllString(s, "${1}"+maskedToken)
	return s
}

// ScrubPath masks secrets embedded in a request path before it is logged.
//
// It targets the incoming-webhook push routes
// /v1/incoming-webhooks/{webhook_id}/{token} and the /v1/webhooks/... alias: the
// trailing {token} segment (and anything after it, e.g. a stray query string) is
// replaced with "***". The non-secret {webhook_id} is preserved so logs remain
// useful for correlation. Any path without a push prefix, or with no token
// segment, is returned unchanged.
//
// The prefix match is case-INSENSITIVE: gin logs c.Request.URL.Path verbatim
// for every request including 404s (the access logger runs after c.Next()
// regardless of route match), so a request to /V1/INCOMING-WEBHOOKS/... that
// 404s on the case-sensitive router would otherwise still log the token
// unmasked. Scrubbing is the security control here, so it must survive
// path-casing variants. Original casing of the prefix/webhook_id is preserved
// in the output; only the token segment is replaced.
func ScrubPath(path string) string {
	for _, prefix := range webhookPushPrefixes {
		if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
			continue
		}
		rest := path[len(prefix):]
		// rest == "{webhook_id}/{token}[?query]". The token is everything after
		// the first '/'. No '/' means only {webhook_id} is present — nothing to
		// mask.
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return scrubSecretPatterns(path)
		}
		return scrubSecretPatterns(path[:len(prefix)] + rest[:slash] + "/" + maskedToken)
	}
	return scrubSecretPatterns(path)
}

// tokenInText matches a webhook push path embedded anywhere in free-form log
// text and captures the prefix up to and including the webhook_id segment; the
// trailing token segment is everything up to the next whitespace / quote / ? .
// Used to scrub tokens from the gin.Recovery panic dump, which writes the full
// request line (httputil.DumpRequest) including the token-bearing path.
// Case-insensitive ((?i)) for the same reason as ScrubPath — a non-canonical
// path casing must not slip a token past the mask. Matches BOTH the canonical
// /v1/incoming-webhooks/ and the /v1/webhooks/ alias (#455) via the optional
// "incoming-" group. This regex and webhookPushPrefixes are two independent
// encodings of the same prefix set — TestTokenInText_CoversEveryPrefix enforces
// that the regex masks every prefix in the slice, so adding a prefix to the
// slice without widening the regex fails the build (PR #456 review).
var tokenInText = regexp.MustCompile(`(?i)(/v1/(?:incoming-)?webhooks/[^/\s?"']+/)[^\s?"']+`)

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
	// Same maskers the access-log line gets. The panic dump embeds the full
	// request line, so a poll_secret query or an auth-code path segment reaches
	// this sink too — and it is the sink that persists on failure, which is
	// exactly when a credential is most likely still live.
	scrubbed = []byte(scrubSecretPatterns(string(scrubbed)))
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
	// trace_id 串联同一次请求的各分段慢日志(auth/handler/service);由 reqid
	// 中间件写入 gin.Context,经 param.Keys 透出。
	// 追加在行尾(path 之后)而非插在中间:保持原有按 `|` 分列的位置不变,避免
	// 破坏任何按列解析的下游日志管道(PR#479 F1)。缺失时整段省略。
	traceID, _ := param.Keys[reqid.GinKey].(string)
	traceSuffix := ""
	if traceID != "" {
		traceSuffix = " | trace_id=" + traceID
	}
	// bot 归因(issue #696)。2026-08-05 的限流事故里,access log 能看出"某个出网 IP
	// 在冲刷 /v1/bot/typing",但看不出**是哪个 bot**——最后只能靠 Redis 里残留的
	// typing_start:* key 去猜,无法确证。这一段补上那最后一跳。
	//
	// 只有经过 bot 鉴权中间件的请求才带此字段(modules/bot_api.authBot 写入
	// CtxKeyRobotID),所以:
	//   - 普通用户路由的日志行**逐字节不变**——它们没有这个 key;
	//   - /v1/bot/register 拿不到(它跑在 authBot 之前,token 无效时压根没有 bot 身份),
	//     那类失败仍只能按 IP 归因,这是已知限制而非遗漏。
	//
	// 与 offenders ZSet 的分工:ZSet 只记**被限流拒绝**的 bot;这里覆盖它看不到的
	// 部分——未触发限流但异常的请求(参数错误的 4xx、慢请求),以及限流整层关闭时。
	//
	// 记 robotID 而非 token:robotID 是公开标识,token 是凭据,永不入日志(#246 教训)。
	botSuffix := ""
	if botID, _ := param.Keys[ctxKeyRobotID].(string); botID != "" {
		botSuffix = " | bot=" + botID
	}
	return fmt.Sprintf("[GIN] %v |%s %3d %s| %13v | %15s |%s %-7s %s %#v%s%s\n%s",
		param.TimeStamp.Format("2006/01/02 - 15:04:05"),
		statusColor, param.StatusCode, resetColor,
		param.Latency,
		param.ClientIP,
		methodColor, param.Method, resetColor,
		ScrubPath(param.Path),
		traceSuffix,
		botSuffix,
		param.ErrorMessage,
	)
}

// ctxKeyRobotID 必须与 modules/bot_api.CtxKeyRobotID 保持一致。
//
// 刻意用字面量而不 import 那个常量:pkg/ 不应依赖 modules/。
// 两边一致性由 TestCtxKeyRobotIDMatchesBotAPI 源码守卫钉住——否则哪天有人改了
// bot_api 侧的常量,这里会安静地永远取不到值,而"日志里没有 bot 字段"和
// "这个请求不是 bot 发的"在线上看起来一模一样。
const ctxKeyRobotID = "robot_id"
