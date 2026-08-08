package accesslog

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestScrubPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "push path masks token, keeps webhook_id",
			in:   "/v1/incoming-webhooks/iwh_abc123/deadbeefcafetoken",
			want: "/v1/incoming-webhooks/iwh_abc123/***",
		},
		{
			name: "push path with trailing query is fully masked after id",
			in:   "/v1/incoming-webhooks/iwh_abc123/secrettoken?foo=bar",
			want: "/v1/incoming-webhooks/iwh_abc123/***",
		},
		{
			name: "uppercase prefix still masks token (case-insensitive), original case kept",
			in:   "/V1/INCOMING-WEBHOOKS/iwh_abc123/secrettoken",
			want: "/V1/INCOMING-WEBHOOKS/iwh_abc123/***",
		},
		{
			name: "mixed-case prefix still masks token",
			in:   "/v1/Incoming-Webhooks/iwh_abc123/secrettoken",
			want: "/v1/Incoming-Webhooks/iwh_abc123/***",
		},
		{
			name: "webhook_id only, no token segment, unchanged",
			in:   "/v1/incoming-webhooks/iwh_abc123",
			want: "/v1/incoming-webhooks/iwh_abc123",
		},
		{
			name: "alias push path masks token, keeps webhook_id (#455)",
			in:   "/v1/webhooks/iwh_abc123/deadbeefcafetoken",
			want: "/v1/webhooks/iwh_abc123/***",
		},
		{
			name: "alias adapter suffix is fully masked after id (#455)",
			in:   "/v1/webhooks/iwh_abc123/secrettoken/github",
			want: "/v1/webhooks/iwh_abc123/***",
		},
		{
			name: "alias path with trailing query is fully masked after id (#455)",
			in:   "/v1/webhooks/iwh_abc123/secrettoken?foo=bar",
			want: "/v1/webhooks/iwh_abc123/***",
		},
		{
			name: "alias uppercase prefix still masks token, original case kept (#455)",
			in:   "/V1/WEBHOOKS/iwh_abc123/secrettoken",
			want: "/V1/WEBHOOKS/iwh_abc123/***",
		},
		{
			name: "alias webhook_id only, no token segment, unchanged (#455)",
			in:   "/v1/webhooks/iwh_abc123",
			want: "/v1/webhooks/iwh_abc123",
		},
		{
			name: "management route is not a push path, unchanged",
			in:   "/v1/groups/g_1/incoming-webhooks",
			want: "/v1/groups/g_1/incoming-webhooks",
		},
		{
			name: "singular /v1/webhook (different module) is not a push path, unchanged",
			in:   "/v1/webhook/github",
			want: "/v1/webhook/github",
		},
		{
			name: "unrelated path unchanged",
			in:   "/v1/message/send",
			want: "/v1/message/send",
		},
		{
			name: "empty path unchanged",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScrubPath(tt.in); got != tt.want {
				t.Fatalf("ScrubPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestTokenInText_CoversEveryPrefix pins the single-source invariant flagged in
// PR #456 review: webhookPushPrefixes (used by ScrubPath) and the panic-dump
// tokenInText regex independently encode the set of webhook push prefixes. They
// must stay in sync — a prefix added to the slice but not covered by the regex
// would silently leak tokens from the gin.Recovery panic dump. This fails loudly
// if the regex stops covering any prefix in the slice.
func TestTokenInText_CoversEveryPrefix(t *testing.T) {
	const token = "SyncGuardTokenMustMask"
	for _, prefix := range webhookPushPrefixes {
		line := []byte("POST " + prefix + "wid123/" + token + " HTTP/1.1")
		got := string(tokenInText.ReplaceAll(line, []byte("${1}***")))
		if strings.Contains(got, token) {
			t.Fatalf("prefix %q: panic-dump regex must mask the token but did not "+
				"(regex out of sync with webhookPushPrefixes); got %q", prefix, got)
		}
		want := prefix + "wid123/***"
		if !strings.Contains(got, want) {
			t.Fatalf("prefix %q: expected masked path %q in %q", prefix, want, got)
		}
	}
}

// TestScrubPath_NeverLeaksToken guards the security invariant: for any push
// path, the plaintext token must not survive scrubbing.
func TestScrubPath_NeverLeaksToken(t *testing.T) {
	const token = "S3cr3tT0k3nMustNotLeak"
	got := ScrubPath("/v1/incoming-webhooks/iwh_xyz/" + token)
	if strings.Contains(got, token) {
		t.Fatalf("scrubbed path %q still contains the token", got)
	}
}

// TestErrorWriter_ScrubsPanicDump simulates gin.Recovery's panic dump (a full
// HTTP request line via httputil.DumpRequest) and asserts the token is masked
// while the rest of the text passes through unchanged.
func TestErrorWriter_ScrubsPanicDump(t *testing.T) {
	const token = "deadbeefTOKENmustNotLeak"
	var buf bytes.Buffer
	w := NewErrorWriter(&buf)

	dump := "[Recovery] panic recovered:\n" +
		"POST /v1/incoming-webhooks/iwh_abc/" + token + " HTTP/1.1\r\n" +
		"Host: example\r\n"
	n, err := w.Write([]byte(dump))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(dump) {
		t.Fatalf("Write returned n=%d, want %d (must report full consumption)", n, len(dump))
	}
	got := buf.String()
	if strings.Contains(got, token) {
		t.Fatalf("panic dump still leaks token: %q", got)
	}
	if !strings.Contains(got, "/v1/incoming-webhooks/iwh_abc/***") {
		t.Fatalf("expected masked path in dump, got: %q", got)
	}
	if !strings.Contains(got, "Host: example") {
		t.Fatalf("non-token content must pass through unchanged, got: %q", got)
	}
}

// TestErrorWriter_ScrubsUppercasePath asserts the panic-dump scrubber is also
// case-insensitive (a non-canonical path casing must not leak the token).
func TestErrorWriter_ScrubsUppercasePath(t *testing.T) {
	const token = "UPPERtokenLEAK"
	var buf bytes.Buffer
	w := NewErrorWriter(&buf)
	if _, err := w.Write([]byte("POST /V1/INCOMING-WEBHOOKS/iwh_x/" + token + " HTTP/1.1\r\n")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if strings.Contains(buf.String(), token) {
		t.Fatalf("uppercase-path panic dump leaks token: %q", buf.String())
	}
}

// TestErrorWriter_ScrubsAliasPanicDump asserts the panic-dump scrubber masks the
// token for the /v1/webhooks/ alias too (#455) — the alias reaches the same
// token-bearing handlers, so a panic while serving it must not leak the token.
func TestErrorWriter_ScrubsAliasPanicDump(t *testing.T) {
	const token = "aliasPANICtokenLEAK"
	var buf bytes.Buffer
	w := NewErrorWriter(&buf)
	dump := "[Recovery] panic recovered:\n" +
		"POST /v1/webhooks/iwh_abc/" + token + " HTTP/1.1\r\n" +
		"Host: example\r\n"
	if _, err := w.Write([]byte(dump)); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, token) {
		t.Fatalf("alias panic dump leaks token: %q", got)
	}
	if !strings.Contains(got, "/v1/webhooks/iwh_abc/***") {
		t.Fatalf("expected masked alias path in dump, got: %q", got)
	}
}

// TestErrorWriter_DoesNotScrubSingularWebhook guards against a false positive:
// the singular /v1/webhook* routes (the separate modules/webhook module) carry
// no path token and must pass through the alias-aware regex unchanged (#455).
func TestErrorWriter_DoesNotScrubSingularWebhook(t *testing.T) {
	var buf bytes.Buffer
	w := NewErrorWriter(&buf)
	const text = "POST /v1/webhook/github HTTP/1.1\r\nHost: example\r\n"
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if buf.String() != text {
		t.Fatalf("singular /v1/webhook path mutated: got %q want %q", buf.String(), text)
	}
}

// TestErrorWriter_PassesThroughUnrelated ensures unrelated text is written
// byte-for-byte (no accidental mutation).
func TestErrorWriter_PassesThroughUnrelated(t *testing.T) {
	var buf bytes.Buffer
	w := NewErrorWriter(&buf)
	const text = "GET /v1/message/send HTTP/1.1\r\nplain panic stack frame\n"
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if buf.String() != text {
		t.Fatalf("unrelated text mutated: got %q want %q", buf.String(), text)
	}
}

// TestFormatter_ScrubsToken asserts the wired formatter masks the token in the
// rendered access-log line.
func TestFormatter_ScrubsToken(t *testing.T) {
	gin.DisableConsoleColor()
	const token = "tok_DEADBEEF"
	line := Formatter(gin.LogFormatterParams{
		TimeStamp:  time.Unix(0, 0).UTC(),
		StatusCode: 200,
		Latency:    time.Millisecond,
		ClientIP:   "127.0.0.1",
		Method:     "POST",
		Path:       "/v1/incoming-webhooks/iwh_1/" + token,
	})
	if strings.Contains(line, token) {
		t.Fatalf("formatted log line leaks token: %q", line)
	}
	if !strings.Contains(line, "/v1/incoming-webhooks/iwh_1/***") {
		t.Fatalf("formatted log line missing masked path: %q", line)
	}
}

// TestFormatterAppendsBotID —— bot 归因字段（issue #696）。
func TestFormatterAppendsBotID(t *testing.T) {
	base := gin.LogFormatterParams{
		TimeStamp: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		Method:    "POST",
		Path:      "/v1/bot/typing",
		Latency:   time.Millisecond,
		ClientIP:  "1.2.3.4",
	}

	t.Run("bot 请求带 bot= 字段", func(t *testing.T) {
		p := base
		p.Keys = map[string]any{ctxKeyRobotID: "ytgeo_bot"}
		require.Contains(t, Formatter(p), "| bot=ytgeo_bot")
	})

	// 载荷性：普通用户路由**不得**因本改动改变日志行。它们没有 robot_id key，
	// 所以整段省略——按列解析的下游管道不受影响（同 trace_id 的既有约定）。
	t.Run("非 bot 请求逐字节不变", func(t *testing.T) {
		p := base
		p.Path = "/v1/users/self"
		p.Keys = map[string]any{"uid": "10000"} // 用户 uid 不应被记入
		out := Formatter(p)
		require.NotContains(t, out, "bot=")
		require.NotContains(t, out, "10000", "用户 uid 不属于 access log")
	})

	t.Run("key 缺失或类型不对时省略", func(t *testing.T) {
		p := base
		p.Keys = map[string]any{ctxKeyRobotID: 12345} // 非 string
		require.NotContains(t, Formatter(p), "bot=")

		p.Keys = nil
		require.NotContains(t, Formatter(p), "bot=")
	})
}

// TestCtxKeyRobotIDMatchesBotAPI 钉住跨包的字符串一致性。
//
// pkg/ 不 import modules/，所以这里的 key 是字面量。若 bot_api 侧改了常量而这里没跟，
// 症状是"日志里永远没有 bot 字段"——而那和"这个请求不是 bot 发的"在线上无法区分，
// 属于静默失效，只能靠守卫拦。
func TestCtxKeyRobotIDMatchesBotAPI(t *testing.T) {
	src, err := os.ReadFile("../../modules/bot_api/auth.go")
	require.NoError(t, err)

	m := regexp.MustCompile(`CtxKeyRobotID\s*=\s*"([^"]+)"`).FindStringSubmatch(string(src))
	require.Len(t, m, 2, "在 modules/bot_api/auth.go 里找不到 CtxKeyRobotID 定义，守卫失效")
	require.Equal(t, m[1], ctxKeyRobotID,
		"bot_api.CtxKeyRobotID 已改为 %q，但 pkg/accesslog 的字面量仍是 %q —— "+
			"access log 会安静地不再输出 bot 字段", m[1], ctxKeyRobotID)
}

// TestScrubPath_MasksPollSecret pins that the scan-login poll credential never
// reaches an access-log line.
//
// gin composes the logged path as URL.Path + "?" + URL.RawQuery, so a credential
// passed as a query parameter lands verbatim on the log line — the same class of
// exposure as #246, just via the query string. Holding poll_secret together with
// the uuid (also on that line) is exactly what lets a caller read auth_code out of
// /v1/user/loginstatus, so logging both would defeat the gate for anyone with log
// read access.
func TestScrubPath_MasksPollSecret(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "loginstatus poll",
			in:   "/v1/user/loginstatus?uuid=U1&poll_secret=S3CR3T",
			want: "/v1/user/loginstatus?uuid=U1&poll_secret=***",
		},
		{
			name: "secret first, uuid after",
			in:   "/v1/user/loginstatus?poll_secret=S3CR3T&uuid=U1",
			want: "/v1/user/loginstatus?poll_secret=***&uuid=U1",
		},
		{
			name: "casing variant still masked",
			in:   "/V1/USER/LOGINSTATUS?POLL_SECRET=S3CR3T",
			want: "/V1/USER/LOGINSTATUS?POLL_SECRET=***",
		},
		{
			name: "empty value is a no-op, not a crash",
			in:   "/v1/user/loginstatus?uuid=U1&poll_secret=",
			want: "/v1/user/loginstatus?uuid=U1&poll_secret=***",
		},
		{
			name: "uuid alone is preserved for correlation",
			in:   "/v1/user/loginstatus?uuid=U1",
			want: "/v1/user/loginstatus?uuid=U1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScrubPath(tc.in); got != tc.want {
				t.Errorf("ScrubPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestScrubPath_MasksAuthCode pins the redeemable scan-login code, which travels
// as a path segment.
//
// loginWithAuthCode deletes the code on success, so a logged line for a successful
// redemption is already spent. But the early returns for an IM failure or a
// destroyed account happen BEFORE that delete, so a failed redemption leaves a
// still-valid code in the log for up to ScanLoginAuthCodeTTL.
func TestScrubPath_MasksAuthCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "redeem path",
			in:   "/v1/user/login_authcode/A-REDEEMABLE-CODE",
			want: "/v1/user/login_authcode/***",
		},
		{
			name: "with query suffix",
			in:   "/v1/user/login_authcode/A-CODE?flag=1",
			want: "/v1/user/login_authcode/***?flag=1",
		},
		{
			name: "casing variant still masked",
			in:   "/V1/USER/LOGIN_AUTHCODE/A-CODE",
			want: "/V1/USER/LOGIN_AUTHCODE/***",
		},
		{
			name: "prefix without a code is untouched",
			in:   "/v1/user/login_authcode/",
			want: "/v1/user/login_authcode/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScrubPath(tc.in); got != tc.want {
				t.Errorf("ScrubPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestErrorWriter_MasksScanLoginSecrets covers the OTHER sink. gin.Recovery's
// panic dump embeds the full request line and bypasses the access logger
// entirely — and it is the sink that persists on failure, which is exactly when a
// credential is most likely still live.
func TestErrorWriter_MasksScanLoginSecrets(t *testing.T) {
	var buf bytes.Buffer
	w := NewErrorWriter(&buf)

	dump := "panic serving request\nGET /v1/user/loginstatus?uuid=U1&poll_secret=S3CR3T HTTP/1.1\n" +
		"POST /v1/user/login_authcode/A-CODE HTTP/1.1\n"
	if _, err := w.Write([]byte(dump)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := buf.String()
	for _, leaked := range []string{"S3CR3T", "A-CODE"} {
		if strings.Contains(got, leaked) {
			t.Errorf("panic dump leaked %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "uuid=U1") {
		t.Errorf("uuid should survive for correlation: %s", got)
	}
}
